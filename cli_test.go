package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIExposesServiceRunner(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "image-builder")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build %v %s", err, out)
	}
	out, err := exec.Command(binary, "-h").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "-run-image") {
		t.Fatal(string(out))
	}
	for _, flag := range []string{"-firecracker", "-console-log", "-kernel", "-tap"} {
		if strings.Contains(string(out), flag) {
			t.Errorf("obsolete %s", flag)
		}
	}
}
