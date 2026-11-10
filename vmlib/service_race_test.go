package vmlib_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Introspection must be safe and non-blocking while lifecycle operations run,
// including a Stop that is waiting for a delayed exit receipt.
func TestConcurrentIntrospectionAndLifecycleIsRaceFree(t *testing.T) {
	b := vmlibtest.New(4, "base")
	b.AutoExit = false
	s, _ := vmlib.NewService(b)
	for i := 0; i < 3; i++ {
		create(t, s, fmt.Sprintf("r%d", i))
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for g := 0; g < 6; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < 3; i++ {
					id := fmt.Sprintf("r%d", i)
					_, _ = s.Get(context.Background(), id)
					_, _ = s.SSHInfo(context.Background(), id)
				}
				_, _ = s.List(context.Background())
			}
		}()
	}
	var ops sync.WaitGroup
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("r%d", i)
		ops.Add(1)
		go func() {
			defer ops.Done()
			if i == 2 { // delete while the exit receipt is still delayed
				time.AfterFunc(40*time.Millisecond, func() { b.ExitVM(id, true) })
				_, _ = s.Delete(context.Background(), id)
				return
			}
			time.AfterFunc(40*time.Millisecond, func() { b.ExitVM(id, true) })
			if _, err := s.Stop(context.Background(), id); err != nil {
				t.Errorf("stop %s: %v", id, err)
			}
			if i == 1 {
				_, _ = s.Commit(context.Background(), id, "img1")
			}
		}()
	}
	// Get must not wait for a lifecycle op that is blocked on a receipt.
	done := make(chan struct{})
	go func() { _, _ = s.Get(context.Background(), "r0"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked behind a lifecycle operation")
	}
	ops.Wait()
	close(stop)
	readers.Wait()
}

func TestStopOfNeverStartedVMIsIdempotentAndCommitStillRejected(t *testing.T) {
	b := vmlibtest.New(1, "base")
	b.StartUncertainErr = nil
	s, _ := vmlib.NewService(b)
	// Prepared but never started: Create succeeded in Prepare, Start refused.
	b.StartErr = errors.New("launch refused")
	vm, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "n1", ImageID: "base", Profile: "small"})
	_ = vm
	if err == nil {
		t.Fatal("expected start failure")
	}
	// The definite pre-start failure released it; use the backend directly to
	// keep a prepared VM for the facade to adopt.
	b.StartErr = nil
	if _, err := b.Prepare(context.Background(), vmlib.PrepareRequest{VMID: "n2", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := s.Stop(context.Background(), "n2")
		if err != nil || got.State != vmlib.StateStopped {
			t.Fatalf("stop #%d: %v %+v", i, err, got)
		}
	}
	if _, err := s.Commit(context.Background(), "n2", "never"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("commit of never-booted VM: %v", err)
	}
	if b.Published != 0 {
		t.Fatal("published a never-booted disk")
	}
}
