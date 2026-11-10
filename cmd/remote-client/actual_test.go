package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/remote"
)

// Exercise both executables without accessing a VMM: a fixed local build must
// fail its socket preflight and report a failed job through authenticated TLS.
func TestActualServerAndClientNoVMM(t *testing.T) {
	ca, cert, key := clientPKI(t)
	dir := t.TempDir()
	serverBin := filepath.Join(dir, "builder")
	clientBin := filepath.Join(dir, "client")
	for _, build := range [][2]string{{serverBin, "../.."}, {clientBin, "."}} {
		if b, e := exec.Command("go", "build", "-o", build[0], build[1]).CombinedOutput(); e != nil {
			t.Fatalf("build: %v %s", e, b)
		}
	}
	probe, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := probe.Addr().String()
	probe.Close()
	cmd := exec.Command(serverBin, "-serve", "-listen", addr, "-tls-ca", ca, "-tls-cert", cert, "-tls-key", key, "-job-dir", filepath.Join(dir, "jobs"), "-socket", filepath.Join(dir, "absent.sock"))
	var serverLog bytes.Buffer
	cmd.Stdout = &serverLog
	cmd.Stderr = &serverLog
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("server exit: %v %s", e, serverLog.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("server did not stop")
		}
	}()
	cfg, e := remote.ClientTLS(ca, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	transport := &http.Transport{TLSClientConfig: cfg}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: time.Second}
	endpoint := "https://" + addr
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		r, e := hc.Get(endpoint + "/v1/builds/" + strings.Repeat("a", 32))
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 404 {
				ready = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server not ready")
	}
	flags := []string{"-endpoint", endpoint, "-tls-ca", ca, "-tls-cert", cert, "-tls-key", key}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log, e := exec.CommandContext(ctx, clientBin, flags...).CombinedOutput()
	if e == nil || !strings.Contains(string(log), "failed") || !strings.Contains(string(log), "Job ") {
		t.Fatalf("expected failed socket preflight: %v %s", e, log)
	}
	fields := strings.Fields(string(log))
	if len(fields) < 2 {
		t.Fatal(string(log))
	}
	id := fields[1]
	log, e = exec.CommandContext(ctx, clientBin, append(flags, "-status", id)...).CombinedOutput()
	if e != nil || !strings.Contains(string(log), `"state":"failed"`) {
		t.Fatalf("status: %v %s", e, log)
	}
}
