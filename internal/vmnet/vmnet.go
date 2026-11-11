// Package vmnet manages the host side of one VM slot: its TAP device, /30
// address, and the tagged firewall/NAT rules. It is the Go replacement of the
// former fc-vm-network.sh and keeps its ownership rules:
//
//   - every chain and hook rule belongs exclusively to slot N (FCVM<N>F/I/N/M/V6,
//     comment "firecracker-vms:slot<N>"); there is no global flush or policy change;
//   - an existing TAP is only adopted when it already has the expected address and
//     is never deleted unless this tool created it (ownership marker);
//   - routes in every routing table and every interface address are checked for
//     overlap with the slot /30 before anything is changed.
//
// All external commands go through Runner so behavior is testable without root.
package vmnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

// Fixed locations (the tool accepts no caller-controlled paths).
const (
	DefaultPoolConf = "/etc/firecracker-vms/pool.conf"
	DefaultStateDir = "/run/firecracker-vms-net"
	Comment         = "firecracker-vms"
)

// Result is the outcome of one external command.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Runner executes an external command with optional stdin. The error is non-nil
// only when the command could not be run at all; a non-zero exit is a Result.
type Runner interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (Result, error)
}

// Blocked is the reserved destination set denied to guests (same set as
// vmhelper.ValidatePublicIPv4).
var Blocked = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
}

// Slot is the network manager for one slot of a pool.
type Slot struct {
	Pool     vmhelper.Pool
	N        int
	Run      Runner
	StateDir string
	// Euid and IPForward are seams for tests; nil means os.Geteuid and /proc.
	Euid      func() int
	IPForward func() (bool, error)

	// derived
	tap, netAddr, guest, hostAddr, state string
	fc, ic, nc, mc, v6c, tag             string
	port                                 int
}

// New validates the slot against the pool and derives its names.
func New(pool vmhelper.Pool, n int, run Runner, stateDir string) (*Slot, error) {
	if n < 0 || n >= pool.MaxSlots {
		return nil, fmt.Errorf("slot %d outside pool of %d slots", n, pool.MaxSlots)
	}
	s := &Slot{Pool: pool, N: n, Run: run, StateDir: stateDir}
	p := pool.Prefix
	s.tap = fmt.Sprintf("tap-vm%d", n)
	s.netAddr = fmt.Sprintf("%s.%d.0", p, n)
	s.guest = fmt.Sprintf("%s.%d.2", p, n)
	s.hostAddr = fmt.Sprintf("%s.%d.1/30", p, n)
	s.state = filepath.Join(stateDir, fmt.Sprintf("%d.iface", n))
	s.fc, s.ic, s.nc, s.mc, s.v6c = fmt.Sprintf("FCVM%dF", n), fmt.Sprintf("FCVM%dI", n), fmt.Sprintf("FCVM%dN", n), fmt.Sprintf("FCVM%dM", n), fmt.Sprintf("FCVM%dV6", n)
	s.tag = fmt.Sprintf("%s:slot%d", Comment, n)
	if pool.SSHPortBase != 0 {
		s.port = pool.SSHPortBase + n
	}
	return s, nil
}

// Tap returns the slot's TAP name.
func (s *Slot) Tap() string { return s.tap }

var (
	routeTypes = map[string]bool{"unicast": true, "local": true, "broadcast": true, "multicast": true, "throw": true,
		"unreachable": true, "prohibit": true, "blackhole": true, "nat": true, "anycast": true}
	ifacePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)
	destPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$`)
)

func (s *Slot) must(r Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if r.Code != 0 {
		return fmt.Errorf("%s: exit %d: %s", what, r.Code, strings.TrimSpace(r.Stderr))
	}
	return nil
}

func (s *Slot) ip(ctx context.Context, args ...string) (Result, error) {
	return s.Run.Run(ctx, "", "ip", args...)
}

// overlaps reports whether cidr intersects the slot /30 (a /0 default route is
// the expected default and never a collision).
func (s *Slot) overlaps(cidr string) bool {
	addrStr, lenStr, hasLen := strings.Cut(cidr, "/")
	length := 32
	if hasLen {
		n, err := strconv.Atoi(lenStr)
		if err != nil {
			return false
		}
		length = n
	}
	addr, err := netip.ParseAddr(addrStr)
	if err != nil || !addr.Is4() || length < 1 || length > 32 {
		return false
	}
	eff := length
	if eff > 30 {
		eff = 30
	}
	slotNet := netip.MustParseAddr(s.netAddr)
	a, _ := addr.Prefix(eff)
	b, _ := slotNet.Prefix(eff)
	return a == b
}

// routeDest returns the destination and dev of one `ip route` line.
func routeFields(line string) (dest, dev string, ok bool) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return "", "", false
	}
	dest = f[0]
	if routeTypes[dest] {
		dest = ""
		if len(f) > 1 {
			dest = f[1]
		}
	}
	for i := 0; i < len(f)-1; i++ {
		if f[i] == "dev" {
			dev = f[i+1]
		}
	}
	return dest, dev, true
}

// firstDev is like the old script's check loop: first "dev" wins there.
func firstDev(line string) string {
	f := strings.Fields(line)
	for i := 0; i < len(f)-1; i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

// CheckCollisions inspects every routing table and interface address.
func (s *Slot) CheckCollisions(ctx context.Context) error {
	r, err := s.ip(ctx, "-4", "route", "show", "table", "all")
	if err := s.must(r, err, "cannot list routes"); err != nil {
		return err
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		dest, _, ok := routeFields(line)
		if !ok || dest == "" || dest == "default" || !destPattern.MatchString(dest) {
			continue
		}
		if firstDev(line) == s.tap {
			continue
		}
		if s.overlaps(dest) {
			return fmt.Errorf("route %s (%s) overlaps guest subnet %s/30; choose another GUEST_PREFIX", dest, strings.Join(strings.Fields(line), " "), s.netAddr)
		}
	}
	r, err = s.ip(ctx, "-4", "-o", "addr", "show")
	if err := s.must(r, err, "cannot list addresses"); err != nil {
		return err
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		f := strings.Fields(line)
		for i := 0; i < len(f)-1; i++ {
			if f[i] == "inet" && (len(f) < 2 || f[1] != s.tap) && s.overlaps(f[i+1]) {
				return fmt.Errorf("interface address %s overlaps guest subnet %s/30", f[i+1], s.netAddr)
			}
		}
	}
	return nil
}

// hook inserts (or re-inserts at the top, repairing precedence) one exact,
// commented jump rule atomically.
func (s *Slot) hook(ctx context.Context, tool, table, base string, rule ...string) error {
	check := append([]string{"-w", "-t", table, "-C", base}, rule...)
	r, err := s.Run.Run(ctx, "", tool, check...)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s\n", table)
	if r.Code == 0 {
		fmt.Fprintf(&b, "-D %s %s\n", base, strings.Join(rule, " "))
	}
	fmt.Fprintf(&b, "-I %s %s\nCOMMIT\n", base, strings.Join(rule, " "))
	r, err = s.Run.Run(ctx, b.String(), tool+"-restore", "-w", "--noflush")
	return s.must(r, err, tool+"-restore hook")
}

func (s *Slot) unhook(ctx context.Context, tool, table, base string, rule ...string) error {
	for i := 0; i < 1000; i++ {
		r, err := s.Run.Run(ctx, "", tool, append([]string{"-w", "-t", table, "-C", base}, rule...)...)
		if err != nil {
			return err
		}
		if r.Code != 0 {
			return nil
		}
		r, err = s.Run.Run(ctx, "", tool, append([]string{"-w", "-t", table, "-D", base}, rule...)...)
		if err := s.must(r, err, tool+" -D"); err != nil {
			return err
		}
	}
	return errors.New("too many duplicate owned rules")
}

func (s *Slot) findExternal(ctx context.Context) (string, error) {
	r, err := s.ip(ctx, "-4", "-o", "route", "show", "default")
	if err := s.must(r, err, "cannot list default route"); err != nil {
		return "", err
	}
	routes := strings.TrimRight(r.Stdout, "\n")
	if routes == "" || strings.Contains(routes, "\n") {
		return "", errors.New("need exactly one IPv4 default route")
	}
	iface := firstDev(routes)
	if !ifacePattern.MatchString(iface) || iface == s.tap {
		return "", errors.New("Cannot find outbound interface")
	}
	if s.Pool.PublicIPv4 != "" {
		r, err := s.ip(ctx, "-4", "-o", "addr", "show")
		if err := s.must(r, err, "cannot list addresses"); err != nil {
			return "", err
		}
		found := false
		for _, line := range strings.Split(r.Stdout, "\n") {
			f := strings.Fields(line)
			for i := 0; i < len(f)-1; i++ {
				addr, _, _ := strings.Cut(f[i+1], "/")
				if f[i] == "inet" && addr == s.Pool.PublicIPv4 && f[1] == iface {
					found = true
				}
			}
		}
		if !found {
			return "", errors.New("PUBLIC_IPV4 must be assigned to the default-route interface")
		}
	}
	return iface, nil
}

func (s *Slot) verifyTap(ctx context.Context) error {
	r, err := s.ip(ctx, "tuntap", "show", "dev", s.tap)
	if err := s.must(r, err, "ip tuntap show"); err != nil {
		return err
	}
	matched := 0
	// Some iproute2 versions ignore the dev filter: validate the exact device.
	for _, row := range strings.Split(r.Stdout, "\n") {
		if strings.HasPrefix(row, s.tap+":") {
			if row != s.tap+": tap" && !strings.HasPrefix(row, s.tap+": tap ") {
				return errors.New("existing interface is not the expected TAP")
			}
			matched++
		}
	}
	if matched != 1 {
		return errors.New("existing interface is not the expected TAP")
	}
	r, err = s.ip(ctx, "-4", "-o", "addr", "show", "dev", s.tap)
	if err := s.must(r, err, "ip addr show"); err != nil {
		return err
	}
	if !strings.Contains(r.Stdout, " inet "+s.hostAddr+" ") {
		return fmt.Errorf("existing TAP lacks expected %s; not modifying it", s.hostAddr)
	}
	return nil
}

// FlushEndpoint deletes conntrack entries of this slot's public endpoint.
func (s *Slot) FlushEndpoint(ctx context.Context) error {
	if s.Pool.PublicIPv4 == "" {
		return nil
	}
	r, err := s.Run.Run(ctx, "", "conntrack", "-D", "-f", "ipv4", "-p", "tcp", "--orig-dst", s.Pool.PublicIPv4, "--dport", strconv.Itoa(s.port))
	if err != nil {
		return fmt.Errorf("endpoint conntrack deletion failed: %w", err)
	}
	if r.Code != 0 {
		out := r.Stdout + r.Stderr
		// Exit 1 also covers permission/backend errors: accept it ONLY with the
		// tool's explicit zero-deletions diagnostic.
		if r.Code != 1 || !strings.Contains(out, ": 0 flow entries have been deleted.") {
			return fmt.Errorf("endpoint conntrack deletion failed: %s", strings.TrimSpace(out))
		}
	}
	return nil
}

// rules builds the restore payloads. It is pure so tests can inspect it.
type payloads struct{ v6, filter, nat string }

func (s *Slot) buildRules(iface, routes string) payloads {
	var v6, f, n strings.Builder
	fmt.Fprintf(&v6, "*filter\n:%s - [0:0]\n-F %s\n-A %s -j DROP\nCOMMIT\n", s.v6c, s.v6c, s.v6c)

	fmt.Fprintf(&f, "*filter\n:%s - [0:0]\n:%s - [0:0]\n-F %s\n-F %s\n", s.fc, s.ic, s.fc, s.ic)
	fmt.Fprintf(&f, "-A %s -i %s ! -s %s/32 -j DROP\n", s.fc, s.tap, s.guest)
	fmt.Fprintf(&f, "-A %s -m conntrack --ctstate INVALID -j DROP\n", s.fc)
	public := s.Pool.PublicIPv4
	if public != "" {
		// Only the REPLY direction of this exact DNAT endpoint bypasses destination blocks.
		fmt.Fprintf(&f, "-A %s -i %s -o %s -s %s/32 -p tcp --sport 22 -m conntrack --ctstate DNAT --ctdir REPLY --ctorigdst %s --ctorigdstport %d -j ACCEPT\n", s.fc, s.tap, iface, s.guest, public, s.port)
	}
	for _, cidr := range Blocked {
		fmt.Fprintf(&f, "-A %s -i %s -d %s -j DROP\n", s.fc, s.tap, cidr)
	}
	// Global-address VPN/policy routes are also denied (not just RFC1918).
	for _, line := range strings.Split(routes, "\n") {
		dest, dev, ok := routeFields(line)
		if !ok || dest == "" || dest == "default" || dest == "0.0.0.0/0" || !destPattern.MatchString(dest) {
			continue
		}
		if dev == "" || dev == iface {
			continue
		}
		fmt.Fprintf(&f, "-A %s -i %s -d %s -j DROP\n", s.fc, s.tap, dest)
	}
	fmt.Fprintf(&f, "-A %s -i %s -o %s -j ACCEPT\n", s.fc, s.tap, iface)
	fmt.Fprintf(&f, "-A %s -i %s -o %s -d %s/32 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n", s.fc, iface, s.tap, s.guest)
	if public != "" {
		fmt.Fprintf(&f, "-A %s -i %s -o %s -d %s/32 -p tcp --dport 22 -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdst %s --ctorigdstport %d -j ACCEPT\n", s.fc, iface, s.tap, s.guest, public, s.port)
	}
	fmt.Fprintf(&f, "-A %s -j DROP\n", s.fc)
	fmt.Fprintf(&f, "-A %s ! -s %s/32 -j DROP\n", s.ic, s.guest)
	fmt.Fprintf(&f, "-A %s -m conntrack --ctstate ESTABLISHED,RELATED --ctdir REPLY -j ACCEPT\n", s.ic)
	fmt.Fprintf(&f, "-A %s -j DROP\nCOMMIT\n", s.ic)

	fmt.Fprintf(&n, "*nat\n:%s - [0:0]\n:%s - [0:0]\n-F %s\n-F %s\n", s.nc, s.mc, s.nc, s.mc)
	if public != "" {
		fmt.Fprintf(&n, "-A %s -i %s -d %s/32 -p tcp --dport %d -j DNAT --to-destination %s:22\n", s.nc, iface, public, s.port, s.guest)
	}
	fmt.Fprintf(&n, "-A %s -o %s -s %s/32 -j MASQUERADE\nCOMMIT\n", s.mc, iface, s.guest)
	return payloads{v6.String(), f.String(), n.String()}
}

func (s *Slot) restore(ctx context.Context, tool, payload string) error {
	r, err := s.Run.Run(ctx, payload, tool+"-restore", "-w", "--noflush")
	return s.must(r, err, tool+"-restore")
}

func (s *Slot) applyRules(ctx context.Context, iface string) error {
	rr, err := s.ip(ctx, "-4", "route", "show", "table", "all")
	if err := s.must(rr, err, "cannot list routes"); err != nil {
		return err
	}
	p := s.buildRules(iface, rr.Stdout)
	tag := []string{"-m", "comment", "--comment", s.tag}
	if err := s.restore(ctx, "ip6tables-nft", p.v6); err != nil {
		return err
	}
	for _, h := range []struct {
		base, dir string
	}{{"INPUT", "-i"}, {"OUTPUT", "-o"}, {"FORWARD", "-i"}, {"FORWARD", "-o"}} {
		if err := s.hook(ctx, "ip6tables-nft", "filter", h.base, append([]string{h.dir, s.tap}, append(append([]string{}, tag...), "-j", s.v6c)...)...); err != nil {
			return err
		}
	}
	if err := s.restore(ctx, "iptables-nft", p.filter); err != nil {
		return err
	}
	jump := func(chain string) []string { return append(append([]string{}, tag...), "-j", chain) }
	for _, h := range []struct{ base, dir, chain string }{{"INPUT", "-i", s.ic}, {"FORWARD", "-i", s.fc}, {"FORWARD", "-o", s.fc}} {
		if err := s.hook(ctx, "iptables-nft", "filter", h.base, append([]string{h.dir, s.tap}, jump(h.chain)...)...); err != nil {
			return err
		}
	}
	if err := s.restore(ctx, "iptables-nft", p.nat); err != nil {
		return err
	}
	if err := s.hook(ctx, "iptables-nft", "nat", "PREROUTING", jump(s.nc)...); err != nil {
		return err
	}
	return s.hook(ctx, "iptables-nft", "nat", "POSTROUTING", append([]string{"-s", s.guest + "/32"}, jump(s.mc)...)...)
}

// removeLegacy migrates only the three exact rules emitted by the first
// version of the tool, never an unrelated broad rule.
func (s *Slot) removeLegacy(ctx context.Context) error {
	data, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	old, _, _ := strings.Cut(string(data), "\n")
	if !ifacePattern.MatchString(old) {
		return errors.New("invalid ownership marker")
	}
	c := []string{"-m", "comment", "--comment", Comment}
	if err := s.unhook(ctx, "iptables-nft", "filter", "FORWARD", append(append([]string{"-i", old, "-o", s.tap, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED"}, c...), "-j", "ACCEPT")...); err != nil {
		return err
	}
	if err := s.unhook(ctx, "iptables-nft", "filter", "FORWARD", append(append([]string{"-i", s.tap, "-o", old}, c...), "-j", "ACCEPT")...); err != nil {
		return err
	}
	return s.unhook(ctx, "iptables-nft", "nat", "POSTROUTING", append(append([]string{"-o", old, "-s", s.guest + "/32"}, c...), "-j", "MASQUERADE")...)
}

func (s *Slot) removeRules(ctx context.Context) error {
	tag := []string{"-m", "comment", "--comment", s.tag}
	j := func(chain string) []string { return append(append([]string{}, tag...), "-j", chain) }
	steps := []struct {
		tool, table, base string
		rule              []string
	}{
		{"iptables-nft", "nat", "PREROUTING", j(s.nc)},
		{"iptables-nft", "nat", "POSTROUTING", append([]string{"-s", s.guest + "/32"}, j(s.mc)...)},
		{"iptables-nft", "filter", "INPUT", append([]string{"-i", s.tap}, j(s.ic)...)},
		{"iptables-nft", "filter", "FORWARD", append([]string{"-i", s.tap}, j(s.fc)...)},
		{"iptables-nft", "filter", "FORWARD", append([]string{"-o", s.tap}, j(s.fc)...)},
		{"ip6tables-nft", "filter", "INPUT", append([]string{"-i", s.tap}, j(s.v6c)...)},
		{"ip6tables-nft", "filter", "OUTPUT", append([]string{"-o", s.tap}, j(s.v6c)...)},
		{"ip6tables-nft", "filter", "FORWARD", append([]string{"-i", s.tap}, j(s.v6c)...)},
		{"ip6tables-nft", "filter", "FORWARD", append([]string{"-o", s.tap}, j(s.v6c)...)},
	}
	for _, st := range steps {
		if err := s.unhook(ctx, st.tool, st.table, st.base, st.rule...); err != nil {
			return err
		}
	}
	// Best-effort chain removal (the chains may already be gone).
	for _, c := range []struct{ tool, table, chain string }{
		{"iptables-nft", "filter", s.fc}, {"iptables-nft", "filter", s.ic},
		{"iptables-nft", "nat", s.nc}, {"iptables-nft", "nat", s.mc},
		{"ip6tables-nft", "filter", s.v6c},
	} {
		_, _ = s.Run.Run(ctx, "", c.tool, "-w", "-t", c.table, "-F", c.chain)
		_, _ = s.Run.Run(ctx, "", c.tool, "-w", "-t", c.table, "-X", c.chain)
	}
	return nil
}

func (s *Slot) tapExists(ctx context.Context) (bool, error) {
	r, err := s.ip(ctx, "link", "show", s.tap)
	if err != nil {
		return false, err
	}
	return r.Code == 0, nil
}

// Up creates (or adopts) the TAP and applies the owned rules; Refresh reapplies
// rules on an existing TAP without link, address, VMM or conntrack changes.
func (s *Slot) Up(ctx context.Context) error      { return s.upOrRefresh(ctx, false) }
func (s *Slot) Refresh(ctx context.Context) error { return s.upOrRefresh(ctx, true) }

func (s *Slot) upOrRefresh(ctx context.Context, refresh bool) error {
	if err := s.CheckCollisions(ctx); err != nil {
		return err
	}
	fwd := s.IPForward
	if fwd == nil {
		fwd = ipForwardEnabled
	}
	if on, err := fwd(); err != nil || !on {
		return errors.New("Enable host IPv4 forwarding once during setup")
	}
	iface, err := s.findExternal(ctx)
	if err != nil {
		return err
	}
	created := false
	exists, err := s.tapExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		if err := s.verifyTap(ctx); err != nil {
			return err
		}
	} else {
		if refresh {
			return errors.New("refresh requires an existing TAP")
		}
		if _, err := os.Lstat(s.state); err == nil {
			return errors.New("stale ownership marker; operator review required")
		}
		// The endpoint must not retain an old tenant's NAT mapping at reuse.
		if err := s.FlushEndpoint(ctx); err != nil {
			return err
		}
		r, err := s.ip(ctx, "tuntap", "add", "dev", s.tap, "mode", "tap")
		if err := s.must(r, err, "ip tuntap add"); err != nil {
			return err
		}
		if err := os.WriteFile(s.state, []byte(iface+"\n"), 0o644); err != nil {
			return err
		}
		created = true
		r, err = s.ip(ctx, "addr", "add", s.hostAddr, "dev", s.tap)
		if err := s.must(r, err, "ip addr add"); err != nil {
			return err
		}
		// The new TAP stays down until its deny/allow rules are installed.
	}
	if err := s.applyRules(ctx, iface); err != nil {
		return err
	}
	if err := s.removeLegacy(ctx); err != nil {
		return err
	}
	if created {
		r, err := s.ip(ctx, "link", "set", s.tap, "up")
		return s.must(r, err, "ip link set up")
	}
	return nil
}

// Down removes a TAP this tool created and its owned rules. An adopted TAP
// (no ownership marker) is never deleted and its isolation is kept.
func (s *Slot) Down(ctx context.Context) error {
	if _, err := os.Lstat(s.state); err == nil {
		r, err := s.ip(ctx, "link", "del", s.tap)
		if err := s.must(r, err, "ip link del"); err != nil {
			return err
		}
		if err := s.removeLegacy(ctx); err != nil {
			return err
		}
		if err := os.Remove(s.state); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		exists, err := s.tapExists(ctx)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("unowned existing TAP; refusing to remove its isolation rules")
		}
	}
	if err := s.removeRules(ctx); err != nil {
		return err
	}
	return s.FlushEndpoint(ctx)
}

func ipForwardEnabled() (bool, error) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "1", nil
}
