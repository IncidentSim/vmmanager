package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

// Opt-in because not every deployment promises public SSH or guest internet.
// With PRIVATE_PREFIX set this is explicitly HOST-LOCAL, not off-host ingress.
func TestLiveGuestNetworkAndSlotReuse(t *testing.T) {
	if os.Getenv("VMAPI_E2E_NETWORK") != "1" {
		t.Skip("set VMAPI_E2E_NETWORK=1 for public endpoint/egress/isolation acceptance")
	}
	s := newSuite(t)
	s.preflight()
	host := os.Getenv("VMAPI_E2E_NETWORK_HOST")
	base, err := strconv.Atoi(os.Getenv("VMAPI_E2E_NETWORK_PORT_BASE"))
	if net.ParseIP(host) == nil || base < 1024 || base > 65532 || err != nil {
		t.Fatal("configure NETWORK_HOST literal IP and NETWORK_PORT_BASE with four ports")
	}
	prefix := os.Getenv("VMAPI_E2E_NETWORK_PRIVATE_PREFIX")
	if prefix != "" && net.ParseIP(prefix+".0.2") == nil {
		t.Fatal("invalid NETWORK_PRIVATE_PREFIX")
	}
	if prefix != "" {
		t.Log("HOST-LOCAL SSH; this run does NOT prove public ingress")
	} else {
		t.Log("SSH uses advertised public endpoint; off-host evidence requires running this test from another machine")
	}
	previousPort := 0
	for attempt := 0; attempt < 2; attempt++ {
		a := s.createActive(fmt.Sprintf("net%d", attempt), s.cfg.BaseImageID)
		ctx, cancel := context.WithTimeout(s.ctx, sshReadyWait)
		var meta controlclient.SSH
		err = pollUntil(ctx, pollEvery, func(ctx context.Context) (bool, error) {
			m, e := s.c.SSH(ctx, s.owner, a.VMID)
			meta = m
			return m.Ready, e
		})
		cancel()
		if err != nil {
			t.Fatal("private API readiness: ", err)
		}
		if meta.Host != host || meta.Port < base || meta.Port >= base+4 || meta.Bastion != nil {
			t.Fatalf("unexpected public SSH endpoint: host=%s port=%d bastion=%t", meta.Host, meta.Port, meta.Bastion != nil)
		}
		if attempt > 0 && os.Getenv("VMAPI_E2E_NETWORK_REUSE") == "1" && previousPort != meta.Port {
			t.Fatal("exclusive reuse acceptance did not reacquire the same public slot")
		}
		previousPort = meta.Port
		slot := meta.Port - base
		target := meta
		if prefix != "" {
			target.Host = fmt.Sprintf("%s.%d.2", prefix, slot)
			target.Port = 22
		}
		g, e := pinGuest(s.ctx, t.TempDir(), s.generatedKeyFile(a.VMID), s.cfg.KnownHostsFile, target)
		if e != nil {
			t.Fatal("strict guest host-key pinning: ", e)
		}
		reboot := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = g.exec(ctx, "systemctl reboot")
		}
		// A failed assertion must still cooperate with normal receipt-checked cleanup.
		deleted := false
		t.Cleanup(func() {
			if !deleted {
				reboot()
			}
		})
		ctx, cancel = context.WithTimeout(s.ctx, sshReadyWait)
		err = pollUntil(ctx, pollEvery, func(ctx context.Context) (bool, error) {
			out, e := g.exec(ctx, "printf network-login-ok")
			return out == "network-login-ok", e
		})
		cancel()
		if err != nil {
			t.Fatal("authenticated guest login: ", err)
		}
		out, e := g.exec(s.ctx, `set -eu
ip -4 route show default | grep -q '^default via '
getent ahostsv4 archive.ubuntu.com >/dev/null
[ "$(curl -fsS --max-time 15 -o /dev/null -w '%{http_code}' https://example.com)" = 200 ]
[ "$(curl -fsS --max-time 15 -o /dev/null -w '%{http_code}' https://archive.ubuntu.com/ubuntu/)" = 200 ]
printf automatic-network-ok`)
		if e != nil || strings.TrimSpace(out) != "automatic-network-ok" {
			t.Fatalf("unmodified guest route/DNS/HTTPS failed: %v", e)
		}
		// Derive the actual gateway from the guest rather than altering guest routes.
		// Timeout is required; a refused connection is not isolation evidence.
		script := fmt.Sprintf(`python3 - <<'PY'
import socket,subprocess
socket.setdefaulttimeout(2)
r=subprocess.check_output(['ip','-4','route','show','default'],text=True).split()
gateway=r[r.index('via')+1]
for host,port in [(gateway,22),(%q,19453),('169.254.169.254',80),('172.17.0.1',22)]:
 s=socket.socket(); rc=s.connect_ex((host,port)); s.close()
 assert rc in (11,110), (host,port,rc)
print('isolation-ok')
PY`, host)
		out, e = g.exec(s.ctx, script)
		if e != nil || strings.TrimSpace(out) != "isolation-ok" {
			t.Fatalf("guest host/private/metadata filtering failed: %v", e)
		}
		t.Logf("VM %s public port %d: automatic route/DNS + two HTTPS 200 + isolation passed", a.VMID, meta.Port)
		reboot()
		s.deleteVM("network", a.VMID)
		deleted = true
	}
}
