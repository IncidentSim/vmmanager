package vmbot

// SSH-idle expiry tests (docs/adr/0008-ssh-idle-expiry.md). Real DBOS owner over
// file SQLite and the in-memory vmlib fake, with a fake clock and a fake
// connection counter. They prove the policy and its integration with the owner;
// they say nothing about a live guest's sshd or `ss`.
//
// Run focused: GOMAXPROCS=2 go test -race ./internal/vmbot -run Idle -count=1

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type idleClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *idleClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *idleClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeCounter answers from a script: next() yields (count, error) per call.
type fakeCounter struct {
	mu    sync.Mutex
	next  func(call int) (int, error)
	calls int
	hosts []string
	users []string
}

func (f *fakeCounter) Count(_ context.Context, vmID, host, user string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.hosts = append(f.hosts, host)
	f.users = append(f.users, user)
	return f.next(f.calls)
}
func (f *fakeCounter) set(n int, err error) {
	f.mu.Lock()
	f.next = func(int) (int, error) { return n, err }
	f.mu.Unlock()
}
func (f *fakeCounter) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

const testIdle = 600 * time.Second

// idleEnv enables idle expiry on the controller of an owner environment. The
// monitor goroutine is not started: tests drive idleTick deterministically.
func idleEnv(t *testing.T) (*ownerEnv, *idleClock, *fakeCounter) {
	t.Helper()
	e := newOwnerEnv(t)
	return e, e.enableIdle(), &fakeCounter{next: func(int) (int, error) { return 0, nil }}
}

func (e *ownerEnv) enableIdle() *idleClock {
	clk := &idleClock{t: time.Now()}
	e.ctl.clock = clk.now
	return clk
}

func (e *ownerEnv) armIdle(f *fakeCounter) {
	e.ctl.cfg.IdleProbe = f
	e.ctl.cfg.IdleTimeout = testIdle
}

func (e *ownerEnv) tick(clk *idleClock, after time.Duration) {
	clk.advance(after)
	e.ctl.idleTick(context.Background())
}

func (e *ownerEnv) idleOps(vm string) []ControlOperation {
	var out []ControlOperation
	for _, o := range e.st.Snapshot().Operations {
		if o.VMID == vm && strings.HasPrefix(o.ID, idleOpPrefix) {
			out = append(out, o)
		}
	}
	return out
}

func TestIdleActiveSSHKeepsVMAlive(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	probe.set(1, nil) // an idle terminal or SFTP session is still a connection
	for i := 0; i < 8; i++ {
		e.tick(clk, 300*time.Second)
	}
	if probe.count() == 0 || len(e.idleOps(vm)) != 0 || e.phase(vm) != PhaseActive {
		t.Fatalf("calls=%d ops=%v phase=%s", probe.count(), e.idleOps(vm), e.phase(vm))
	}
}

func TestIdleNoSSHThresholdDeletesThroughOwner(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	e.tick(clk, 0) // first successful observation opens the window
	e.tick(clk, testIdle)
	if len(e.idleOps(vm)) != 0 || e.phase(vm) != PhaseActive {
		t.Fatal("deleted at exactly the threshold; must be strictly more than 600s")
	}
	e.tick(clk, time.Second)
	ops := e.idleOps(vm)
	if len(ops) != 1 || ops[0].Kind != "delete" || ops[0].Exec != execOwner {
		t.Fatalf("ops %+v", ops)
	}
	e.eventually("idle delete projected", func() bool { return e.gone(vm) })
	if o := e.op(ops[0].ID); o.Status != "succeeded" {
		t.Fatalf("op %+v", o)
	}
	if e.lc.count("delete:"+vm) != 1 {
		t.Fatalf("calls %v", e.lc.calls)
	}
	if _, err := e.ctl.ControlVM(ownerUser, vm); err == nil {
		t.Fatal("deleted VM still visible to the ordinary API")
	}
	e.terms.mu.Lock()
	revoked := strings.Join(e.terms.revoked, ",")
	e.terms.mu.Unlock()
	if !strings.Contains(revoked, vm) {
		t.Fatal("terminal access not revoked by the ordinary delete")
	}
	for _, h := range probe.hosts {
		if !strings.HasPrefix(h, "172.16.") || probe.users[0] == "" {
			t.Fatalf("probe not aimed at the internal guest address: %v %v", probe.hosts, probe.users)
		}
	}
}

func TestIdleReconnectResetsWindow(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	e.tick(clk, 0)
	e.tick(clk, 500*time.Second)
	probe.set(1, nil) // reconnect
	e.tick(clk, 10*time.Second)
	probe.set(0, nil)
	e.tick(clk, 10*time.Second) // new window opens here
	e.tick(clk, 590*time.Second)
	if len(e.idleOps(vm)) != 0 {
		t.Fatal("window not reset by the reconnect")
	}
	e.tick(clk, 11*time.Second)
	e.eventually("deleted after a full fresh window", func() bool { return e.gone(vm) })
}

func TestIdleProbeFailureNeverDeletes(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	e.tick(clk, 0)
	e.tick(clk, 500*time.Second)
	probe.set(0, errors.New("no trust / no ss / ambiguous"))
	for i := 0; i < 10; i++ {
		e.tick(clk, 600*time.Second)
	}
	if len(e.idleOps(vm)) != 0 || e.phase(vm) != PhaseActive {
		t.Fatal("deleted on failed observations")
	}
	// Continuity was lost: after the failure, a clean zero starts a new window.
	probe.set(0, nil)
	e.tick(clk, 0)
	e.tick(clk, 590*time.Second)
	if len(e.idleOps(vm)) != 0 {
		t.Fatal("window not restarted from the first success after a failure")
	}
}

func TestIdleUnconfiguredIsFailClosed(t *testing.T) {
	e, clk, _ := idleEnv(t)
	vm := e.create("c1").VMID
	for i := 0; i < 5; i++ {
		e.tick(clk, 1000*time.Second) // no probe/timeout configured
	}
	e.ctl.cfg.IdleProbe = &fakeCounter{next: func(int) (int, error) { return 0, nil }} // but no timeout
	e.tick(clk, 1000*time.Second)
	if len(e.idleOps(vm)) != 0 || e.phase(vm) != PhaseActive {
		t.Fatal("idle expiry ran without an explicit configuration")
	}
}

func TestIdlePendingCreateAndBusyCommandAreProtected(t *testing.T) {
	e, clk, probe := idleEnv(t)
	e.armIdle(probe)
	release := e.lc.arm("create")
	o := e.accept("c1", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	e.lc.waitEntered(t, "create")
	for i := 0; i < 4; i++ {
		e.tick(clk, 700*time.Second)
	}
	if probe.count() != 0 || len(e.idleOps(o.VMID)) != 0 {
		t.Fatal("a VM with a pending create was observed or expired")
	}
	release()
	if got := awaitControl(t, e.ctl, ownerUser, o.ID); got.Status != "succeeded" {
		t.Fatalf("create %+v", got)
	}
	// An active owner command (stop held open) protects the VM as well.
	e.tick(clk, 0) // opens a window
	relStop := e.lc.arm("stop")
	st := e.accept("s1", ControlRequest{Kind: "stop", VMID: o.VMID})
	e.lc.waitEntered(t, "stop")
	before := probe.count()
	for i := 0; i < 3; i++ {
		e.tick(clk, 700*time.Second)
	}
	if probe.count() != before || len(e.idleOps(o.VMID)) != 0 {
		t.Fatal("expired while an owner command was running")
	}
	relStop()
	awaitControl(t, e.ctl, ownerUser, st.ID)
}

func TestIdleDeleteFailureRetriesSameVMWithNewAttempt(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	e.lc.mu.Lock()
	e.lc.fail["delete"] = errors.New("backend down")
	e.lc.mu.Unlock()
	e.tick(clk, 0)
	e.tick(clk, testIdle+time.Second)
	e.eventually("first attempt fails", func() bool {
		ops := e.idleOps(vm)
		return len(ops) == 1 && e.op(ops[0].ID).Status == "failed"
	})
	if e.gone(vm) || e.phase(vm) != PhaseDeleting {
		t.Fatalf("a failed delete must retain the VM as deleting, got %q", e.phase(vm))
	}
	e.lc.mu.Lock()
	delete(e.lc.fail, "delete")
	e.lc.mu.Unlock()
	e.tick(clk, 0)
	e.tick(clk, testIdle+time.Second)
	e.eventually("retry deletes the same VM", func() bool { return e.gone(vm) })
	ops := e.idleOps(vm)
	if len(ops) != 2 || ops[0].ID == ops[1].ID || ops[0].VMID != ops[1].VMID {
		t.Fatalf("ops %+v", ops)
	}
}

func TestIdleFinalConfirmationInOwnerCancelsDelete(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	probe.mu.Lock()
	// calls: 1 window open, 2 threshold observation, 3 final pre-admission check
	// all clean; 4 is the owner's check at begin: a connection arrived.
	probe.next = func(call int) (int, error) {
		if call >= 4 {
			return 1, nil
		}
		return 0, nil
	}
	probe.mu.Unlock()
	e.tick(clk, 0)
	e.tick(clk, testIdle+time.Second)
	e.eventually("idle op settled", func() bool {
		ops := e.idleOps(vm)
		return len(ops) == 1 && e.op(ops[0].ID).Status == "failed"
	})
	if e.gone(vm) || e.phase(vm) != PhaseActive || e.lc.count("delete:") != 0 {
		t.Fatalf("a connection seen at the owner's recheck must keep the VM: phase=%s calls=%v", e.phase(vm), e.lc.calls)
	}
	if e.op(e.idleOps(vm)[0].ID).ErrorCode != "conflict" {
		t.Fatal("cancelled idle delete should report conflict")
	}
}

func TestIdleCoversLegacyVMWithoutDeadline(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	if err := e.st.Update(func(s *State) {
		s.update(vm, func(v *VMRecord) { v.ExpiresAt, v.TTLSeconds = nil, 0 })
	}); err != nil {
		t.Fatal(err)
	}
	e.armIdle(probe)
	e.tick(clk, 0)
	e.tick(clk, testIdle+time.Second)
	e.eventually("legacy VM without TTL expired by idleness", func() bool { return e.gone(vm) })
}

func TestIdleWindowIsNotDurableAcrossRestart(t *testing.T) {
	e, clk, probe := idleEnv(t)
	vm := e.create("c1").VMID
	e.armIdle(probe)
	e.tick(clk, 0)
	e.tick(clk, 590*time.Second)
	e.reopen(false)
	e.ctl.clock = clk.now
	e.armIdle(probe)
	e.tick(clk, 20*time.Second) // would be past the old window; a new one only opens
	e.tick(clk, 500*time.Second)
	if len(e.idleOps(vm)) != 0 {
		t.Fatal("restart must conservatively restart the continuous-zero window")
	}
	e.tick(clk, 101*time.Second)
	e.eventually("deleted after a full window since restart", func() bool { return e.gone(vm) })
}

func TestIdleMonitorStartsAndStopsWithController(t *testing.T) {
	e, _, probe := idleEnv(t)
	e.armIdle(probe)
	e.ctl.cfg.IdlePoll = 5 * time.Millisecond
	e.create("c1")
	e.ctl.startIdleMonitor()
	e.eventually("monitor polls", func() bool { return probe.count() >= 2 })
	e.ctl.stopIdleMonitor()
	n := probe.count()
	time.Sleep(30 * time.Millisecond)
	if probe.count() != n {
		t.Fatal("monitor kept polling after stop")
	}
}
