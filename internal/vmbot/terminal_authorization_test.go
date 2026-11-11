package vmbot

import (
	"context"
	"net/http"
	"testing"
)

type startHookTerms struct {
	Terminals
	start func(context.Context, TerminalTarget) error
}

func (s startHookTerms) Start(ctx context.Context, t TerminalTarget) error { return s.start(ctx, t) }

func TestDeleteIntentPreventsTransientTerminalRepublication(t *testing.T) {
	e := newEnv(t)
	rec := e.seedActive(t, "a")
	term := newTerm(t, nil)
	var observed int
	e.ctl.term = startHookTerms{Terminals: term.m, start: func(ctx context.Context, tg TerminalTarget) error {
		// Deterministic interleave: Delete persists intent and completes Revoke
		// after the controller's active check, but before Manager.Start executes.
		if err := e.store.Update(func(s *State) { s.update(rec.ID, func(r *VMRecord) { r.Phase = PhaseDeleting }) }); err != nil {
			return err
		}
		term.m.Revoke(rec.ID)
		err := term.m.Start(ctx, tg)
		resp, _ := term.get(t, http.MethodGet, "/t/"+rec.Token+"/", nil)
		observed = resp.StatusCode
		return err
	}}
	_ = e.ctl.ensureTerminalLocked(context.Background(), rec)
	if observed != http.StatusNotFound {
		t.Fatalf("revoked capability was transiently reachable: HTTP %d", observed)
	}
}

func TestDurableDeleteIntentRejectsNewHTTPBeforeCleanup(t *testing.T) {
	e := newEnv(t)
	rec := e.seedActive(t, "a")
	term := newTerm(t, nil)
	e.ctl.term = term.m
	if err := e.ctl.ensureTerminalLocked(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	resp, _ := term.get(t, http.MethodGet, "/t/"+rec.Token+"/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal("terminal never became available")
	}
	if err := e.store.Update(func(s *State) { s.update(rec.ID, func(r *VMRecord) { r.Phase = PhaseDeleting }) }); err != nil {
		t.Fatal(err)
	}
	resp, _ = term.get(t, http.MethodGet, "/t/"+rec.Token+"/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting capability served before cleanup: HTTP %d", resp.StatusCode)
	}
}
