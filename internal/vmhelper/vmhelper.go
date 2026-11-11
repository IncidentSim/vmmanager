// Package vmhelper is the narrow privilege boundary between the unprivileged
// vmapi and the root-owned systemd units of a slot. It defines a one-line
// protocol ("start-slot N CPU_MILLI"), the strict pool configuration shared by
// the helper, the network script and vmapi, and a handler that can only set the
// CPU quota of, and then start, an idle firecracker-vm@N.service. CPU_MILLI is
// the VMM's CPU-time ceiling in thousandths of a CPU (500 = 50% of one CPU); it
// is mandatory, so no slot can start with an unspecified quota. There is
// deliberately no stop, kill, restart, path, unit-name or command verb.
package vmhelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MaxSlots is the hard upper bound for every component.
const MaxSlots = 64

// SocketPath is where the socket-activated helper listens (fixed; no override).
const SocketPath = "/run/fc-vm-helper.sock"

// Pool is the operator-chosen guest address pool shared by the helper, the
// per-slot network script and vmapi. Slot N uses Prefix.N.0/30 (guest .2).
type Pool struct {
	Prefix   string // "A.B"
	MaxSlots int
	// PublicIPv4 and SSHPortBase are paired operator-only network publication
	// knobs; neither grants the helper protocol a new operation.
	PublicIPv4  string
	SSHPortBase int
	// MaxSlotCPUMilli is the root-owned ceiling for any single slot's requested
	// CPU quota (the unprivileged caller can never exceed it).
	MaxSlotCPUMilli uint32
}

// DefaultMaxSlotCPUMilli applies when pool.conf has no MAX_SLOT_CPU_MILLI.
const DefaultMaxSlotCPUMilli = 2000

// Quota bounds shared with vmlib (kept literal: this package stays dependency-free).
const (
	cpuStepMilli = 10
	cpuMaxMilli  = 64000
)

// ValidateCPUMilli accepts a positive whole-percent CPU quota (multiple of 10).
func ValidateCPUMilli(m uint32) error {
	if m == 0 || m%cpuStepMilli != 0 || m > cpuMaxMilli {
		return fmt.Errorf("cpu quota %d must be a positive multiple of %d and at most %d millicores", m, cpuStepMilli, cpuMaxMilli)
	}
	return nil
}

// Limits is what Serve enforces on top of syntax: the slot count and the
// per-slot CPU ceiling from pool.conf.
type Limits struct {
	Slots       int
	MaxCPUMilli uint32
}

var (
	prefixPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$`)
	slotPattern   = regexp.MustCompile(`^(0|[1-9][0-9]?)$`)
)

// ValidatePrefix rejects prefixes that are never a sane private guest pool.
func ValidatePrefix(prefix string) error {
	m := prefixPattern.FindStringSubmatch(prefix)
	if m == nil {
		return fmt.Errorf("guest prefix %q must be two decimal octets A.B", prefix)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	switch {
	case a < 1 || a > 223 || b > 255:
		return fmt.Errorf("guest prefix %q out of range", prefix)
	case a == 127:
		return errors.New("guest prefix must not be loopback")
	case a == 169 && b == 254:
		return errors.New("guest prefix must not be link-local")
	}
	return nil
}

// ParsePool parses the root-owned pool file. The format is KEY=VALUE lines
// (GUEST_PREFIX, MAX_SLOTS) with # comments; nothing is ever evaluated.
func ParsePool(data []byte) (Pool, error) {
	if strings.IndexByte(string(data), 0) >= 0 {
		return Pool{}, errors.New("pool.conf contains NUL")
	}
	p := Pool{MaxSlotCPUMilli: DefaultMaxSlotCPUMilli}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || seen[k] {
			return Pool{}, fmt.Errorf("invalid or duplicate pool line %q", line)
		}
		seen[k] = true
		switch k {
		case "GUEST_PREFIX":
			if err := ValidatePrefix(v); err != nil {
				return Pool{}, err
			}
			p.Prefix = v
		case "MAX_SLOTS":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > MaxSlots || strconv.Itoa(n) != v {
				return Pool{}, fmt.Errorf("MAX_SLOTS must be 1..%d", MaxSlots)
			}
			p.MaxSlots = n
		case "PUBLIC_IPV4":
			if err := ValidatePublicIPv4(v); err != nil {
				return Pool{}, err
			}
			p.PublicIPv4 = v
		case "SSH_PORT_BASE":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1024 || n > 65535 || strconv.Itoa(n) != v {
				return Pool{}, errors.New("SSH_PORT_BASE must be canonical decimal in 1024..65535")
			}
			p.SSHPortBase = n
		case "MAX_SLOT_CPU_MILLI":
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil || strconv.FormatUint(n, 10) != v || ValidateCPUMilli(uint32(n)) != nil {
				return Pool{}, fmt.Errorf("MAX_SLOT_CPU_MILLI must be a canonical multiple of %d in 10..%d", cpuStepMilli, cpuMaxMilli)
			}
			p.MaxSlotCPUMilli = uint32(n)
		default:
			return Pool{}, fmt.Errorf("unknown pool key %q", k)
		}
	}
	if p.Prefix == "" || p.MaxSlots == 0 {
		return Pool{}, errors.New("pool needs GUEST_PREFIX and MAX_SLOTS")
	}
	if (p.PublicIPv4 == "") != (p.SSHPortBase == 0) {
		return Pool{}, errors.New("PUBLIC_IPV4 and SSH_PORT_BASE must be paired")
	}
	if p.SSHPortBase != 0 && p.SSHPortBase+p.MaxSlots-1 > 65535 {
		return Pool{}, errors.New("public port range exceeds 65535")
	}
	return p, nil
}

// ValidatePublicIPv4 requires canonical IPv4 outside the helper's denied
// special/private ranges. Keep this list equal to fc-vm-network.sh's blocked.
func ValidatePublicIPv4(s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.String() != s {
		return errors.New("PUBLIC_IPV4 must be canonical IPv4")
	}
	for _, cidr := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
	} {
		if netip.MustParsePrefix(cidr).Contains(a) {
			return errors.New("PUBLIC_IPV4 must not be private or special-use")
		}
	}
	return nil
}

// ReadPool loads and parses a pool file.
func ReadPool(path string) (Pool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pool{}, err
	}
	return ParsePool(data)
}

// Start is one validated start-slot request.
type Start struct {
	Slot     int
	CPUMilli uint32
}

var quotaPattern = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)

// ParseRequest accepts exactly "start-slot N CPU_MILLI": N a canonical decimal
// below slots, CPU_MILLI a canonical positive multiple of 10 not above maxCPU.
func ParseRequest(line string, slots int, maxCPU uint32) (Start, error) {
	fields := strings.Split(line, " ")
	if len(fields) != 3 || fields[0] != "start-slot" || !slotPattern.MatchString(fields[1]) || !quotaPattern.MatchString(fields[2]) {
		return Start{}, errors.New("unsupported request; only 'start-slot N CPU_MILLI' is allowed")
	}
	n, _ := strconv.Atoi(fields[1])
	if n >= slots || n >= MaxSlots {
		return Start{}, fmt.Errorf("slot %d out of range", n)
	}
	q, _ := strconv.ParseUint(fields[2], 10, 32)
	if err := ValidateCPUMilli(uint32(q)); err != nil {
		return Start{}, err
	}
	if uint32(q) > maxCPU {
		return Start{}, fmt.Errorf("cpu quota %d exceeds the host ceiling %d", q, maxCPU)
	}
	return Start{Slot: n, CPUMilli: uint32(q)}, nil
}

// Handler performs the privileged action for an already validated request.
type Handler interface {
	StartSlot(ctx context.Context, slot int, cpuMilli uint32) error
}

func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Serve reads ONE request line (at most 64 bytes, newline-terminated), validates
// it, calls the handler and writes "ok" or "error: ..." . Anything after the
// first line is ignored.
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler, lim Limits) error {
	br := bufio.NewReaderSize(io.LimitReader(r, 64), 64)
	line, err := br.ReadString('\n')
	fail := func(err error) error {
		fmt.Fprintf(w, "error: %s\n", oneLine(err.Error()))
		return err
	}
	if err != nil {
		return fail(errors.New("request must be one newline-terminated line of at most 63 bytes"))
	}
	req, err := ParseRequest(strings.TrimSuffix(line, "\n"), lim.Slots, lim.MaxCPUMilli)
	if err != nil {
		return fail(err)
	}
	if err := h.StartSlot(ctx, req.Slot, req.CPUMilli); err != nil {
		return fail(err)
	}
	_, err = io.WriteString(w, "ok\n")
	return err
}

// Request is the client side: it refuses out-of-range slots locally.
func Request(conn io.ReadWriter, slot int, cpuMilli uint32) error {
	if slot < 0 || slot >= MaxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	if err := ValidateCPUMilli(cpuMilli); err != nil {
		return err
	}
	if conn == nil {
		return errors.New("no connection")
	}
	if _, err := fmt.Fprintf(conn, "start-slot %d %d\n", slot, cpuMilli); err != nil {
		return err
	}
	reply, err := bufio.NewReaderSize(io.LimitReader(conn, 256), 256).ReadString('\n')
	if err != nil {
		return fmt.Errorf("helper gave no complete reply: %w", err)
	}
	reply = strings.TrimSuffix(reply, "\n")
	if reply == "ok" {
		return nil
	}
	return errors.New(strings.TrimPrefix(reply, "error: "))
}

// SystemdHandler sets a runtime CPUQuota on firecracker-vm@N.service, verifies
// systemd took it, and only then starts the unit, and only when it is idle.
// Systemctl is the absolute path of systemctl; it is a field only for tests.
type SystemdHandler struct {
	Systemctl string
	// LockDir defaults to the root-owned /run/firecracker-vms. Tests alone
	// override it; the helper command exposes no caller-controlled path.
	LockDir string
	// PublicEnabled comes only from the parsed root-owned pool, never the
	// socket request. Every public start must clear the old tenant's SSH tuple.
	PublicEnabled bool
	// RunNetworkCommand is a test-only execution seam. Production leaves it nil;
	// the executable and arguments are fixed below, not protocol inputs.
	RunNetworkCommand func(context.Context, string, ...string) ([]byte, error)
}

func (h SystemdHandler) StartSlot(ctx context.Context, slot int, cpuMilli uint32) error {
	if slot < 0 || slot >= MaxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	if err := ValidateCPUMilli(cpuMilli); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := h.lockSlot(slot)
	if err != nil {
		return err
	}
	defer unlock()
	unit := fmt.Sprintf("firecracker-vm@%d.service", slot)
	out, err := exec.CommandContext(ctx, h.Systemctl, "show", "--property=ActiveState", "--value", unit).Output()
	if err != nil {
		return fmt.Errorf("cannot determine state of %s: %w", unit, err)
	}
	switch state := strings.TrimSpace(string(out)); state {
	case "inactive", "failed":
	default:
		// Never start (or restart) a unit that might hold a running guest, and
		// never change the quota of one that may.
		return fmt.Errorf("slot %d is %q; refusing", slot, state)
	}
	// The network unit is RemainAfterExit: its ExecStart does not run again on
	// retained TAP reuse. Flush on EVERY start while holding this slot's lock,
	// only after confirming it idle. The script's global network lock nests
	// inside this lock; it never takes helper slot locks (no reverse ordering).
	if h.PublicEnabled {
		run := h.RunNetworkCommand
		if run == nil {
			run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
				return exec.CommandContext(ctx, path, args...).CombinedOutput()
			}
		}
		if out, err := run(ctx, "/usr/local/libexec/fc-vm-network", "flush-conntrack", strconv.Itoa(slot)); err != nil {
			return fmt.Errorf("flush conntrack of slot %d: %w: %s", slot, err, oneLine(string(out)))
		}
	}
	// systemd CPUQuota= is a whole percent: millicores/10. --runtime keeps the
	// per-VM value out of persistent configuration; every start overwrites it.
	if out, err := exec.CommandContext(ctx, h.Systemctl, "set-property", "--runtime", "--no-ask-password", unit,
		fmt.Sprintf("CPUQuota=%d%%", cpuMilli/cpuStepMilli)).CombinedOutput(); err != nil {
		return fmt.Errorf("set CPUQuota of %s: %w: %s", unit, err, oneLine(string(out)))
	}
	// Do not trust the write: read back what systemd will apply at start.
	out, err = exec.CommandContext(ctx, h.Systemctl, "show", "--property=CPUQuotaPerSecUSec", "--value", unit).Output()
	if err != nil {
		return fmt.Errorf("cannot read back CPUQuota of %s: %w", unit, err)
	}
	got, err := parseUSecSpan(string(out))
	if want := uint64(cpuMilli) * 1000; err != nil || got != want {
		return fmt.Errorf("CPUQuota of %s reads back as %q, want %d us per second; refusing to start", unit, oneLine(strings.TrimSpace(string(out))), want)
	}
	if out, err := exec.CommandContext(ctx, h.Systemctl, "start", "--no-ask-password", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %w: %s", unit, err, oneLine(string(out)))
	}
	return nil
}

var spanUnits = map[string]uint64{"us": 1, "ms": 1000, "s": 1000000, "min": 60000000}

var spanToken = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]{1,6}))?(us|ms|s|min)$`)

// parseUSecSpan reads systemd's human timespan ("500ms", "1s 500ms", "1.500s")
// as exact microseconds. "infinity" and anything unrecognized is an error.
func parseUSecSpan(s string) (uint64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, errors.New("empty timespan")
	}
	var total uint64
	for _, f := range fields {
		m := spanToken.FindStringSubmatch(f)
		if m == nil {
			return 0, fmt.Errorf("unrecognized timespan %q", oneLine(f))
		}
		unit := spanUnits[m[3]]
		whole, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return 0, err
		}
		total += whole * unit
		if m[2] != "" {
			frac, _ := strconv.ParseUint(m[2], 10, 32)
			scale := uint64(1)
			for range len(m[2]) {
				scale *= 10
			}
			if frac*unit%scale != 0 {
				return 0, fmt.Errorf("timespan %q is not a whole number of microseconds", f)
			}
			total += frac * unit / scale
		}
	}
	return total, nil
}
