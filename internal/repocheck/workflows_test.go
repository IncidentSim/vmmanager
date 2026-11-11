package repocheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

type workflow struct {
	Name        string            `yaml:"name"`
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group  string `yaml:"group"`
		Cancel bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Name    string            `yaml:"name"`
		If      string            `yaml:"if"`
		RunsOn  any               `yaml:"runs-on"`
		Timeout int               `yaml:"timeout-minutes"`
		Perms   map[string]string `yaml:"permissions"`
		Env     map[string]string `yaml:"env"`
		Steps   []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]any    `yaml:"with"`
			If   string            `yaml:"if"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func load(t *testing.T, name string) workflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.UnmarshalStrict(b, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w
}

func TestWorkflowsHaveMinimalSafeContract(t *testing.T) {
	for _, name := range []string{"ci.yml", "e2e-kvm.yml"} {
		w := load(t, name)
		if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
			t.Errorf("%s: top-level permissions must be exactly contents: read, got %v", name, w.Permissions)
		}
		if !w.Concurrency.Cancel || !strings.Contains(w.Concurrency.Group, "github.ref") {
			t.Errorf("%s: cancellation by ref required: %+v", name, w.Concurrency)
		}
		if _, ok := w.On["pull_request"]; !ok {
			t.Errorf("%s: must run on pull_request", name)
		}
		if _, ok := w.On["push"]; !ok {
			t.Errorf("%s: must run on push", name)
		}
		for id, job := range w.Jobs {
			if job.Timeout <= 0 {
				t.Errorf("%s/%s: timeout-minutes required", name, id)
			}
			for k, v := range job.Perms {
				if v != "read" && v != "none" {
					t.Errorf("%s/%s: permission %s=%s exceeds read", name, id, k, v)
				}
			}
			var checkout, goSetup bool
			for _, s := range job.Steps {
				if strings.HasPrefix(s.Uses, "actions/checkout@") {
					checkout = true
					if s.With["persist-credentials"] != false {
						t.Errorf("%s/%s: checkout must not persist credentials", name, id)
					}
				}
				if strings.HasPrefix(s.Uses, "actions/setup-go@") {
					goSetup = true
					if s.With["go-version-file"] != "go.mod" {
						t.Errorf("%s/%s: Go must be pinned through go.mod", name, id)
					}
				}
				if strings.Contains(s.Run, "${{ github.event") || strings.Contains(s.Run, "${{ github.head_ref") {
					t.Errorf("%s/%s: untrusted event data interpolated into a shell step", name, id)
				}
			}
			if !checkout || !goSetup {
				t.Errorf("%s/%s: needs checkout and setup-go", name, id)
			}
		}
	}
}

func TestRootFreeJobRunsTheGoChecks(t *testing.T) {
	w := load(t, "ci.yml")
	var runs []string
	for _, s := range w.Jobs["go"].Steps {
		runs = append(runs, s.Run)
	}
	all := strings.Join(runs, "\n")
	for _, want := range []string{"make check", "make control-smoke", "--require-systemd-analyze"} {
		if !strings.Contains(all, want) {
			t.Errorf("ci job lacks %q", want)
		}
	}
	if strings.Contains(all, "sudo") {
		t.Error("the root-free job must not use sudo")
	}
}

// The real E2E job must provision through the installer, use verified
// artifacts, fail without KVM, run the e2e target and never use secrets.
func TestKVMJobIsARealE2EAndGatesSelfHostedRunners(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/e2e-kvm.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "secrets.") {
		t.Error("the E2E job must not use secrets")
	}
	w := load(t, "e2e-kvm.yml")
	job := w.Jobs["e2e"]
	for _, want := range []string{"head.repo.full_name == github.repository", "vars.E2E_RUNS_ON == ''"} {
		if !strings.Contains(job.If, want) {
			t.Errorf("fork/self-hosted gate lacks %q", want)
		}
	}
	var runs []string
	for _, s := range job.Steps {
		runs = append(runs, s.Run)
	}
	all := strings.Join(runs, "\n")
	for _, want := range []string{"/dev/kvm", "exit 1", "e2e-ci fetch", "make install", "vmhost setup --apply", "vmhost enable --apply", "make e2e-kvm"} {
		if !strings.Contains(all, want) {
			t.Errorf("e2e job lacks %q", want)
		}
	}
	for _, bad := range []string{"continue-on-error", "|| true # skip"} {
		if strings.Contains(text, bad) && bad == "continue-on-error" {
			t.Error("the E2E job must not ignore failures")
		}
	}
}
