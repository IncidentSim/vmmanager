package repocheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	out, err := Go(".", nil, "list", "-m", "-f", "{{.Dir}}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestEvaluateRules(t *testing.T) {
	transitive := Evaluate([]Package{{ImportPath: "example/app"}}, []string{"net/http"}, []Require{{Path: "google.golang.org/grpc", Indirect: true}}, nil)
	if len(transitive) != 0 {
		t.Fatalf("transitive module metadata must not fail: %v", transitive)
	}
	cases := map[string][]string{
		"test import": Evaluate([]Package{{ImportPath: "example/app", TestImports: []string{"google.golang.org/grpc/status"}}}, nil, nil, nil),
		"linked":      Evaluate(nil, []string{"google.golang.org/protobuf/proto"}, nil, nil),
		"direct":      Evaluate(nil, nil, []Require{{Path: "google.golang.org/grpc"}}, nil),
		"old tree":    Evaluate(nil, nil, nil, []string{"proto"}),
	}
	for name, errs := range cases {
		if len(errs) == 0 {
			t.Errorf("%s was not rejected", name)
		}
	}
}

func TestBrokenGoListIsNotHiddenAsEmptyMatches(t *testing.T) {
	if _, err := Go(t.TempDir(), nil, "list", "-json", "./..."); err == nil {
		t.Fatal("go list outside a module must fail loudly")
	}
}

// TestNoApplicationRPC: the VM plane has no RPC transport; no application
// package may import gRPC/protobuf and the API/CLI binaries must not link them.
func TestNoApplicationRPC(t *testing.T) {
	r := root(t)
	pk, err := Go(r, nil, "list", "-json", "./...")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := DecodeStream[Package](pk)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := Go(r, nil, "list", "-deps", "./cmd/vmapi", "./cmd/vmctl")
	if err != nil {
		t.Fatal(err)
	}
	mod, err := Go(r, nil, "mod", "edit", "-json")
	if err != nil {
		t.Fatal(err)
	}
	var gomod struct{ Require []Require }
	if err := json.Unmarshal(mod, &gomod); err != nil {
		t.Fatal(err)
	}
	var existing []string
	for _, gone := range []string{"proto", "vmlib/gen", "vmlib/remote", "cmd/vmd", "scripts/gen-proto.sh", "tools"} {
		if _, err := os.Stat(filepath.Join(r, gone)); err == nil {
			existing = append(existing, gone)
		}
	}
	if errs := Evaluate(pkgs, strings.Fields(string(deps)), gomod.Require, existing); len(errs) > 0 {
		t.Fatal(strings.Join(errs, "\n"))
	}
}

// TestPublicSDKIsConsumableFromAnotherModule proves vmlib, vmlibtest and
// controlclient work from a SEPARATE module (local replace; a real `go get`
// works only after publication) and that the portable client packages
// cross-build for macOS/Windows without Linux/KVM.
func TestPublicSDKIsConsumableFromAnotherModule(t *testing.T) {
	if testing.Short() {
		t.Skip("consumer module build skipped in -short mode")
	}
	r := root(t)
	mod, err := Go(r, nil, "list", "-m")
	if err != nil {
		t.Fatal(err)
	}
	modPath := strings.TrimSpace(string(mod))
	ver, err := Go(r, nil, "list", "-m", "-f", "{{.GoVersion}}")
	if err != nil {
		t.Fatal(err)
	}
	goVersion := strings.TrimSpace(string(ver))
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.invalid/consumer\n\ngo "+goVersion+"\n\nrequire "+modPath+" v0.0.0\nreplace "+modPath+" => "+r+"\n")
	write("main.go", `package main

import (
	"context"
	"fmt"

	"`+modPath+`/controlclient"
	"`+modPath+`/vmlib"
	"`+modPath+`/vmlib/vmlibtest"
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
	if _, err := controlclient.New("http://127.0.0.1:1", "placeholder-token", nil); err != nil {
		panic(err)
	}
	fmt.Println("consumer ok", vm.State)
}
`)
	out, err := Go(dir, []string{"GOFLAGS=-mod=mod"}, "run", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "consumer ok") {
		t.Fatalf("output %q", out)
	}
	for _, target := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}} {
		if _, err := Go(r, []string{"GOOS=" + target[0], "GOARCH=" + target[1], "CGO_ENABLED=0"}, "build", "-o", os.DevNull, "./vmlib", "./vmlib/vmlibtest", "./controlclient", "./cmd/vmctl"); err != nil {
			t.Errorf("cross-build %s/%s: %v", target[0], target[1], err)
		}
	}
}
