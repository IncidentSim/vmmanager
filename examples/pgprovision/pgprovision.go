// Package pgprovision is an APPLICATION built on vmlib (the library core knows
// nothing about PostgreSQL): it creates a VM from a base image, installs
// PostgreSQL in the guest over SSH through the bastion, gracefully stops it and
// commits a new image. It works with any vmlib.Lifecycle (local or remote).
//
// The legacy single-VM CLI (package main) and legacy HTTPS build API are
// unchanged and remain the default; this is the library-based equivalent.
package pgprovision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// ScriptFor renders the guest-side provisioning script. It runs IN THE GUEST
// (never on the host). The base image's address comes from the MAC (or from the
// caller for an "external" base), but it has no default route or resolver, so the
// script installs both from the structured metadata the server returned; without
// usable metadata it refuses instead of guessing.
func ScriptFor(info vmlib.SSHInfo) (string, error) {
	g := info.Guest
	if g == nil {
		return "", errors.New("server returned no guest network metadata; cannot configure route/DNS")
	}
	if net.ParseIP(g.Gateway) == nil {
		return "", fmt.Errorf("invalid guest gateway %q", g.Gateway)
	}
	if len(g.DNS) == 0 {
		return "", errors.New("server policy provides no DNS for the guest (set guest_dns); package installation would fail")
	}
	var resolv strings.Builder
	for _, d := range g.DNS {
		if net.ParseIP(d) == nil {
			return "", fmt.Errorf("invalid DNS address %q", d)
		}
		fmt.Fprintf(&resolv, "nameserver %s\\n", d)
	}
	return fmt.Sprintf(`set -euxo pipefail
ip route replace default via %s dev eth0
printf '%s' > /etc/resolv.conf
export DEBIAN_FRONTEND=noninteractive
apt-get -o Acquire::Retries=3 update
apt-get -o Acquire::Retries=3 install -y postgresql
systemctl enable postgresql
systemctl start postgresql
runuser -u postgres -- psql -Atqc 'SHOW server_version;'
apt-get clean
rm -rf /var/lib/apt/lists/*
sync
`, g.Gateway, resolv.String()), nil
}

type Options struct {
	BaseImage, Profile, OutputImage string
	// KeyFile and KnownHostsFile are CLIENT-local paths used by SSHRunner.
	KeyFile, KnownHostsFile string
}

// Runner executes a script in the guest described by info.
type Runner interface {
	RunScript(ctx context.Context, info vmlib.SSHInfo, script string) error
}

// ReadyFunc waits until SSH is reachable (e.g. Service.WaitSSHReady, or a
// bastion-aware probe for a remote facade client).
type ReadyFunc func(ctx context.Context, id string) (vmlib.SSHInfo, error)

// Build creates, provisions, stops and commits. The builder VM is always
// deleted afterwards; a failed run never publishes an image. If the disk must
// be retained, the returned error says so (vmlib.ErrRetained).
func Build(ctx context.Context, l vmlib.Lifecycle, ready ReadyFunc, run Runner, o Options) (img vmlib.Image, err error) {
	for _, c := range [][2]string{{"image", o.BaseImage}, {"profile", o.Profile}, {"image", o.OutputImage}} {
		if verr := vmlib.ValidateID(c[0], c[1]); verr != nil {
			return vmlib.Image{}, verr
		}
	}
	vm, err := l.Create(ctx, vmlib.CreateRequest{ImageID: o.BaseImage, Profile: o.Profile})
	if err != nil && vm.ID == "" {
		return vmlib.Image{}, err
	}
	// Cleanup must survive cancellation of ctx.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if _, derr := l.Delete(cctx, vm.ID); derr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup of %s: %w", vm.ID, derr))
		}
	}()
	if err != nil {
		return vmlib.Image{}, err
	}
	info, err := ready(ctx, vm.ID)
	if err != nil {
		return vmlib.Image{}, err
	}
	script, serr := ScriptFor(info)
	if serr != nil {
		return vmlib.Image{}, serr
	}
	if err = run.RunScript(ctx, info, script); err != nil {
		return vmlib.Image{}, fmt.Errorf("guest provisioning failed: %w", err)
	}
	img, err = l.Commit(ctx, vm.ID, o.OutputImage)
	return img, err
}

// SSHRunner runs scripts with the system ssh client through the bastion.
type SSHRunner struct{ KeyFile, KnownHostsFile string }

func (r SSHRunner) RunScript(ctx context.Context, info vmlib.SSHInfo, script string) error {
	args := append([]string{"-T", "-o", "BatchMode=yes", "-o", "LogLevel=ERROR"}, info.SSHArgs(r.KnownHostsFile, r.KeyFile)...)
	cmd := exec.CommandContext(ctx, "ssh", append(args, "bash -se")...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
