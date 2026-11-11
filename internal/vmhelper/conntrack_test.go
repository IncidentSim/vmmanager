package vmhelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPublicStartFlushesEveryTimeUnderSlotLock(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		t.Run(state, func(t *testing.T) {
			sc, log := fakeSystemctl(t, state, "500ms")
			dir := secureLockDir(t)
			h := SystemdHandler{Systemctl: sc, LockDir: dir, PublicEnabled: true}
			h.RunNetworkCommand = func(ctx context.Context, path string, args ...string) ([]byte, error) {
				if path != "/usr/local/libexec/fc-vm-network" || !reflect.DeepEqual(args, []string{"flush-conntrack", "3"}) {
					t.Fatalf("untrusted command: %q %q", path, args)
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				f, err := os.OpenFile(fmt.Sprintf("%s/.helper-slot-3.lock", dir), os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
					t.Fatal("flush ran without slot lock")
				}
				fLog, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer fLog.Close()
				_, err = fmt.Fprintln(fLog, "flush-conntrack 3")
				return nil, err
			}
			for range 2 {
				if err := h.StartSlot(context.Background(), 3, 500); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			want := "show --property=ActiveState --value firecracker-vm@3.service\nflush-conntrack 3\nset-property --runtime --no-ask-password firecracker-vm@3.service CPUQuota=50%\nshow --property=CPUQuotaPerSecUSec --value firecracker-vm@3.service\nstart --no-ask-password firecracker-vm@3.service\n"
			if string(raw) != strings.Repeat(want, 2) {
				t.Fatalf("transaction order:\n%s", raw)
			}
		})
	}
}

func TestPublicFlushFailureStopsBeforeQuotaAndReleasesLock(t *testing.T) {
	sc, log := fakeSystemctl(t, "inactive", "500ms")
	sentinel := errors.New("conntrack failed")
	h := SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t), PublicEnabled: true, RunNetworkCommand: func(context.Context, string, ...string) ([]byte, error) { return []byte("denied\n"), sentinel }}
	if err := h.StartSlot(context.Background(), 0, 500); !errors.Is(err, sentinel) {
		t.Fatalf("flush error not returned: %v", err)
	}
	raw, _ := os.ReadFile(log)
	if strings.Count(string(raw), "\n") != 1 || !strings.Contains(string(raw), "ActiveState") {
		t.Fatalf("continued after flush failure: %s", raw)
	}
	h.PublicEnabled = false
	if err := h.StartSlot(context.Background(), 0, 500); err != nil {
		t.Fatalf("lock not released/disabled flush not skipped: %v", err)
	}
}

func TestPublicActiveAndBusyNeverFlush(t *testing.T) {
	for _, state := range []string{"active", "activating", "deactivating", "reloading", "maintenance", "", "inactive"} {
		t.Run(state, func(t *testing.T) {
			sc, log := fakeSystemctl(t, state, "500ms")
			dir := secureLockDir(t)
			if state == "inactive" {
				f, err := os.OpenFile(dir+"/.helper-slot-0.lock", os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			h := SystemdHandler{Systemctl: sc, LockDir: dir, PublicEnabled: true, RunNetworkCommand: func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("flush called on active/busy slot")
				return nil, nil
			}}
			if err := h.StartSlot(context.Background(), 0, 500); err == nil {
				t.Fatal("active/busy start accepted")
			}
			raw, _ := os.ReadFile(log)
			if strings.Contains(string(raw), "set-property") || strings.Contains(string(raw), "start ") {
				t.Fatalf("active/busy mutated: %s", raw)
			}
		})
	}
}
