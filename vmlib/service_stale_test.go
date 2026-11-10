package vmlib_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// delayedGet starts a Get whose backend read is captured BEFORE other operations
// run but returned AFTER they finish. It returns once the read is parked.
func delayedGet(t *testing.T, b *vmlibtest.Backend, s *vmlib.Service, id string) (result chan struct {
	vm  vmlib.VM
	err error
}, release func()) {
	t.Helper()
	parked, gate := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	b.OnInspect = func(string) {
		if armed.CompareAndSwap(true, false) {
			close(parked)
			<-gate
		}
	}
	result = make(chan struct {
		vm  vmlib.VM
		err error
	}, 1)
	go func() {
		vm, err := s.Get(context.Background(), id)
		result <- struct {
			vm  vmlib.VM
			err error
		}{vm, err}
	}()
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("Get never reached the backend")
	}
	return result, func() { close(gate) }
}

func wait(t *testing.T, ch chan struct {
	vm  vmlib.VM
	err error
}) (vmlib.VM, error) {
	t.Helper()
	select {
	case r := <-ch:
		return r.vm, r.err
	case <-time.After(3 * time.Second):
		t.Fatal("delayed Get never returned")
		return vmlib.VM{}, nil
	}
}

// Finding 4: a stale read applied late must not turn a stopped VM back into "running".
func TestDelayedGetCannotRegressAStoppedVM(t *testing.T) {
	b := vmlibtest.New(1, "base")
	s, _ := vmlib.NewService(b)
	create(t, s, "g1")
	res, release := delayedGet(t, b, s, "g1") // captured HostRunning
	if vm, err := s.Stop(context.Background(), "g1"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("%v %+v", err, vm)
	}
	release()
	vm, err := wait(t, res)
	if err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("stale Get regressed the state: %v %+v", err, vm)
	}
	b.OnInspect = nil
	if got, _ := s.Get(context.Background(), "g1"); got.State != vmlib.StateStopped {
		t.Fatalf("state stayed regressed: %+v", got)
	}
	// A repeated Stop (served from the cached exit) reports the true state.
	if vm, err := s.Stop(context.Background(), "g1"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("repeated Stop returned nil error with %s", vm.State)
	}
}

func TestDelayedGetCannotRegressACommittedVM(t *testing.T) {
	b := vmlibtest.New(1, "base")
	s, _ := vmlib.NewService(b)
	create(t, s, "c1")
	res, release := delayedGet(t, b, s, "c1")
	if _, err := s.Commit(context.Background(), "c1", "img"); err != nil {
		t.Fatal(err)
	}
	release()
	if vm, err := wait(t, res); err != nil || vm.State != vmlib.StateCommitted {
		t.Fatalf("%v %+v", err, vm)
	}
}

func TestDelayedGetAfterDeleteIsNotFound(t *testing.T) {
	b := vmlibtest.New(1, "base")
	s, _ := vmlib.NewService(b)
	create(t, s, "d1")
	res, release := delayedGet(t, b, s, "d1")
	if r, err := s.Delete(context.Background(), "d1"); err != nil || !r.Deleted {
		t.Fatalf("%v %+v", err, r)
	}
	release()
	if _, err := wait(t, res); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("a Get that raced a Delete returned %v", err)
	}
}

// Several delayed readers against the whole lifecycle: no deadlock, no race, and
// the final state is the true one.
func TestManyStaleReadersAgainstLifecycleConverge(t *testing.T) {
	b := vmlibtest.New(1, "base")
	b.AutoExit = false
	s, _ := vmlib.NewService(b)
	create(t, s, "m1")
	parked := make(chan struct{}, 8)
	gate := make(chan struct{})
	b.OnInspect = func(string) {
		select {
		case parked <- struct{}{}:
			<-gate
		default:
		}
	}
	done := make(chan vmlib.VM, 8)
	for i := 0; i < 4; i++ {
		go func() { vm, _ := s.Get(context.Background(), "m1"); done <- vm }()
	}
	for i := 0; i < 4; i++ {
		<-parked
	}
	b.OnInspect = nil
	go func() { time.Sleep(30 * time.Millisecond); b.ExitVM("m1", true) }()
	if _, err := s.Stop(context.Background(), "m1"); err != nil {
		t.Fatal(err)
	}
	close(gate)
	for i := 0; i < 4; i++ {
		if vm := <-done; vm.State == vmlib.StateRunning {
			t.Fatalf("a stale reader reported running after the VM stopped")
		}
	}
	if vm, _ := s.Get(context.Background(), "m1"); vm.State != vmlib.StateStopped {
		t.Fatalf("%+v", vm)
	}
}
