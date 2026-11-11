package sshaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// connectionsCommand is the fixed, read-only guest command. The first line is the
// probe's own SSH_CONNECTION tuple (client address/port, server address/port); the
// rest are the established TCP sockets whose local port is 22. No argument is
// interpolated into it.
const connectionsCommand = `printf '%s\n' "$SSH_CONNECTION"; PATH=/usr/sbin:/sbin:/usr/bin:/bin ss -H -n -t state established '( sport = :22 )'`

var errConnections = errors.New("SSH connection count unavailable or ambiguous")

// ConnCounter counts established SSH connections of one managed guest, over a
// strictly host-key-verified SSH session that uses the VM's dedicated terminal
// credential lease. KnownHostsFile must be explicitly validated operator-trusted
// configuration; it is never auto-populated. Every failure is an error: callers
// must treat an error as "unknown", never as "zero".
type ConnCounter struct {
	Credentials                TerminalCredentials
	RuntimeDir, KnownHostsFile string
	binary                     string // package-private test seam; production always /usr/bin/ssh
}

// Count returns the number of established TCP/22 connections other than the
// probe's own connection (the exact tuple reported in SSH_CONNECTION). SFTP
// sessions and any other connection to sshd count. host must be an IP literal.
func (c ConnCounter) Count(ctx context.Context, vmID, host, user string) (int, error) {
	if c.Credentials == nil || c.KnownHostsFile == "" || net.ParseIP(host) == nil || !shutdownUser.MatchString(user) {
		return 0, errConnections
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	key, release, err := c.Credentials.TerminalKeyFile(vmID, c.RuntimeDir)
	if err != nil {
		return 0, errConnections
	}
	defer release()
	binary := c.binary
	if binary == "" {
		binary = "/usr/bin/ssh"
	}
	args := []string{"-F", "/dev/null", "-T", "-n",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + c.KnownHostsFile,
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no",
		"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "CertificateFile=none",
		"-o", "PreferredAuthentications=publickey", "-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "PermitLocalCommand=no", "-o", "LogLevel=QUIET", "-i", key, "--", user + "@" + host, connectionsCommand}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, left: 1 << 20}
	if cmd.Run() != nil {
		return 0, errConnections // no raw error: it could embed paths
	}
	n, err := CountEstablished(out.String())
	if err != nil {
		return 0, errConnections
	}
	return n, nil
}

type limitWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, errors.New("output too large")
	}
	l.left -= len(p)
	return l.w.Write(p)
}

type endpoint struct {
	addr netip.Addr
	port uint16
}

func parseAddr(s string) (netip.Addr, error) {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return a.Unmap(), nil
}

func parsePort(s string) (uint16, error) {
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(p), nil
}

// parseEndpoint reads ss -n "addr:port" (IPv4), "[addr]:port", "[addr]%zone:port"
// or "addr%zone:port".
func parseEndpoint(s string) (endpoint, error) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return endpoint{}, errConnections
	}
	host, ps := s[:i], s[i+1:]
	if strings.HasPrefix(host, "[") {
		j := strings.IndexByte(host, ']')
		if j < 0 {
			return endpoint{}, errConnections
		}
		rest := host[j+1:]
		if rest != "" && !strings.HasPrefix(rest, "%") {
			return endpoint{}, errConnections
		}
		host = host[1:j]
	}
	a, err := parseAddr(host)
	if err != nil {
		return endpoint{}, errConnections
	}
	p, err := parsePort(ps)
	if err != nil {
		return endpoint{}, errConnections
	}
	return endpoint{a, p}, nil
}

// CountEstablished parses the output of connectionsCommand. The first line must
// be a well-formed SSH_CONNECTION. Exactly one socket must match its tuple (the
// probe itself) and is excluded; every other socket is counted. Anything
// unparseable, a socket not on local port 22, or a missing or duplicated own
// tuple is an error: the count would not be trustworthy.
func CountEstablished(out string) (int, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	f := strings.Fields(lines[0])
	if len(f) != 4 {
		return 0, errConnections
	}
	cl, err1 := parseAddr(f[0])
	cp, err2 := parsePort(f[1])
	sv, err3 := parseAddr(f[2])
	sp, err4 := parsePort(f[3])
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return 0, errConnections
	}
	self, others := 0, 0
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		c := strings.Fields(l)
		if len(c) != 4 {
			return 0, errConnections
		}
		local, err := parseEndpoint(c[2])
		if err != nil {
			return 0, errConnections
		}
		peer, err := parseEndpoint(c[3])
		if err != nil || local.port != 22 {
			return 0, errConnections
		}
		if local.addr == sv && local.port == sp && peer.addr == cl && peer.port == cp {
			self++
			continue
		}
		others++
	}
	if self != 1 {
		return 0, fmt.Errorf("%w: own connection seen %d times", errConnections, self)
	}
	return others, nil
}
