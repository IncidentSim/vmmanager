package vmnet

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

const (
	base   = "GUEST_PREFIX=10.213\nMAX_SLOTS=4\n"
	public = base + "PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=22000\n"
	clean  = "default via 192.168.1.1 dev eth0 proto dhcp\n" +
		"192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.50\n" +
		"172.16.0.0/30 dev tap-fcpg proto kernel scope link src 172.16.0.1\n" +
		"172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1 linkdown\n"
)

// fake is a small command/state model of ip/iptables/conntrack. It is not a
// replacement for off-host proof, only for the ownership and ordering logic.
type fake struct {
	routes, defaults, addrs string
	tap                     bool
	tuntapOut               string
	conntrackCode           int
	conntrackOut            string
	calls                   [][]string
	payloads                [][2]string // tool, payload
	rules                   map[string][][]string
}

func newFake() *fake {
	return &fake{
		routes:   "default via 8.8.4.1 dev eth0\n",
		defaults: "default via 8.8.4.1 dev eth0\n",
		rules:    map[string][][]string{},
	}
}

func key(tool, table, chain string) string {
	return strings.TrimSuffix(tool, "-restore") + ":" + table + ":" + chain
}

func (f *fake) Run(_ context.Context, stdin, name string, a ...string) (Result, error) {
	f.calls = append(f.calls, append([]string{name}, a...))
	var out string
	code := 0
	switch {
	case name == "ip":
		switch {
		case slices.Equal(a, []string{"-4", "route", "show", "table", "all"}):
			out = f.routes
		case slices.Equal(a, []string{"-4", "-o", "route", "show", "default"}):
			out = f.defaults
		case slices.Equal(a, []string{"-4", "-o", "addr", "show"}):
			out = "2: eth0 inet 8.8.4.4/24 scope global eth0\n" + f.addrs
			if f.tap {
				out += "3: tap-vm0 inet 10.213.0.1/30 scope global tap-vm0\n"
			}
		case slices.Equal(a, []string{"-4", "-o", "addr", "show", "dev", "tap-vm0"}):
			if f.tap {
				out = "3: tap-vm0 inet 10.213.0.1/30 scope global tap-vm0\n"
			}
		case a[0] == "tuntap" && a[1] == "show":
			out = f.tuntapOut
			if out == "" {
				out = "tap-vm0: tap persist\n"
			}
		case a[0] == "link" && a[1] == "show":
			if !f.tap {
				code = 1
			}
		case a[0] == "tuntap" && a[1] == "add":
			f.tap = true
		case a[0] == "link" && a[1] == "del":
			f.tap = false
		}
	case name == "conntrack":
		code, out = f.conntrackCode, f.conntrackOut
	case strings.HasSuffix(name, "-restore"):
		f.payloads = append(f.payloads, [2]string{name, stdin})
		table := "filter"
		for _, line := range strings.Split(stdin, "\n") {
			w := strings.Fields(line)
			switch {
			case strings.HasPrefix(line, "*"):
				table = line[1:]
			case strings.HasPrefix(line, ":"):
				k := key(name, table, strings.TrimPrefix(w[0], ":"))
				if _, ok := f.rules[k]; !ok {
					f.rules[k] = nil
				}
			case len(w) > 1 && w[0] == "-F":
				f.rules[key(name, table, w[1])] = nil
			case len(w) > 2 && (w[0] == "-A" || w[0] == "-I" || w[0] == "-D"):
				k := key(name, table, w[1])
				rule := splitRule(line)
				switch w[0] {
				case "-I":
					f.rules[k] = append([][]string{rule}, f.rules[k]...)
				case "-D":
					i := slices.IndexFunc(f.rules[k], func(r []string) bool { return slices.Equal(r, rule) })
					if i < 0 {
						return Result{Code: 1}, nil
					}
					f.rules[k] = slices.Delete(f.rules[k], i, i+1)
				default:
					f.rules[k] = append(f.rules[k], rule)
				}
			}
		}
	case name == "iptables-nft" || name == "ip6tables-nft":
		if a[0] == "-w" {
			a = a[1:]
		}
		table := "filter"
		if a[0] == "-t" {
			table, a = a[1], a[2:]
		}
		op, chain, rule := a[0], a[1], a[2:]
		k := key(name, table, chain)
		switch op {
		case "-C":
			if !slices.ContainsFunc(f.rules[k], func(r []string) bool { return slices.Equal(r, rule) }) {
				code = 1
			}
		case "-D":
			i := slices.IndexFunc(f.rules[k], func(r []string) bool { return slices.Equal(r, rule) })
			if i < 0 {
				code = 1
			} else {
				f.rules[k] = slices.Delete(f.rules[k], i, i+1)
			}
		case "-F":
			f.rules[k] = nil
		case "-X":
			delete(f.rules, k)
		}
	}
	return Result{Stdout: out, Code: code}, nil
}

func splitRule(line string) []string {
	w := strings.Fields(line)
	var out []string
	for i := 2; i < len(w); i++ {
		// the comment value is a single word in every emitted rule
		out = append(out, w[i])
	}
	return out
}

func (f *fake) called(prefix ...string) bool {
	return slices.ContainsFunc(f.calls, func(c []string) bool { return len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) })
}
func (f *fake) index(prefix ...string) int {
	return slices.IndexFunc(f.calls, func(c []string) bool { return len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) })
}
func (f *fake) allPayloads() string {
	var b strings.Builder
	for _, p := range f.payloads {
		b.WriteString(p[1])
	}
	return b.String()
}

func poolOf(t *testing.T, conf string) vmhelper.Pool {
	t.Helper()
	p, err := vmhelper.ParsePool([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func slotOf(t *testing.T, f *fake, conf string, n int) *Slot {
	t.Helper()
	s, err := New(poolOf(t, conf), n, f, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.IPForward = func() (bool, error) { return true, nil }
	return s
}

func check(t *testing.T, slot int, routes, addrs, pool string) (*fake, error) {
	t.Helper()
	f := newFake()
	f.routes, f.addrs = routes, addrs
	if pool == "" {
		pool = "GUEST_PREFIX=10.213\nMAX_SLOTS=2\n"
	}
	return f, slotOf(t, f, pool, slot).CheckCollisions(context.Background())
}

func TestCollisionFreeHost(t *testing.T) {
	if _, err := check(t, 0, clean, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestCoveringDockerRouteBlocksPool(t *testing.T) {
	_, err := check(t, 0, clean, "", "GUEST_PREFIX=172.17\nMAX_SLOTS=2\n")
	if err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverlappingRoutesAreCollisions(t *testing.T) {
	dflt := "default via 1.1.1.1 dev eth0\n"
	for name, route := range map[string]string{
		"covering /8":           "10.0.0.0/8 via 192.168.1.1 dev eth0",
		"covering /16":          "10.213.0.0/16 dev tun0",
		"exact /30":             "10.213.0.0/30 dev x",
		"host route inside":     "10.213.0.2 dev x scope link",
		"/31 inside":            "10.213.0.2/31 dev x",
		"typed route":           "unreachable 10.213.0.0/24",
		"vpn table":             "10.213.0.0/16 dev wg0 table 100 scope link",
		"vpn table named":       "10.213.0.0/24 dev tun0 table vpn proto static",
		"local table address":   "local 10.213.0.1 dev lo table local proto kernel scope host src 10.213.0.1",
		"local table broadcast": "broadcast 10.213.0.3 dev eth1 table local proto kernel scope link src 10.213.0.1",
		"throw":                 "throw 10.213.0.0/24 table 200",
		"blackhole":             "blackhole 10.213.0.0/30",
		"prohibit":              "prohibit 10.213.0.0/16 table 7",
		"unreachable in table":  "unreachable 10.213.0.0/28 table 9",
		"foreign tap":           "10.213.0.0/30 dev tap-vm1 proto kernel scope link src 10.213.0.1",
		"foreign tap broadcast": "broadcast 10.213.0.3 dev tap-vm1 table local proto kernel scope link src 10.213.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := check(t, 0, dflt+route, "", "")
			if err == nil || !strings.Contains(err.Error(), "overlaps") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOtherSlotsNetworkIsNotACollision(t *testing.T) {
	routes := "default via 1.1.1.1 dev eth0\n10.213.1.0/30 dev tap-vm1 proto kernel"
	for slot := 0; slot < 2; slot++ {
		if _, err := check(t, slot, routes, "", ""); err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
	}
	foreign := "default via 1.1.1.1 dev eth0\n10.213.1.0/30 dev docker1 proto kernel"
	if _, err := check(t, 0, foreign, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t, 1, foreign, "", ""); err == nil {
		t.Fatal("someone else on slot 1's range must collide")
	}
}

func TestEveryRoutingTableIsInspected(t *testing.T) {
	f, err := check(t, 0, clean, "", "")
	if err != nil || !f.called("ip", "-4", "route", "show", "table", "all") {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestUnrelatedRoutesInOtherTablesAreFine(t *testing.T) {
	routes := strings.Join([]string{
		"default via 10.0.0.1 dev wg0 table 100",
		"192.168.5.0/24 dev wg0 table 100 scope link",
		"local 192.168.1.50 dev eth0 table local proto kernel scope host src 192.168.1.50",
		"broadcast 192.168.1.255 dev eth0 table local proto kernel scope link src 192.168.1.50",
		"local 127.0.0.1 dev lo table local proto kernel scope host src 127.0.0.1",
		"throw 10.99.0.0/16 table 200",
		"unreachable 203.0.113.0/24 table 9",
		"blackhole 198.51.100.0/24",
		"default via 192.168.1.1 dev eth0 proto dhcp",
	}, "\n")
	if _, err := check(t, 0, routes, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestSlotsOwnInterfaceRoutesAreNotACollision(t *testing.T) {
	own := strings.Join([]string{
		"default via 192.168.1.1 dev eth0",
		"10.213.0.0/30 dev tap-vm0 proto kernel scope link src 10.213.0.1",
		"local 10.213.0.1 dev tap-vm0 table local proto kernel scope host src 10.213.0.1",
		"broadcast 10.213.0.0 dev tap-vm0 table local proto kernel scope link src 10.213.0.1",
		"broadcast 10.213.0.3 dev tap-vm0 table local proto kernel scope link src 10.213.0.1",
	}, "\n")
	if _, err := check(t, 0, own, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t, 0, strings.ReplaceAll(own, "tap-vm0", "tap-other"), "", ""); err == nil {
		t.Fatal("a different device claiming the slot range must collide")
	}
}

func TestInterfaceAddresses(t *testing.T) {
	own := "3: tap-vm0    inet 10.213.0.1/30 brd 10.213.0.3 scope global tap-vm0"
	if _, err := check(t, 0, clean, own, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t, 0, clean, strings.ReplaceAll(own, "tap-vm0", "tap-other"), ""); err == nil {
		t.Fatal("foreign owner of the slot address must collide")
	}
	_, err := check(t, 0, "default via 1.1.1.1 dev eth0", "2: eth1    inet 10.213.0.9/24 brd 10.213.0.255 scope global eth1", "")
	if err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultRouteIsExpected(t *testing.T) {
	if _, err := check(t, 0, "default via 10.213.0.1 dev eth0\n0.0.0.0/0 dev eth0", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMainRejectsBadInputWithoutTouchingTheNetwork(t *testing.T) {
	dir := t.TempDir()
	write := func(conf string) string {
		p := filepath.Join(dir, "pool.conf")
		if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, conf := range []string{
		"GUEST_PREFIX=10.213;reboot\nMAX_SLOTS=2\n", "GUEST_PREFIX=127.0\nMAX_SLOTS=2\n",
		"GUEST_PREFIX=10.213\nMAX_SLOTS=65\n", "GUEST_PREFIX=10.213\n", "FOO=bar\nGUEST_PREFIX=10.213\nMAX_SLOTS=2\n",
		"GUEST_PREFIX=10.213\nGUEST_PREFIX=10.214\nMAX_SLOTS=2\n", "GUEST_PREFIX=10.213\x00\nMAX_SLOTS=2\n",
		"GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=505\n", "GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=1e3\n",
		"GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=500\nMAX_SLOT_CPU_MILLI=500\n",
		base + "PUBLIC_IPV4=8.8.4.4\n", base + "SSH_PORT_BASE=22000\n", base + "PUBLIC_IPV4=008.8.4.4\nSSH_PORT_BASE=22000\n",
		base + "PUBLIC_IPV4=10.0.0.1\nSSH_PORT_BASE=22000\n", base + "PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=65534\n",
		base + "PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=022000\n", base + "PUBLIC_IPV4=$(id)\nSSH_PORT_BASE=22000\n",
	} {
		f := newFake()
		if err := Main(context.Background(), []string{"check", "0"}, write(conf), dir, f, 0); err == nil {
			t.Errorf("pool %q accepted", conf)
		}
		if len(f.calls) != 0 {
			t.Errorf("pool %q touched the network: %v", conf, f.calls)
		}
	}
	for _, slot := range []string{"4", "-1", "01", "x", "1;reboot", "", "+1"} {
		f := newFake()
		if err := Main(context.Background(), []string{"check", slot}, write(base), dir, f, 0); err == nil || len(f.calls) != 0 {
			t.Errorf("slot %q: err=%v calls=%v", slot, err, f.calls)
		}
	}
	// The optional CPU ceiling accepts exactly canonical multiples of 10.
	for _, v := range []string{"10", "500", "2000", "64000"} {
		if err := Main(context.Background(), []string{"check", "0"}, write(base+"MAX_SLOT_CPU_MILLI="+v+"\n"), dir, newFake(), 0); err != nil {
			t.Errorf("cpu %s: %v", v, err)
		}
	}
}

func TestPrivilegedActionsRequireRootAndChangeNothing(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "pool.conf")
	if err := os.WriteFile(conf, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"up", "refresh", "down", "flush-conntrack"} {
		f := newFake()
		err := Main(context.Background(), []string{action, "0"}, conf, filepath.Join(dir, "run"), f, 1000)
		if err == nil || !strings.Contains(err.Error(), "root") || len(f.calls) != 0 {
			t.Errorf("%s: err=%v calls=%v", action, err, f.calls)
		}
		if _, err := os.Stat(filepath.Join(dir, "run")); err == nil {
			t.Errorf("%s created the state directory", action)
		}
	}
	if err := Main(context.Background(), []string{"bogus", "0"}, conf, dir, newFake(), 0); err == nil {
		t.Error("unknown action accepted")
	}
}

// --- firewall model -------------------------------------------------------------

func refresher(t *testing.T, conf string) (*fake, *Slot) {
	f := newFake()
	f.tap = true
	f.rules["iptables-nft:filter:FORWARD"] = [][]string{{"-j", "FOREIGN"}}
	return f, slotOf(t, f, conf, 0)
}

func TestTuntapShowMayReturnAllDevices(t *testing.T) {
	f, s := refresher(t, public)
	f.tuntapOut = "tap-vm9: tap vnet_hdr persist\ntap-vm0: tap vnet_hdr persist\ntap-vm2: tap persist\n"
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRequestedDeviceMustItselfBeTap(t *testing.T) {
	f, s := refresher(t, public)
	f.tuntapOut = "tap-vm9: tap persist\ntap-vm0: tun persist\n"
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the expected TAP") {
		t.Fatalf("err = %v", err)
	}
}

func snapshot(f *fake) string { return fmt.Sprint(f.rules) }

func TestRefreshIsNonDestructiveAndIdempotent(t *testing.T) {
	f, s := refresher(t, public)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := snapshot(f)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first != snapshot(f) {
		t.Fatal("second refresh changed rules")
	}
	if !slices.ContainsFunc(f.rules["iptables-nft:filter:FORWARD"], func(r []string) bool { return slices.Equal(r, []string{"-j", "FOREIGN"}) }) {
		t.Fatal("foreign rule was removed")
	}
	for _, c := range [][]string{{"ip", "link", "del"}, {"ip", "link", "set"}, {"ip", "addr", "add"}, {"ip", "tuntap", "add"}, {"conntrack"}} {
		if f.called(c...) {
			t.Fatalf("refresh executed %v", c)
		}
	}
}

func TestRefreshReassertsHookPrecedence(t *testing.T) {
	f, s := refresher(t, public)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := "iptables-nft:filter:FORWARD"
	f.rules[k] = append([][]string{{"-j", "FOREIGN_ACCEPT"}}, f.rules[k]...)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := f.rules[k]
	if rows[0][len(rows[0])-1] != "FCVM0F" || rows[1][len(rows[1])-1] != "FCVM0F" {
		t.Fatalf("hooks not first: %v", rows)
	}
	if !slices.ContainsFunc(rows, func(r []string) bool { return slices.Equal(r, []string{"-j", "FOREIGN_ACCEPT"}) }) {
		t.Fatal("foreign rule removed")
	}
}

func TestRulesAreNarrow(t *testing.T) {
	f, s := refresher(t, public)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := f.allPayloads()
	for _, want := range []string{
		"-i eth0 -d 8.8.4.4/32 -p tcp --dport 22000", "DNAT --to-destination 10.213.0.2:22",
		"! -s 10.213.0.2/32 -j DROP", "-d 10.0.0.0/8 -j DROP", "-d 169.254.0.0/16 -j DROP",
		"-o eth0 -j ACCEPT", "MASQUERADE", "--ctdir REPLY",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(text, "SNAT") {
		t.Error("SNAT emitted")
	}
	for _, p := range f.payloads {
		if p[0] == "ip6tables-nft-restore" {
			if !strings.Contains(p[1], "-j DROP") {
				t.Error("IPv6 not dropped")
			}
			return
		}
	}
	t.Error("no IPv6 payload")
}

func TestDisabledDefaultHasNoDNAT(t *testing.T) {
	f, s := refresher(t, base)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.allPayloads(), "-j DNAT") {
		t.Fatal("DNAT emitted without public publication")
	}
}

func TestDownNeverDeletesUnownedTap(t *testing.T) {
	f, s := refresher(t, public)
	err := s.Down(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unowned existing TAP") || !f.tap {
		t.Fatalf("err=%v tap=%v", err, f.tap)
	}
}

// verdict evaluates the emitted match subset of a chain, failing on any
// option it does not model, so a new rule cannot silently escape these tests.
func verdict(t *testing.T, f *fake, chain string, pkt map[string]string) string {
	t.Helper()
	names := map[string]string{"-i": "i", "-o": "o", "-p": "proto", "--sport": "sport", "--dport": "dport",
		"--ctstate": "state", "--ctdir": "dir", "--ctorigdst": "origdst", "--ctorigdstport": "origport"}
	get := func(k, def string) string {
		if v, ok := pkt[k]; ok {
			return v
		}
		return def
	}
	for _, row := range f.rules["iptables-nft:filter:"+chain] {
		match, negate := true, false
		for i := 0; i < len(row); i++ {
			key := row[i]
			if key == "!" {
				negate = true
				continue
			}
			i++
			value := row[i]
			if key == "-m" {
				continue
			}
			if key == "-j" {
				if match {
					return value
				}
				break
			}
			var res bool
			switch key {
			case "-s", "-d":
				field := "src"
				if key == "-d" {
					field = "dst"
				}
				res = netip.MustParsePrefix(value).Contains(netip.MustParseAddr(get(field, "0.0.0.0")))
			case "--ctstate":
				if value == "DNAT" {
					res = pkt["status"] == "DNAT"
				} else {
					res = slices.Contains(strings.Split(value, ","), pkt["state"])
				}
			default:
				name, ok := names[key]
				if !ok {
					t.Fatalf("unmodelled option %s in %v", key, row)
				}
				res = slices.Contains(strings.Split(value, ","), pkt[name])
			}
			match = match && (res != negate)
			negate = false
		}
	}
	return "RETURN"
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestGuestSpoofHostPrivateAndInternetPolicy(t *testing.T) {
	f, s := refresher(t, public)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	guest := map[string]string{"i": "tap-vm0", "o": "eth0", "src": "10.213.0.2", "dst": "1.1.1.1", "proto": "udp", "dport": "53", "state": "NEW", "dir": "ORIGINAL"}
	eq := func(chain string, pkt map[string]string, want string) {
		t.Helper()
		if got := verdict(t, f, chain, pkt); got != want {
			t.Errorf("%s %v = %s, want %s", chain, pkt, got, want)
		}
	}
	eq("FCVM0F", guest, "ACCEPT")
	eq("FCVM0F", with(guest, "dst", "9.9.9.9", "proto", "tcp", "dport", "443"), "ACCEPT")
	eq("FCVM0F", with(guest, "src", "10.213.1.2"), "DROP")
	for _, dst := range []string{"10.213.1.2", "169.254.169.254", "100.100.100.200", "192.168.1.1", "127.0.0.1"} {
		eq("FCVM0F", with(guest, "dst", dst), "DROP")
	}
	// New INPUT is denied even for the host's public address; replies to
	// host-originated connections are allowed.
	eq("FCVM0I", with(guest, "dst", "8.8.4.4"), "DROP")
	eq("FCVM0I", with(guest, "state", "ESTABLISHED", "dir", "REPLY"), "ACCEPT")
	inbound := map[string]string{"i": "eth0", "o": "tap-vm0", "src": "9.9.9.9", "dst": "10.213.0.2", "proto": "tcp", "dport": "22",
		"state": "NEW", "dir": "ORIGINAL", "status": "DNAT", "origdst": "8.8.4.4", "origport": "22000"}
	eq("FCVM0F", inbound, "ACCEPT")
	for _, diff := range [][]string{{"dport", "80"}, {"origport", "22001"}, {"origdst", "8.8.8.8"}, {"status", ""}, {"i", "wg0"}} {
		eq("FCVM0F", with(inbound, diff...), "DROP")
	}
	reply := with(guest, "proto", "tcp", "sport", "22", "state", "ESTABLISHED", "dir", "REPLY", "status", "DNAT", "origdst", "8.8.4.4", "origport", "22000", "dst", "192.168.1.2")
	eq("FCVM0F", reply, "ACCEPT")
	eq("FCVM0F", with(reply, "dir", "ORIGINAL"), "DROP")
}

func TestVPNGlobalRouteDeniedAndConntrackFailureBlocksCreation(t *testing.T) {
	f, s := refresher(t, public)
	f.routes = "default via 8.8.4.1 dev eth0\n9.9.0.0/16 dev wg0 table 100\n"
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := verdict(t, f, "FCVM0F", map[string]string{"i": "tap-vm0", "o": "eth0", "src": "10.213.0.2", "dst": "9.9.9.9", "state": "NEW"}); got != "DROP" {
		t.Fatalf("VPN destination verdict = %s", got)
	}
	f, s = refresher(t, public)
	f.tap, f.conntrackCode = false, 1
	err := s.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conntrack deletion failed") || f.tap {
		t.Fatalf("err=%v tap=%v", err, f.tap)
	}
	// The explicit zero-deletions diagnostic is the only accepted exit 1.
	f.conntrackOut = "conntrack v1.4.8 (conntrack-tools): 0 flow entries have been deleted.\n"
	if err := s.FlushEndpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewTapIsProtectedBeforeLinkUpAndCleansConntrack(t *testing.T) {
	f, s := refresher(t, public)
	f.tap = false
	ctx := context.Background()
	if err := s.Up(ctx); err != nil {
		t.Fatal(err)
	}
	flush, create, up := f.index("conntrack"), f.index("ip", "tuntap", "add"), f.index("ip", "link", "set")
	lastRestore := -1
	for i, c := range f.calls {
		if strings.HasSuffix(c[0], "-restore") {
			lastRestore = i
		}
	}
	if !(flush >= 0 && flush < create && lastRestore < up && up > create) {
		t.Fatalf("bad order flush=%d create=%d lastRestore=%d up=%d", flush, create, lastRestore, up)
	}
	if err := s.Up(ctx); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range f.calls {
		if c[0] == "conntrack" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("conntrack calls = %d", n)
	}
	if err := s.Down(ctx); err != nil || f.tap {
		t.Fatalf("down: err=%v tap=%v", err, f.tap)
	}
	if err := s.Down(ctx); err != nil {
		t.Fatalf("second down: %v", err)
	}
	if !slices.ContainsFunc(f.rules["iptables-nft:filter:FORWARD"], func(r []string) bool { return slices.Equal(r, []string{"-j", "FOREIGN"}) }) {
		t.Fatal("foreign rule removed by down")
	}
	for k, rows := range f.rules {
		for _, r := range rows {
			if strings.Contains(strings.Join(r, " "), "firecracker-vms:slot0") {
				t.Fatalf("owned rule left in %s: %v", k, r)
			}
		}
	}
}

func TestStaleMarkerRequiresOperatorReview(t *testing.T) {
	f, s := refresher(t, base)
	f.tap = false
	if err := os.WriteFile(s.state, []byte("eth0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "stale ownership marker") {
		t.Fatalf("err = %v", err)
	}
	if f.called("ip", "tuntap", "add") {
		t.Fatal("created TAP despite stale marker")
	}
}

func TestPublicAddressMustBeOnDefaultInterface(t *testing.T) {
	f, s := refresher(t, public)
	f.defaults = "default via 8.8.4.1 dev eth9\n"
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "PUBLIC_IPV4") {
		t.Fatalf("err = %v", err)
	}
	f.defaults = "default via 1.1.1.1 dev eth0\ndefault via 2.2.2.2 dev eth1\n"
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v", err)
	}
}

func TestForwardingMustBeEnabled(t *testing.T) {
	_, s := refresher(t, base)
	s.IPForward = func() (bool, error) { return false, nil }
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "forwarding") {
		t.Fatalf("err = %v", err)
	}
}

func TestExplicitConntrackFlushScopesOriginalTuple(t *testing.T) {
	f, s := refresher(t, public)
	if err := s.FlushEndpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, c := range f.calls {
		if c[0] == "conntrack" {
			got = append(got, c)
		}
	}
	want := [][]string{{"conntrack", "-D", "-f", "ipv4", "-p", "tcp", "--orig-dst", "8.8.4.4", "--dport", "22000"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v", got)
	}
}

// The tool may only ever touch its own interface and tagged rules: it contains
// no route mutation, no global flush and no foreign interface name.
func TestSourceOnlyTouchesOwnedObjects(t *testing.T) {
	src, err := os.ReadFile("vmnet.go")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, l := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			code = append(code, l)
		}
	}
	text := strings.Join(code, "\n")
	for _, bad := range []string{"tap-fcpg", `"route", "add"`, `"route", "del"`, `"route", "replace"`, `"-P"`, `"-Z"`, `"--flush"`, "nft flush"} {
		if strings.Contains(text, bad) {
			t.Errorf("source contains %q", bad)
		}
	}
}
