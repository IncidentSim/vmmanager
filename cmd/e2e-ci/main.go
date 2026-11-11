// e2e-ci drives the real end-to-end job on a DISPOSABLE Linux/KVM host. It is a
// CI harness, not part of the installed product.
//
//	e2e-ci fetch         download pinned, SHA-256-verified artifacts
//	e2e-ci prepare-image convert the verified squashfs to ext4 under fakeroot
//	e2e-ci configure     generate pool.conf, vmapi config, TLS, token and master key
//	e2e-ci install-image copy the verified rootfs into the images directory
//	e2e-ci run           start vmapi, run the existing ./e2e suite, stop, clean up
//
// run FAILS (never skips) when KVM, the helper socket, pool.conf or the base
// image are missing. It talks only to the vmapi it starts on loopback.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/e2eci"
	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-ci:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: e2e-ci fetch|prepare-image|configure|install-image|run [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("dir", "", "private working directory (absolute)")
	manifest := fs.String("manifest", "e2e/ci/artifacts.json", "fetch: pinned artifact manifest")
	outDir := fs.String("out", "", "fetch: output directory; prepare-image: output ext4 file")
	storage := fs.String("storage-root", "/var/lib/firecracker-builder/storage", "existing storage mount")
	libexec := fs.String("libexec", "/usr/local/libexec", "directory of fc-vm-helper-client")
	pool := fs.String("pool-conf", "/etc/firecracker-vms/pool.conf", "installed pool.conf path")
	userID := fs.Int64("user-id", 424242, "numeric E2E principal")
	imageID := fs.String("image-id", "e2e-base", "base image ID")
	rootfs := fs.String("rootfs", "", "install-image: verified rootfs ext4")
	vmapi := fs.String("vmapi", "bin/vmapi", "run: vmapi executable")
	timeout := fs.Duration("timeout", 45*time.Minute, "run: go test timeout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s := e2eci.Settings{Dir: *dir, UserID: *userID, StorageRoot: *storage, Libexec: *libexec, PoolConf: *pool, ImageID: *imageID}
	switch args[0] {
	case "fetch":
		m, err := e2eci.LoadManifest(*manifest)
		if err != nil {
			return err
		}
		if *outDir == "" {
			return errors.New("--out is required")
		}
		if err := e2eci.Fetch(ctx, m, *outDir, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "fetched and verified %d artifacts into %s\n", len(m.Artifacts), *outDir)
	case "configure":
		r, err := e2eci.Configure(ctx, s)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "guest prefix %s\npool.conf (install root-owned at %s): %s\nvmapi config: %s\n", r.Prefix, *pool, r.PoolConf, r.APIConfig)
	case "prepare-image":
		return e2eci.PrepareImage(ctx, *rootfs, *outDir, out)
	case "install-image":
		return installImage(*rootfs, filepath.Join(*storage, "vms/images"), *imageID, out)
	case "run":
		return runSuite(ctx, s, *vmapi, *timeout, out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}

func installImage(src, imagesDir, id string, out io.Writer) error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(id) {
		return errors.New("invalid image ID")
	}
	if src == "" {
		return errors.New("--rootfs is required")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dst := filepath.Join(imagesDir, id+".ext4")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("refusing to overwrite or create %s: %w", dst, err)
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	fmt.Fprintf(out, "installed base image %s\n", dst)
	return nil
}

// require fails loudly when a prerequisite of the real E2E is missing.
func require(s e2eci.Settings) error {
	var missing []string
	if fi, err := os.Stat("/dev/kvm"); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		missing = append(missing, "/dev/kvm (this runner has no KVM; the real E2E cannot run)")
	}
	if _, err := os.Stat(vmhelper.SocketPath); err != nil {
		missing = append(missing, vmhelper.SocketPath+" (run: vmhost enable --apply)")
	}
	if _, err := os.Stat(s.PoolConf); err != nil {
		missing = append(missing, s.PoolConf+" (install the generated pool.conf)")
	}
	if _, err := os.Stat(filepath.Join(s.StorageRoot, "vms/images", s.ImageID+".ext4")); err != nil {
		missing = append(missing, "base image "+s.ImageID+" (run: e2e-ci install-image)")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "e2e.env")); err != nil {
		missing = append(missing, "e2e.env (run: e2e-ci configure)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing prerequisites (the E2E never skips silently):\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

func readEnv(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			env = append(env, l)
		}
	}
	return env, nil
}

func runSuite(ctx context.Context, s e2eci.Settings, vmapi string, timeout time.Duration, out io.Writer) error {
	if s.Dir == "" {
		return errors.New("--dir is required")
	}
	if err := require(s); err != nil {
		return err
	}
	fixture, err := readEnv(filepath.Join(s.Dir, "e2e.env"))
	if err != nil {
		return err
	}
	// Only the generated fixture may drive the suite: any inherited VMAPI_E2E_*
	// setting (for example one pointing at a real server) is dropped.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "VMAPI_E2E") && !strings.HasPrefix(kv, "SSL_CERT_") {
			env = append(env, kv)
		}
	}
	env = append(env, fixture...)
	kv := map[string]string{}
	for _, l := range fixture {
		k, v, _ := strings.Cut(l, "=")
		kv[k] = v
	}

	api := exec.Command(vmapi, "-config", filepath.Join(s.Dir, "vmapi.json"))
	api.Stdout, api.Stderr = os.Stdout, os.Stderr
	if err := api.Start(); err != nil {
		return fmt.Errorf("start vmapi: %w", err)
	}
	exited := make(chan error, 1)
	done := make(chan struct{})
	go func() { exited <- api.Wait(); close(done) }()
	defer func() {
		_ = api.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = api.Process.Kill()
			<-done
		}
	}()

	ca, err := os.ReadFile(kv["SSL_CERT_FILE"])
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	if err := waitHealthy(ctx, hc, kv["VMAPI_E2E_BASE_URL"], exited); err != nil {
		return err
	}

	results, err := os.Create(filepath.Join(s.Dir, "e2e-results.json"))
	if err != nil {
		return err
	}
	defer results.Close()
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "-p", "1", "-timeout", timeout.String(), "./e2e")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out, results), os.Stderr
	testErr := cmd.Run()

	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	cleanErr := cleanup(cleanCtx, hc, kv)
	if testErr != nil {
		return fmt.Errorf("e2e suite failed: %w", testErr)
	}
	if cleanErr != nil {
		return cleanErr
	}
	if _, err := results.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return verifySuite(results, out)
}

// Required lifecycle checks cannot turn green by skipping for lack of capacity.
// Optional external integrations (Nexterm, restart, public SSH, host readback)
// remain explicitly optional; their skip events are printed and archived.
var requiredTests = []string{
	"TestPublicDocsHealthAndAuth", "TestLifecycleStopCommitsAndConflicts",
	"TestDerivativeFromCommittedImage", "TestEarlyDeleteAfterCreate",
	"TestCommitCredentialHygiene", "TestSuppliedKeyLogin",
	"TestFiniteTTL", "TestProfileAndQuotaMetadata",
}

func verifySuite(r io.Reader, out io.Writer) error {
	dec := json.NewDecoder(r)
	passed := map[string]bool{}
	for {
		var event struct{ Action, Test string }
		if err := dec.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("E2E JSON report: %w", err)
		}
		if event.Action == "pass" {
			passed[event.Test] = true
		}
		if event.Action == "skip" {
			fmt.Fprintf(out, "E2E explicitly skipped: %s\n", event.Test)
		}
	}
	for _, name := range requiredTests {
		if !passed[name] {
			return fmt.Errorf("required real E2E %s did not pass (missing, skipped or failed)", name)
		}
	}
	return nil
}

func waitHealthy(ctx context.Context, hc *http.Client, base string, exited <-chan error) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("vmapi exited before becoming healthy: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return errors.New("vmapi did not become healthy within 2 minutes")
}

// cleanup deletes any VM still owned by the generated E2E principal on this
// disposable host. The suite deletes its own fixtures; this only catches
// leftovers of a failed run and never touches other principals.
func cleanup(ctx context.Context, hc *http.Client, kv map[string]string) error {
	token, err := os.ReadFile(kv["VMAPI_E2E_TOKEN_FILE"])
	if err != nil {
		return err
	}
	uid, err := strconv.ParseInt(kv["VMAPI_E2E_USER_ID"], 10, 64)
	if err != nil {
		return err
	}
	c, err := controlclient.NewUser(kv["VMAPI_E2E_BASE_URL"], strings.TrimSpace(string(token)), uid, hc)
	if err != nil {
		return err
	}
	vms, err := c.ListVMs(ctx, uid)
	if err != nil {
		return fmt.Errorf("cleanup list: %w", err)
	}
	var failed []string
	for _, vm := range vms {
		acc, err := c.Delete(ctx, uid, vm.ID, "e2e-ci-cleanup-"+vm.ID)
		if err == nil {
			_, err = c.Wait(ctx, uid, acc.OperationID, time.Second)
		}
		if err != nil {
			failed = append(failed, vm.ID+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("leftover VMs could not be deleted: %s", strings.Join(failed, "; "))
	}
	return nil
}
