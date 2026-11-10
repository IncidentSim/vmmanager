package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote/pkitest"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"

	"encoding/pem"
)

// syncBuf is a goroutine-safe log sink: exec copies child output on its own
// goroutine while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestExecutableJWTAndComposedHTTPSGateway runs the REAL vmapi binary with the
// new configuration: public-key-only JWT verification, usernames, durable
// username bindings and expose_api_on_terminal (API + Swagger on the HTTPS
// terminal gateway). Boundary: the VM backend behind the mTLS facade is the
// in-memory vmlibtest FAKE (no Firecracker/KVM/ttyd/guest) and tokens are minted
// by the real bot signer package, not through Telegram. All usernames and IDs
// are synthetic.
func TestExecutableJWTAndComposedHTTPSGateway(t *testing.T) {
	dir := t.TempDir()
	ca := pkitest.NewCA(t, "jwt-smoke")
	facade := ca.Server(t, "facade.test")
	apiClient := ca.Client(t, "api")
	tlsCfg, e := remote.ServerTLSFromPEM(ca.CertPEM, facade.CertPEM, facade.KeyPEM)
	if e != nil {
		t.Fatal(e)
	}
	grpc := remote.NewGRPCServer(tlsCfg)
	svc, e := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	if e != nil {
		t.Fatal(e)
	}
	remote.RegisterFacade(grpc, svc)
	gl, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	go grpc.Serve(gl)
	defer grpc.Stop()
	gw := ca.Server(t, "localhost")
	files := map[string][]byte{"ca": ca.CertPEM, "cert": apiClient.CertPEM, "key": apiClient.KeyPEM, "gw.crt": gw.CertPEM, "gw.key": gw.KeyPEM}
	for name, b := range files {
		if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	// RSA key pair: the API gets ONLY the public half.
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	privDER, _ := x509.MarshalPKCS8PrivateKey(k)
	pubDER, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	privPath, pubPath := filepath.Join(dir, "jwt-private.pem"), filepath.Join(dir, "jwt-public.pem")
	os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600)
	os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0644)
	signer, e := jwtauth.LoadSigner(privPath, "smoke-bot", "smoke-api")
	if e != nil {
		t.Fatal(e)
	}
	mint := func(id int64, name string) string {
		tok, _, e := signer.Issue(id, name, time.Hour, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		return tok
	}

	apiAddr, gwAddr := freeAddr(t), freeAddr(t)
	_, gwPort, _ := net.SplitHostPort(gwAddr)
	cfg := map[string]any{
		"listen_addr": apiAddr, "state_dir": filepath.Join(dir, "state"),
		"allowed_usernames": []string{"@Smoke_User_One", "smoke_user_two"},
		"shared_image_ids":  []string{"ubuntu"}, "max_vms": 2, "max_vms_per_user": 1,
		"credentials":            []any{},
		"jwt":                    map[string]string{"public_key_file": pubPath, "issuer": "smoke-bot", "audience": "smoke-api"},
		"expose_api_on_terminal": true,
		"facade":                 map[string]string{"addr": gl.Addr().String(), "ca_file": filepath.Join(dir, "ca"), "cert_file": filepath.Join(dir, "cert"), "key_file": filepath.Join(dir, "key"), "server_name": "facade.test"},
		"terminal": map[string]any{"listen_addr": gwAddr, "public_origin": "https://localhost:" + gwPort,
			"tls_cert_file": filepath.Join(dir, "gw.crt"), "tls_key_file": filepath.Join(dir, "gw.key"),
			"ttyd_path": "ttyd", "ssh_path": "ssh", "ssh_key_file": filepath.Join(dir, "unused-ssh-key"), "max_clients": 2},
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "api.json")
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	binary := filepath.Join(dir, "vmapi")
	bctx, bcancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer bcancel()
	if out, e := exec.CommandContext(bctx, "go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, out)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM)
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	origin := "https://localhost:" + gwPort

	start := func() (*exec.Cmd, *syncBuf) {
		cmd := exec.Command(binary, "-config", path)
		logs := &syncBuf{}
		cmd.Stdout, cmd.Stderr = logs, logs
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		for until := time.Now().Add(15 * time.Second); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
			if p, e := hc.Get(origin + "/healthz"); e == nil {
				p.Body.Close()
				if p.StatusCode == 200 {
					return cmd, logs
				}
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("vmapi did not become healthy over the composed HTTPS gateway: %s", logs.String())
		return nil, nil
	}
	stop := func(cmd *exec.Cmd, logs *syncBuf) {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("vmapi shutdown: %v %s", e, logs.String())
			}
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Errorf("vmapi did not stop: %s", logs.String())
		}
	}
	get := func(rawURL, token string, hdr map[string]string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", rawURL, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		p, e := hc.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer p.Body.Close()
		body, _ := io.ReadAll(p.Body)
		return p.StatusCode, string(body), p.Header
	}

	cmd, logs := start()
	// Swagger UI, assets and spec over the SAME HTTPS origin, no credentials.
	if code, body, h := get(origin+"/docs", "", nil); code != 200 || !strings.Contains(body, "swagger-ui-bundle.js") || h.Get("Content-Security-Policy") == "" {
		t.Fatalf("/docs %d", code)
	}
	if code, body, _ := get(origin+"/docs/assets/swagger-ui-bundle.js", "", nil); code != 200 || len(body) < 100000 {
		t.Fatalf("bundle %d %d", code, len(body))
	}
	if code, body, _ := get(origin+"/openapi.yaml", "", nil); code != 200 || !strings.Contains(body, "bearerFormat: JWT") || !strings.Contains(body, "url: /") {
		t.Fatalf("openapi %d", code)
	}
	// Terminal gateway invariants stay: bad capability 404, wrong Host not served.
	if code, _, _ := get(origin+"/t/"+strings.Repeat("A", 43)+"/", "", nil); code != 404 {
		t.Fatalf("unknown terminal capability: %d", code)
	}
	req, _ := http.NewRequest("GET", origin+"/healthz", nil)
	req.Host = "evil.test"
	if p, e := hc.Do(req); e != nil || p.StatusCode != 404 {
		t.Fatalf("foreign Host must be refused: %v", e)
	}
	// Authentication: none / forged / wrong-key tokens are 401.
	if code, _, _ := get(origin+"/v1/vms", "", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherDER, _ := x509.MarshalPKCS8PrivateKey(other)
	otherPath := filepath.Join(dir, "other.pem")
	os.WriteFile(otherPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER}), 0600)
	forger, _ := jwtauth.LoadSigner(otherPath, "smoke-bot", "smoke-api")
	forged, _, _ := forger.Issue(7001, "smoke_user_one", time.Hour, time.Now())
	if code, _, _ := get(origin+"/v1/vms", forged, nil); code != 401 {
		t.Fatalf("forged token: %d", code)
	}
	// A bot-signed token for the configured username (spelling/case differ from
	// config) bootstraps and pins slot -> ID 7001; X-Acting-User-ID is refused.
	one := mint(7001, "smoke_user_one")
	if code, _, _ := get(origin+"/v1/vms", one, nil); code != 200 {
		t.Fatalf("bound user: %d", code)
	}
	if code, _, _ := get(origin+"/v1/vms", one, map[string]string{"X-Acting-User-ID": "7002"}); code != 403 {
		t.Fatalf("acting header: %d", code)
	}
	if code, _, _ := get(origin+"/v1/vms", mint(7002, "smoke_user_one"), nil); code != 403 {
		t.Fatalf("recycled handle: %d", code)
	}
	if code, _, _ := get(origin+"/v1/vms", mint(7003, "not_listed_user"), nil); code != 403 {
		t.Fatalf("unlisted: %d", code)
	}
	// Real SDK with per-user tokens through the loopback API listener.
	loop := "http://" + apiAddr
	cli, e := controlclient.NewPerUser(loop, func(id int64) (string, error) { return mint(id, "smoke_user_one"), nil }, hc)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	created, e := cli.Create(ctx, 7001, "jwt-smoke-create", controlclient.CreateRequest{Name: "smoke", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = cli.Wait(ctx, 7001, created.OperationID, 20*time.Millisecond); e != nil {
		t.Fatal(e)
	}
	// Restart: pinned binding and the VM survive; the recycled ID stays refused.
	stop(cmd, logs)
	cmd, logs = start()
	defer func() { stop(cmd, logs) }()
	if code, _, _ := get(origin+"/v1/vms", mint(7002, "smoke_user_one"), nil); code != 403 {
		t.Fatalf("binding lost on restart: %d", code)
	}
	if code, body, _ := get(origin+"/v1/vms/"+created.VMID, mint(7001, ""), nil); code != 200 || !strings.Contains(body, created.VMID) {
		t.Fatalf("VM after restart: %d %s", code, body)
	}
	fi, e := os.Stat(filepath.Join(dir, "state", "username-bindings.json"))
	if e != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("binding file: %v %v", fi, e)
	}
	if strings.Contains(logs.String(), one) || strings.Contains(logs.String(), "eyJ") {
		t.Fatal("token leaked into server logs")
	}
}
