package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeCLIContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "builder")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"-serve", "-listen", "-tls-ca", "-tls-cert", "-tls-key", "-job-dir"} {
		if !strings.Contains(string(out), f) {
			t.Errorf("missing %s", f)
		}
	}
	out, err = exec.Command(binary, "-serve").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "tls-ca") {
		t.Fatalf("server did not fail closed: %v %s", err, out)
	}
}

func TestBuildChildReceivesFixedArgumentsAndGracefulCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "child")
	argsFile := filepath.Join(dir, "args")
	ready := filepath.Join(dir, "ready")
	cleaned := filepath.Join(dir, "cleaned")
	scriptBody := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\ntrap \"printf cleaned > '" + cleaned + "'; exit 0\" TERM\nprintf ready > '" + ready + "'\nwhile :; do sleep 0.01; done\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- buildChild(script, "/base", "/key", "/socket")(ctx, "/fixed/output", io.Discard) }()
	deadline := time.After(3 * time.Second)
	for {
		if _, e := os.Stat(ready); e == nil {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("child exited early: %v", e)
		case <-deadline:
			t.Fatal("child not ready")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	if _, e := os.Stat(cleaned); e != nil {
		t.Fatal("child cleanup skipped", e)
	}
	args, _ := os.ReadFile(argsFile)
	if string(args) != "-base\n/base\n-ssh-key\n/key\n-socket\n/socket\n-output\n/fixed/output\n" {
		t.Fatal(string(args))
	}
}
