package vmbot

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lifecycleGatedConn blocks inside Close, which holds a Revoke/Close in the middle
// of its cleanup deterministically.
type lifecycleGatedConn struct {
	net.Conn
	entered chan struct{}
	gate    chan struct{}
}

func (c *lifecycleGatedConn) Close() error {
	select {
	case <-c.entered:
	default:
		close(c.entered)
	}
	<-c.gate
	return c.Conn.Close()
}

// holdRevoke starts bot-a and begins a Revoke that stays blocked in connection
// cleanup until the returned gate is closed.
func holdRevoke(t *testing.T, e *tenv) (old *session, gate chan struct{}, done chan struct{}) {
	t.Helper()
	if err := e.m.Start(context.Background(), target("bot-a", tokA)); err != nil {
		t.Fatal(err)
	}
	old = e.m.lookup(tokA)
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	g := &lifecycleGatedConn{Conn: a, entered: make(chan struct{}), gate: make(chan struct{})}
	old.mu.Lock()
	old.conns[g] = struct{}{}
	old.mu.Unlock()
	done = make(chan struct{})
	go func() { e.m.Revoke("bot-a"); close(done) }()
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke never reached connection cleanup")
	}
	return old, g.gate, done
}

func notDone(ch chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return false
	case <-time.After(d):
		return true
	}
}

func TestCloseWaitsForAConcurrentRevokeToFinishCleanup(t *testing.T) {
	e := newTerm(t, nil)
	old, gate, done := holdRevoke(t, e)
	closed := make(chan struct{})
	go func() { e.m.Close(); close(closed) }()
	if !notDone(closed, 150*time.Millisecond) {
		t.Fatal("Close returned while a concurrent Revoke still owned a live ttyd and an unclosed connection")
	}
	if !alive(old.cmd.Process.Pid) && notDone(done, 0) {
		t.Fatal("test premise: ttyd should still be up while the connection cleanup is blocked")
	}
	close(gate)
	<-done
	<-closed
	waitDead(t, old.cmd.Process.Pid)
}

func TestSecondRevokeWaitsWhenAnotherGoroutineOwnsTermination(t *testing.T) {
	e := newTerm(t, nil)
	_, gate, done := holdRevoke(t, e)
	second := make(chan struct{})
	go func() { e.m.Revoke("bot-a"); close(second) }()
	if !notDone(second, 150*time.Millisecond) {
		t.Fatal("a second Revoke returned before the first finished cleanup")
	}
	close(gate)
	<-done
	<-second
}

func TestSameVMStartIsRefusedUntilOlderCleanupCompletes(t *testing.T) {
	e := newTerm(t, nil)
	_, gate, done := holdRevoke(t, e)
	if err := e.m.Start(context.Background(), target("bot-a", tokB)); err == nil {
		t.Fatal("a replacement must not be admitted/published while the older session is still being cleaned up")
	}
	if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 404 {
		t.Fatalf("the replacement link must not exist yet: %d", r.StatusCode)
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatalf("the revoked link must already be dead: %d", r.StatusCode)
	}
	close(gate)
	<-done
	// after the cleanup finished, a replacement works and keeps working: the old
	// cleanup can no longer unlink its socket or known_hosts
	if err := e.m.Start(context.Background(), target("bot-a", tokB)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 200 {
			t.Fatalf("replacement must serve: %d", r.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(e.m.KnownHostsPath("bot-a")); err != nil {
		t.Fatalf("replacement known_hosts must exist: %v", err)
	}
}

func TestCapabilityTokenAndCapacityStayReservedWhileTerminating(t *testing.T) {
	e := newTerm(t, func(c *ManagerConfig) { c.MaxSessions = 1 })
	_, gate, done := holdRevoke(t, e)
	if err := e.m.Start(context.Background(), target("bot-b", tokA)); err == nil {
		t.Fatal("the token of a still-terminating session must stay reserved")
	}
	if err := e.m.Start(context.Background(), target("bot-b", tokB)); err == nil {
		t.Fatal("a terminating session must keep occupying its slot (bounded processes)")
	}
	close(gate)
	<-done
	if err := e.m.Start(context.Background(), target("bot-b", tokB)); err != nil {
		t.Fatalf("slot free after cleanup: %v", err)
	}
}

func TestCloseWaitsForFailedStartupCleanup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	wrapper := filepath.Join(dir, "ttyd-wrapper")
	// ignores TERM so that cleanup takes the SIGKILL path
	script := "#!/bin/sh\ntrap '' TERM\nprintf '%s' \"$$\" > '" + pidFile + "'\nexec sleep 30\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newTerm(t, func(c *ManagerConfig) { c.TTYDPath = wrapper; c.StartTimeout = 200 * time.Millisecond })
	started := make(chan error, 1)
	go func() { started <- e.m.Start(context.Background(), target("bot-a", tokA)) }()
	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				pid = p
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no child pid")
		}
		time.Sleep(time.Millisecond)
	}
	for { // wait until the startup attempt failed and moved on to cleanup
		e.m.mu.Lock()
		p := e.m.pending["bot-a"]
		e.m.mu.Unlock()
		if p == nil {
			break
		}
		if time.Now().After(deadline.Add(2 * time.Second)) {
			t.Fatal("startup cleanup not reached")
		}
		time.Sleep(time.Millisecond)
	}
	e.m.Close()
	if alive(pid) {
		// zombies of a dead parent count as dead for this purpose
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && !strings.Contains(string(b), ") Z ") {
			t.Fatal("Close returned while the failed Start's ttyd was still alive")
		}
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("start should have failed")
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned before the failed Start finished its cleanup")
	}
}

func TestConcurrentPendingStartsCannotShareAToken(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	wrapper := filepath.Join(t.TempDir(), "ttyd-wrapper")
	script := "#!/bin/sh\ncase \"$*\" in *known_hosts/bot-a*) while [ ! -f '" + gate + "' ]; do sleep 0.01; done;; esac\nexec '" + os.Args[0] + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newTerm(t, func(c *ManagerConfig) { c.TTYDPath = wrapper })
	errA := make(chan error, 1)
	go func() { errA <- e.m.Start(context.Background(), target("bot-a", tokA)) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.m.mu.Lock()
		p := e.m.pending["bot-a"]
		e.m.mu.Unlock()
		if p != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not pending")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.m.Start(context.Background(), target("bot-b", tokA)); err == nil {
		t.Fatal("a token reserved by a pending start must not be admitted for another VM")
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	e.m.mu.Lock()
	n := len(e.m.sess)
	e.m.mu.Unlock()
	if n != 1 {
		t.Fatalf("exactly one session expected, have %d", n)
	}
}
