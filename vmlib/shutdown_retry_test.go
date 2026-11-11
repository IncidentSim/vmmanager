package vmlib

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Embedding the interface makes unexpected calls fail rather than silently
// simulating lifecycle proof. Only the exercised backend seam is implemented.
type missingExitBackend struct {
	Backend
	actions  atomic.Int32
	releases atomic.Int32
}

func (b *missingExitBackend) Inspect(context.Context, string) (HostVM, error) {
	return HostVM{ID: "boot", State: HostRunning}, nil
}
func (b *missingExitBackend) RequestShutdown(context.Context, string) (HostVM, error) {
	b.actions.Add(1)
	return HostVM{ID: "boot", State: HostExiting}, nil
}
func (b *missingExitBackend) WaitExit(context.Context, string) (ExitReceipt, error) {
	return ExitReceipt{}, context.DeadlineExceeded
}
func (b *missingExitBackend) Release(context.Context, string) (ReleaseResult, error) {
	b.releases.Add(1)
	return ReleaseResult{Released: true}, nil
}

func TestShutdownRetryHasOwnBoundAndDoesNotSpin(t *testing.T) {
	b := &missingExitBackend{}
	s, err := NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	// Shorten the production policy, not caller cancellation: Background must
	// still terminate. A backend returning DeadlineExceeded immediately must
	// not turn receipt polling into an unbounded stream of guest actions.
	s.shutdownTimeout = 90 * time.Millisecond
	s.shutdownRetry = 20 * time.Millisecond
	started := time.Now()
	res, err := s.Delete(context.Background(), "boot")
	if !errors.Is(err, ErrRetained) || !res.Retained {
		t.Fatalf("%+v %v", res, err)
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond || elapsed > time.Second {
		t.Fatalf("shutdown bound: %s", elapsed)
	}
	if calls := b.actions.Load(); calls < 2 || calls > 5 {
		t.Fatalf("actions=%d; bounded spaced retries required", calls)
	}
	if b.releases.Load() != 0 {
		t.Fatal("released without original exit proof")
	}
}
