package vmhelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if r, err := ParseRequest("start-slot 1 500", 2, 2000); err != nil || r.Slot != 1 || r.CPUMilli != 500 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "start-slot", "start-slot 2 500", "start-slot -1 500", "start-slot 01 500", "start-slot 1 2 3",
		"stop-slot 1 500", "start-slot 1;reboot", " start-slot 1 500", "start-slot  1 500", "START-SLOT 1 500", "start-slot 1 500\x00", "start-slot ../1 500",
		"restart main", "start-slot 99999999999999999999 500",
		// The quota is mandatory: a bare slot must never start a unit with an unspecified CPU limit.
		"start-slot 1", "start-slot 1 ",
		"start-slot 1 0", "start-slot 1 5", "start-slot 1 505", "start-slot 1 0500", "start-slot 1 -500", "start-slot 1 +500", "start-slot 1 5e2",
		"start-slot 1 0.5", "start-slot 1 2010", "start-slot 1 99999999999999999999", "start-slot 1 infinity", "start-slot 1 500%"} {
		if _, err := ParseRequest(bad, 2, 2000); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ParseRequest("start-slot 0 2000", 2, 2000); err != nil {
		t.Fatalf("the pool maximum itself must be allowed: %v", err)
	}
}

type recHandler struct {
	mu     sync.Mutex
	slots  []int
	quotas []uint32
	err    error
}

func (h *recHandler) StartSlot(_ context.Context, slot int, cpuMilli uint32) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slots = append(h.slots, slot)
	h.quotas = append(h.quotas, cpuMilli)
	return h.err
}

func TestServeValidatesBeforeCallingTheHandler(t *testing.T) {
	for _, in := range []string{"start-slot 5 500\n", "stop-slot 0 500\n", strings.Repeat("A", 500), "\n", "start-slot 0 500", "\x00\x01\x02\n", "start-slot 1\n", "start-slot 1 5000\n"} {
		h := &recHandler{}
		var out bytes.Buffer
		err := Serve(context.Background(), strings.NewReader(in), &out, h, Limits{Slots: 2, MaxCPUMilli: 2000})
		if err == nil || len(h.slots) != 0 || !strings.HasPrefix(out.String(), "error: ") {
			t.Errorf("%q: err=%v called=%v out=%q", in, err, h.slots, out.String())
		}
	}
	h := &recHandler{}
	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader("start-slot 1 500\n"), &out, h, Limits{Slots: 2, MaxCPUMilli: 2000}); err != nil || out.String() != "ok\n" || len(h.slots) != 1 || h.slots[0] != 1 || h.quotas[0] != 500 {
		t.Fatalf("%v %q %v", err, out.String(), h.slots)
	}
}

func TestServeIgnoresDataAfterTheFirstLine(t *testing.T) {
	h := &recHandler{}
	var out bytes.Buffer
	_ = Serve(context.Background(), strings.NewReader("start-slot 1 500\nstart-slot 0 500\n"), &out, h, Limits{Slots: 2, MaxCPUMilli: 2000})
	if len(h.slots) != 1 {
		t.Fatalf("pipelined request executed: %v", h.slots)
	}
}

func TestClientAndServerOverASocket(t *testing.T) {
	h := &recHandler{}
	c, s := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), s, s, h, Limits{Slots: 2, MaxCPUMilli: 2000}); s.Close() }()
	if err := Request(c, 1, 500); err != nil {
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
	go func() { _ = Serve(context.Background(), s2, s2, h2, Limits{Slots: 2, MaxCPUMilli: 2000}); s2.Close() }()
	if err := Request(c2, 0, 500); err == nil || !strings.Contains(err.Error(), "unit busy") {
		t.Fatalf("server error not surfaced: %v", err)
	}
	if err := Request(nil, 99, 500); err == nil {
		t.Fatal("client must refuse an out-of-range slot locally")
	}
	for _, q := range []uint32{0, 5, 505, 64010} {
		if err := Request(nil, 1, q); err == nil || strings.Contains(err.Error(), "no connection") {
			t.Fatalf("client must refuse quota %d locally, got %v", q, err)
		}
	}
}

// fakeSystemctl records every call. "show -P CPUQuotaPerSecUSec" answers with
// readback, so a test can model a unit that did or did not take the property.
func fakeSystemctl(t *testing.T, state, readback string) (string, string) {
	d := t.TempDir()
	log := filepath.Join(d, "calls")
	p := filepath.Join(d, "systemctl")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"case \"$*\" in *CPUQuotaPerSecUSec*) echo '" + readback + "';; *ActiveState*) echo " + state + ";; esac\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p, log
}

func TestSystemdHandlerSetsTheCPUQuotaThenStartsOnlyIdleSlotUnits(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		sc, log := fakeSystemctl(t, state, "500ms")
		if err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(context.Background(), 3, 500); err != nil {
			t.Fatal(err)
		}
		calls, _ := os.ReadFile(log)
		want := "show --property=ActiveState --value firecracker-vm@3.service\n" +
			"set-property --runtime --no-ask-password firecracker-vm@3.service CPUQuota=50%\n" +
			"show --property=CPUQuotaPerSecUSec --value firecracker-vm@3.service\n" +
			"start --no-ask-password firecracker-vm@3.service\n"
		if string(calls) != want {
			t.Fatalf("%s:\n%q\nwant\n%q", state, calls, want)
		}
	}
	for _, state := range []string{"active", "activating", "deactivating", "reloading", "maintenance", ""} {
		sc, log := fakeSystemctl(t, state, "500ms")
		if err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(context.Background(), 1, 500); err == nil {
			t.Errorf("%q: started a slot that may hold a guest", state)
		}
		calls, _ := os.ReadFile(log)
		if strings.Contains(string(calls), "start ") || strings.Contains(string(calls), "set-property") {
			t.Errorf("%q: a busy slot's quota or state was touched: %s", state, calls)
		}
	}
}

func TestSystemdHandlerQuotaPercentRendering(t *testing.T) {
	for milli, want := range map[uint32]string{10: "CPUQuota=1%", 250: "CPUQuota=25%", 500: "CPUQuota=50%", 1000: "CPUQuota=100%", 1500: "CPUQuota=150%", 2000: "CPUQuota=200%"} {
		sc, log := fakeSystemctl(t, "inactive", usecSpan(milli))
		if err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(context.Background(), 0, milli); err != nil {
			t.Fatalf("%d: %v", milli, err)
		}
		if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), " "+want+"\n") {
			t.Errorf("%d: %s", milli, calls)
		}
	}
}

func usecSpan(milli uint32) string {
	if milli%1000 == 0 {
		return fmt.Sprintf("%ds", milli/1000)
	}
	if milli > 1000 {
		return fmt.Sprintf("%d.%03ds", milli/1000, milli%1000) // systemd prints fractional seconds
	}
	return fmt.Sprintf("%dms", milli)
}

func TestSystemdHandlerRefusesToStartWhenTheQuotaDidNotTake(t *testing.T) {
	for name, readback := range map[string]string{"unset": "infinity", "wrong": "1s", "empty": "", "garbage": "lots", "half": "250ms"} {
		sc, log := fakeSystemctl(t, "inactive", readback)
		err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(context.Background(), 2, 500)
		if err == nil {
			t.Errorf("%s: started without a verified CPU quota", name)
		}
		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "start --no") {
			t.Errorf("%s: start issued: %s", name, calls)
		}
	}
}

func TestSystemdHandlerRejectsUnrepresentableQuotaBeforeAnyCall(t *testing.T) {
	for _, q := range []uint32{0, 5, 505, 64010} {
		sc, log := fakeSystemctl(t, "inactive", "500ms")
		if err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(context.Background(), 1, q); err == nil {
			t.Errorf("%d accepted", q)
		}
		if _, err := os.Stat(log); err == nil {
			t.Errorf("%d: systemctl was invoked", q)
		}
	}
}

func TestParseUSecSpan(t *testing.T) {
	for in, want := range map[string]uint64{"500ms": 500000, "1s": 1000000, "1s 500ms": 1500000, "1.5s": 1500000, "1.500s": 1500000, "1min 1.5s": 61500000, "0.5s": 500000, "10ms": 10000, "2s": 2000000, "1min": 60000000, "250us": 250, "10ms ": 10000} {
		if got, err := parseUSecSpan(in); err != nil || got != want {
			t.Errorf("%q: %d %v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "infinity", "x", "1", "5 ms", "-1s", "1.5.5s", "ms", "1.5", "1.1234567s", ".5s"} {
		if _, err := parseUSecSpan(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSystemdHandlerHonoursContext(t *testing.T) {
	sc, _ := fakeSystemctl(t, "inactive", "500ms")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := (SystemdHandler{Systemctl: sc, LockDir: secureLockDir(t)}).StartSlot(ctx, 0, 500); err == nil {
		t.Fatal("ignored cancelled context")
	}
}

func TestPoolMaxSlotCPUIsOptionalStrictAndBounded(t *testing.T) {
	p, err := ParsePool([]byte("GUEST_PREFIX=10.213\nMAX_SLOTS=4\n"))
	if err != nil || p.MaxSlotCPUMilli != DefaultMaxSlotCPUMilli {
		t.Fatalf("%v %+v", err, p)
	}
	p, err = ParsePool([]byte("GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=1000\n"))
	if err != nil || p.MaxSlotCPUMilli != 1000 {
		t.Fatalf("%v %+v", err, p)
	}
	for _, bad := range []string{"0", "5", "505", "-1", "64010", "01000", "1e3", " 1000"} {
		if _, err := ParsePool([]byte("GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=" + bad + "\n")); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
