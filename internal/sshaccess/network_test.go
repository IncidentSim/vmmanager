package sshaccess

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func networkFixture(t *testing.T) *fixture {
	f := newFixture(t, stdPasswd)
	f.write("usr/lib/os-release", "ID=ubuntu\nVERSION_ID=24.04\n", 0644)
	f.write("etc/systemd/system/fcnet.service", supportedFCNetUnit, 0644)
	f.write("usr/local/bin/fcnet-setup.sh", supportedFCNetSetup, 0755)
	f.symlink("/etc/systemd/system/fcnet.service", "etc/systemd/system/sysinit.target.wants/fcnet.service")
	f.write("etc/resolv.conf", "# guest resolver preserved\nnameserver 9.9.9.9\n", 0644)
	f.write("etc/systemd/system/fcnet.service.d/10-user.conf", "# unrelated user notes preserved\n", 0644)
	return f
}
func TestGuestNetworkRealExt4BootConfigAndSSHCoexist(t *testing.T) {
	f := networkFixture(t)
	img := f.image()
	p := UbuntuFCNetProvisioner{}
	network := vmlib.GuestNetwork{Address: "10.213.3.2/30", Gateway: "10.213.3.1", DNS: []string{"1.1.1.1", "8.8.8.8"}, Configuration: "mac-derived"}
	if err := p.InstallNetwork(context.Background(), img, network); err != nil {
		t.Fatal(err)
	}
	script, ok := readGuest(t, img, "/etc/vibe-fc-network.sh")
	if !ok || !strings.Contains(script, "default via 10.213.3.1 dev eth0") || !strings.Contains(script, "nameserver 1.1.1.1") || !strings.Contains(script, "nameserver 8.8.8.8") || !strings.Contains(script, "route show default") {
		t.Fatalf("bad guest network script: %s", script)
	}
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell: %v %s", err, out)
	}
	drop, ok := readGuest(t, img, "/etc/systemd/system/fcnet.service.d/90-vibe-network.conf")
	if !ok || !strings.Contains(drop, "ExecStartPost=/bin/sh /etc/vibe-fc-network.sh") {
		t.Fatalf("boot ordering missing: %s", drop)
	}
	for path, want := range map[string]string{"/etc/resolv.conf": "# guest resolver preserved\nnameserver 9.9.9.9\n", "/etc/systemd/system/fcnet.service.d/10-user.conf": "# unrelated user notes preserved\n"} {
		got, _ := readGuest(t, img, path)
		if got != want {
			t.Fatalf("guest config changed %s: %q", path, got)
		}
	}
	// Regenerate only our managed files for a derivative/new slot; old slot does not stick.
	network.Gateway = "10.213.1.1"
	network.Address = "10.213.1.2/30"
	if err := p.InstallNetwork(context.Background(), img, network); err != nil {
		t.Fatal(err)
	}
	script, _ = readGuest(t, img, "/etc/vibe-fc-network.sh")
	if strings.Contains(script, "10.213.3.1") || !strings.Contains(script, "10.213.1.1") {
		t.Fatalf("stale persisted gateway: %s", script)
	}
	ssh, store := newProv(t, "root")
	if err := ssh.Install(context.Background(), "network-ssh", img, ""); err != nil {
		t.Fatal(err)
	}
	pub, _ := store.Public("network-ssh")
	keys, _ := readGuest(t, img, "/root/.ssh/authorized_keys")
	if !strings.Contains(keys, pub.OwnerPublic) || !strings.Contains(keys, pub.TerminalPublic) {
		t.Fatal("personal SSH missing")
	}
	// Exercise generated boot shell with a fake ip executable; the only script
	// substitution is the resolver path into this test's sandbox (never host /etc).
	for _, preconfigured := range []bool{false, true} {
		sandbox := t.TempDir()
		resolver := filepath.Join(sandbox, "resolv.conf")
		calls := filepath.Join(sandbox, "ip-calls")
		before := "# user comment without newline"
		route := ""
		if preconfigured {
			before = "nameserver 9.9.9.9\n"
			route = "default via 10.99.0.1 dev eth0"
		}
		if err := os.WriteFile(resolver, []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
		fakeIP := "#!/bin/sh\nif [ \"$*\" = '-4 route show default' ]; then printf '%s' \"$EXISTING_ROUTE\"; else printf '%s\\n' \"$*\" >> \"$IP_CALLS\"; fi\n"
		if err := os.WriteFile(filepath.Join(sandbox, "ip"), []byte(fakeIP), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh")
		cmd.Stdin = strings.NewReader(strings.ReplaceAll(script, "/etc/resolv.conf", resolver))
		cmd.Env = append(os.Environ(), "PATH="+sandbox+":"+os.Getenv("PATH"), "EXISTING_ROUTE="+route, "IP_CALLS="+calls)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated boot shell: %v %s", err, out)
		}
		got, err := os.ReadFile(resolver)
		if err != nil {
			t.Fatal(err)
		}
		if preconfigured {
			if string(got) != before {
				t.Fatalf("replaced user resolver: %s", got)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("replaced user default route")
			}
		} else {
			if !strings.Contains(string(got), "\nnameserver 1.1.1.1\n") {
				t.Fatalf("DNS not configured: %s", got)
			}
			ipCalls, err := os.ReadFile(calls)
			if err != nil || !strings.Contains(string(ipCalls), "-4 route replace default via 10.213.1.1 dev eth0") {
				t.Fatalf("gateway not configured: %s %v", ipCalls, err)
			}
		}
	}
	fsck(t, img)
}
func TestGuestNetworkSupportedBasePrivateCopy(t *testing.T) {
	base := os.Getenv("VIBE_NETWORK_BASE_TEST")
	if base == "" {
		t.Skip("opt-in supported image read-only source check")
	}
	copy := filepath.Join(t.TempDir(), "supported-copy.ext4")
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", base, copy).CombinedOutput(); err != nil {
		t.Fatalf("private copy: %v %s", err, out)
	}
	if err := os.Chmod(copy, 0600); err != nil {
		t.Fatal(err)
	}
	baseline := map[string]string{}
	for _, path := range []string{"/usr/lib/os-release", "/usr/local/bin/fcnet-setup.sh", "/etc/systemd/system/fcnet.service", "/etc/resolv.conf"} {
		data, ok := readGuest(t, base, path)
		if !ok {
			t.Fatal("missing source baseline", path)
		}
		baseline[path] = data
	}
	n := vmlib.GuestNetwork{Address: "10.213.1.2/30", Gateway: "10.213.1.1", DNS: []string{"1.1.1.1"}, Configuration: "mac-derived"}
	if err := (UbuntuFCNetProvisioner{}).InstallNetwork(context.Background(), copy, n); err != nil {
		t.Fatal(err)
	}
	script, _ := readGuest(t, copy, networkScript)
	if !strings.Contains(script, "default via 10.213.1.1 dev eth0") {
		t.Fatal("missing supported image boot config")
	}
	for path, want := range baseline {
		for _, image := range []string{base, copy} {
			got, ok := readGuest(t, image, path)
			if !ok || got != want {
				t.Fatalf("baseline changed %s in %s", path, image)
			}
		}
	}
	fsck(t, copy)
}

func TestGuestNetworkRejectsUnsupportedAndUnsafeImages(t *testing.T) {
	for _, kind := range []string{"not ubuntu", "resolver symlink", "dropin symlink", "unmanaged script", "bad gateway", "bad dns"} {
		t.Run(kind, func(t *testing.T) {
			f := networkFixture(t)
			network := vmlib.GuestNetwork{Address: "10.213.0.2/30", Gateway: "10.213.0.1", DNS: []string{"1.1.1.1"}, Configuration: "mac-derived"}
			switch kind {
			case "not ubuntu":
				f.write("usr/lib/os-release", "ID=debian\n", 0644)
			case "resolver symlink":
				f.t.Helper() /* replace regular fixture file */
				if err := os.Remove(f.root + "/etc/resolv.conf"); err != nil {
					t.Fatal(err)
				}
				f.symlink("/run/resolv.conf", "etc/resolv.conf")
			case "dropin symlink":
				if err := os.RemoveAll(f.root + "/etc/systemd/system/fcnet.service.d"); err != nil {
					t.Fatal(err)
				}
				f.symlink("/elsewhere", "etc/systemd/system/fcnet.service.d")
			case "unmanaged script":
				f.write("etc/vibe-fc-network.sh", "#!/bin/sh\n# custom user file\n", 0755)
			case "bad gateway":
				network.Gateway = "10.213.0.1; reboot"
			case "bad dns":
				network.DNS = []string{"1.1.1.1; reboot"}
			}
			img := f.image()
			if err := (UbuntuFCNetProvisioner{}).InstallNetwork(context.Background(), img, network); err == nil {
				t.Fatal("accepted unsafe/unsupported network image")
			}
			fsck(t, img)
		})
	}
}
