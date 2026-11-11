package vmhelper

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSystemctlFailuresStopTheTransactionAndReleaseTheLock(t *testing.T) {
	for _, stage := range []string{"ActiveState", "set-property", "CPUQuotaPerSecUSec", "start --no-ask-password"} {
		t.Run(stage, func(t *testing.T) {
			sc, log := fakeSystemctl(t, "inactive", "500ms")
			raw, err := os.ReadFile(sc)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(string(raw), "case ", "case \"$*\" in *\""+stage+"\"*) exit 1;; esac\ncase ", 1)
			if err := os.WriteFile(sc, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			h := SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}
			if err := h.StartSlot(context.Background(), 0, 500); err == nil {
				t.Fatal("failed systemctl accepted")
			}
			calls, _ := os.ReadFile(log)
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if !strings.Contains(lines[len(lines)-1], stage) {
				t.Fatalf("continued after failure: %s", calls)
			}
			if err := os.WriteFile(sc, raw, 0700); err != nil {
				t.Fatal(err)
			}
			if err := h.StartSlot(context.Background(), 0, 500); err != nil {
				t.Fatal("lock not released after failure:", err)
			}
		})
	}
}
