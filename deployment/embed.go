// Package deployment embeds the unit templates and example configuration that
// `vmhost install` renders and installs, so the installer binary is
// self-contained and always matches the sources reviewed in this repository.
package deployment

import "embed"

// Files holds the template and example trees.
//
//go:embed multivm/*.slice multivm/*.service multivm/*.socket multivm/*.conf multivm/*.example multivm/four-slot vmapi vmbot local/*
var Files embed.FS
