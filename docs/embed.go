// Package docs embeds the OpenAPI contract so vmapi serves exactly the file kept
// in this directory (single source of truth for Swagger UI and readers).
package docs

import _ "embed"

// OpenAPI is docs/control-api-openapi.yaml.
//
//go:embed control-api-openapi.yaml
var OpenAPI []byte
