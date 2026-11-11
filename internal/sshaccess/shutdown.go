package sshaccess

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"regexp"
	"time"
)

// TerminalCredentials exposes only per-VM dedicated terminal key leases, never
// the owner or shared bootstrap identity.
type TerminalCredentials interface {
	TerminalKeyFile(vmID, runtimeDir string) (path string, release func(), err error)
}

// SSHShutdown is an optional cooperative request, not a lifecycle manager.
// KnownHostsFile must be explicitly validated operator-trusted configuration;
// it is never auto-populated. RuntimeDir holds short-lived dedicated key leases.
// The backend must fence the original invocation before and after calling it.
type SSHShutdown struct {
	Credentials                TerminalCredentials
	RuntimeDir, KnownHostsFile string
	binary                     string // package-private test seam; production always /usr/bin/ssh
}

var shutdownUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var errShutdown = errors.New("cooperative SSH shutdown unavailable or unacknowledged")

func (s SSHShutdown) Reboot(ctx context.Context, vmID, host, user string) error {
	if s.Credentials == nil || s.KnownHostsFile == "" || net.ParseIP(host) == nil || !shutdownUser.MatchString(user) {
		return errShutdown
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return errShutdown
	}
	key, release, err := s.Credentials.TerminalKeyFile(vmID, s.RuntimeDir)
	if err != nil {
		return errShutdown
	}
	defer release()
	binary := s.binary
	if binary == "" {
		binary = "/usr/bin/ssh"
	}
	args := []string{"-F", "/dev/null", "-T", "-n",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=2", "-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no",
		"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "CertificateFile=none",
		"-o", "PreferredAuthentications=publickey", "-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "PermitLocalCommand=no", "-o", "LogLevel=QUIET", "-i", key, "--", user + "@" + host, "systemctl reboot"}
	// No shell, stdin, captured output, or raw subprocess errors. Timeout kills
	// only this local SSH client, never the VMM. Run waits for process exit before
	// the key lease is removed; a remote EOF is still not evidence of guest exit.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return errShutdown
	}
	return nil
}
