package main

import (
	"os"
	"strings"
	"testing"
)

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
