package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The real E2E must fail, not skip, when prerequisites are absent.
func TestRunFailsLoudlyWithoutPrerequisites(t *testing.T) {
	err := run(context.Background(), []string{"run", "--dir", t.TempDir(), "--storage-root", t.TempDir(), "--pool-conf", "/nonexistent/pool.conf"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "missing prerequisites") {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"pool.conf", "e2e-ci install-image", "e2e-ci configure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report lacks %q:\n%v", want, err)
		}
	}
}

// Unpinned fixtures must fail before any network access.
func TestFetchRefusesUnpinnedManifestOffline(t *testing.T) {
	manifest := t.TempDir() + "/manifest.json"
	if err := writeFile(manifest, `{"artifacts":[{"name":"bad","url":"https://example.invalid/artifact","sha256":"PIN_REQUIRED","bytes":10}]}`); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"fetch", "--manifest", manifest, "--out", t.TempDir()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned manifest not refused: %v", err)
	}
}

func TestVerifySuiteRequiresActualLifecyclePasses(t *testing.T) {
	for _, mode := range []string{"pass", "skip", "missing", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			var b bytes.Buffer
			for i, name := range requiredTests {
				if mode == "missing" && i == 0 {
					continue
				}
				action := "pass"
				if mode == "skip" && i == 0 {
					action = "skip"
				}
				if err := json.NewEncoder(&b).Encode(map[string]string{"Action": action, "Test": name}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "malformed" {
				b.WriteString("{")
			}
			err := verifySuite(&b, io.Discard)
			if (err == nil) != (mode == "pass") {
				t.Fatalf("mode %s: %v", mode, err)
			}
		})
	}
}

func TestWaitHealthyFailsImmediatelyOnProcessExit(t *testing.T) {
	exited := make(chan error, 1)
	exited <- nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitHealthy(ctx, &http.Client{}, "https://127.0.0.1:1", exited); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("err: %v", err)
	}
}

func TestInstallImageRejectsEscapingID(t *testing.T) {
	if err := installImage("unused", t.TempDir(), "../bad", io.Discard); err == nil || !strings.Contains(err.Error(), "invalid image ID") {
		t.Fatalf("err: %v", err)
	}
}

func TestInstallImageNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/rootfs"
	if err := writeFile(src, "img"); err != nil {
		t.Fatal(err)
	}
	if err := installImage(src, dir, "base", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := installImage(src, dir, "base", io.Discard); err == nil {
		t.Fatal("overwrote an existing image")
	}
}
