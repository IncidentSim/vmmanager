package repocheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestReleaseBuildsAndPublishesExampleBundle(t *testing.T) {
	_, steps, _ := releaseSteps(t)
	idx := func(sub string) int {
		return stepIndex(steps, func(s relStep) bool { return strings.Contains(s.Run, sub) })
	}
	chk, apt, bun := idx("make check"), idx("apt-get install"), idx("make bundle")
	rel := stepIndex(steps, func(s relStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	if !(chk >= 0 && chk < apt && apt < bun && bun < rel) {
		t.Fatalf("order must be check < apt < bundle < publish: %d %d %d %d", chk, apt, bun, rel)
	}
	if !strings.Contains(steps[apt].Run, "fakeroot squashfs-tools e2fsprogs") {
		t.Errorf("missing image build tools: %q", steps[apt].Run)
	}
	for _, i := range []int{apt, bun} {
		if len(steps[i].Env) != 0 {
			t.Errorf("step %q must not receive environment secrets: %v", steps[i].Name, steps[i].Env)
		}
		if strings.Contains(steps[i].Run, "secrets.") || strings.Contains(steps[i].Run, "GITHUB_TOKEN") {
			t.Errorf("step %q must not use credentials", steps[i].Name)
		}
	}
	// Unprivileged assembly: only apt uses sudo; the bundle runs as the runner user.
	if strings.Contains(steps[bun].Run, "sudo") {
		t.Error("bundle assembly must be unprivileged")
	}
	for _, want := range []string{"BUNDLE_WORK=", "$TMPDIR/", "sha256sum", "VMMANAGER_EXAMPLE_DIR="} {
		if !strings.Contains(steps[bun].Run, want) {
			t.Errorf("bundle step lacks %q", want)
		}
	}
	for i, s := range steps {
		if i != rel && s.Env["GITHUB_TOKEN"] != "" {
			t.Errorf("step %q must not hold GITHUB_TOKEN", s.Name)
		}
	}

	b, err := os.ReadFile(filepath.Join(root(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Release struct {
			Extra []struct {
				Glob string `yaml:"glob"`
			} `yaml:"extra_files"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range g.Release.Extra {
		got[e.Glob] = true
	}
	for _, f := range []string{"vmmanager-example.tar.gz", "vmmanager-example.tar.gz.sha256"} {
		if !got["{{ .Env.VMMANAGER_EXAMPLE_DIR }}/"+f] {
			t.Errorf("release.extra_files lacks %s: %v", f, g.Release.Extra)
		}
	}
}

// Branch CI must stay root/KVM-free and never download images or tools.
func TestCIDoesNotBuildExampleBundle(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"make bundle", "vmbundle", "apt-get", "VMMANAGER_EXAMPLE_DIR", "sudo"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("ci.yml must not contain %q", bad)
		}
	}
}
