package sshaccess

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func diskDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}

func TestGuestNetworkPinnedContractRejectsMutationsBeforeWrites(t *testing.T) {
	for _, kind := range []string{"comment-only setup", "changed address", "extra shell", "commented ExecStart", "no ExecStart", "reset ExecStart", "disabled link", "wrong link", "not executable", "reset dropin", "override dropin", "vendor dropin", "global dropin", "managed fake dropin", "alternate unit"} {
		t.Run(kind, func(t *testing.T) {
			f := networkFixture(t)
			switch kind {
			case "comment-only setup":
				f.write("usr/local/bin/fcnet-setup.sh", "#!/bin/sh\n# 06:00: MAC-derived address setup\n", 0755)
			case "changed address":
				f.write("usr/local/bin/fcnet-setup.sh", strings.Replace(supportedFCNetSetup, "$ip/30", "$ip/24", 1), 0755)
			case "extra shell":
				f.write("usr/local/bin/fcnet-setup.sh", supportedFCNetSetup+"\nexec /bin/false\n", 0755)
			case "commented ExecStart":
				f.write("etc/systemd/system/fcnet.service", strings.Replace(supportedFCNetUnit, "\nExecStart=", "\n#ExecStart=", 1), 0644)
			case "no ExecStart":
				f.write("etc/systemd/system/fcnet.service", "[Service]\nType=oneshot\n", 0644)
			case "reset ExecStart":
				f.write("etc/systemd/system/fcnet.service", supportedFCNetUnit+"\n[Service]\nExecStart=\n", 0644)
			case "disabled link", "wrong link":
				if err := os.Remove(filepath.Join(f.root, "etc/systemd/system/sysinit.target.wants/fcnet.service")); err != nil {
					t.Fatal(err)
				}
				if kind == "wrong link" {
					f.symlink("/dev/null", "etc/systemd/system/sysinit.target.wants/fcnet.service")
				}
			case "not executable":
				if err := os.Chmod(filepath.Join(f.root, "usr/local/bin/fcnet-setup.sh"), 0644); err != nil {
					t.Fatal(err)
				}
			case "reset dropin":
				f.write("etc/systemd/system/fcnet.service.d/99-reset.conf", "[Service]\nExecStart=\n", 0644)
			case "override dropin":
				f.write("etc/systemd/system/fcnet.service.d/99-override.conf", "[Service]\nExecStart=/bin/true\n", 0644)
			case "vendor dropin":
				f.write("usr/lib/systemd/system/fcnet.service.d/20-reset.conf", "[Service]\nExecStart=\n", 0644)
			case "global dropin":
				f.write("etc/systemd/system/service.d/20-reset.conf", "[Service]\nExecStart=\n", 0644)
			case "managed fake dropin":
				f.write("etc/systemd/system/fcnet.service.d/90-vibe-network.conf", networkMarker+"[Service]\nExecStart=\n", 0644)
			case "alternate unit":
				f.write("etc/systemd/system.control/fcnet.service", supportedFCNetUnit, 0644)
			}
			img := f.image()
			before := diskDigest(t, img)
			n := vmlib.GuestNetwork{Address: "10.213.0.2/30", Gateway: "10.213.0.1", DNS: []string{"1.1.1.1"}, Configuration: "mac-derived"}
			err := (UbuntuFCNetProvisioner{}).InstallNetwork(context.Background(), img, n)
			if !errors.Is(err, ErrUnsupportedImage) {
				t.Fatalf("want unsupported image, got %v", err)
			}
			if diskDigest(t, img) != before {
				t.Fatal("rejected contract changed disk")
			}
		})
	}
}
