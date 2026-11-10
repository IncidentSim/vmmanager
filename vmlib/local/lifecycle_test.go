//go:build linux

package local_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

// treeHash fingerprints names, sizes and contents under root, so "nothing was
// mutated" is a byte-exact comparison.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			h := sha256.Sum256(data)
			lines = append(lines, rel+" "+hex.EncodeToString(h[:]))
		} else {
			lines = append(lines, rel+"/")
		}
		return nil
	})
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Finding 1: Close must not release ownership while an admitted operation is
// still running, or a second owner could reuse the ID and have its workspace
// deleted by the first owner's late cleanup.
func TestCloseDrainsInFlightPrepareBeforeReleasingOwnership(t *testing.T) {
	r := newRig(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.cfg.CopyFile = func(src, dst string) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("copy failed after Close began")
	}
	a, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	prepErr := make(chan error, 1)
	go func() { _, err := prep(a, "x"); prepErr <- err }()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- a.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while an admitted operation was still running")
	case <-time.After(150 * time.Millisecond):
	}
	// Ownership is still held: a second owner must not be admitted yet.
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("second owner admitted while the first still has operations in flight")
	}
	// New admissions are refused as soon as Close has begun.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := a.Inspect(ctx, "x")
		if errors.Is(err, local.ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation admitted after Close began: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := a.Prepare(ctx, vmlib.PrepareRequest{VMID: "y", ImageID: "base", Profile: "small"}); !errors.Is(err, local.ErrClosed) {
		t.Fatalf("Prepare after Close began: %v", err)
	}
	close(release)
	if err := <-prepErr; err == nil {
		t.Fatal("expected the copy failure")
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	// Only now may another owner proceed, and its state is untouched by A.
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatalf("second owner after drain: %v", err)
	}
	defer b.Close()
	if _, err := prep(b, "x"); err == nil {
		// CopyFile still fails by design; the point is no stale cleanup from A.
		t.Fatal("expected copy failure for B as well")
	}
}

// The repro from the review: A is suspended in CopyFile, A closes, B takes over
// the roots and reuses the ID; A's late failure cleanup must not touch B.
func TestLateCleanupOfClosedOwnerCannotDeleteNewOwnersWorkspace(t *testing.T) {
	r := newRig(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls int32
	r.cfg.CopyFile = func(src, dst string) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(entered)
			<-release
			return errors.New("A copy failed")
		}
		return os.WriteFile(dst, []byte("B-DISK"), 0o600)
	}
	a, _ := local.New(r.cfg)
	go prep(a, "x")
	<-entered
	go a.Close()
	// B cannot start until A has drained; poll for the moment it can.
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	var b *local.Backend
	for i := 0; i < 400; i++ {
		var err error
		if b, err = local.New(r.cfg); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b == nil {
		t.Fatal("B never became owner after A drained")
	}
	defer b.Close()
	if _, err := prep(b, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(r.root, "vms", "x", "disk.ext4")); err != nil {
		t.Fatal("the closed owner deleted the new owner's workspace")
	}
	if h, err := b.Inspect(ctx, "x"); err != nil || h.State != vmlib.HostRunning {
		t.Fatalf("%v %+v", err, h)
	}
}

// After Close returns, no operation may mutate the workspace, whatever is racing.
func TestConcurrentOperationsAndCloseNeverMutateAfterClosed(t *testing.T) {
	r := newRig(t, 2)
	a, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	prep(a, "keep")
	a.Start(ctx, "keep")
	r.receipt(0, inv, "success", "exited", "0")

	var closed atomic.Bool
	var wg sync.WaitGroup
	var lateOK atomic.Int32
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("v%d", (g+i)%3)
				wasClosed := closed.Load()
				var err error
				switch (g + i) % 5 {
				case 0:
					_, err = prep(a, id)
				case 1:
					_, err = a.Start(ctx, id)
				case 2:
					_, err = a.Publish(ctx, "keep", fmt.Sprintf("img%d", i%4))
				case 3:
					_, err = a.Release(ctx, id)
				case 4:
					_, err = a.Inspect(ctx, id)
				}
				if wasClosed && !errors.Is(err, local.ErrClosed) {
					lateOK.Add(1) // an operation that began after Close returned was served
				}
			}
		}(g)
	}
	time.Sleep(40 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	closed.Store(true)
	snap := treeHash(t, filepath.Dir(r.root))
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	if got := treeHash(t, filepath.Dir(r.root)); got != snap {
		t.Fatal("the workspace or image catalog changed after Close returned")
	}
	if lateOK.Load() != 0 {
		t.Fatalf("%d operations were served after Close returned", lateOK.Load())
	}
	// And a fresh owner reconciles cleanly.
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}

func TestWaitExitIsReleasedByClose(t *testing.T) {
	r := newRig(t, 1)
	a := r.backend()
	startRunning(t, r, a, "w")
	done := make(chan error, 1)
	go func() { _, err := a.WaitExit(ctx, "w"); done <- err }() // no receipt, no deadline
	time.Sleep(50 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, local.ErrClosed) {
			t.Fatalf("%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitExit kept Close from completing")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close never completed")
	}
}
