package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareImageRequiresFreshOutput(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "base.ext4")
	if err := os.WriteFile(dst, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareImage(context.Background(), "unused", dst, io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "keep" {
		t.Fatalf("output altered: %s, %v", b, err)
	}
}

func TestConfigureRefusesAnExistingDirectory(t *testing.T) {
	if _, err := Configure(context.Background(), Settings{Dir: t.TempDir(), Runner: fakeIP{}}); err == nil {
		t.Fatal("reused an existing directory")
	}
}

func TestLoadManifestRejectsTrailingData(t *testing.T) {
	src, err := os.ReadFile("testdata/artifacts.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, append(src, []byte(" {}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(p); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("err: %v", err)
	}
}
