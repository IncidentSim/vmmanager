package vmbot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOrchestrationSubsecondStatusReturnsCompletedResult(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ctl.Create(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	e.ctl.cfg.QueryTimeout = 250 * time.Millisecond
	s, err := e.ctl.Status(context.Background(), Ref{Sel: "a"})
	if err != nil {
		t.Fatalf("completed workflow must return within query budget: %v", err)
	}
	if !s.Present || s.Busy || s.Uncertain || s.Phase != PhaseActive {
		t.Fatalf("status: %+v", s)
	}
}

func TestOrchestrationOneEnginePerStore(t *testing.T) {
	e := newEnv(t)
	other := NewController(e.lc, e.store, e.terms, e.ctl.cfg)
	err := other.Start(context.Background())
	if err == nil {
		_ = other.Close()
		t.Fatal("a shared Store must not bypass single-executor ownership")
	}
}

func TestOrchestrationStoreLockLivesUntilEngineStops(t *testing.T) {
	e := newEnv(t)
	if err := e.store.Close(); err == nil {
		t.Fatal("must not release lifetime lock while DBOS can execute work")
	}
	if st, err := OpenStore(e.dir); !errors.Is(err, ErrLocked) {
		if st != nil {
			st.Close()
		}
		t.Fatalf("second instance: %v", err)
	}
	if err := e.ctl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrationCannotStartOnClosedStore(t *testing.T) {
	e := newEnv(t)
	if err := e.ctl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	other := NewController(e.lc, e.store, e.terms, e.ctl.cfg)
	if err := other.Start(context.Background()); err == nil {
		_ = other.Close()
		t.Fatal("Start must require an open locked store")
	}
}
