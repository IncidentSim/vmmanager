package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

func svc(t *testing.T) *vmlib.Service {
	s, err := vmlib.NewService(vmlibtest.New(3, "base"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(t *testing.T, s vmlib.Lifecycle, args ...string) (string, error) {
	var out bytes.Buffer
	err := execute(context.Background(), s, args, &out)
	return out.String(), err
}

func TestLifecycleCommandsProduceJSON(t *testing.T) {
	s := svc(t)
	out, err := do(t, s, "create", "-id", "a", "-image", "base", "-profile", "small")
	if err != nil {
		t.Fatal(err)
	}
	var vm map[string]any
	if json.Unmarshal([]byte(out), &vm) != nil || vm["ID"] != "a" || vm["State"] != "running" {
		t.Fatalf("%s", out)
	}
	if out, err = do(t, s, "stop", "a"); err != nil || !strings.Contains(out, `"stopped"`) {
		t.Fatalf("%v %s", err, out)
	}
	if out, err = do(t, s, "commit", "a", "golden"); err != nil || !strings.Contains(out, "golden") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err = do(t, s, "commit", "a", "base"); err == nil {
		t.Fatal("overwrite accepted")
	}
	if out, err = do(t, s, "images"); err != nil || !strings.Contains(out, "golden") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err = do(t, s, "delete", "a"); err != nil || !strings.Contains(out, `"Deleted": true`) {
		t.Fatalf("%v %s", err, out)
	}
}

func TestSSHFormats(t *testing.T) {
	s := svc(t)
	do(t, s, "create", "-id", "a", "-image", "base", "-profile", "small")
	out, err := do(t, s, "ssh-info", "-format", "ssh", "-known-hosts", "/tmp/kh a", "-key", "/home/me/.ssh/id", "a")
	if err != nil || !strings.HasPrefix(out, "-J jump@bastion.example.test ") || !strings.HasSuffix(strings.TrimSpace(out), "root@172.16.1.2") {
		t.Fatalf("%v %q", err, out)
	}
	if !strings.Contains(out, "'UserKnownHostsFile=/tmp/kh a'") {
		t.Fatalf("not shell-quoted: %q", out)
	}
	out, err = do(t, s, "ssh-info", "-format", "ansible", "a")
	if err != nil || !strings.Contains(out, "ansible_host=172.16.1.2") || !strings.Contains(out, "ProxyJump=jump@bastion.example.test") {
		t.Fatalf("%v %q", err, out)
	}
	if out, err = do(t, s, "ssh-info", "a"); err != nil || strings.Contains(strings.ToLower(out), "private") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestUsageErrors(t *testing.T) {
	s := svc(t)
	for _, args := range [][]string{{}, {"nope"}, {"stop"}, {"commit", "a"}, {"create"}, {"delete", "a", "b"}} {
		if _, err := do(t, s, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
