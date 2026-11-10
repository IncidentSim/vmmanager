package vmhelper

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePoolIsStrict(t *testing.T) {
	good := "# comment\n\nGUEST_PREFIX=10.213\nMAX_SLOTS=2\n"
	p, err := ParsePool([]byte(good))
	if err != nil || p.Prefix != "10.213" || p.MaxSlots != 2 {
		t.Fatalf("%v %+v", err, p)
	}
	for name, bad := range map[string]string{
		"three octets":  "GUEST_PREFIX=10.213.1\nMAX_SLOTS=2\n",
		"one octet":     "GUEST_PREFIX=10\nMAX_SLOTS=2\n",
		"out of range":  "GUEST_PREFIX=300.1\nMAX_SLOTS=2\n",
		"loopback":      "GUEST_PREFIX=127.0\nMAX_SLOTS=2\n",
		"zero":          "GUEST_PREFIX=0.1\nMAX_SLOTS=2\n",
		"multicast":     "GUEST_PREFIX=230.1\nMAX_SLOTS=2\n",
		"link-local":    "GUEST_PREFIX=169.254\nMAX_SLOTS=2\n",
		"no slots":      "GUEST_PREFIX=10.213\n",
		"zero slots":    "GUEST_PREFIX=10.213\nMAX_SLOTS=0\n",
		"too many":      "GUEST_PREFIX=10.213\nMAX_SLOTS=65\n",
		"unknown key":   "GUEST_PREFIX=10.213\nMAX_SLOTS=2\nEVIL=$(reboot)\n",
		"duplicate":     "GUEST_PREFIX=10.213\nGUEST_PREFIX=10.214\nMAX_SLOTS=2\n",
		"shell syntax":  "GUEST_PREFIX=10.213;reboot\nMAX_SLOTS=2\n",
		"no prefix":     "MAX_SLOTS=2\n",
		"leading zeros": "GUEST_PREFIX=010.213\nMAX_SLOTS=2\n",
	} {
		if _, err := ParsePool([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseRequestAcceptsOnlyFixedVerbAndBoundedSlot(t *testing.T) {
	if n, err := ParseRequest("start-slot 1", 2); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "start-slot", "start-slot 2", "start-slot -1", "start-slot 01", "start-slot 1 2",
		"stop-slot 1", "start-slot 1;reboot", " start-slot 1", "start-slot  1", "START-SLOT 1", "start-slot 1\x00", "start-slot ../1",
		"restart main", "start-slot 99999999999999999999"} {
		if _, err := ParseRequest(bad, 2); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

type recHandler struct {
	mu    sync.Mutex
	slots []int
	err   error
}

func (h *recHandler) StartSlot(_ context.Context, slot int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slots = append(h.slots, slot)
	return h.err
}

func TestServeValidatesBeforeCallingTheHandler(t *testing.T) {
	for _, in := range []string{"start-slot 5\n", "stop-slot 0\n", strings.Repeat("A", 500), "\n", "start-slot 0", "\x00\x01\x02\n"} {
		h := &recHandler{}
		var out bytes.Buffer
		err := Serve(context.Background(), strings.NewReader(in), &out, h, 2)
		if err == nil || len(h.slots) != 0 || !strings.HasPrefix(out.String(), "error: ") {
			t.Errorf("%q: err=%v called=%v out=%q", in, err, h.slots, out.String())
		}
	}
	h := &recHandler{}
	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader("start-slot 1\n"), &out, h, 2); err != nil || out.String() != "ok\n" || len(h.slots) != 1 || h.slots[0] != 1 {
		t.Fatalf("%v %q %v", err, out.String(), h.slots)
	}
}

func TestServeIgnoresDataAfterTheFirstLine(t *testing.T) {
	h := &recHandler{}
	var out bytes.Buffer
	_ = Serve(context.Background(), strings.NewReader("start-slot 1\nstart-slot 0\n"), &out, h, 2)
	if len(h.slots) != 1 {
		t.Fatalf("pipelined request executed: %v", h.slots)
	}
}

func TestClientAndServerOverASocket(t *testing.T) {
	h := &recHandler{}
	c, s := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), s, s, h, 2); s.Close() }()
	if err := Request(c, 1); err != nil {
		t.Fatal(err)
	}
	<-done
	h.mu.Lock()
	n := len(h.slots)
	h.mu.Unlock()
	if n != 1 {
		t.Fatal("not called")
	}
	h2 := &recHandler{err: errors.New("unit busy")}
	c2, s2 := net.Pipe()
	go func() { _ = Serve(context.Background(), s2, s2, h2, 2); s2.Close() }()
	if err := Request(c2, 0); err == nil || !strings.Contains(err.Error(), "unit busy") {
		t.Fatalf("server error not surfaced: %v", err)
	}
	if err := Request(nil, 99); err == nil {
		t.Fatal("client must refuse an out-of-range slot locally")
	}
}

func fakeSystemctl(t *testing.T, state string) (string, string) {
	d := t.TempDir()
	log := filepath.Join(d, "calls")
	p := filepath.Join(d, "systemctl")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n[ \"$1\" = show ] && echo " + state + "\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p, log
}

func TestSystemdHandlerStartsOnlyIdleSlotUnits(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		sc, log := fakeSystemctl(t, state)
		if err := (SystemdHandler{Systemctl: sc}).StartSlot(context.Background(), 3); err != nil {
			t.Fatal(err)
		}
		calls, _ := os.ReadFile(log)
		want := "show --property=ActiveState --value firecracker-vm@3.service\nstart --no-ask-password firecracker-vm@3.service\n"
		if string(calls) != want {
			t.Fatalf("%s: %q", state, calls)
		}
	}
	for _, state := range []string{"active", "activating", "deactivating", "reloading", "maintenance", ""} {
		sc, log := fakeSystemctl(t, state)
		if err := (SystemdHandler{Systemctl: sc}).StartSlot(context.Background(), 1); err == nil {
			t.Errorf("%q: started a slot that may hold a guest", state)
		}
		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "start ") {
			t.Errorf("%q: start was issued: %s", state, calls)
		}
	}
}

func TestSystemdHandlerHonoursContext(t *testing.T) {
	sc, _ := fakeSystemctl(t, "inactive")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := (SystemdHandler{Systemctl: sc}).StartSlot(ctx, 0); err == nil {
		t.Fatal("ignored cancelled context")
	}
}
