package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Records a stopped instance; an empty bundle models a missing store path.
func stoppedInstance(t *testing.T, bundle string) *Identity {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "aaaa00000001"
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "main", KeySource: "branch", Bundle: bundle, Workspace: worktree,
	}); err != nil {
		t.Fatal(err)
	}
	return &Identity{ID: id, Name: "main", KeySource: "branch", Worktree: worktree}
}

// Wake-on-access belongs to the router, not to every entry point, so `shell`
// and `exec` carry the recovery in their error instead. `up` always works.
func TestStoppedErrorLeadsWithUp(t *testing.T) {
	id := stoppedInstance(t, filepath.Join(t.TempDir(), "gone-from-the-store"))

	err := stoppedError(id, "")
	if err == nil {
		t.Fatal("no error for a stopped instance")
	}
	if want := `instance "main" is stopped; start it with: sprout up`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not read %q", err, want)
	}
	// The recorded build is gone, so `start` could only fail.
	if strings.Contains(err.Error(), "sprout start") {
		t.Errorf("error %q suggests `start` for an instance whose build is no longer in the store", err)
	}
}

// With the build still on disk `start` skips the rebuild, worth naming as a
// second line rather than in place of `up`.
func TestStoppedErrorOffersStartWhenTheBuildSurvives(t *testing.T) {
	bundle := t.TempDir()
	id := stoppedInstance(t, bundle)

	got := stoppedError(id, "").Error()
	if !strings.Contains(got, "sprout up") {
		t.Errorf("error %q does not lead with `sprout up`", got)
	}
	if !strings.Contains(got, "sprout start") {
		t.Errorf("error %q does not offer `start`, though the build is still in the store", got)
	}
}

// Both suggestions must address the instance just named, not whatever the
// current worktree resolves to.
func TestStoppedErrorCarriesTheSelector(t *testing.T) {
	id := stoppedInstance(t, t.TempDir())

	got := stoppedError(id, "feature-x").Error()
	for _, want := range []string{"sprout up -i feature-x", "sprout start -i feature-x"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q does not suggest %q", got, want)
		}
	}
}
