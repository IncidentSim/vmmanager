package e2e

import "github.com/petruekhin/vibe-firecracker/internal/fixture"

// The artifact download and image preparation live in internal/fixture (shared
// with the example bundle builder); the suite keeps using the short names.
type (
	Artifact = fixture.Artifact
	Manifest = fixture.Manifest
)

var (
	LoadManifest = fixture.LoadManifest
	Fetch        = fixture.Fetch
	PrepareImage = fixture.PrepareImage
)
