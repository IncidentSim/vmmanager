package main

import (
	"os"
	"strings"
	"testing"
)

func TestHelpersUseOnlySocketAndPreflightCopies(t *testing.T) {
	for _, name := range []string{"run-image.sh", "verify-image.py", "demo-lifecycle.sh"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if strings.Contains(text, "systemctl") {
			t.Errorf("%s still controls/inspects services", name)
		}
		if !strings.Contains(text, "FC_SOCKET") || !strings.Contains(text, "-socket") {
			t.Errorf("%s missing socket option", name)
		}
		if name == "run-image.sh" {
			check := strings.Index(text, "--check-ready")
			copy := strings.Index(text, "cp --reflink")
			if check < 0 || check > copy {
				t.Error("shell copies disk before readiness check")
			}
		}
	}
}

func TestImageHelpersUseServiceRunner(t *testing.T) {
	for _, name := range []string{"run-image.sh", "verify-image.py"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "-run-image") {
			t.Errorf("%s missing runner", name)
		}
		for _, obsolete := range []string{"network.sh", "chown", "geteuid", "-firecracker", "-kernel", "--no-api", "--config-file"} {
			if strings.Contains(string(b), obsolete) {
				t.Errorf("%s still uses %s", name, obsolete)
			}
		}
	}
}
