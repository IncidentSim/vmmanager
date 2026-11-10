package vmbot

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type composedEnv struct {
	*tenv
	api   *httptest.Server
	hits  atomic.Int32
	front *httptest.Server
}

func newComposed(t *testing.T) *composedEnv {
	t.Helper()
	e := &composedEnv{tenv: newTerm(t, nil)}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		w.Header().Set("X-Api-Path", r.URL.Path)
		io.WriteString(w, "API:"+r.Method+":"+r.URL.Path)
	})
	e.front = httptest.NewServer(ComposeGateway(e.m, api))
	t.Cleanup(e.front.Close)
	return e
}

func (e *composedEnv) do(t *testing.T, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.front.URL+path, strings.NewReader(""))
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

func TestComposedGatewayRoutesAPIAndTerminalOnTheSameOrigin(t *testing.T) {
	e := newComposed(t)
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	own := "https://" + pubHost
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
		code         int
		api          bool
	}{
		{"GET", "/v1/vms", nil, 200, true},
		{"POST", "/v1/vms", map[string]string{"Origin": own}, 200, true},
		{"DELETE", "/v1/vms/x", map[string]string{"Origin": own}, 200, true},
		{"GET", "/healthz", nil, 200, true},
		{"GET", "/docs", nil, 200, true},
		{"GET", "/docs/", nil, 200, true},
		{"GET", "/docs/assets/swagger-ui.css", nil, 200, true},
		{"GET", "/openapi.yaml", nil, 200, true},
		{"POST", "/v1/vms", map[string]string{"Origin": "https://evil.test"}, 403, false},
		{"POST", "/v1/vms", map[string]string{"Origin": "http://" + pubHost}, 403, false},
		{"GET", "/v1/vms", map[string]string{"Origin": "https://evil.test"}, 403, false},
		{"GET", "/v1/vms", map[string]string{"Host": "evil.test"}, 404, false},
		{"GET", "/v1/vms", map[string]string{"Host": pubHost + ".evil.test"}, 404, false},
		{"GET", "/healthz", map[string]string{"Host": "evil.test"}, 404, false},
		{"GET", "/docs", map[string]string{"Host": "127.0.0.1"}, 404, false},
		{"GET", "/", nil, 404, false},
		{"GET", "/v2/vms", nil, 404, false},
		{"GET", "/openapi.json", nil, 404, false},
		{"GET", "/healthz/", nil, 404, false},
		{"GET", "/docsx", nil, 404, false},
		{"GET", "/t", nil, 404, false},
		{"POST", "/t/" + tokA + "/", nil, 405, false}, // terminal paths stay GET/HEAD only
		{"GET", "/t/" + tokB + "/", nil, 404, false},
		{"GET", "/t/" + tokA + "/", nil, 200, false},
	} {
		before := e.hits.Load()
		resp, body := e.do(t, c.method, c.path, c.hdr)
		if resp.StatusCode != c.code {
			t.Errorf("%s %s %v: %d want %d (%q)", c.method, c.path, c.hdr, resp.StatusCode, c.code, body)
		}
		if got := e.hits.Load() != before; got != c.api {
			t.Errorf("%s %s %v: API reached=%v want %v", c.method, c.path, c.hdr, got, c.api)
		}
		for h := range resp.Header {
			if strings.HasPrefix(strings.ToLower(h), "access-control-") {
				t.Errorf("%s %s: no CORS headers may be emitted, got %s", c.method, c.path, h)
			}
		}
	}
	// A preflight is not served (no broad CORS): it never reaches the API.
	before := e.hits.Load()
	resp, _ := e.do(t, "OPTIONS", "/v1/vms", map[string]string{"Origin": own, "Access-Control-Request-Method": "POST"})
	if e.hits.Load() != before && resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("preflight must not grant cross-origin access")
	}
}

func TestComposedGatewayKeepsWebSocketAndNoShell(t *testing.T) {
	e := newComposed(t)
	if err := e.m.Start(context.Background(), target("bot-1", tokA)); err != nil {
		t.Fatal(err)
	}
	// WebSocket upgrade through the composed handler is still capability + Origin gated.
	via := &tenv{m: e.m, srv: e.front, dir: e.dir} // tenv.ws dials srv.Listener: here the composed front door
	ws := func(path string, hdr map[string]string) int {
		resp, conn, _ := via.ws(t, path, hdr)
		conn.Close()
		return resp.StatusCode
	}
	if c := ws("/t/"+tokA+"/ws", nil); c != 101 {
		t.Fatalf("valid terminal WS via composed gateway: %d", c)
	}
	if c := ws("/t/"+tokA+"/ws", map[string]string{"Origin": "https://evil.test"}); c == 101 {
		t.Fatal("bad Origin upgraded")
	}
	if c := ws("/t/"+tokB+"/ws", nil); c == 101 {
		t.Fatal("bad capability upgraded")
	}
	if c := ws("/t/"+tokA+"/ws", map[string]string{"Host": "evil.test"}); c == 101 {
		t.Fatal("bad Host upgraded")
	}
	// An API path can never be upgraded into a terminal.
	if c := ws("/v1/vms", nil); c == 101 {
		t.Fatal("API route upgraded")
	}
}

func TestServeGatewayHandlerServesComposedHandler(t *testing.T) {
	e := newTerm(t, nil)
	e.srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "API") })
	go func() { done <- ServeGatewayHandler(ctx, ln, "", "", e.m, ComposeGateway(e.m, api)) }()
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/healthz", nil)
	req.Host = pubHost
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "API" {
		t.Fatalf("%q", b)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
