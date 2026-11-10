#!/usr/bin/env bash
# Regenerate (or --check) protobuf/gRPC Go code with generators pinned in tools/go.mod.
# Needs network only to populate the Go module cache the first time. No sudo.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-generate}"
case "$mode" in generate|--check) ;; *) echo "usage: $0 [--check]" >&2; exit 2;; esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/bin"
mkdir -p "$bin"
(cd "$root/tools" && GOFLAGS=-mod=mod go build -o "$bin/" \
  github.com/bufbuild/buf/cmd/buf \
  google.golang.org/protobuf/cmd/protoc-gen-go \
  google.golang.org/grpc/cmd/protoc-gen-go-grpc)
export PATH="$bin:$PATH"

out="$tmp/out"
mkdir -p "$out"
(cd "$root/proto" && buf generate --template buf.gen.yaml -o "$out")

if [ "$mode" = "--check" ]; then
  if diff -r "$out/gen" "$root/vmlib/gen" >/dev/null; then
    echo "generated code is up to date"
  else
    diff -r "$out/gen" "$root/vmlib/gen" | head -40 >&2 || true
    echo "generated code is stale: run scripts/gen-proto.sh" >&2
    exit 1
  fi
else
  rm -rf "$root/vmlib/gen"
  mkdir -p "$root/vmlib"
  cp -r "$out/gen" "$root/vmlib/gen"
  echo "generated into vmlib/gen"
fi
