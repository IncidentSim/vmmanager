package vmbot

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const tokC = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

// openWS opens an upgraded (fake ttyd) connection for a link and returns the pid
// of the ssh stand-in that the fake backend spawned for it.
func openWS(t *testing.T, e *tenv, vm, tok string) (net.Conn, func() string, int) {
	t.Helper()
	resp, conn, br := e.ws(t, "/t/"+tok+"/ws", nil)
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade for %s failed: %d", vm, resp.StatusCode)
	}
	echo := func() string {
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("ping-" + vm + "\n")); err != nil {
			return "write error: " + err.Error()
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return "read error: " + err.Error()
		}
		return strings.TrimSpace(line)
	}
	if got := echo(); got != "ping-"+vm {
		t.Fatalf("%s echo: %q", vm, got)
	}
	pidb, err := os.ReadFile(filepath.Join(e.dir, "ttyd-"+vm+".sock.child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(pidb))
	return conn, echo, pid
}

func TestRevokingOneVMKeepsTheOtherInteractiveOnTheSameURL(t *testing.T) {
	e := newTerm(t, nil)
	ctx := WithUser(context.Background(), testUser)
	if err := e.m.Start(ctx, target("bot-a", tokA)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(ctx, target("bot-b", tokB)); err != nil {
		t.Fatal(err)
	}
	connA, _, pidA := openWS(t, e, "bot-a", tokA)
	defer connA.Close()
	connB, echoB, pidB := openWS(t, e, "bot-b", tokB)
	defer connB.Close()
	if pidA == pidB {
		t.Fatal("each VM needs its own backend process")
	}

	e.m.Revoke("bot-a")

	connA.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := connA.Read(make([]byte, 1)); err == nil {
		t.Fatal("A's open connection must be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("A's connection was not closed")
	}
	waitDead(t, pidA)
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatalf("A link must be dead: %d", r.StatusCode)
	}
	if _, err := os.Stat(e.m.KnownHostsPath("bot-a")); !os.IsNotExist(err) {
		t.Fatal("A known_hosts must be removed")
	}

	// B: the already open upgraded connection and its command still work,
	// the same URL still serves, a new client can attach and B's process lives.
	if got := echoB(); got != "ping-bot-b" {
		t.Fatalf("B's open connection must survive A's revocation: %q", got)
	}
	if !alive(pidB) {
		t.Fatal("B's ssh stand-in was killed by A's revocation")
	}
	if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 200 {
		t.Fatalf("B URL must keep working: %d", r.StatusCode)
	}
	if _, err := os.Stat(e.m.KnownHostsPath("bot-b")); err != nil {
		t.Fatalf("B known_hosts must remain: %v", err)
	}
	c2, _, _ := openWS(t, e, "bot-b", tokB)
	c2.Close()
	// A new, different VM can reuse the freed session slot.
	if err := e.m.Start(ctx, target("bot-c", tokC)); err != nil {
		t.Fatal(err)
	}
}

func TestCloseShutsEverySessionWithoutNeedingVMs(t *testing.T) {
	e := newTerm(t, nil)
	ctx := WithUser(context.Background(), testUser)
	e.m.Start(ctx, target("bot-a", tokA))
	e.m.Start(ctx, target("bot-b", tokB))
	connA, _, pidA := openWS(t, e, "bot-a", tokA)
	defer connA.Close()
	connB, _, pidB := openWS(t, e, "bot-b", tokB)
	defer connB.Close()
	e.m.Close()
	for name, c := range map[string]net.Conn{"a": connA, "b": connB} {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s: connection must close on Close", name)
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s: not closed", name)
		}
	}
	waitDead(t, pidA)
	waitDead(t, pidB)
	for _, id := range []string{"bot-a", "bot-b"} {
		if _, err := os.Stat(filepath.Join(e.dir, "ttyd-"+id+".sock")); err == nil {
			t.Fatalf("%s socket must be removed", id)
		}
	}
	e.m.Close() // idempotent
}

func TestSessionLimitsAndTokenUniquenessAreBounded(t *testing.T) {
	e := newTerm(t, func(c *ManagerConfig) { c.MaxClients = 1; c.MaxSessions = 2 })
	ctx := WithUser(context.Background(), testUser)
	if err := e.m.Start(ctx, target("bot-a", tokA)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(ctx, target("bot-b", tokA)); err == nil {
		t.Fatal("a capability token must never be shared between VMs")
	}
	if err := e.m.Start(ctx, target("bot-b", tokB)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(ctx, target("bot-c", tokC)); err == nil {
		t.Fatal("a third terminal must exceed the session bound")
	}
	// the per-VM client limit is per VM: A being full does not block B
	cA, _, _ := openWS(t, e, "bot-a", tokA)
	defer cA.Close()
	if resp, c, _ := e.ws(t, "/t/"+tokA+"/ws", nil); resp.StatusCode != 503 {
		t.Fatalf("A's second client must get 503, got %d", resp.StatusCode)
	} else {
		c.Close()
	}
	cB, _, _ := openWS(t, e, "bot-b", tokB)
	cB.Close()
}

func TestManagerConfigBoundsAreConsistent(t *testing.T) {
	dir := t.TempDir()
	base := ManagerConfig{TTYDPath: "ttyd", SSHPath: "ssh", SSHKeyFile: "/k", StateDir: dir, PublicOrigin: "https://h", MaxClients: 8, MaxSessions: 8}
	if _, err := NewManager(base); err != nil {
		t.Fatalf("8x8=64 is the gateway bound: %v", err)
	}
	for name, mut := range map[string]func(*ManagerConfig){
		"zero sessions":     func(c *ManagerConfig) { c.MaxSessions = 0 },
		"too many sessions": func(c *ManagerConfig) { c.MaxSessions = 9 },
		"over gateway":      func(c *ManagerConfig) { c.MaxClients = 9 },
	} {
		c := base
		mut(&c)
		if _, err := NewManager(c); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

// Slow startup of one VM must not block another VM, and a Revoke arriving during
// startup cancels it: the half-started backend is killed and no link ever goes live.
func TestRevokeDuringSlowStartAndOtherVMsAreNotBlocked(t *testing.T) {
	// Wrapper backend: VM "bot-a" (named in its known_hosts argument) never
	// comes up; every other VM execs the fake ttyd (this test binary).
	slow := filepath.Join(t.TempDir(), "wrapper-ttyd")
	script := "#!/bin/sh\ncase \"$*\" in *known_hosts/bot-a*) exec sleep 30;; esac\nexec '" + os.Args[0] + "' \"$@\"\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newTerm(t, func(c *ManagerConfig) { c.TTYDPath = slow; c.StartTimeout = 20 * time.Second })
	errA := make(chan error, 1)
	go func() { errA <- e.m.Start(WithUser(context.Background(), testUser), target("bot-a", tokA)) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.m.mu.Lock()
		n := len(e.m.pending)
		e.m.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start of A never became pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// While A is still starting, the manager lock is free: lookups and B's start proceed.
	started := make(chan error, 1)
	go func() { started <- e.m.Start(WithUser(context.Background(), testUser), target("bot-b", tokB)) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B's start was blocked by A's slow startup")
	}
	if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 200 {
		t.Fatalf("B must serve during A's startup: %d", r.StatusCode)
	}

	done := make(chan struct{})
	go func() { e.m.Revoke("bot-a"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Revoke must cancel a start in flight")
	}
	select {
	case err := <-errA:
		if err == nil {
			t.Fatal("a start revoked in flight must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Revoke")
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatalf("revoked-during-start link must never go live: %d", r.StatusCode)
	}
	if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 200 {
		t.Fatalf("B must be unaffected: %d", r.StatusCode)
	}
}

// Race detector target: Start/Revoke/Close/lookup across different VMs.
func TestConcurrentStartRevokeCloseLookupAcrossVMs(t *testing.T) {
	e := newTerm(t, nil)
	ctx := WithUser(context.Background(), testUser)
	vms := []struct{ id, tok string }{{"bot-a", tokA}, {"bot-b", tokB}}
	var wg sync.WaitGroup
	for _, v := range vms {
		for i := 0; i < 3; i++ {
			wg.Add(3)
			go func() { defer wg.Done(); _ = e.m.Start(ctx, target(v.id, v.tok)) }()
			go func() { defer wg.Done(); e.m.Revoke(v.id) }()
			go func() { defer wg.Done(); _, _ = e.srvGet("/t/" + v.tok + "/") }()
		}
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock among Start/Revoke/lookup")
	}
	// Steady state must be coherent: a fresh Start works for both, Close ends all.
	for _, v := range vms {
		e.m.Revoke(v.id)
		if err := e.m.Start(ctx, target(v.id, v.tok)); err != nil {
			t.Fatalf("restart %s: %v", v.id, err)
		}
	}
	closed := make(chan struct{})
	go func() { e.m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close deadlocked")
	}
	e.m.mu.Lock()
	n := len(e.m.sess) + len(e.m.pending) + len(e.m.slots)
	e.m.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close must leave no sessions: %d", n)
	}
}

// srvGet is a goroutine-safe GET that never calls t.Fatal.
func (e *tenv) srvGet(path string) (int, error) {
	c, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: " + pubHost + "\r\nConnection: close\r\n\r\n"))
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	return n, nil
}
