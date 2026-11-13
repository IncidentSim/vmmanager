package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/fixture"
	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

const disposableGate = "VMAPI_E2E_DISPOSABLE"

var requiredTests = []string{"TestPublicDocsHealthAndAuth", "TestLifecycleStopCommitsAndConflicts", "TestDerivativeFromCommittedImage", "TestEarlyDeleteAfterCreate", "TestCommitCredentialHygiene", "TestSuppliedKeyLogin", "TestFiniteTTL", "TestProfileAndQuotaMetadata"}
var disposableActive bool
var disposableContext = context.Background()
var fixturePasses = map[string]bool{}

// Ordinary go test never prepares a host. Only this explicit disposable-host
// opt-in may install files or activate the product's helper socket.
func TestMain(m *testing.M) {
	if os.Getenv(disposableGate) != "1" {
		os.Exit(m.Run())
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Minute)
	defer cancel()
	code, err := runDisposable(ctx, m.Run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "disposable E2E:", err)
		code = 1
	}
	os.Exit(code)
}

type fixtureUndo []func() error

func (u *fixtureUndo) close() error {
	var errs []error
	for i := len(*u) - 1; i >= 0; i-- {
		errs = append(errs, (*u)[i]())
	}
	return errors.Join(errs...)
}

type fixtureCommand func(context.Context, string, ...string) error

func execFixture(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func disposablePrerequisites(getenv func(string) string, stat func(string) (os.FileInfo, error), look func(string) (string, error)) error {
	if getenv(disposableGate) != "1" {
		return errors.New("set VMAPI_E2E_DISPOSABLE=1 only on a disposable KVM host")
	}
	var missing []string
	for _, p := range []string{"/dev/kvm", "/dev/net/tun", "/var/lib/firecracker-builder/storage"} {
		if _, err := stat(p); err != nil {
			missing = append(missing, p)
		}
	}
	for _, n := range []string{"sudo", "ip", "fakeroot", "unsquashfs", "mkfs.ext4", "debugfs", "ssh", "ssh-keygen"} {
		if _, err := look(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing prerequisites (see docs/ci.md): %s", strings.Join(missing, ", "))
	}
	for _, p := range []string{"/etc/firecracker-vms/pool.conf", "/usr/local/share/vm-firecracker/install-manifest.json", vmhelper.SocketPath} {
		if _, err := stat(p); !os.IsNotExist(err) {
			return fmt.Errorf("refusing non-fresh host: %s exists or cannot be inspected", p)
		}
	}
	return nil
}

// Strip inherited deployment settings before either the API or tests see them.
func fixtureEnvironment(inherited, fixture []string) []string {
	var env []string
	for _, kv := range inherited {
		if !strings.HasPrefix(kv, "VMAPI_E2E") && !strings.HasPrefix(kv, "SSL_CERT_") {
			env = append(env, kv)
		}
	}
	return append(env, fixture...)
}
func applyFixtureEnvironment(fixture []string) error {
	env := fixtureEnvironment(os.Environ(), fixture)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "VMAPI_E2E") || strings.HasPrefix(k, "SSL_CERT_") {
			_ = os.Unsetenv(k)
		}
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

func runDisposable(ctx context.Context, tests func() int) (code int, err error) {
	if err = disposablePrerequisites(os.Getenv, os.Stat, exec.LookPath); err != nil {
		return 1, err
	}
	build, err := filepath.Abs(os.Getenv("VMAPI_E2E_BUILD_DIR"))
	if err != nil {
		return 1, err
	}
	if os.Getenv("VMAPI_E2E_BUILD_DIR") == "" {
		return 1, errors.New("VMAPI_E2E_BUILD_DIR is required; use make e2e")
	}
	host, binary := filepath.Join(build, "vmhost"), filepath.Join(build, "vmapi")
	for _, p := range []string{host, binary} {
		if _, err = os.Stat(p); err != nil {
			return 1, fmt.Errorf("build product first: %w", err)
		}
	}
	if err = execFixture(ctx, "sudo", "-n", "true"); err != nil {
		return 1, errors.New("passwordless sudo required on the disposable host")
	}
	// Credential-store trust rejects writable ancestors such as /tmp and the
	// hosted runner's shared RUNNER_TEMP before a private barrier. Use the
	// unprivileged account's home, never weaken production directory checks.
	home, err := os.UserHomeDir()
	if err != nil {
		return 1, err
	}
	dir, err := os.MkdirTemp(home, ".vm-e2e-*")
	if err != nil {
		return 1, err
	}
	var undo fixtureUndo
	defer func() {
		err = errors.Join(err, undo.close())
		if err != nil {
			fmt.Fprintln(os.Stderr, "fixture retained for diagnosis:", dir)
		} else {
			err = os.RemoveAll(dir)
		}
	}()
	s := Settings{Dir: filepath.Join(dir, "fixture")}.withDefaults()
	r, err := Configure(ctx, s)
	if err != nil {
		return 1, err
	}
	m, err := LoadManifest("../internal/fixture/artifacts.json")
	if err != nil {
		return 1, err
	}
	artifacts := filepath.Join(dir, "artifacts")
	if err = Fetch(ctx, m, artifacts, nil); err != nil {
		return 1, err
	}
	image := filepath.Join(dir, "base.ext4")
	if err = PrepareImage(ctx, filepath.Join(artifacts, "rootfs.squashfs"), image, os.Stdout); err != nil {
		return 1, err
	}
	// Trust is derived offline from the verified image, never from a network scan.
	known := filepath.Join(s.Dir, "known_hosts")
	if err = offlineKnownHosts(ctx, image, known, r.Prefix, s.MaxVMs); err != nil {
		return 1, err
	}
	who, err := user.Current()
	if err != nil {
		return 1, err
	}
	group, err := user.LookupGroupId(who.Gid)
	if err != nil {
		return 1, err
	}
	if err = installFixtureHost(ctx, execFixture, &undo, host, build, group.Name, who.Username, r.PoolConf, artifacts); err != nil {
		return 1, err
	}
	dst := filepath.Join(r.ImagesDir, s.ImageID+".ext4")
	if err = copyFixtureImage(image, dst); err != nil {
		return 1, err
	}
	undo = append(undo, func() error { return os.Remove(dst) })
	raw, err := os.ReadFile(r.EnvFile)
	if err != nil {
		return 1, err
	}
	fixture := append(strings.Split(strings.TrimSpace(string(raw)), "\n"), envKnownF+"="+known)
	disposableActive = true
	disposableContext = ctx
	if err = applyFixtureEnvironment(fixture); err != nil {
		return 1, err
	}
	ca, err := os.ReadFile(r.CAFile)
	if err != nil {
		return 1, err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	// The suite uses its ordinary HTTP clients too; avoid the process-global
	// system-root cache retaining inherited SSL_CERT_FILE from another deployment.
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = oldTransport }()
	api := exec.Command(binary, "-config", r.APIConfig)
	api.Env = fixtureEnvironment(os.Environ(), fixture)
	api.Stdout, api.Stderr = os.Stdout, os.Stderr
	done, result, shutdown, err := startFixtureAPI(api)
	if err != nil {
		return 1, err
	}
	undo = append(undo, shutdown)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	if err = waitFixtureHealthy(ctx, hc, "https://"+s.Listen, done, result); err != nil {
		return 1, err
	}
	token, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return 1, err
	}
	client, err := controlclient.NewUser("https://"+s.Listen, strings.TrimSpace(string(token)), s.UserID, hc)
	if err != nil {
		return 1, err
	}
	baseline, err := client.ListVMs(ctx, s.UserID)
	if err != nil || len(baseline) != 0 {
		return 1, errors.New("fresh fixture API must have no preexisting VMs")
	}
	undo = append(undo, func() error {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return drainFixture(cleanCtx, client, s.UserID)
	})
	code = tests()
	err = errors.Join(err, requireFixturePasses(fixturePasses))
	return code, err
}

func installFixtureHost(ctx context.Context, run fixtureCommand, undo *fixtureUndo, host, build, group, user, pool, artifacts string) error {
	// Register rollback before effects: a failed command may have partially applied.
	clean := func(args ...string) error {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return run(c, "sudo", append([]string{"-n"}, args...)...)
	}
	*undo = append(*undo, func() error { return clean(host, "uninstall", "--group", group) })
	if err := run(ctx, "sudo", "-n", host, "install", "--build-dir", build, "--group", group, "--firecracker", filepath.Join(artifacts, "firecracker"), "--kernel", filepath.Join(artifacts, "vmlinux")); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return clean("rm", "-f", "/etc/firecracker-vms/pool.conf") })
	if err := run(ctx, "sudo", "-n", "install", "-D", "-m", "0644", "-o", "root", "-g", "root", pool, "/etc/firecracker-vms/pool.conf"); err != nil {
		return err
	}
	if err := run(ctx, "sudo", "-n", host, "setup", "--apply", "--user", user); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return clean("systemctl", "disable", "fc-vm-helper.socket") }, func() error {
		return clean("systemctl", "stop", "firecracker-vm@*.service", "firecracker-vm-network@*.service", "fc-vm-helper.socket")
	})
	if err := run(ctx, "sudo", "-n", host, "enable", "--apply", "--group", group); err != nil {
		return err
	}
	return run(ctx, host, "preflight", "--group", group)
}

func copyFixtureImage(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, in)
	err = errors.Join(err, f.Close())
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

func offlineKnownHosts(ctx context.Context, image, dst, prefix string, slots int) error {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat /etc/ssh/ssh_host_ed25519_key.pub", image).Output()
	if err != nil {
		return fmt.Errorf("offline guest host key: %w", err)
	}
	data, err := fixture.KnownHosts(out, prefix, slots)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

func waitFixtureHealthy(ctx context.Context, hc *http.Client, base string, done <-chan struct{}, result func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		select {
		case <-done:
			return fmt.Errorf("vmapi exited before healthy: %v", result())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/healthz", nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
	}
}

func requireFixturePasses(passed map[string]bool) error {
	var errs []error
	for _, name := range requiredTests {
		if !passed[name] {
			errs = append(errs, fmt.Errorf("required real E2E %s did not pass (missing, skipped or failed)", name))
		}
	}
	return errors.Join(errs...)
}

// Only the freshly created, empty API and generated principal reach here. The
// per-test trackers clean their resources first; this catches failure leftovers.
func drainFixture(ctx context.Context, c *controlclient.Client, uid int64) error {
	vms, err := c.ListVMs(ctx, uid)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range vms {
		acc, e := c.Delete(ctx, uid, v.ID, "e2e-cleanup-"+v.ID)
		if e == nil {
			e = waitOp(ctx, c, uid, acc.OperationID, time.Second)
		}
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}

// Both readiness and teardown may observe child exit. A closed channel plus
// stored result is reusable; consuming a single exit value is not.
func startFixtureAPI(cmd *exec.Cmd) (<-chan struct{}, func() error, func() error, error) {
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	done := make(chan struct{})
	var result error
	go func() { result = cmd.Wait(); close(done) }()
	read := func() error { <-done; return result }
	shutdown := func() error {
		select {
		case <-done:
			return result
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			return result
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return errors.New("fixture vmapi required forced termination")
		}
	}
	return done, read, shutdown, nil
}
