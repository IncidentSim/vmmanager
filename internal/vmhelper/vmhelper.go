// Package vmhelper is the narrow privilege boundary between the unprivileged
// vmapi and the root-owned systemd units of a slot. It defines a one-line
// protocol ("start-slot N"), the strict pool configuration shared by the helper,
// the network script and vmapi, and a handler that can only start an idle
// firecracker-vm@N.service. There is deliberately no stop, kill, restart, path,
// unit-name or command verb.
package vmhelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
	var p Pool
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
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
		default:
			return Pool{}, fmt.Errorf("unknown pool key %q", k)
		}
	}
	if p.Prefix == "" || p.MaxSlots == 0 {
		return Pool{}, errors.New("pool needs GUEST_PREFIX and MAX_SLOTS")
	}
	return p, nil
}

// ReadPool loads and parses a pool file.
func ReadPool(path string) (Pool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pool{}, err
	}
	return ParsePool(data)
}

// ParseRequest accepts exactly "start-slot N" with N a canonical decimal < max.
func ParseRequest(line string, max int) (int, error) {
	verb, arg, ok := strings.Cut(line, " ")
	if !ok || verb != "start-slot" || !slotPattern.MatchString(arg) {
		return 0, errors.New("unsupported request; only 'start-slot N' is allowed")
	}
	n, _ := strconv.Atoi(arg)
	if n >= max || n >= MaxSlots {
		return 0, fmt.Errorf("slot %d out of range", n)
	}
	return n, nil
}

// Handler performs the privileged action for an already validated slot.
type Handler interface {
	StartSlot(ctx context.Context, slot int) error
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
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler, max int) error {
	br := bufio.NewReaderSize(io.LimitReader(r, 64), 64)
	line, err := br.ReadString('\n')
	fail := func(err error) error {
		fmt.Fprintf(w, "error: %s\n", oneLine(err.Error()))
		return err
	}
	if err != nil {
		return fail(errors.New("request must be one newline-terminated line of at most 63 bytes"))
	}
	slot, err := ParseRequest(strings.TrimSuffix(line, "\n"), max)
	if err != nil {
		return fail(err)
	}
	if err := h.StartSlot(ctx, slot); err != nil {
		return fail(err)
	}
	_, err = io.WriteString(w, "ok\n")
	return err
}

// Request is the client side: it refuses out-of-range slots locally.
func Request(conn io.ReadWriter, slot int) error {
	if slot < 0 || slot >= MaxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	if conn == nil {
		return errors.New("no connection")
	}
	if _, err := fmt.Fprintf(conn, "start-slot %d\n", slot); err != nil {
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

// SystemdHandler starts firecracker-vm@N.service, and only when it is idle.
// Systemctl is the absolute path of systemctl; it is a field only for tests.
type SystemdHandler struct{ Systemctl string }

func (h SystemdHandler) StartSlot(ctx context.Context, slot int) error {
	if slot < 0 || slot >= MaxSlots {
		return fmt.Errorf("slot %d out of range", slot)
	}
	unit := fmt.Sprintf("firecracker-vm@%d.service", slot)
	out, err := exec.CommandContext(ctx, h.Systemctl, "show", "--property=ActiveState", "--value", unit).Output()
	if err != nil {
		return fmt.Errorf("cannot determine state of %s: %w", unit, err)
	}
	switch state := strings.TrimSpace(string(out)); state {
	case "inactive", "failed":
	default:
		// Never start (or restart) a unit that might hold a running guest.
		return fmt.Errorf("slot %d is %q; refusing", slot, state)
	}
	if out, err := exec.CommandContext(ctx, h.Systemctl, "start", "--no-ask-password", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %w: %s", unit, err, oneLine(string(out)))
	}
	return nil
}
