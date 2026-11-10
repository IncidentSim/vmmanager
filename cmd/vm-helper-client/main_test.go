package main

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

type rec struct {
	mu    sync.Mutex
	slots []int
}

func (r *rec) StartSlot(_ context.Context, s int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots = append(r.slots, s)
	return nil
}

func TestClientRejectsAnythingButStartSlot(t *testing.T) {
	dialed := false
	dial := func() (io.ReadWriteCloser, error) { dialed = true; return nil, io.EOF }
	for _, args := range [][]string{{}, {"start-slot"}, {"stop-slot", "1"}, {"start-slot", "x"}, {"start-slot", "-1"}, {"start-slot", "64"},
		{"start-slot", "01"}, {"start-slot", "1", "2"}, {"start-slot", "1;reboot"}} {
		if err := run(args, dial); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if dialed {
		t.Fatal("connected for an invalid request")
	}
}

func TestClientTalksToTheRealServerLoop(t *testing.T) {
	h := &rec{}
	dial := func() (io.ReadWriteCloser, error) {
		c, s := net.Pipe()
		go func() { _ = vmhelper.Serve(context.Background(), s, s, h, 2); s.Close() }()
		return c, nil
	}
	if err := run([]string{"start-slot", "1"}, dial); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"start-slot", "2"}, dial); err == nil { // beyond the pool's MAX_SLOTS
		t.Fatal("server accepted a slot beyond the pool")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.slots) != 1 || h.slots[0] != 1 {
		t.Fatalf("%v", h.slots)
	}
}
