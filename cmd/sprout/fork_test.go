package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fork is a new, independently addressable instance carrying the source's
// /var and build, bound to the directory the command ran in.
func TestForkSeedsNewInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	// Outside any git repository, so identity falls back to this directory and
	// the test does not depend on the checkout it runs from.
	work := t.TempDir()
	t.Chdir(work)

	srcID := "bbbb1111cccc"
	newTestInstance(t, root, srcID, "source", "seeded /var")

	if err := cmdFork(srcID, false, "forked"); err != nil {
		t.Fatalf("fork: %v", err)
	}

	ids, err := instancesNamed("forked")
	if err != nil || len(ids) != 1 {
		t.Fatalf("instancesNamed(forked) = %v (err %v), want exactly one", ids, err)
	}
	inst, dstDir, err := loadInstance(ids[0])
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "var.img"))
	if err != nil || string(got) != "seeded /var" {
		t.Fatalf("forked /var = %q (err %v), want the source's", got, err)
	}
	if inst.Bundle != "/nix/store/deadbeef-sprout-vm-dev" {
		t.Errorf("bundle = %q, want the source's build", inst.Bundle)
	}
	// Belonging to the directory rather than the source is what lets a second
	// branch pick up an expensive /var.
	wantWorkspace, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Workspace != wantWorkspace {
		t.Errorf("workspace = %q, want the forking directory %q", inst.Workspace, wantWorkspace)
	}
	if inst.PID != 0 {
		t.Errorf("PID = %d, want 0: a fork is created stopped", inst.PID)
	}
}

// A fork must not silently replace the /var of an instance already there.
func TestForkRefusesExistingDestination(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "bbbb2222cccc"
	newTestInstance(t, root, srcID, "source", "seeded /var")

	if err := cmdFork(srcID, false, "forked"); err != nil {
		t.Fatalf("first fork: %v", err)
	}
	err := cmdFork(srcID, false, "forked")
	if err == nil {
		t.Fatal("second fork succeeded, want an already-exists error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An instance that never booted has no image, and "no /var volume yet" beats a
// bare ENOENT on a path the user never named.
func TestForkRefusesUnbootedSource(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "bbbb3333cccc"
	newTestInstance(t, root, srcID, "source", "") // recorded, never booted

	err := cmdFork(srcID, false, "forked")
	if err == nil {
		t.Fatal("fork of a never-booted instance succeeded")
	}
	if !strings.Contains(err.Error(), "no /var volume yet") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A bare `sprout fork` has both source and destination resolving to this branch,
// so it can only be a mistake. The error has to say what to supply, since
// "would fork onto itself" alone leaves the user with no next command.
func TestBareForkIsRejectedWithBothWaysOut(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Chdir(t.TempDir())

	_, err := runCLI(t, "fork")
	if err == nil {
		t.Fatal("`sprout fork` with no source and no destination succeeded")
	}
	if !isUsageError(err) {
		t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
	}
	for _, want := range []string{"-i", "NEWNAME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, one of the two ways to make the command useful", err, want)
		}
	}
}

// The state a fork would overwrite is the persistent volume someone forked in
// order to keep, so unlike delete and restore it has no --force.
func TestForkHasNoForce(t *testing.T) {
	if f := newForkCmd().Flags().Lookup("force"); f != nil {
		t.Error("fork grew a --force; refusing an existing destination is not negotiable")
	}
	if f := newForkCmd().Flags().Lookup("from"); f != nil {
		t.Error("--from is back; the source is selected with -i like every other existing instance")
	}
}

// The dest-exists check races a concurrent fork to the same name; the mkdir
// settles it, and the loser must not run the cleanup that removes the winner's
// state.
func TestForkDoesNotCleanUpAnotherForksDestination(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "eeee1111ffff"
	newTestInstance(t, root, srcID, "source", "seeded /var")

	// Stand in for the winner: the destination directory exists with a volume
	// in it, but the record it would be found by is not written yet.
	dst, err := resolveIdentity("forked")
	if err != nil {
		t.Fatal(err)
	}
	dstDir, err := instanceDir(dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "var.img"), []byte("winner's /var"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdFork(srcID, false, "forked"); err == nil {
		t.Fatal("fork onto a destination another fork already claimed succeeded")
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "var.img"))
	if err != nil || string(got) != "winner's /var" {
		t.Fatalf("destination /var = %q (err %v); the losing fork deleted state it did not create", got, err)
	}
}
