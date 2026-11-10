package controlapi

import (
	"embed"
	"net/http"
	"strings"

	"github.com/petruekhin/vibe-firecracker/docs"
)

// Swagger UI is vendored from the public swagger-ui-dist package (see
// swaggerui/PROVENANCE.md) and embedded, so the browser never needs a CDN.
//
//go:embed swaggerui/swagger-ui.css swaggerui/swagger-ui-bundle.js swaggerui/swagger-ui-bundle.js.LICENSE.txt swaggerui/swagger-initializer.js swaggerui/favicon-32x32.png swaggerui/LICENSE swaggerui/NOTICE
var swaggerAssets embed.FS

// assetTypes is an allowlist: only these names are ever served, so no path
// (traversal, source maps, provenance notes) can reach other embedded files.
var assetTypes = map[string]string{
	"swagger-ui.css":                   "text/css; charset=utf-8",
	"swagger-ui-bundle.js":             "text/javascript; charset=utf-8",
	"swagger-initializer.js":           "text/javascript; charset=utf-8",
	"swagger-ui-bundle.js.LICENSE.txt": "text/plain; charset=utf-8",
	"favicon-32x32.png":                "image/png",
	"LICENSE":                          "text/plain; charset=utf-8",
	"NOTICE":                           "text/plain; charset=utf-8",
}

const swaggerPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Control API</title>
<link rel="icon" type="image/png" href="/docs/assets/favicon-32x32.png">
<link rel="stylesheet" href="/docs/assets/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="/docs/assets/swagger-ui-bundle.js"></script>
<script src="/docs/assets/swagger-initializer.js"></script>
</body>
</html>
`

// The UI only talks to its own origin. Swagger UI injects inline style
// attributes, so style-src needs 'unsafe-inline'; scripts do not.
const docsCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func staticHeaders(w http.ResponseWriter, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", docsCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// serveDocs serves the public, read-only documentation routes. It reports
// whether the path belongs to them (it then wrote the response).
func serveDocs(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	var body []byte
	var ctype string
	switch {
	case p == "/docs" || p == "/docs/":
		body, ctype = []byte(swaggerPage), "text/html; charset=utf-8"
	case p == "/openapi.yaml":
		body, ctype = docs.OpenAPI, "application/yaml; charset=utf-8"
	case strings.HasPrefix(p, "/docs/assets/"):
		name := strings.TrimPrefix(p, "/docs/assets/")
		t, ok := assetTypes[name]
		if !ok {
			return false
		}
		b, err := swaggerAssets.ReadFile("swaggerui/" + name)
		if err != nil {
			return false
		}
		body, ctype = b, t
	default:
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		problem(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return true
	}
	staticHeaders(w, ctype)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
	return true
}
