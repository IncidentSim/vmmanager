package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/docs"
	"gopkg.in/yaml.v2"
)

func getDoc(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func apiHandler(t *testing.T) http.Handler {
	t.Helper()
	return newJWT(t, []string{"alice_example"}, nil).srv.Config.Handler
}

func TestSwaggerUIIsSelfHostedAndPublic(t *testing.T) {
	h := apiHandler(t)
	w := getDoc(t, h, "GET", "/docs")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/docs: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	page := w.Body.String()
	for _, want := range []string{"/docs/assets/swagger-ui.css", "/docs/assets/swagger-ui-bundle.js", "/docs/assets/swagger-initializer.js", `id="swagger-ui"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"http://", "https://", "unpkg", "cdn", "jsdelivr", "<script>", "onclick"} {
		if strings.Contains(strings.ToLower(page), bad) {
			t.Errorf("page must not contain %q (no CDN, no inline script)", bad)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP too weak: %s", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("headers: %v", w.Header())
	}
	if w2 := getDoc(t, h, "GET", "/docs/"); w2.Code != 200 || w2.Body.String() != page {
		t.Errorf("/docs/: %d", w2.Code)
	}
	if w3 := getDoc(t, h, "HEAD", "/docs"); w3.Code != 200 || w3.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", w3.Code, w3.Body.Len())
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if w := getDoc(t, h, m, "/docs"); w.Code != 405 {
			t.Errorf("%s /docs: %d", m, w.Code)
		}
	}
}

func TestSwaggerInitializerConfiguresTryItOutSafely(t *testing.T) {
	h := apiHandler(t)
	w := getDoc(t, h, "GET", "/docs/assets/swagger-initializer.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	js := w.Body.String()
	for _, want := range []string{`url: "/openapi.yaml"`, "SwaggerUIBundle", "validatorUrl: null", "tryItOutEnabled: true"} {
		if !strings.Contains(js, want) {
			t.Errorf("initializer lacks %q", want)
		}
	}
	for _, bad := range []string{"persistAuthorization: true", "http://", "https://", "Bearer ey", "token"} {
		if strings.Contains(js, bad) {
			t.Errorf("initializer must not contain %q", bad)
		}
	}
}

func TestSwaggerAssetsMatchPinnedUpstreamChecksums(t *testing.T) {
	prov, err := os.ReadFile("swaggerui/PROVENANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	rows := regexp.MustCompile("\\| ([A-Za-z0-9.\\-]+) \\| ([0-9a-f]{64}) \\|").FindAllStringSubmatch(string(prov), -1)
	if len(rows) < 5 {
		t.Fatalf("provenance table incomplete: %d rows", len(rows))
	}
	h := apiHandler(t)
	for _, r := range rows {
		name, want := r[1], r[2]
		raw, err := os.ReadFile("swaggerui/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s: checksum differs from upstream provenance", name)
		}
		if name == "LICENSE" || strings.HasSuffix(name, ".LICENSE.txt") {
			continue
		}
		w := getDoc(t, h, "GET", "/docs/assets/"+name)
		if w.Code != 200 {
			t.Errorf("%s: %d", name, w.Code)
			continue
		}
		if got := sha256.Sum256(w.Body.Bytes()); hex.EncodeToString(got[:]) != want {
			t.Errorf("%s: served bytes differ from vendored file", name)
		}
	}
	for name, ct := range map[string]string{"swagger-ui.css": "text/css", "swagger-ui-bundle.js": "javascript", "favicon-32x32.png": "image/png"} {
		if got := getDoc(t, h, "GET", "/docs/assets/"+name).Header().Get("Content-Type"); !strings.Contains(got, ct) {
			t.Errorf("%s content type %q", name, got)
		}
	}
	if w := getDoc(t, h, "GET", "/docs/assets/LICENSE"); w.Code != 200 {
		t.Errorf("upstream license must be served alongside assets: %d", w.Code)
	}
}

func TestSwaggerRejectsUnknownAndTraversalPaths(t *testing.T) {
	h := apiHandler(t)
	for _, p := range []string{"/docs/assets/", "/docs/assets", "/docs/assets/nope.js", "/docs/assets/swagger-ui-bundle.js.map",
		"/docs/assets/../openapi.yaml", "/docs/assets/%2e%2e/x", "/docs/assets/..%2fPROVENANCE.md", "/docs/assets/PROVENANCE.md",
		"/docs/assets/sub/swagger-ui.css", "/docs/other", "/docs/index.html", "/openapi.json", "/openapi.yaml/"} {
		if w := getDoc(t, h, "GET", p); w.Code != 404 && w.Code != 401 && w.Code != 301 && w.Code != 400 {
			t.Errorf("%s: %d", p, w.Code)
		} else if w.Code == 200 {
			t.Errorf("%s served", p)
		}
	}
	// API paths still require authentication.
	if w := getDoc(t, h, "GET", "/v1/vms"); w.Code != 401 {
		t.Errorf("/v1/vms without token: %d", w.Code)
	}
}

func TestOpenAPISpecIsServedAndDescribesJWTAndSameOrigin(t *testing.T) {
	h := apiHandler(t)
	w := getDoc(t, h, "GET", "/openapi.yaml")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var deployed map[string]interface{}
	if err := yaml.UnmarshalStrict(w.Body.Bytes(), &deployed); err != nil {
		t.Fatal(err)
	}
	spec := w.Body.String()
	for _, want := range []string{"bearerFormat: JWT", "servers:\n  - url: /\n", "RS256", "tg_username", "28800"} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %q", want)
		}
	}
	for _, bad := range []string{"127.0.0.1", "petruekhin", "eyJ"} {
		if strings.Contains(spec, bad) {
			t.Errorf("spec must not contain %q", bad)
		}
	}
	// No operation but healthz and docs is public.
	if got := strings.Count(spec, "security: []"); got != 1 {
		t.Errorf("exactly the health endpoint is public, found %d", got)
	}
}

func TestOpenAPINameConstraintsMatchLabelValidation(t *testing.T) {
	var parsed map[string]interface{}
	if err := yaml.UnmarshalStrict(docs.OpenAPI, &parsed); err != nil {
		t.Fatalf("invalid OpenAPI YAML: %v", err)
	}
	if parsed["openapi"] != "3.0.3" {
		t.Fatalf("unexpected OpenAPI version: %v", parsed["openapi"])
	}
	spec := string(docs.OpenAPI)
	// vmbot.ValidateLabel: ^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$, no "bot-" prefix,
	// exact match (no trimming or case folding), 1-20 characters.
	const pattern = `pattern: '^(?!bot-)[a-z]([a-z0-9-]{0,18}[a-z0-9])?$'`
	for _, schema := range []string{"CreateVM", "Name"} {
		m := regexp.MustCompile(`(?s)\n    ` + schema + `:\n(.*?)\n    [A-Za-z]+:\n`).FindStringSubmatch(spec)
		if m == nil {
			t.Fatalf("schema %s not found", schema)
		}
		i := strings.Index(m[1], "        name:")
		if i < 0 {
			t.Fatalf("%s.name not found", schema)
		}
		prop := m[1][i:]
		if loc := regexp.MustCompile(`\n {8}[^ ]`).FindStringIndex(prop); loc != nil {
			prop = prop[:loc[0]]
		}
		minLength, expectedPattern := "minLength: 1", pattern
		if schema == "CreateVM" {
			minLength = "minLength: 0"
			expectedPattern = `pattern: '^(?!bot-)([a-z]([a-z0-9-]{0,18}[a-z0-9])?)?$'`
		}
		for _, want := range []string{minLength, "maxLength: 20", expectedPattern} {
			if !strings.Contains(prop, want) {
				t.Errorf("%s.name lacks %q: %s", schema, want, prop)
			}
		}
	}
}
