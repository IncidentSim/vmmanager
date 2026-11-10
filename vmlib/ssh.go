package vmlib

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// WaitReady polls prober until it succeeds, ctx ends, or the VM is not running.
// It is the single readiness loop shared by Service and the remote facade client.
//
// "Ready" means exactly what the prober proves. The default TCPProber only shows
// that a TCP port accepted a connection from THIS process; it neither
// authenticates nor works for a client that cannot route to the host-private
// guest. Use SSHCommandProber for authenticated readiness through the bastion.
func WaitReady(ctx context.Context, l Lifecycle, id string, prober SSHProber, poll time.Duration) (SSHInfo, error) {
	if poll <= 0 {
		poll = time.Second
	}
	for {
		vm, err := l.Get(ctx, id)
		if err != nil {
			return SSHInfo{}, err
		}
		if vm.State != StateRunning {
			return SSHInfo{}, fmt.Errorf("%w: VM %s is %s", ErrNotStopped, id, vm.State)
		}
		info, err := l.SSHInfo(ctx, id)
		if err != nil {
			return SSHInfo{}, err
		}
		if perr := prober(ctx, info); perr == nil {
			info.Ready = true
			return info, nil
		}
		select {
		case <-ctx.Done():
			return SSHInfo{}, fmt.Errorf("waiting for SSH on %s: %w", id, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// SSHCommandProber proves authenticated SSH reachability by running `true` on the
// guest with the system ssh client, through the bastion (ProxyJump) from
// SSHInfo. KeyFile and KnownHostsFile are CLIENT-local paths. Host-key checking
// stays on (accept-new into KnownHostsFile). Use Probe as a WithSSHProber value.
type SSHCommandProber struct {
	Binary         string // default "ssh"
	KeyFile        string
	KnownHostsFile string
}

func (p SSHCommandProber) Probe(ctx context.Context, info SSHInfo) error {
	bin := p.Binary
	if bin == "" {
		bin = "ssh"
	}
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "LogLevel=ERROR"}
	args = append(args, info.SSHArgs(p.KnownHostsFile, p.KeyFile)...)
	args = append(args, "true")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ssh probe: %w: %.200s", err, out)
	}
	return nil
}
