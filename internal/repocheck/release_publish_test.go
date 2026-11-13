package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

type relStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func releaseSteps(t *testing.T) (map[string]any, []relStep, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		On          map[string]any `yaml:"on"`
		Concurrency struct {
			Group  string `yaml:"group"`
			Cancel bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			If    string    `yaml:"if"`
			Steps []relStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.Concurrency.Cancel {
		t.Error("release concurrency must not cancel in progress")
	}
	if w.Jobs["release"].If != "github.event_name == 'push' || github.ref == 'refs/heads/main'" {
		t.Errorf("manual publication must be limited to main: %q", w.Jobs["release"].If)
	}
	return w.On, w.Jobs["release"].Steps, w.Concurrency.Group
}

func stepIndex(steps []relStep, pred func(relStep) bool) int {
	for i, s := range steps {
		if pred(s) {
			return i
		}
	}
	return -1
}

func TestReleaseWorkflowTagPlumbingAndOrdering(t *testing.T) {
	on, steps, group := releaseSteps(t)
	dispatch, _ := on["workflow_dispatch"].(map[any]any)
	in, _ := dispatch["inputs"].(map[any]any)
	tagIn, _ := in["tag"].(map[any]any)
	if tagIn == nil || tagIn["required"] != true || tagIn["type"] != "string" {
		t.Errorf("workflow_dispatch needs a required string tag input: %v", in)
	}
	// Push and manual runs of the same tag must share one concurrency group.
	if group != "release-${{ github.event_name == 'workflow_dispatch' && inputs.tag || github.ref_name }}" {
		t.Errorf("concurrency group: %q", group)
	}
	val := stepIndex(steps, func(s relStep) bool { return s.ID == "tag" })
	co := stepIndex(steps, func(s relStep) bool { return strings.HasPrefix(s.Uses, "actions/checkout@") })
	goS := stepIndex(steps, func(s relStep) bool { return strings.HasPrefix(s.Uses, "actions/setup-go@") })
	scr := stepIndex(steps, func(s relStep) bool { return strings.Contains(s.Run, `"TMPDIR=$scratch" >> "$GITHUB_ENV"`) })
	chk := stepIndex(steps, func(s relStep) bool { return strings.Contains(s.Run, "make check") })
	rel := stepIndex(steps, func(s relStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	if !(val == 0 && val < co && co < goS && co < scr && scr < chk && chk < rel) {
		t.Fatalf("bad order validate=%d checkout=%d setup-go=%d scratch=%d check=%d publish=%d", val, co, goS, scr, chk, rel)
	}
	for _, s := range steps {
		if strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates expressions into shell; use env", s.Name)
		}
	}
	const out = "${{ steps.tag.outputs.tag }}"
	if steps[co].With["ref"] != "refs/tags/"+out || steps[co].With["fetch-depth"] != 0 || steps[co].With["persist-credentials"] != false {
		t.Errorf("checkout must use the validated tag ref: %v", steps[co].With)
	}
	if steps[rel].Env["GORELEASER_CURRENT_TAG"] != out {
		t.Errorf("publisher tag must equal the checked-out tag: %v", steps[rel].Env)
	}
	if steps[val].Env["INPUT_TAG"] != "${{ inputs.tag }}" {
		t.Errorf("inputs must reach the validator through env: %v", steps[val].Env)
	}
}

func runSnippet(script string, env map[string]string, dir string) (string, error) {
	cmd := exec.Command("bash", "-eo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseTagValidationSnippet(t *testing.T) {
	_, steps, _ := releaseSteps(t)
	script := steps[stepIndex(steps, func(s relStep) bool { return s.ID == "tag" })].Run
	dir := t.TempDir()
	gh := filepath.Join(dir, "out")
	good := []string{"v0.1.0", "v1.20.300", "v1.0.0-rc.1", "v2.0.0-alpha", "v1.0.0-0.3.7"}
	bad := []string{"", "0.1.0", "v1", "v1.2", "v01.2.3", "v1.2.3.4", "v1.2.3+meta", "v1.2.3-", "v1.2.3-01",
		"v1.2.3\nv9.9.9", "v1.2.3\n", "\nv1.2.3", "v1.2.3\r", "v1.2.3 ", " v1.2.3",
		"v1.2.3$(touch pwned)", "v1.2.3;touch pwned", "v1.2.3`touch pwned`", "refs/tags/v1.2.3",
		"main", "v1.2.3\ntag=v6.6.6", "../v1.2.3", "v1.2.3-rc..1"}
	run := func(event, input, ref string) (string, error) {
		if err := os.WriteFile(gh, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := runSnippet(script, map[string]string{"EVENT_NAME": event, "INPUT_TAG": input, "REF_NAME": ref, "GITHUB_OUTPUT": gh}, dir)
		b, _ := os.ReadFile(gh)
		return string(b), err
	}
	for _, g := range good {
		got, err := run("workflow_dispatch", g, "main")
		if err != nil || got != "tag="+g+"\n" {
			t.Errorf("manual %q: out=%q err=%v", g, got, err)
		}
		got, err = run("push", "ignored", g)
		if err != nil || got != "tag="+g+"\n" {
			t.Errorf("push %q: out=%q err=%v", g, got, err)
		}
	}
	for _, b := range bad {
		if got, err := run("workflow_dispatch", b, "main"); err == nil || got != "" {
			t.Errorf("manual %q must be rejected without output: out=%q err=%v", b, got, err)
		}
		if got, err := run("push", "v1.0.0", b); err == nil || got != "" {
			t.Errorf("push ref %q must be rejected without output: out=%q err=%v", b, got, err)
		}
	}
	// Manual runs take the input, never the (branch) ref.
	if got, err := run("workflow_dispatch", "v1.0.0", "v9.9.9"); err != nil || got != "tag=v1.0.0\n" {
		t.Errorf("manual run must use the input: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("hostile tag was executed by the shell")
	}
}

func TestReleaseScratchSnippetIsPrivateHomeTMPDIR(t *testing.T) {
	_, steps, _ := releaseSteps(t)
	script := steps[stepIndex(steps, func(s relStep) bool { return strings.Contains(s.Run, "TMPDIR=") })].Run
	home := t.TempDir()
	ghEnv := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(ghEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSnippet(script, map[string]string{"HOME": home, "GITHUB_ENV": ghEnv}, home); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ := os.ReadFile(ghEnv)
	line := strings.TrimSuffix(string(b), "\n")
	scratch, ok := strings.CutPrefix(line, "TMPDIR=")
	if !ok || strings.Contains(scratch, "\n") || filepath.Dir(scratch) != home || !strings.HasPrefix(filepath.Base(scratch), ".vm-go-check-") {
		t.Fatalf("scratch must be a direct private child of $HOME: %q", line)
	}
	fi, err := os.Stat(scratch)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("scratch must be a 0700 directory: %v %v", fi, err)
	}
}
