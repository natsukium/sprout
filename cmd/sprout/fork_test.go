package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fork canonicalizes the source's recorded build and refuses one that is
// gone, so tests forking successfully need a bundle that exists on disk.
// Returns the canonical path the fork is expected to record.
func pointBundleAtRealDir(t *testing.T, id string) string {
	t.Helper()
	inst, dir, err := loadInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "recorded-bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	inst.Bundle = bundle
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

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
	srcBundle := pointBundleAtRealDir(t, srcID)

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
	if inst.Bundle != srcBundle {
		t.Errorf("bundle = %q, want the source's build %q", inst.Bundle, srcBundle)
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
	pointBundleAtRealDir(t, srcID)

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

// Same rule as `snapshot --live`: a held boot lock with nothing answering
// control is a busy source, not a running one, and copying its image would
// race whoever holds it.
func TestForkLiveRefusesAHeldSourceWithNoDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "eeee2222ffff"
	t.Cleanup(func() { removeSocketDir(srcID) })
	srcDir := newTestInstance(t, root, srcID, "source", "running /var")

	lock, err := acquireInstanceLock(srcDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	err = cmdFork(srcID, true, "forked")
	if err == nil {
		t.Fatal("fork --live succeeded with nothing answering control")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error should report the source busy, got: %v", err)
	}
	if ids, _ := instancesNamed("forked"); len(ids) != 0 {
		t.Fatalf("a refused fork left state behind: %v", ids)
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

// Fork is the one command holding two instances' lifecycle locks at once, so
// two forks running in opposite directions must both finish rather than wait on
// each other forever.
func TestConcurrentOppositeForksBothFinish(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Chdir(t.TempDir())

	idA, idB := "aaaa1111dddd", "aaaa2222dddd"
	newTestInstance(t, root, idA, "a", "a's /var")
	newTestInstance(t, root, idB, "b", "b's /var")
	pointBundleAtRealDir(t, idA)
	pointBundleAtRealDir(t, idB)

	errs := make(chan error, 2)
	go func() { errs <- cmdFork(idA, false, "fork-of-a") }()
	go func() { errs <- cmdFork(idB, false, "fork-of-b") }()
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent fork of a stopped source: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent opposite-direction forks deadlocked")
		}
	}

	for _, name := range []string{"fork-of-a", "fork-of-b"} {
		ids, err := instancesNamed(name)
		if err != nil || len(ids) != 1 {
			t.Fatalf("instancesNamed(%s) = %v (err %v), want exactly one", name, ids, err)
		}
	}
}

// Seeding is the point of no return for a fork; if the copy fails, the
// destination must be gone, not left as a half-built instance the next command
// would have to refuse.
func TestForkRemovesTheDestinationWhenSeedingFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Chdir(t.TempDir())

	srcID := "bbbb4444cccc"
	srcDir := newTestInstance(t, root, srcID, "source", "seeded /var")
	pointBundleAtRealDir(t, srcID)
	srcImg := filepath.Join(srcDir, "var.img")
	if err := os.Chmod(srcImg, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restored so the temporary state root can be removed again.
	t.Cleanup(func() { os.Chmod(srcImg, 0o644) })

	dst, err := resolveIdentity("forked")
	if err != nil {
		t.Fatal(err)
	}
	dstDir, err := instanceDir(dst.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := cmdFork(srcID, false, "forked"); err == nil {
		t.Fatal("fork succeeded with an unreadable source volume")
	}
	if ids, _ := instancesNamed("forked"); len(ids) != 0 {
		t.Errorf("a failed fork left a findable instance: %v", ids)
	}
	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Errorf("destination %s survived a failed fork (stat err = %v)", dstDir, err)
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
