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
	mu     sync.Mutex
	slots  []int
	quotas []uint32
}

func (r *rec) StartSlot(_ context.Context, s int, q uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots = append(r.slots, s)
	r.quotas = append(r.quotas, q)
	return nil
}

func TestClientRejectsAnythingButStartSlot(t *testing.T) {
	dialed := false
	dial := func() (io.ReadWriteCloser, error) { dialed = true; return nil, io.EOF }
	for _, args := range [][]string{{}, {"start-slot"}, {"stop-slot", "1"}, {"start-slot", "x"}, {"start-slot", "-1"}, {"start-slot", "64"},
		{"start-slot", "01"}, {"start-slot", "1", "2"}, {"start-slot", "1;reboot"},
		{"start-slot", "1"}, {"start-slot", "1", "0"}, {"start-slot", "1", "505"}, {"start-slot", "1", "-500"}, {"start-slot", "1", "0.5"},
		{"start-slot", "1", "500", "extra"}, {"start-slot", "1", "1e3"}, {"start-slot", "1", "99999999999"}} {
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
		go func() {
			_ = vmhelper.Serve(context.Background(), s, s, h, vmhelper.Limits{Slots: 2, MaxCPUMilli: 2000})
			s.Close()
		}()
		return c, nil
	}
	if err := run([]string{"start-slot", "1", "500"}, dial); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"start-slot", "2", "500"}, dial); err == nil { // beyond the pool's MAX_SLOTS
		t.Fatal("server accepted a slot beyond the pool")
	}
	if err := run([]string{"start-slot", "1", "2500"}, dial); err == nil { // above the pool's CPU ceiling
		t.Fatal("server accepted a quota above the pool ceiling")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.slots) != 1 || h.slots[0] != 1 || h.quotas[0] != 500 {
		t.Fatalf("%v", h.slots)
	}
}
