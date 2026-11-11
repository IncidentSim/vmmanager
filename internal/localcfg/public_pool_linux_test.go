//go:build linux

package localcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

func publicationPool(t *testing.T, publication string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pool.conf")
	if err := os.WriteFile(path, []byte("GUEST_PREFIX=10.213\nMAX_SLOTS=4\nMAX_SLOT_CPU_MILLI=2000\n"+publication), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublicSSHRootPoolAssemblyBeforeAccessEffects(t *testing.T) {
	for _, tc := range []struct {
		name, publication string
		policy            *vmlib.PublicSSHConfig
		ok                bool
	}{
		{"disabled omitted", "", nil, true},
		{"disabled explicit", "", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000}, false},
		{"enabled omitted", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", nil, true},
		{"enabled matching", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000}, true},
		{"host mismatch", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.4.4", PortBase: 22000}, false},
		{"base mismatch", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 23000}, false},
		{"DNS prohibited", "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n", &vmlib.PublicSSHConfig{Host: "ssh.example.test", PortBase: 22000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := Parse([]byte(good))
			if err != nil {
				t.Fatal(err)
			}
			l.PoolConf = publicationPool(t, tc.publication)
			l.PublicSSH = tc.policy
			c, err := l.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
			if tc.ok {
				if tc.publication == "" {
					if c.PublicSSH != nil {
						t.Fatal("disabled pool advertised")
					}
				} else if c.PublicSSH == nil || c.PublicSSH.Host != "8.8.8.8" || c.PublicSSH.PortBase != 22000 {
					t.Fatalf("wrong authority: %+v", c.PublicSSH)
				}
				return
			}
			// Deliberately missing key and absent store: publication errors must win
			// before opening credentials, backend locks or roots.
			d := t.TempDir()
			l.WorkRoot = filepath.Join(d, "work")
			l.ImagesDir = filepath.Join(d, "images")
			l.RunDir = filepath.Join(d, "run")
			l.PersonalSSH = &PersonalSSHConfig{Enabled: true, StoreDir: filepath.Join(d, "store"), MasterKeyFile: filepath.Join(d, "missing-key")}
			b, _, err := BuildWithAccess(l, Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
			if b != nil {
				b.Close()
				t.Fatal("opened invalid backend")
			}
			if err == nil || !strings.Contains(err.Error(), "public_ssh must match root pool") {
				t.Fatalf("wrong preflight order: %v", err)
			}
			entries, err := os.ReadDir(d)
			if err != nil || len(entries) != 0 {
				t.Fatalf("preflight effects: %v %v", entries, err)
			}
		})
	}
}

func TestPublicSSHRootPoolChangesBetweenAssemblyAndOpen(t *testing.T) {
	l, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	l.PoolConf = publicationPool(t, "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n")
	c, err := l.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.PoolConf, []byte("GUEST_PREFIX=10.213\nMAX_SLOTS=4\nPUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=23000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Backend must reject stale assembled policy, not advertise the old address.
	if b, err := local.New(c); err == nil {
		b.Close()
		t.Fatal("accepted changed root publication")
	}
	next, err := l.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
	if err != nil || next.PublicSSH.Host != "8.8.4.4" || next.PublicSSH.PortBase != 23000 {
		t.Fatalf("fresh assembly not authoritative: %+v %v", next.PublicSSH, err)
	}
	if c.PublicSSH.Host != "8.8.8.8" {
		t.Fatal("mutated earlier snapshot")
	}
}
