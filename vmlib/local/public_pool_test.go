//go:build linux

package local_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

func TestPublicSSHRootPoolReconciliationBeforeBackendEffects(t *testing.T) {
	for _, tc := range []struct {
		name, publication string
		policy            *vmlib.PublicSSHConfig
		ok                bool
	}{
		{"disabled omitted", "", nil, true},
		{"disabled explicit", "", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000}, false},
		{"inherits", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", nil, true},
		{"matching", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000}, true},
		{"host mismatch", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.4.4", PortBase: 22000}, false},
		{"base mismatch", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22001}, false},
		{"hostname", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "ssh.example.test", PortBase: 22000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 4)
			r.cfg.PoolConf = filepath.Join(t.TempDir(), "pool.conf")
			r.cfg.PublicSSH = tc.policy
			if err := os.WriteFile(r.cfg.PoolConf, []byte("GUEST_PREFIX=172.30\nMAX_SLOTS=4\n"+tc.publication), 0600); err != nil {
				t.Fatal(err)
			}
			b, err := local.New(r.cfg)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
			if tc.ok {
				defer b.Close()
				h, err := prep(b, "root-public")
				if err != nil {
					t.Fatal(err)
				}
				if tc.publication == "" {
					if h.SSHEndpoint != nil {
						t.Fatal("disabled root advertised")
					}
				} else if h.SSHEndpoint == nil || h.SSHEndpoint.Host != "8.8.8.8" || h.SSHEndpoint.Port != 22000 {
					t.Fatalf("not inherited: %+v", h.SSHEndpoint)
				}
			} else {
				for _, path := range []string{filepath.Join(r.root, ".backend.lock"), filepath.Join(r.images, ".backend.lock"), filepath.Join(r.root, "vms")} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("unexpected preflight effect at %s: %v", path, err)
					}
				}
				if len(r.launcher.slots) != 0 || len(r.vmm.specs) != 0 {
					t.Fatal("host seams called")
				}
			}
		})
	}
}

func TestPublicSSHRootPoolChangedOnRestart(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.PoolConf = filepath.Join(t.TempDir(), "pool.conf")
	write := func(host string, base int) {
		t.Helper()
		publication := ""
		if host != "" {
			publication = fmt.Sprintf("PUBLIC_IPV4=%s\nSSH_PORT_BASE=%d\n", host, base)
		}
		if err := os.WriteFile(r.cfg.PoolConf, []byte("GUEST_PREFIX=172.30\nMAX_SLOTS=4\n"+publication), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("8.8.8.8", 22000)
	b := r.backend()
	if _, err := prep(b, "persisted-public"); err != nil {
		t.Fatal(err)
	}
	b.Close()
	write("8.8.4.4", 23000)
	r.cfg.PublicSSH = &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000}
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("stale explicit policy accepted")
	} else if !strings.Contains(err.Error(), "public_ssh") {
		t.Fatal(err)
	}
	r.cfg.PublicSSH = nil
	b = r.backend()
	info, err := b.Inspect(ctx, "persisted-public")
	if err != nil || info.SSHEndpoint == nil || info.SSHEndpoint.Host != "8.8.4.4" || info.SSHEndpoint.Port != 23000 {
		t.Fatalf("stale restart metadata: %+v %v", info, err)
	}
	b.Close()
	write("", 0)
	b = r.backend()
	info, err = b.Inspect(ctx, "persisted-public")
	if err != nil || info.SSHEndpoint != nil {
		t.Fatalf("disabled restart metadata: %+v %v", info, err)
	}
}
