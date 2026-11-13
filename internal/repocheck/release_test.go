package repocheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

type goreleaser struct {
	Version int `yaml:"version"`
	Builds  []struct {
		ID     string   `yaml:"id"`
		Main   string   `yaml:"main"`
		Binary string   `yaml:"binary"`
		Env    []string `yaml:"env"`
		Goos   []string `yaml:"goos"`
	} `yaml:"builds"`
	Archives []struct {
		ID    string   `yaml:"id"`
		IDs   []string `yaml:"ids"`
		Files []struct {
			Src string `yaml:"src"`
			Dst string `yaml:"dst"`
		} `yaml:"files"`
	} `yaml:"archives"`
	Checksum struct {
		Algorithm string `yaml:"algorithm"`
	} `yaml:"checksum"`
}

func loadGoreleaser(t *testing.T) goreleaser {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var g goreleaser
	if err := yaml.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// The host archive must be exactly the installer's input set, so release
// packaging can never drift from `make install` / `vmhost install`.
func TestReleaseArchiveMatchesInstallerInputs(t *testing.T) {
	g := loadGoreleaser(t)
	if g.Version != 2 || g.Checksum.Algorithm != "sha256" {
		t.Fatalf("version/checksum: %+v", g)
	}
	built := map[string]bool{}
	for _, b := range g.Builds {
		if !strings.HasPrefix(b.Main, "./cmd/") {
			t.Errorf("build %s: unexpected main %s", b.ID, b.Main)
		}
		if _, err := os.Stat(filepath.Join(root(t), b.Main)); err != nil {
			t.Errorf("build %s: %v", b.ID, err)
		}
		hasCgoOff := false
		for _, e := range b.Env {
			hasCgoOff = hasCgoOff || e == "CGO_ENABLED=0"
		}
		if !hasCgoOff {
			t.Errorf("build %s must be static (CGO_ENABLED=0): jail executables require it", b.ID)
		}
		built[b.ID] = true
	}
	// Makefile PRODUCT is the other source of truth.
	mk, err := os.ReadFile(filepath.Join(root(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var product []string
	for _, l := range strings.Split(string(mk), "\n") {
		if strings.HasPrefix(l, "PRODUCT :=") {
			product = strings.Fields(strings.TrimPrefix(l, "PRODUCT :="))
		}
	}
	if len(product) == 0 {
		t.Fatal("PRODUCT not found")
	}
	var host []string
	for _, a := range g.Archives {
		if a.ID == "host" {
			host = a.IDs
		}
	}
	if strings.Join(host, " ") != strings.Join(product, " ") {
		t.Errorf("host archive ids %v differ from Makefile PRODUCT %v", host, product)
	}
	for _, id := range host {
		if !built[id] {
			t.Errorf("archive references unknown build %s", id)
		}
	}
	for _, a := range g.Archives {
		for _, f := range a.Files {
			if f.Src == "docs/install.md" && f.Dst != "docs/install.md" {
				t.Error("GoReleaser dst must be the full document path, not docs/ (which creates a regular file named docs)")
			}
			if _, err := os.Stat(filepath.Join(root(t), f.Src)); err != nil {
				t.Errorf("archive %s ships missing file %s", a.ID, f.Src)
			}
		}
	}
}

func TestReleaseWorkflowPermissionsAndTrigger(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Name        string            `yaml:"name"`
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				ID   string            `yaml:"id"`
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
			Permissions map[string]string `yaml:"permissions"`
			Timeout     int               `yaml:"timeout-minutes"`
			RunsOn      string            `yaml:"runs-on"`
			Concurrency any               `yaml:"concurrency"`
		} `yaml:"jobs"`
		Concurrency any `yaml:"concurrency"`
	}
	if err := yaml.UnmarshalStrict(b, &w); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.On["push"]; !ok || len(w.On) != 2 {
		t.Fatalf("release must trigger only on tag pushes and manual dispatch: %v", w.On)
	}
	if _, ok := w.On["workflow_dispatch"]; !ok {
		t.Fatal("release must allow workflow_dispatch to retry an existing tag")
	}
	if !strings.Contains(string(b), `tags: ["v*"]`) {
		t.Error("release must be limited to v* tags")
	}
	if len(w.Permissions) != 1 || w.Permissions["contents"] != "write" {
		t.Errorf("permissions must be exactly contents: write, got %v", w.Permissions)
	}
	text := string(b)
	for _, want := range []string{"make check", "goreleaser/goreleaser-action@", "release --clean", "fetch-depth: 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow lacks %q", want)
		}
	}
	// make check must precede publishing
	if strings.Index(text, "make check") > strings.Index(text, "release --clean") {
		t.Error("tests must run before the release step")
	}
}

func TestCIPublishesNothing(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "--snapshot") || !strings.Contains(s, "--skip=publish") {
		t.Error("PR snapshot build must be --snapshot --skip=publish")
	}
	if strings.Contains(s, "GITHUB_TOKEN") || strings.Contains(s, "contents: write") {
		t.Error("ci.yml must not hold release credentials")
	}
}
