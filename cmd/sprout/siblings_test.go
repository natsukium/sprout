package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Records an instance bound to worktree and serves its control socket, so
// runningSiblings sees it as running.
func seedRunningSibling(t *testing.T, root, id, name, worktree string) {
	t.Helper()
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: name, KeySource: "branch", Workspace: worktree,
	}); err != nil {
		t.Fatal(err)
	}
	(&fakeDaemon{sawTrack: make(chan bool, 1)}).serve(t, filepath.Join(dir, "control.sock"))
}

// After a branch switch in place, the previous branch's instance keeps running
// while every command addresses the branch checked out now. Nothing else in
// the failing command names it, so the hint has to.
func TestSiblingHintNamesTheInstanceRunningForThisWorktree(t *testing.T) {
	root := shortStateRoot(t)
	worktree := t.TempDir()
	seedRunningSibling(t, root, "aaaa00000001", "main", worktree)

	got := siblingHint(&Identity{ID: "bbbb00000002", Name: "feature-x", KeySource: "branch", Worktree: worktree})
	for _, want := range []string{`"main"`, "-i", "sprout stop -i main"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q does not mention %q", got, want)
		}
	}
}

// Two worktrees each running their own instance is the normal side-by-side
// workflow, so naming the other one would be noise on every `up`.
func TestSiblingHintIgnoresInstancesOfOtherWorktrees(t *testing.T) {
	root := shortStateRoot(t)
	seedRunningSibling(t, root, "aaaa00000001", "main", t.TempDir())

	got := siblingHint(&Identity{ID: "bbbb00000002", Name: "feature-x", KeySource: "branch", Worktree: t.TempDir()})
	if got != "" {
		t.Errorf("hint %q names an instance bound to a different worktree", got)
	}
}

// A stopped instance is not reachable with -i either, so pointing at it would
// send the user to a second failure.
func TestSiblingHintIgnoresStoppedInstances(t *testing.T) {
	root := shortStateRoot(t)
	worktree := t.TempDir()
	dir := filepath.Join(root, "sprout", "instances", "aaaa00000001")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: "aaaa00000001", Name: "main", KeySource: "branch", Workspace: worktree,
	}); err != nil {
		t.Fatal(err)
	}

	got := siblingHint(&Identity{ID: "bbbb00000002", Name: "feature-x", KeySource: "branch", Worktree: worktree})
	if got != "" {
		t.Errorf("hint %q names a stopped instance", got)
	}
}

// `sprout exec` after a branch switch reports the new branch as stopped, so
// that error is where the previous branch's live instance has to be named.
func TestStoppedErrorCarriesTheSiblingHint(t *testing.T) {
	root := shortStateRoot(t)
	worktree := t.TempDir()
	seedRunningSibling(t, root, "aaaa00000001", "main", worktree)

	got := stoppedError(&Identity{ID: "bbbb00000002", Name: "feature-x", KeySource: "branch", Worktree: worktree}, "").Error()
	if !strings.Contains(got, `instance "feature-x" is stopped`) {
		t.Errorf("error %q does not name the branch it addressed", got)
	}
	if !strings.Contains(got, `"main"`) {
		t.Errorf("error %q does not name the instance still running for this worktree", got)
	}
}
