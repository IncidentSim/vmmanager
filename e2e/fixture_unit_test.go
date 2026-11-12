package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/vmnet"
)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func tgz(t *testing.T, name string, typ byte, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dir/other", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	_, _ = tw.Write([]byte("x"))
	_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Size: int64(len(body)), Mode: 0o755, Linkname: "/etc/passwd"})
	if typ == tar.TypeReg {
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func server(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchVerifiesAndExtracts(t *testing.T) {
	kernel := []byte("kernel-bytes")
	archive := tgz(t, "fc/firecracker", tar.TypeReg, "ELF")
	srv := server(t, map[string][]byte{"/vmlinux": kernel, "/fc.tgz": archive})
	m := Manifest{Artifacts: []Artifact{
		{Name: "vmlinux", URL: srv.URL + "/vmlinux", SHA256: digest(kernel), Bytes: 1 << 20},
		{Name: "firecracker", URL: srv.URL + "/fc.tgz", SHA256: digest(archive), Bytes: 1 << 20, Member: "fc/firecracker", Mode: 0o755},
	}}
	dir := filepath.Join(t.TempDir(), "art")
	if err := Fetch(context.Background(), m, dir, srv.Client()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "vmlinux")); string(b) != "kernel-bytes" {
		t.Fatal("kernel content")
	}
	fi, err := os.Stat(filepath.Join(dir, "firecracker"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("extracted member: %v %v", fi, err)
	}
}

func TestFetchRefusesUntrustedInput(t *testing.T) {
	good := []byte("data")
	srv := server(t, map[string][]byte{
		"/a": good, "/link.tgz": tgz(t, "fc/firecracker", tar.TypeSymlink, ""), "/big": bytes.Repeat([]byte("x"), 100),
	})
	link := tgz(t, "fc/firecracker", tar.TypeSymlink, "")
	cases := map[string]Artifact{
		"hash mismatch":  {Name: "a", URL: srv.URL + "/a", SHA256: digest([]byte("other")), Bytes: 1 << 10},
		"unpinned":       {Name: "a", URL: srv.URL + "/a", SHA256: "PIN_REQUIRED", Bytes: 1 << 10},
		"empty pin":      {Name: "a", URL: srv.URL + "/a", Bytes: 1 << 10},
		"plain http":     {Name: "a", URL: "http://example.invalid/a", SHA256: digest(good), Bytes: 1 << 10},
		"size bound":     {Name: "a", URL: srv.URL + "/big", SHA256: digest(bytes.Repeat([]byte("x"), 100)), Bytes: 10},
		"missing":        {Name: "a", URL: srv.URL + "/nope", SHA256: digest(good), Bytes: 1 << 10},
		"symlink member": {Name: "a", URL: srv.URL + "/link.tgz", SHA256: digest(link), Bytes: 1 << 20, Member: "fc/firecracker"},
		"no member":      {Name: "a", URL: srv.URL + "/link.tgz", SHA256: digest(link), Bytes: 1 << 20, Member: "fc/absent"},
		"traversal":      {Name: "a", URL: srv.URL + "/a", SHA256: digest(good), Bytes: 1 << 10, Member: "../../etc/passwd"},
		"bad name":       {Name: "../a", URL: srv.URL + "/a", SHA256: digest(good), Bytes: 1 << 10},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "art")
			if err := Fetch(context.Background(), Manifest{Artifacts: []Artifact{a}}, dir, srv.Client()); err == nil {
				t.Fatal("accepted")
			}
			ents, _ := os.ReadDir(dir)
			if len(ents) != 0 {
				t.Fatalf("unverified file left behind: %v", ents)
			}
		})
	}
}

func TestShippedManifestIsStrict(t *testing.T) {
	b, err := os.ReadFile("testdata/artifacts.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sha256\": \"\"") {
		t.Fatal("empty pin")
	}
	// Every shipped artifact must have a real pin, never a placeholder.
	if _, err := LoadManifest("testdata/artifacts.json"); err != nil {
		t.Fatal(err)
	}
}

type fakeIP struct{ routes string }

func (f fakeIP) Run(_ context.Context, _ string, name string, a ...string) (vmnet.Result, error) {
	if name == "ip" && len(a) > 1 && a[1] == "route" {
		return vmnet.Result{Stdout: f.routes}, nil
	}
	return vmnet.Result{}, nil
}

func TestConfigureProducesValidPrivateConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "e2e")
	r, err := Configure(context.Background(), Settings{Dir: dir, PoolConf: "/etc/firecracker-vms/pool.conf", Runner: fakeIP{"default via 192.168.1.1 dev eth0\n10.213.0.0/16 dev wg0\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Prefix != "10.214" {
		t.Fatalf("prefix %s: the colliding 10.213 must be skipped", r.Prefix)
	}
	cfg, err := controlapi.LoadConfig(r.APIConfig)
	if err != nil {
		t.Fatalf("vmapi would reject the generated config: %v", err)
	}
	if cfg.Backend.GuestPrefix != "10.214" || cfg.Backend.PoolConf != "/etc/firecracker-vms/pool.conf" {
		t.Fatalf("backend: %+v", cfg.Backend)
	}
	pool, _ := os.ReadFile(r.PoolConf)
	if string(pool) != "GUEST_PREFIX=10.214\nMAX_SLOTS=2\n" {
		t.Fatalf("pool.conf %q", pool)
	}
	for _, p := range []string{r.TokenFile, filepath.Join(dir, "secrets", "ssh-master.key"), filepath.Join(dir, "tls", "server.key"), r.APIConfig, r.EnvFile, dir} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s must be private: %v %v", p, fi, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "secrets", "ssh-master.key")); fi.Size() != 32 {
		t.Error("master key must be exactly 32 bytes")
	}
	env, _ := os.ReadFile(r.EnvFile)
	for _, want := range []string{"VMAPI_E2E=1", "VMAPI_E2E_BASE_URL=https://127.0.0.1:18443", "VMAPI_E2E_SSH_DIRECT=1", "SSL_CERT_FILE=" + r.CAFile} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env lacks %s", want)
		}
	}
	// the generated CA authenticates the generated server certificate
	caPEM, _ := os.ReadFile(r.CAFile)
	pool2 := x509.NewCertPool()
	if !pool2.AppendCertsFromPEM(caPEM) {
		t.Fatal("bad CA")
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls", "server.pem"), filepath.Join(dir, "tls", "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool2, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	// secrets differ between runs
	r2, _ := Configure(context.Background(), Settings{Dir: filepath.Join(t.TempDir(), "e2e"), Runner: fakeIP{}})
	a, _ := os.ReadFile(r.TokenFile)
	b, _ := os.ReadFile(r2.TokenFile)
	if bytes.Equal(a, b) {
		t.Fatal("token reused across runs")
	}
}

func TestChoosePrefixFailsWhenEverythingCollides(t *testing.T) {
	_, err := ChoosePrefix(context.Background(), Settings{Prefixes: []string{"10.213"}, Runner: fakeIP{"10.0.0.0/8 dev wg0\n"}})
	if err == nil || !strings.Contains(err.Error(), "no collision-free") {
		t.Fatalf("err = %v", err)
	}
}
