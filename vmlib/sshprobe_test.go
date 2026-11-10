package vmlib_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func fakeSSH(t *testing.T, exit string) (string, string) {
	d := t.TempDir()
	log := filepath.Join(d, "args")
	p := filepath.Join(d, "ssh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\nexit " + exit + "\n"
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p, log
}

func info() vmlib.SSHInfo {
	return vmlib.SSHInfo{Host: "10.213.0.2", Port: 22, User: "root", Bastion: &vmlib.Bastion{Host: "bastion.example.test", Port: 22, User: "jump"}}
}

func TestSSHCommandProberAuthenticatesThroughTheBastion(t *testing.T) {
	bin, log := fakeSSH(t, "0")
	p := vmlib.SSHCommandProber{Binary: bin, KeyFile: "/home/me/.ssh/id", KnownHostsFile: "/tmp/kh"}
	if err := p.Probe(context.Background(), info()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	args := strings.Fields(string(got))
	joined := strings.Join(args, " ")
	for _, want := range []string{"-J jump@bastion.example.test", "BatchMode=yes", "-i /home/me/.ssh/id", "UserKnownHostsFile=/tmp/kh", "root@10.213.0.2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if args[len(args)-1] != "true" {
		t.Fatalf("must run a real remote command, got %v", args)
	}
	if strings.Contains(joined, "StrictHostKeyChecking=no") {
		t.Fatal("host key checking disabled")
	}
}

func TestSSHCommandProberFailsWhenAuthenticationFails(t *testing.T) {
	bin, _ := fakeSSH(t, "255")
	if err := (vmlib.SSHCommandProber{Binary: bin}).Probe(context.Background(), info()); err == nil {
		t.Fatal("a failed ssh login counted as ready")
	}
}

func TestSSHCommandProberHonoursContext(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (vmlib.SSHCommandProber{Binary: p}).Probe(ctx, info()); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("probe did not stop with its context: %v", err)
	}
}

func TestLifecycleWaitSSHReadyIsPartOfTheInterface(t *testing.T) {
	s, _ := newSvc(t, 1)
	var l vmlib.Lifecycle = s
	create(t, l, "w")
	if _, err := l.WaitSSHReady(context.Background(), "nope"); err == nil {
		t.Fatal("expected not found")
	}
}
