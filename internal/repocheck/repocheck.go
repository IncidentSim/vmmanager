// Package repocheck holds repository-level contract checks that used to be
// shell scripts: the VM plane has no RPC transport, and the public SDK
// packages are consumable from a separate module and cross-build for client
// platforms. The checks run as ordinary `go test`.
package repocheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

var rpcPrefixes = []string{"google.golang.org/grpc", "google.golang.org/protobuf", "google.golang.org/genproto", "github.com/golang/protobuf"}

func isRPC(s string) bool {
	for _, p := range rpcPrefixes {
		if s == p || strings.HasPrefix(s, p+"/") {
			return true
		}
	}
	return false
}

// Package is the subset of `go list -json` the check needs.
type Package struct {
	ImportPath                         string
	Imports, TestImports, XTestImports []string
}

// Require is one go.mod requirement.
type Require struct {
	Path     string
	Indirect bool
}

// Evaluate returns every violation of the no-application-RPC rule. Transitive
// module metadata is allowed to remain; source imports, linked binary
// dependencies and direct requirements are not.
func Evaluate(pkgs []Package, binaryDeps []string, requires []Require, existing []string) []string {
	var errs []string
	for _, p := range pkgs {
		for field, names := range map[string][]string{"Imports": p.Imports, "TestImports": p.TestImports, "XTestImports": p.XTestImports} {
			for _, n := range names {
				if isRPC(n) {
					errs = append(errs, fmt.Sprintf("%s: %s imports %s", p.ImportPath, field, n))
				}
			}
		}
	}
	for _, gone := range existing {
		errs = append(errs, "removed application RPC tree exists: "+gone)
	}
	for _, r := range requires {
		if isRPC(r.Path) && !r.Indirect {
			errs = append(errs, "direct RPC requirement: "+r.Path)
		}
	}
	for _, n := range binaryDeps {
		if isRPC(n) {
			errs = append(errs, "API/CLI binary dependency graph includes "+n)
		}
	}
	return errs
}

// Go runs the go tool in dir and returns stdout; any failure (including a
// broken package graph) is an error, never "no matches".
func Go(dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, errb.String())
	}
	return out.Bytes(), nil
}

// DecodeStream decodes concatenated JSON values (the `go list -json` format).
func DecodeStream[T any](b []byte) ([]T, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []T
	for {
		var v T
		err := dec.Decode(&v)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}
