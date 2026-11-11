package vmhelper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func secureLockDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSlotLockExcludesAnotherConnectionAndOnlyThatSlot(t *testing.T) {
	dir := secureLockDir(t)
	sc, log := fakeSystemctl(t, "inactive", "500ms")
	f, err := os.OpenFile(filepath.Join(dir, ".helper-slot-1.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	h := SystemdHandler{Systemctl: sc, LockDir: dir}
	if err := h.StartSlot(context.Background(), 1, 500); err == nil {
		t.Fatal("parallel same-slot transaction accepted")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("systemctl called without exclusion")
	}
	if err := h.StartSlot(context.Background(), 2, 500); err != nil {
		t.Fatalf("unrelated slot blocked: %v", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := h.StartSlot(context.Background(), 1, 500); err != nil {
		t.Fatalf("lock not reusable: %v", err)
	}
}

func TestSlotTransactionHoldsLockAcrossSystemctlCalls(t *testing.T) {
	dir := secureLockDir(t)
	sc, log := fakeSystemctl(t, "inactive", "500ms")
	raw, err := os.ReadFile(sc)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	script := strings.Replace(string(raw), "case ", "case \"$*\" in *ActiveState*) touch "+entered+"; while [ ! -f "+release+" ]; do sleep 0.01; done;; esac\ncase ", 1)
	if err := os.WriteFile(sc, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := SystemdHandler{Systemctl: sc, LockDir: dir}
	done := make(chan error, 1)
	go func() { done <- h.StartSlot(ctx, 1, 500) }()
	defer func() { os.WriteFile(release, nil, 0600) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first handler did not reach systemctl")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.StartSlot(ctx, 1, 1000); err == nil {
		t.Fatal("second transaction entered")
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "ActiveState") != 1 || strings.Contains(string(calls), "set-property") {
		t.Fatalf("overlapping transaction: %s", calls)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := h.StartSlot(ctx, 1, 500); err != nil {
		t.Fatal("lock not released:", err)
	}
}

func TestSlotLockRejectsUnsafePathsBeforeSystemctl(t *testing.T) {
	for _, kind := range []string{"directory writable", "lock symlink", "lock writable", "lock directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := secureLockDir(t)
			sc, log := fakeSystemctl(t, "inactive", "500ms")
			path := filepath.Join(dir, ".helper-slot-0.lock")
			switch kind {
			case "directory writable":
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
			case "lock symlink":
				if err := os.Symlink(filepath.Join(dir, "target"), path); err != nil {
					t.Fatal(err)
				}
			case "lock writable":
				if err := os.WriteFile(path, nil, 0666); err != nil {
					t.Fatal(err)
				}
				os.Chmod(path, 0666)
			case "lock directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := (SystemdHandler{Systemctl: sc, LockDir: dir}).StartSlot(context.Background(), 0, 500)
			if err == nil {
				t.Fatal("unsafe lock accepted")
			}
			if raw, _ := os.ReadFile(log); strings.TrimSpace(string(raw)) != "" {
				t.Fatal("systemctl called")
			}
		})
	}
}
