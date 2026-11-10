package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/localcfg"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testBackendPolicy is a syntactically valid private policy; the test opener
// never touches any of these paths (the deployed binary never contains a fake).
var testBackendPolicy = json.RawMessage(`{"work_root":"/w","images_dir":"/i","run_dir":"/r","max_vms":2,"helper":"/usr/local/libexec/fc-vm-helper","ssh_user":"root","storage_mount":"/s","max_total_mem_mib":1024,"max_total_vcpu":2,"unit_mem_limit_mib":768,"guest_prefix":"10.213","pool_conf":"/p","profiles":[{"name":"small","vcpu":1,"mem_mib":256,"disk_mib":1024}]}`)

// fakeOpener hands the SAME in-memory backend to every start, like a host
// whose VMs survive an API restart. It counts lifetime-lock releases.
type fakeOpener struct {
	b      vmlib.Backend
	mu     sync.Mutex
	opens  int
	closes int
}

func (f *fakeOpener) open(c *localcfg.JSONConfig) (vmlib.Backend, func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	return f.b, func() error { f.mu.Lock(); f.closes++; f.mu.Unlock(); return nil }, nil
}
func (f *fakeOpener) counts() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.opens, f.closes }

// proc is one in-process vmapi run: the real run() with real config loading,
// DBOS on SQLite in the test state dir, HTTP/HTTPS listeners and controller.
type proc struct {
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startRun(path string, open backendOpener) *proc {
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{cancel: cancel, done: make(chan error, 1)}
	go func() { p.done <- run(ctx, path, open) }()
	return p
}

// stop requests graceful shutdown (what SIGTERM does) and waits for run to return.
func (p *proc) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	p.stopped = true
	select {
	case e := <-p.done:
		if e != nil {
			t.Errorf("vmapi shutdown: %v", e)
		}
	case <-time.After(20 * time.Second):
		t.Errorf("vmapi did not stop")
	}
}

// kill is the deferred safety net: cancel and wait, ignoring the result.
func (p *proc) kill() {
	if p.stopped {
		return
	}
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
	}
}

func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	b := &syncBuf{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_ = io.Discard
	return b
}
