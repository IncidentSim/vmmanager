#!/usr/bin/env bash
# Proves the public packages are importable from a SEPARATE module (local replace;
# a real `go get` works only after the module is published) and that the portable
# client packages cross-build for macOS. No network beyond the Go module cache.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mod="$(cd "$root" && go list -m)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/consumer"
cat > "$tmp/consumer/go.mod" <<EOF
module example.invalid/consumer

go 1.22

require $mod v0.0.0
replace $mod => $root
EOF
cat > "$tmp/consumer/main.go" <<EOF
package main

import (
	"context"
	"fmt"

	"$mod/vmlib"
	"$mod/vmlib/remote"
	"$mod/vmlib/vmlibtest"
)

func main() {
	var l vmlib.Lifecycle
	svc, err := vmlib.NewService(vmlibtest.New(1, "base"))
	if err != nil {
		panic(err)
	}
	l = svc
	vm, err := l.Create(context.Background(), vmlib.CreateRequest{ImageID: "base", Profile: "small"})
	if err != nil {
		panic(err)
	}
	var _ vmlib.Lifecycle = (*remote.FacadeClient)(nil)
	var _ vmlib.Backend = (*remote.BackendClient)(nil)
	fmt.Println("consumer ok", vm.State)
}
EOF
(cd "$tmp/consumer" && GOFLAGS=-mod=mod go run .)

# Portable clients must not need Linux/KVM.
for target in darwin/arm64 darwin/amd64 windows/amd64; do
  (cd "$root" && GOOS="${target%/*}" GOARCH="${target#*/}" CGO_ENABLED=0 \
    go build -o /dev/null ./vmlib ./vmlib/remote ./vmlib/vmlibtest ./cmd/vmctl)
  echo "cross-build ok: $target"
done
