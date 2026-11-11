package sshaccess

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"strings"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

//go:embed supported/fcnet.service
var supportedFCNetUnit string

//go:embed supported/fcnet-setup.sh
var supportedFCNetSetup string

func supportedContent(got, want string) bool {
	// Only the final newline is immaterial; no shell/systemd parsing heuristics.
	return strings.TrimRight(got, "\n") == strings.TrimRight(want, "\n")
}

// UbuntuFCNetProvisioner adds boot-time egress settings ONLY to an exclusively
// owned, unmounted private disk copy. Only the pinned Firecracker CI Ubuntu
// fcnet script and unit (supported/) enabled at sysinit are accepted; this is not
// arbitrary Ubuntu support. It preserves the address setup and resolver, and
// permits only inert comment-only unrelated fcnet drop-ins. The backend
// must discard a copy after any failure. No credential store is involved.
type UbuntuFCNetProvisioner struct{ Debugfs string }

const networkMarker = "# vibe-fc-managed-network-v1\n"
const networkScript = "/etc/vibe-fc-network.sh"
const networkDropDir = "/etc/systemd/system/fcnet.service.d"
const networkDropIn = networkDropDir + "/90-vibe-network.conf"

func (p UbuntuFCNetProvisioner) InstallNetwork(ctx context.Context, disk string, n vmlib.GuestNetwork) error {
	gateway := net.ParseIP(n.Gateway)
	address, subnet, err := net.ParseCIDR(n.Address)
	if err != nil || gateway == nil || gateway.To4() == nil || address.To4() == nil || !subnet.Contains(gateway) || gateway.Equal(address) || n.Configuration != "mac-derived" || len(n.DNS) == 0 {
		return fmt.Errorf("%w: invalid mac-derived gateway/address/DNS policy", ErrUnsupportedImage)
	}
	for _, dns := range n.DNS {
		if net.ParseIP(dns) == nil {
			return fmt.Errorf("%w: DNS must be literal IP addresses", ErrUnsupportedImage)
		}
	}
	g := d(&Provisioner{Debugfs: p.Debugfs})
	readRegular := func(path string) (string, error) {
		if err := g.realAncestors(ctx, disk, path); err != nil {
			return "", err
		}
		ino, err := g.stat(ctx, disk, path)
		if err != nil {
			return "", err
		}
		if !ino.exists || ino.typ != "regular" {
			return "", fmt.Errorf("%w: required regular file %s", ErrUnsupportedImage, path)
		}
		data, err := g.readFile(ctx, disk, path)
		return string(data), err
	}
	release, err := readRegular("/usr/lib/os-release")
	if err != nil {
		return err
	}
	ubuntu := false
	for _, line := range strings.Split(release, "\n") {
		if line == "ID=ubuntu" || line == `ID="ubuntu"` {
			ubuntu = true
		}
	}
	if !ubuntu {
		return fmt.Errorf("%w: ubuntu-fcnet requires Ubuntu", ErrUnsupportedImage)
	}
	unit, err := readRegular("/etc/systemd/system/fcnet.service")
	if err != nil {
		return err
	}
	if !supportedContent(unit, supportedFCNetUnit) {
		return fmt.Errorf("%w: unsupported fcnet.service", ErrUnsupportedImage)
	}
	setup, err := readRegular("/usr/local/bin/fcnet-setup.sh")
	if err != nil {
		return err
	}
	if !supportedContent(setup, supportedFCNetSetup) {
		return fmt.Errorf("%w: unsupported MAC-derived address setup", ErrUnsupportedImage)
	}
	if err := g.realAncestors(ctx, disk, "/etc/systemd/system/sysinit.target.wants/fcnet.service"); err != nil {
		return err
	}
	enabled, err := g.run(ctx, disk, false, "stat /etc/systemd/system/sysinit.target.wants/fcnet.service")
	if err != nil {
		return err
	}
	if !strings.Contains(enabled, `Fast link dest: "/etc/systemd/system/fcnet.service"`) && !strings.Contains(enabled, `Fast link dest: "../fcnet.service"`) {
		return fmt.Errorf("%w: fcnet.service must be enabled at sysinit", ErrUnsupportedImage)
	}
	setupInode, err := g.stat(ctx, disk, "/usr/local/bin/fcnet-setup.sh")
	if err != nil {
		return err
	}
	if setupInode.mode&0111 == 0 {
		return fmt.Errorf("%w: fcnet setup is not executable", ErrUnsupportedImage)
	}
	if err := validateFCNetOverrides(ctx, g, disk, readRegular); err != nil {
		return err
	}
	if _, err := readRegular("/etc/resolv.conf"); err != nil {
		return err
	}
	// Check every target before creating anything; never overwrite an unmanaged
	// file or traverse a guest symlink. A committed managed image is regenerated
	// for the NEW slot rather than retaining the source VM's gateway.
	for _, path := range []string{networkScript, networkDropIn} {
		if err := g.realAncestors(ctx, disk, path); err != nil {
			return err
		}
		ino, err := g.stat(ctx, disk, path)
		if err != nil {
			return err
		}
		if ino.exists {
			data, err := readRegular(path)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(data, networkMarker) {
				return fmt.Errorf("%w: unmanaged network target %s", ErrUnsupportedImage, path)
			}
		}
	}
	dir, err := g.stat(ctx, disk, networkDropDir)
	if err != nil {
		return err
	}
	if !dir.exists {
		if _, err := g.run(ctx, disk, true, "mkdir "+networkDropDir); err != nil {
			return err
		}
		for _, cmd := range []string{"sif " + networkDropDir + " mode 040755", "sif " + networkDropDir + " uid 0", "sif " + networkDropDir + " gid 0"} {
			if _, err := g.run(ctx, disk, true, cmd); err != nil {
				return err
			}
		}
		dir, err = g.stat(ctx, disk, networkDropDir)
		if err != nil {
			return err
		}
	}
	if !dir.exists || dir.typ != "directory" {
		return ErrUnsupportedImage
	}
	script := networkMarker + "#!/bin/sh\nset -eu\n" +
		"# Preserve any image/user-provided default route. fcnet already set eth0.\n" +
		"if [ -z \"$(ip -4 route show default)\" ]; then\n  ip -4 route replace default via " + gateway.String() + " dev eth0\nfi\n" +
		"# Preserve existing resolver entries; populate only an unconfigured resolver.\n" +
		"if ! grep -Eq '^[[:space:]]*nameserver[[:space:]]+' /etc/resolv.conf; then\n  printf '%s\\n' ''"
	for _, dns := range n.DNS {
		script += " 'nameserver " + net.ParseIP(dns).String() + "'"
	}
	script += " >> /etc/resolv.conf\nfi\n"
	drop := networkMarker + "[Service]\nExecStartPost=/bin/sh " + networkScript + "\n"
	for _, file := range []struct {
		path, content string
		mode          uint32
	}{{networkScript, script, 0755}, {networkDropIn, drop, 0644}} {
		ino, err := g.stat(ctx, disk, file.path)
		if err != nil {
			return err
		}
		if err := g.putFile(ctx, disk, file.path, []byte(file.content), ino.exists, file.mode, 0, 0); err != nil {
			return err
		}
	}
	return nil
}
