package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Once nix GC reclaims the recorded build there is nothing left to boot from,
// so start sends the user back to `up` rather than into a runner failure.
func TestStartRejectsMissingBundle(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	id := "abc123def456"
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "gone-from-store")
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID:     id,
		Name:   "feature",
		Bundle: gone,
	}); err != nil {
		t.Fatal(err)
	}

	err := startForeground(&Identity{ID: id, Name: "feature"})
	if err == nil {
		t.Fatal("expected an error when the bundle is missing, got nil")
	}
	if !strings.Contains(err.Error(), "sprout up") {
		t.Errorf("error should point at `sprout up` to rebuild, got: %v", err)
	}
}

// `start` losing the boot race to another booter is a success, not an error.
func TestStartForegroundHandsOffToConcurrentBoot(t *testing.T) {
	root := shortStateRoot(t)
	const id = "startbootrace"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Version: manifestSchemaVersion}
	m.Guest.IP = "127.0.0.1"
	m.Guest.SSHUser = "sprout"
	if err := writeJSON(filepath.Join(bundle, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "webapp", KeySource: "directory", Bundle: bundle, GuestIP: "127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	holdLockAndServeLater(t, dir, 400*time.Millisecond)

	out := captureStdout(t, func() error {
		return startForeground(&Identity{ID: id, Name: "webapp"})
	})
	if !strings.Contains(out, "already running") {
		t.Errorf("start should have handed off to the concurrent boot, output:\n%s", out)
	}
}
