package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
