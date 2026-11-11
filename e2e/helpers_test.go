package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

func isStatus(err error, status int) bool {
	var a *controlclient.APIError
	return errors.As(err, &a) && a.Status == status
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// namer yields unique valid names (<=20 chars, API label pattern) and
// idempotency keys per run.
type namer struct {
	run string
	n   atomic.Int64
}

var labelPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)

func newNamer() *namer { return &namer{run: randHex(3)} }

// name: "e2e" + 6 hex run + "-" + tag (tag <= 9 chars, [a-z0-9]).
func (n *namer) name(tag string) string { return "e2e" + n.run + "-" + tag }

// key: unique per call, printable ASCII, well under 200 chars.
func (n *namer) key(tag string) string {
	return "e2e-" + n.run + "-" + strconv.FormatInt(n.n.Add(1), 10) + "-" + tag
}

// pollUntil calls f every interval until it reports done, returns an error,
// or ctx expires (then the last not-done error, if any, is wrapped).
func pollUntil(ctx context.Context, interval time.Duration, f func(context.Context) (bool, error)) error {
	var last error
	for {
		done, err := f(ctx)
		if done {
			return nil
		}
		if err != nil {
			if errors.Is(err, errStop) {
				return errors.Unwrap(err)
			}
			last = err
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			if last != nil {
				return fmt.Errorf("%w (last: %v)", ctx.Err(), last)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// errStop wraps a permanent error so pollUntil stops retrying.
var errStop = errors.New("stop")

type stopErr struct{ err error }

func (s stopErr) Error() string   { return s.err.Error() }
func (s stopErr) Unwrap() error   { return s.err }
func (s stopErr) Is(t error) bool { return t == errStop }

func permanent(err error) error { return stopErr{err} }

// api is the subset of controlclient.Client used for polling and cleanup so the
// harness can be unit-tested with a fake.
type api interface {
	VM(ctx context.Context, u int64, id string) (controlclient.VM, error)
	Delete(ctx context.Context, u int64, id, key string) (controlclient.Accepted, error)
	DeleteImage(ctx context.Context, u int64, id, key string) (controlclient.Accepted, error)
	Image(ctx context.Context, u int64, id string) (controlclient.Image, error)
	Operation(ctx context.Context, u int64, id string) (controlclient.Operation, error)
}

// waitOp polls an operation until it succeeds (nil), fails (its APIError) or
// ctx ends. Unlike controlclient.Wait it has no fixed one-minute cap; the
// caller's context is the only bound.
func waitOp(ctx context.Context, c api, u int64, id string, interval time.Duration) error {
	return pollUntil(ctx, interval, func(ctx context.Context) (bool, error) {
		o, err := c.Operation(ctx, u, id)
		if err != nil {
			if ctx.Err() != nil {
				return false, err
			}
			return false, err // transient: keep polling until ctx ends
		}
		switch o.Status {
		case "succeeded":
			return true, nil
		case "failed":
			if o.Error != nil {
				return false, permanent(o.Error)
			}
			return false, permanent(errors.New("operation failed"))
		}
		return false, nil
	})
}

// waitGone polls until the VM/image read returns 404.
func waitGone(ctx context.Context, interval time.Duration, get func(context.Context) error) error {
	return pollUntil(ctx, interval, func(ctx context.Context) (bool, error) {
		err := get(ctx)
		if isStatus(err, 404) {
			return true, nil
		}
		if err == nil {
			return false, errors.New("resource still visible")
		}
		return false, err
	})
}

// tracker records only IDs this run's admitted mutations returned. It can never
// name a preexisting resource or the base image.
type tracker struct {
	user   int64
	base   string
	vms    []string
	images []string
	keySeq func(tag string) string

	// Baseline captured before any mutation; IDs in it are never owned.
	preVMs    map[string]bool
	preImages map[string]bool
}

func (t *tracker) addVM(id string)    { t.vms = append(t.vms, id) }
func (t *tracker) addImage(id string) { t.images = append(t.images, id) }

// setBaseline records resources that existed before this test mutated anything.
func (t *tracker) setBaseline(vms, images []string) {
	t.preVMs, t.preImages = map[string]bool{}, map[string]bool{}
	for _, id := range vms {
		t.preVMs[id] = true
	}
	for _, id := range images {
		t.preImages[id] = true
	}
}

// admitVM registers a newly admitted VM ID for cleanup. It refuses (and does
// not track) an empty ID or one that was already present before the test, so a
// buggy API answer can never make cleanup delete someone else's VM.
func (t *tracker) admitVM(id string) error {
	if id == "" {
		return errors.New("API returned an empty VM ID")
	}
	if t.preVMs[id] {
		return fmt.Errorf("API returned preexisting VM %s as newly admitted", id)
	}
	if !slices.Contains(t.vms, id) {
		t.addVM(id)
	}
	return nil
}

// admitImage is admitVM for images; it also refuses the configured base image.
func (t *tracker) admitImage(id string) error {
	if id == "" {
		return errors.New("API returned an empty image ID")
	}
	if id == t.base {
		return fmt.Errorf("API returned the base image %s as newly admitted", id)
	}
	if t.preImages[id] {
		return fmt.Errorf("API returned preexisting image %s as newly admitted", id)
	}
	if !slices.Contains(t.images, id) {
		t.addImage(id)
	}
	return nil
}

func (t *tracker) mayDeleteVM(id string) error {
	if id == "" || t.preVMs[id] || !slices.Contains(t.vms, id) {
		return errors.New("refusing to delete empty, preexisting or untracked VM")
	}
	return nil
}

// mayDeleteImage is the single guard for every test-side image deletion.
func (t *tracker) mayDeleteImage(id string) error {
	if id == t.base {
		return fmt.Errorf("refusing to delete the base image %s", id)
	}
	if t.preImages[id] {
		return fmt.Errorf("refusing to delete preexisting image %s", id)
	}
	if slices.Contains(t.images, id) {
		return nil
	}
	return fmt.Errorf("refusing to delete untracked image %s", id)
}

// cleanup deletes tracked VMs (newest first), then tracked images, and returns
// every problem encountered. ctx must be a fresh bounded context, not the
// (possibly expired) test context.
func (t *tracker) cleanup(ctx context.Context, c api, interval time.Duration) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	for i := len(t.vms) - 1; i >= 0; i-- {
		id := t.vms[i]
		if err := t.mayDeleteVM(id); err != nil {
			add("cleanup refused VM %s: %v", id, err)
			continue
		}
		if _, err := c.VM(ctx, t.user, id); isStatus(err, 404) {
			continue
		}
		acc, err := c.Delete(ctx, t.user, id, t.keySeq("cl"))
		switch {
		case isStatus(err, 404):
			continue
		case err != nil && !isStatus(err, 409):
			add("cleanup delete VM %s: %v", id, err)
			continue
		case err == nil:
			if werr := waitOp(ctx, c, t.user, acc.OperationID, interval); werr != nil {
				add("cleanup delete VM %s operation: %v", id, werr)
			}
		}
		if err := waitGone(ctx, interval, func(ctx context.Context) error { _, e := c.VM(ctx, t.user, id); return e }); err != nil {
			add("cleanup VM %s not gone: %v", id, err)
		}
	}
	for i := len(t.images) - 1; i >= 0; i-- {
		id := t.images[i]
		if id == t.base {
			add("cleanup refused: tracked image equals the base image")
			continue
		}
		if t.preImages[id] {
			add("cleanup refused: tracked image %s is preexisting", id)
			continue
		}
		var acc controlclient.Accepted
		err := pollUntil(ctx, interval, func(ctx context.Context) (bool, error) {
			var e error
			acc, e = c.DeleteImage(ctx, t.user, id, t.keySeq("cl"))
			if e == nil || isStatus(e, 404) {
				return true, nil
			}
			if isStatus(e, 409) {
				return false, e // a VM reference may still be draining: retry
			}
			return false, permanent(e)
		})
		if err != nil {
			add("cleanup delete image %s: %v", id, err)
			continue
		}
		if acc.OperationID != "" {
			if werr := waitOp(ctx, c, t.user, acc.OperationID, interval); werr != nil {
				add("cleanup delete image %s operation: %v", id, werr)
				continue
			}
		}
		if err := waitGone(ctx, interval, func(ctx context.Context) error { _, e := c.Image(ctx, t.user, id); return e }); err != nil {
			add("cleanup image %s not gone: %v", id, err)
		}
	}
	return errs
}

// ---- guest SSH (direct address, pinned host key) ----

var (
	sshUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hostPattern    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// validEndpoint rejects metadata that could smuggle ssh options.
func validEndpoint(host string, port int, user string) error {
	if net.ParseIP(host) == nil && !hostPattern.MatchString(host) {
		return errors.New("invalid SSH host in API metadata")
	}
	if port < 1 || port > 65535 {
		return errors.New("invalid SSH port in API metadata")
	}
	if !sshUserPattern.MatchString(user) {
		return errors.New("invalid SSH user in API metadata")
	}
	return nil
}

// fingerprintFromKeygen extracts "SHA256:..." from `ssh-keygen -l` output
// ("256 SHA256:abc comment (ED25519)").
func fingerprintFromKeygen(out string) string {
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "SHA256:") {
			return f
		}
	}
	return ""
}

// knownHostsLine builds a known_hosts entry from an ssh-keyscan line
// ("host type base64"), for the endpoint actually dialled.
func knownHostsLine(host string, port int, scanLine string) (string, bool) {
	f := strings.Fields(scanLine)
	if len(f) < 3 || strings.HasPrefix(f[0], "#") {
		return "", false
	}
	h := host
	if port != 22 {
		h = "[" + host + "]:" + strconv.Itoa(port)
	}
	return h + " " + f[1] + " " + f[2], true
}

func run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // never a shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		e := strings.TrimSpace(errb.String())
		if len(e) > 300 {
			e = e[:300]
		}
		return out.String(), fmt.Errorf("%s: %v: %s", name, err, e)
	}
	return out.String(), nil
}

// guest is a pinned SSH target. Host-key policy: the key is fetched with
// ssh-keyscan, accepted ONLY if its SHA256 fingerprint equals the one the
// authenticated API returned for this VM, and then enforced with
// StrictHostKeyChecking=yes against a private known_hosts file. Nothing is
// disabled and the user's own known_hosts is never touched. If the operator
// supplies VMAPI_E2E_SSH_KNOWN_HOSTS_FILE, that file replaces the API
// fingerprint as the trust source (still StrictHostKeyChecking=yes).
type guest struct {
	host, user string
	port       int
	key        string
	known      string
}

func pinGuest(ctx context.Context, dir, keyFile, knownHostsFile string, s controlclient.SSH) (*guest, error) {
	if err := validEndpoint(s.Host, s.Port, s.User); err != nil {
		return nil, err
	}
	if knownHostsFile != "" {
		// Operator-trusted source: used as-is, strictly. No scanning, no
		// auto-trust; an endpoint without an entry fails the ssh handshake.
		return &guest{host: s.Host, port: s.Port, user: s.User, key: keyFile, known: knownHostsFile}, nil
	}
	if !strings.HasPrefix(s.HostKeyFingerprint, "SHA256:") {
		return nil, errors.New("API did not return a SHA256 host key fingerprint; refusing unpinned SSH")
	}
	scan, err := run(ctx, "", "ssh-keyscan", "-T", "5", "-p", strconv.Itoa(s.Port), s.Host)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(scan, "\n") {
		kh, ok := knownHostsLine(s.Host, s.Port, line)
		if !ok {
			continue
		}
		fpOut, err := run(ctx, kh+"\n", "ssh-keygen", "-l", "-f", "-")
		if err != nil {
			continue
		}
		if fingerprintFromKeygen(fpOut) == s.HostKeyFingerprint {
			p := filepath.Join(dir, "known_hosts_"+randHex(4))
			if err := os.WriteFile(p, []byte(kh+"\n"), 0o600); err != nil {
				return nil, err
			}
			return &guest{host: s.Host, port: s.Port, user: s.User, key: keyFile, known: p}, nil
		}
	}
	return nil, errors.New("no guest host key matched the API-reported fingerprint")
}

func (g *guest) sshArgs(remote string) []string {
	return []string{
		"-F", "/dev/null", "-i", g.key, "-p", strconv.Itoa(g.port),
		"-o", "IdentityAgent=none", "-o", "CertificateFile=none", "-o", "PreferredAuthentications=publickey",
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "PasswordAuthentication=no",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + g.known, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=8", "-o", "ControlMaster=no", "-o", "ForwardAgent=no", "-o", "LogLevel=ERROR",
		g.user + "@" + g.host, remote,
	}
}

func (g *guest) exec(ctx context.Context, remote string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return run(ctx, "", "ssh", g.sshArgs(remote)...)
}

// markerPath is a fixed per-run file name in the guest user's home.
func markerPath(runID, tag string) string { return ".vmapi-e2e-" + runID + "-" + tag }

func writeMarkerCmd(path, marker string) string {
	return fmt.Sprintf(`umask 077; printf %%s %s > "$HOME/%s" && sync`, marker, path)
}
func readMarkerCmd(path string) string { return fmt.Sprintf(`cat "$HOME/%s"`, path) }
