package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIExposesSDKImageRunner(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "image-builder")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	for _, flag := range []string{"-run-image", "-console-log"} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("SDK runner flag %s missing from help:\n%s", flag, out)
		}
	}
}
