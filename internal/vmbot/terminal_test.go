package vmbot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the FAKE ttyd used by the offline tests: when the test
// binary is exec'd with ttyd's --writable flag it behaves like a tiny ttyd
// (unix-socket HTTP server, "websocket" echo upgrade, a setsid child per client
// like real ttyd's forkpty). Real ttyd is covered only by the live procedure.
func TestMain(m *testing.M) {
	for _, a := range os.Args[1:] {
		if a == "--writable" {
			fakeTTYD(os.Args[1:])
			return
		}
	}
	os.Exit(m.Run())
}

func fakeTTYD(args []string) {
	var sock string
	checkOrigin := false
	for i, a := range args {
		switch a {
		case "--interface":
			sock = args[i+1]
		case "--check-origin":
			checkOrigin = true
		}
	}
	b, _ := json.Marshal(args)
	os.WriteFile(sock+".argv", b, 0o600)
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "<html>FAKE-TTYD</html>")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"token":""}`) })
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if checkOrigin {
			o := r.Header.Get("Origin")
			if i := strings.Index(o, "://"); i < 0 || o[i+3:] != r.Host {
				http.Error(w, "origin", 403)
				return
			}
		}
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		child := exec.Command("sleep", "300")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if child.Start() == nil {
			os.WriteFile(sock+".child", []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		brw.Flush()
		go func() {
			defer conn.Close()
			io.Copy(conn, brw)
		}()
	})
	http.Serve(l, mux)
	os.Exit(0)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		// a killed child may linger as a zombie of its (dead) parent until init reaps it
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d still alive", pid)
}

const pubHost = "bot.test"

type tenv struct {
	m   *Manager
	srv *httptest.Server
	dir string
}

func newTerm(t *testing.T, mutate func(c *ManagerConfig)) *tenv {
	t.Helper()
	dir, err := os.MkdirTemp("", "vmbot-t") // short path: unix socket limit
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)
	cfg := ManagerConfig{TTYDPath: os.Args[0], SSHPath: "/usr/bin/ssh", SSHKeyFile: "/keys/id_test", StateDir: dir,
		PublicOrigin: "https://" + pubHost, MaxClients: 2, MaxSessions: 2, StartTimeout: 5 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(func() { srv.Close(); m.Close(); os.RemoveAll(dir) })
	return &tenv{m: m, srv: srv, dir: dir}
}

func target(id, tok string) TerminalTarget {
	return TerminalTarget{VMID: id, Token: tok, Host: "172.16.1.2", User: "root"}
}

const tokA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const tokB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

func (e *tenv) get(t *testing.T, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	req.Host = pubHost
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *tenv) ws(t *testing.T, path string, hdr map[string]string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"Host": pubHost, "Origin": "https://" + pubHost}
	for k, v := range hdr {
		h[k] = v
	}
	var sb strings.Builder
	sb.WriteString("GET " + path + " HTTP/1.1\r\n")
	for k, v := range h {
		if v != "" {
			sb.WriteString(k + ": " + v + "\r\n")
		}
	}
	sb.WriteString("Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	c.Write([]byte(sb.String()))
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

func TestSSHArgsAreFixedAndNeverUseAShell(t *testing.T) {
	cfg := ManagerConfig{SSHPath: "/usr/bin/ssh", SSHKeyFile: "/keys/id", StateDir: "/s"}
	args, err := BuildSSHArgs(cfg, TerminalTarget{VMID: "bot-1", Host: "172.16.1.2", User: "root", ProxyJump: "jump@bastion:2222"}, "/s/known_hosts/bot-1")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" -F /dev/null ", "BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "ForwardAgent=no",
		"ForwardX11=no", "ClearAllForwardings=yes", "StrictHostKeyChecking=accept-new", "UserKnownHostsFile=/s/known_hosts/bot-1",
		"GlobalKnownHostsFile=/dev/null", "PasswordAuthentication=no", " -i /keys/id ", " root@172.16.1.2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if args[0] != "/usr/bin/ssh" || args[len(args)-1] != "root@172.16.1.2" {
		t.Fatalf("shape %v", args)
	}
	if strings.Contains(joined, " -J ") || strings.Contains(joined, "ProxyJump") || strings.Contains(joined, "bastion") {
		t.Fatalf("bastion must be ignored by default: %s", joined)
	}
	for _, a := range args {
		if a == "sh" || a == "-c" || a == "bash" || a == "-A" || a == "-X" || a == "-L" || a == "-R" || a == "-D" {
			t.Fatalf("forbidden element %q", a)
		}
	}
	cfg.UseBastion = true
	args, _ = BuildSSHArgs(cfg, TerminalTarget{VMID: "bot-1", Host: "172.16.1.2", User: "root", ProxyJump: "jump@bastion:2222"}, "/s/k")
	if !strings.Contains(strings.Join(args, " "), "-J jump@bastion:2222") {
		t.Fatalf("explicit bastion opt-in: %v", args)
	}
}

func TestSSHInjectionBoundary(t *testing.T) {
	cfg := ManagerConfig{SSHPath: "/usr/bin/ssh", SSHKeyFile: "/keys/id", StateDir: "/s", UseBastion: true}
	bad := []TerminalTarget{
		{VMID: "bot-1", Host: "172.16.1.2 -oProxyCommand=evil", User: "root"},
		{VMID: "bot-1", Host: "-oProxyCommand=evil", User: "root"},
		{VMID: "bot-1", Host: "evil.example.com", User: "root"}, // names are not accepted, only IPs
		{VMID: "bot-1", Host: "172.16.1.2;reboot", User: "root"},
		{VMID: "bot-1", Host: "172.16.1.2", User: "-oProxyCommand=evil"},
		{VMID: "bot-1", Host: "172.16.1.2", User: "root;reboot"},
		{VMID: "bot-1", Host: "172.16.1.2", User: "ro ot"},
		{VMID: "bot-1", Host: "172.16.1.2", User: ""},
		{VMID: "bot-1", Host: "172.16.1.2", User: "root", ProxyJump: "-oProxyCommand=evil"},
		{VMID: "bot-1", Host: "172.16.1.2", User: "root", ProxyJump: "a@b;c"},
		{VMID: "../etc", Host: "172.16.1.2", User: "root"},
		{VMID: "bot-1/../../x", Host: "172.16.1.2", User: "root"},
	}
	for _, tg := range bad {
		if args, err := BuildSSHArgs(cfg, tg, "/s/k"); err == nil {
			t.Errorf("accepted hostile target %+v -> %v", tg, args)
		}
	}
}

func TestTTYDArgvIsFixedAndSecretFree(t *testing.T) {
	e := newTerm(t, nil)
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	var argv []string
	b, err := os.ReadFile(filepath.Join(e.dir, "ttyd-bot-1.sock.argv"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &argv)
	joined := " " + strings.Join(argv, " ") + " "
	for _, want := range []string{" --writable ", " --check-origin ", " --max-clients 2 ", " --interface " + filepath.Join(e.dir, "ttyd-bot-1.sock") + " "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	for _, bad := range []string{"--url-arg", " -a ", "--base-path", " -b ", "--once", "--credential", " sh ", "bash", tokA} {
		if strings.Contains(joined, bad) {
			t.Errorf("forbidden %q in %s", bad, joined)
		}
	}
	if argv[len(argv)-1] != "root@172.16.1.2" || !strings.Contains(joined, " /usr/bin/ssh ") {
		t.Fatalf("command must be the fixed ssh argv: %v", argv)
	}
}

func TestGatewayAuthOnEveryPath(t *testing.T) {
	e := newTerm(t, nil)
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	ok := "/t/" + tokA + "/"
	resp, body := e.get(t, "GET", ok, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "FAKE-TTYD") {
		t.Fatalf("page: %d %q", resp.StatusCode, body)
	}
	h := resp.Header
	if h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers: %v", h)
	}
	if strings.Contains(h.Get("Content-Security-Policy"), "http://") || strings.Contains(h.Get("Content-Security-Policy"), "*") {
		t.Fatalf("CSP must not allow third parties: %s", h.Get("Content-Security-Policy"))
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/token", nil); r.StatusCode != 200 {
		t.Fatalf("token resource: %d", r.StatusCode)
	}
	for name, p := range map[string]string{
		"wrong token":   "/t/" + tokB + "/",
		"short token":   "/t/AAAA/",
		"no token":      "/t/",
		"root":          "/",
		"other prefix":  "/x/" + tokA + "/",
		"unknown res":   "/t/" + tokA + "/admin",
		"traversal":     "/t/" + tokA + "/../" + tokB + "/",
		"wrong ws path": "/t/" + tokB + "/ws",
	} {
		if r, _ := e.get(t, "GET", p, nil); r.StatusCode != 404 {
			t.Errorf("%s: want 404 got %d", name, r.StatusCode)
		}
	}
	if r, _ := e.get(t, "GET", ok+"?arg=--evil&arg=rm", nil); r.StatusCode != 404 && r.StatusCode != 400 {
		t.Errorf("URL arguments must be rejected, got %d", r.StatusCode)
	}
	if r, _ := e.get(t, "POST", ok, nil); r.StatusCode != 405 {
		t.Errorf("POST: %d", r.StatusCode)
	}
	if r, _ := e.get(t, "GET", ok, map[string]string{"Host": "evil.test"}); r.StatusCode != 404 && r.StatusCode != 403 {
		t.Errorf("foreign Host: %d", r.StatusCode)
	}
	if r, _ := e.get(t, "GET", ok, map[string]string{"X-Forwarded-Host": pubHost, "Host": "evil.test"}); r.StatusCode == 200 {
		t.Error("X-Forwarded-Host must never be trusted")
	}
	if r, _ := e.get(t, "GET", ok, map[string]string{"Origin": "https://evil.test"}); r.StatusCode != 403 {
		t.Errorf("cross-origin page fetch: %d", r.StatusCode)
	}
}

func TestWebSocketOriginAndTokenChecks(t *testing.T) {
	e := newTerm(t, nil)
	e.m.Start(context.Background(), target("bot-1", tokA))
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
		want int
	}{
		"wrong token":    {"/t/" + tokB + "/ws", nil, 404},
		"bad origin":     {"/t/" + tokA + "/ws", map[string]string{"Origin": "https://evil.test"}, 403},
		"scheme origin":  {"/t/" + tokA + "/ws", map[string]string{"Origin": "http://" + pubHost}, 403},
		"suffix origin":  {"/t/" + tokA + "/ws", map[string]string{"Origin": "https://" + pubHost + ".evil.test"}, 403},
		"missing origin": {"/t/" + tokA + "/ws", map[string]string{"Origin": ""}, 403},
		"bad host":       {"/t/" + tokA + "/ws", map[string]string{"Host": "evil.test"}, -1},
		"query":          {"/t/" + tokA + "/ws?arg=x", nil, 400},
	} {
		resp, conn, _ := e.ws(t, c.path, c.hdr)
		if resp.StatusCode == 101 {
			t.Errorf("%s: upgrade must be refused", name)
		} else if c.want > 0 && resp.StatusCode != c.want {
			t.Errorf("%s: want %d got %d", name, c.want, resp.StatusCode)
		}
		conn.Close()
	}
	resp, conn, br := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer conn.Close()
	if resp.StatusCode != 101 {
		t.Fatalf("valid upgrade: %d", resp.StatusCode)
	}
	conn.Write([]byte("ping\n"))
	line, err := br.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo %q %v", line, err)
	}
}

func TestRevokeClosesOpenWebSocketKillsChildrenAndRotates(t *testing.T) {
	e := newTerm(t, nil)
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	resp, conn, br := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer conn.Close()
	if resp.StatusCode != 101 {
		t.Fatal("upgrade failed")
	}
	conn.Write([]byte("x\n"))
	br.ReadString('\n')
	pidb, err := os.ReadFile(filepath.Join(e.dir, "ttyd-bot-1.sock.child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(pidb))
	if !alive(pid) {
		t.Fatal("fake ssh child should be running")
	}
	kh := e.m.KnownHostsPath("bot-1")
	if st, err := os.Stat(kh); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("per-VM known_hosts: %v %v", st, err)
	}

	e.m.Revoke("bot-1")

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("open WebSocket must be disconnected on revoke")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("WebSocket was not closed by revoke (read timed out)")
	}
	waitDead(t, pid) // the setsid'd ssh stand-in, outside ttyd's process group
	if _, err := os.Stat(kh); !os.IsNotExist(err) {
		t.Fatal("known_hosts must be deleted on revoke")
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatalf("revoked link must 404, got %d", r.StatusCode)
	}

	// replacement VM: new token, new known_hosts; old link stays dead.
	if err := e.m.Start(context.Background(), target("bot-2", tokB)); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.get(t, "GET", "/t/"+tokB+"/", nil); r.StatusCode != 200 {
		t.Fatalf("new link: %d", r.StatusCode)
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatalf("old link after replacement: %d", r.StatusCode)
	}
	if e.m.KnownHostsPath("bot-2") == kh {
		t.Fatal("known_hosts must be per VM")
	}
}

func TestStartIsIdempotentAndBoundsSessions(t *testing.T) {
	e := newTerm(t, func(c *ManagerConfig) { c.MaxSessions = 1 })
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatalf("idempotent restart of same VM: %v", err)
	}
	if err := e.m.Start(context.Background(), target("bot-2", tokB)); err == nil {
		t.Fatal("a terminal beyond MaxSessions must be refused until another is revoked")
	}
	if err := e.m.Start(context.Background(), target("bot-1", tokB)); err == nil {
		t.Fatal("same VM with a different token must be refused (token rotation = new VM)")
	}
}

func TestMaxClientsBound(t *testing.T) {
	e := newTerm(t, func(c *ManagerConfig) { c.MaxClients = 1 })
	e.m.Start(context.Background(), target("bot-1", tokA))
	resp, c1, _ := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer c1.Close()
	if resp.StatusCode != 101 {
		t.Fatal("first client")
	}
	resp2, c2, _ := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer c2.Close()
	if resp2.StatusCode != 503 {
		t.Fatalf("second client must be refused with 503, got %d", resp2.StatusCode)
	}
	c1.Close()
	time.Sleep(200 * time.Millisecond)
	resp3, c3, _ := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer c3.Close()
	if resp3.StatusCode != 101 {
		t.Fatalf("slot must free after disconnect: %d", resp3.StatusCode)
	}
}

func TestCloseTerminatesBackendAndWebSockets(t *testing.T) {
	e := newTerm(t, nil)
	e.m.Start(context.Background(), target("bot-1", tokA))
	_, conn, br := e.ws(t, "/t/"+tokA+"/ws", nil)
	defer conn.Close()
	conn.Write([]byte("x\n"))
	br.ReadString('\n')
	pidb, _ := os.ReadFile(filepath.Join(e.dir, "ttyd-bot-1.sock.child"))
	pid, _ := strconv.Atoi(string(pidb))
	e.m.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("connection must close on shutdown")
	}
	waitDead(t, pid)
	if _, err := os.Stat(filepath.Join(e.dir, "ttyd-bot-1.sock")); err == nil {
		t.Fatal("socket must be removed on shutdown")
	}
	if err := e.m.Start(context.Background(), target("bot-3", tokB)); err == nil {
		t.Fatal("closed manager must not start terminals")
	}
}

func TestStartFailsLoudlyWhenBackendDies(t *testing.T) {
	e := newTerm(t, func(c *ManagerConfig) { c.TTYDPath = "/bin/false"; c.StartTimeout = 2 * time.Second })
	err := e.m.Start(context.Background(), target("bot-1", tokA))
	if err == nil || strings.Contains(err.Error(), tokA) {
		t.Fatalf("want stage error without secret, got %v", err)
	}
	if r, _ := e.get(t, "GET", "/t/"+tokA+"/", nil); r.StatusCode != 404 {
		t.Fatal("failed start must not leave a live link")
	}
}
