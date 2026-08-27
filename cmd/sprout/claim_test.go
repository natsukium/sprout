package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only `up` and `fork` may bring an instance directory into existence, so a
// claim of an unknown id has to read as "no such instance" and leave the state
// root exactly as it found it.
func TestClaimInstanceNeverCreatesTheDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "eeee00000001"

	lock, err := claimInstance(id, 0, nil)
	if err == nil {
		lock.Close()
		t.Fatal("claimInstance of an id with no state succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claimInstance error = %v, want it to unwrap to os.ErrNotExist", err)
	}
	dir := filepath.Join(root, "sprout", "instances", id)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("claimInstance created %s (stat err = %v)", dir, err)
	}
}

// The confirmation prompt runs unlocked, so what the user agreed to must be
// pinned by the held fd: a directory renamed away and recreated at the same
// path is a different instance, however identical the pathname looks.
func TestIncarnationMarkerDetectsAReplacedDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "instance")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	marker, err := openIncarnationMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close()
	if err := marker.verify(dir); err != nil {
		t.Fatalf("verify of an untouched directory: %v", err)
	}

	if err := os.Rename(dir, filepath.Join(root, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	err = marker.verify(dir)
	if err == nil {
		t.Fatal("verify accepted a directory recreated at the same path")
	}
	if !strings.Contains(err.Error(), "changed since confirmation") {
		t.Fatalf("unexpected error: %v", err)
	}
}
