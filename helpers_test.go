package main

import (
	"os"
	"strings"
	"testing"
)

func TestImageHelpersUseSDKRunner(t *testing.T) {
	for _, name := range []string{"run-image.sh", "verify-image.py"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "--no-api") || strings.Contains(string(b), "--config-file") {
			t.Errorf("%s still bypasses SDK", name)
		}
		if !strings.Contains(string(b), "-run-image") {
			t.Errorf("%s does not use SDK image runner", name)
		}
	}
}
