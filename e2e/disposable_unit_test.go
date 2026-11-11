package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestDisposableOptInBeforeAnyDiscovery(t *testing.T) {
	err := disposablePrerequisites(func(string) string { return "" }, func(string) (os.FileInfo, error) { t.Fatal("discovered host without opt-in"); return nil, nil }, func(string) (string, error) { t.Fatal("looked up commands without opt-in"); return "", nil })
	if err == nil || !strings.Contains(err.Error(), disposableGate) {
		t.Fatal(err)
	}
}

func TestDisposablePrerequisiteFailuresHaveNoEffects(t *testing.T) {
	look := func(n string) (string, error) { return "/usr/bin/" + n, nil }
	enabled := func(string) string { return "1" }
	_, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"", "/etc/firecracker-vms/pool.conf", "/usr/local/share/vm-firecracker/install-manifest.json"} {
		t.Run(existing, func(t *testing.T) {
			stat := func(p string) (os.FileInfo, error) {
				if existing == "" || strings.HasPrefix(p, "/etc/") || strings.Contains(p, "manifest.json") {
					if p != existing {
						return nil, os.ErrNotExist
					}
				}
				return os.Stat(t.TempDir())
			}
			err := disposablePrerequisites(enabled, stat, look)
			if err == nil {
				t.Fatal("accepted missing prerequisites or existing installation")
			}
			if existing != "" && !strings.Contains(err.Error(), "non-fresh") {
				t.Fatal(err)
			}
		})
	}
}

func TestFixtureEnvironmentScrubsProductionSettings(t *testing.T) {
	got := fixtureEnvironment([]string{"PATH=/safe", "VMAPI_E2E_BASE_URL=https://production", "VMAPI_E2E_RESTART_USER_UNIT=production.service", "VMAPI_E2E_OTHER_TOKEN_FILE=/secret", "VMAPI_E2E_DISPOSABLE=1", "SSL_CERT_FILE=/production", "SSL_CERT_DIR=/production"}, []string{"VMAPI_E2E=1", "VMAPI_E2E_BASE_URL=https://127.0.0.1:18443"})
	want := []string{"PATH=/safe", "VMAPI_E2E=1", "VMAPI_E2E_BASE_URL=https://127.0.0.1:18443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestFixtureHostFailureCleanup(t *testing.T) {
	// Fake every host effect, including sudo and service operations. Fail each
	// setup step after it may have applied; rollback must still finish in order.
	for fail := 1; fail <= 5; fail++ {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var calls, clean []string
			cleaning := false
			fake := func(c context.Context, name string, args ...string) error {
				line := name + " " + strings.Join(args, " ")
				if cleaning {
					if c.Err() != nil {
						t.Error("cleanup inherited cancelled context")
					}
					clean = append(clean, line)
					if strings.Contains(line, "systemctl stop") {
						return errors.New("stop failure")
					}
					return nil
				}
				calls = append(calls, line)
				if len(calls) == fail {
					return errors.New("setup failure")
				}
				return nil
			}
			var undo fixtureUndo
			err := installFixtureHost(ctx, fake, &undo, "/build/vmhost", "/build", "runner", "runner", "/fixture/pool.conf", "/artifacts")
			if err == nil {
				t.Fatal("did not fail injected step")
			}
			cancel()
			cleaning = true
			_ = undo.close()
			if len(clean) == 0 || !strings.Contains(clean[len(clean)-1], "vmhost uninstall") {
				t.Fatalf("no installer rollback: %v", clean)
			}
			if fail >= 4 && (!strings.Contains(clean[0], "systemctl stop") || !strings.Contains(clean[1], "systemctl disable")) {
				t.Fatalf("units not stopped before uninstall: %v", clean)
			}
			for _, call := range calls {
				if strings.Contains(call, " install ") && strings.Contains(call, "vmhost") && !strings.Contains(call, "--build-dir /build") {
					t.Fatal("parallel installer")
				}
			}
		})
	}
}

func TestRequiredFixturePassesRejectMissingSkippedFailed(t *testing.T) {
	passed := map[string]bool{}
	if requireFixturePasses(passed) == nil {
		t.Fatal("missing tests accepted")
	}
	for _, n := range requiredTests {
		passed[n] = true
	}
	if err := requireFixturePasses(passed); err != nil {
		t.Fatal(err)
	}
	for _, n := range requiredTests {
		passed[n] = false
		if requireFixturePasses(passed) == nil {
			t.Fatal("skipped/failed test accepted")
		}
		passed[n] = true
	}
}

func TestFixtureHealthExitIsReusableAndCancellationBounded(t *testing.T) {
	done := make(chan struct{})
	close(done)
	for i := 0; i < 2; i++ {
		err := waitFixtureHealthy(context.Background(), &http.Client{}, "https://127.0.0.1:1", done, func() error { return errors.New("exit 7") })
		if err == nil || !strings.Contains(err.Error(), "exit 7") {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitFixtureHealthy(ctx, &http.Client{}, "https://127.0.0.1:1", make(chan struct{}), func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestFixtureImageCopyNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(src, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyFixtureImage(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := copyFixtureImage(src, dst); err == nil {
		t.Fatal("overwrote existing image")
	}
	if b, _ := os.ReadFile(dst); string(b) != "image" {
		t.Fatal("image changed")
	}
}

func TestOfflineGuestHostTrustFromExt4(t *testing.T) {
	for _, n := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(n); err != nil {
			t.Skip(n + " unavailable")
		}
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	keys := filepath.Join(tree, "etc/ssh")
	if err := os.MkdirAll(keys, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(key), 0600); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "rootfs.ext4")
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-d", tree, image, "16M")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, b)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := offlineKnownHosts(context.Background(), image, known, "10.213", 2); err != nil {
		t.Fatal(err)
	}
	check, err := knownhosts.New(known)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("10.213.0.2"), Port: 22}
	for _, host := range []string{"10.213.0.2:22", "10.213.1.2:22"} {
		if err := check(host, addr, key); err != nil {
			t.Fatal(err)
		}
	}
	wrong, _, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey, _ := ssh.NewPublicKey(wrong)
	if check("10.213.0.2:22", addr, wrongKey) == nil {
		t.Fatal("wrong guest key trusted")
	}
	if check("10.213.2.2:22", addr, key) == nil {
		t.Fatal("unallocated slot trusted")
	}
	if err := offlineKnownHosts(context.Background(), filepath.Join(dir, "missing.ext4"), known, "10.213", 2); err == nil {
		t.Fatal("missing key accepted")
	}
}

func TestFixtureAPIActualEarlyExitCanBeObservedAndReaped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	done, result, shutdown, err := startFixtureAPI(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = waitFixtureHealthy(context.Background(), &http.Client{}, "https://127.0.0.1:1", done, result)
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatal(err)
	}
	// Readiness already observed exit; repeated cleanup must not hang or lose it.
	for i := 0; i < 2; i++ {
		if err := shutdown(); err == nil {
			t.Fatal("early exit lost")
		}
	}
	if _, _, _, err := startFixtureAPI(exec.Command("/nonexistent/vmapi")); err == nil {
		t.Fatal("bad executable accepted")
	}
}

func TestFixtureRollbackContinuesAfterFailures(t *testing.T) {
	var got []int
	undo := fixtureUndo{func() error { got = append(got, 1); return nil }, func() error { got = append(got, 2); return errors.New("failure") }, func() error { got = append(got, 3); return nil }}
	if undo.close() == nil || !reflect.DeepEqual(got, []int{3, 2, 1}) {
		t.Fatal(got)
	}
}
