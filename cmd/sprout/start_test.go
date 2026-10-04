package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `start` boots what is already on record: an id with no state is "no such
// instance", never an empty directory conjured for it — creating one is `up`'s
// and `fork`'s privilege alone.
func TestStartForegroundNeverCreatesTheInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "startnostate"
	t.Cleanup(func() { removeSocketDir(id) })

	err := startForeground(&Identity{ID: id, Name: "feature"})
	if err == nil {
		t.Fatal("start of an instance with no state succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start error = %v, want it to unwrap to os.ErrNotExist", err)
	}
	dir := filepath.Join(root, "sprout", "instances", id)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("start created %s (stat err = %v)", dir, err)
	}
}

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
	writeHostManifest(t, bundle)
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
