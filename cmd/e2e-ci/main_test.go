package main

import (
	"context"
	"io"
	"strings"
	"testing"
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

// With the shipped manifest unpinned, fetch must refuse before any network access.
func TestFetchRefusesUnpinnedManifestOffline(t *testing.T) {
	err := run(context.Background(), []string{"fetch", "--manifest", "../../e2e/ci/artifacts.json", "--out", t.TempDir()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Skipf("manifest is pinned (err=%v); fetch would use the network", err)
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
