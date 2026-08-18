package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The classifier ls and inspect hang off: booting/running/stale from a live
// daemon's INFO, stopped/orphan from on-disk state when nothing answers.
func TestInstanceState(t *testing.T) {
	backendFor := func(t *testing.T) string { return startEchoBackend(t) }

	t.Run("daemon up but guest not ready is booting", func(t *testing.T) {
		root := shortStateRoot(t)
		d := &fakeDaemon{backend: backendFor(t), booting: true, pid: 42, sawTrack: make(chan bool, 1)}
		seedInstanceWith(t, root, "ffff00000001", "webapp", d)
		inst := &Instance{ID: "ffff00000001", Name: "webapp", KeySource: "directory", Workspace: root}
		state, info := instanceState("ffff00000001", inst, nil, false)
		if state != "booting" || info == nil {
			t.Fatalf("state = %q info=%v, want booting with info", state, info)
		}
	})

	t.Run("ready daemon with matching worktree is running", func(t *testing.T) {
		root := shortStateRoot(t)
		d := &fakeDaemon{backend: backendFor(t), pid: 42, sawTrack: make(chan bool, 1)}
		seedInstanceWith(t, root, "ffff00000002", "webapp", d)
		// KeySource "directory" never claims a branch, so staleness cannot apply.
		inst := &Instance{ID: "ffff00000002", Name: "webapp", KeySource: "directory", Workspace: root}
		state, info := instanceState("ffff00000002", inst, nil, false)
		if state != "running" || info == nil {
			t.Fatalf("state = %q info=%v, want running with info", state, info)
		}
	})

	t.Run("ready daemon whose worktree switched branch is stale", func(t *testing.T) {
		root := shortStateRoot(t)
		repo := initTestRepo(t, "main")
		d := &fakeDaemon{backend: backendFor(t), pid: 42, sawTrack: make(chan bool, 1)}
		seedInstanceWith(t, root, "ffff00000003", "feature", d)
		inst := &Instance{ID: "ffff00000003", Name: "feature", KeySource: "branch", Workspace: repo}
		state, _ := instanceState("ffff00000003", inst, nil, false)
		if state != "stale" {
			t.Fatalf("state = %q, want stale", state)
		}
	})

	t.Run("no daemon with intact worktree is stopped", func(t *testing.T) {
		root := shortStateRoot(t)
		newTestInstance(t, root, "ffff00000004", "webapp", "")
		inst := &Instance{ID: "ffff00000004", Name: "webapp", KeySource: "directory", Workspace: root}
		state, info := instanceState("ffff00000004", inst, nil, false)
		if state != "stopped" || info != nil {
			t.Fatalf("state = %q info=%v, want stopped without info", state, info)
		}
	})

	t.Run("no daemon and no worktree is orphan", func(t *testing.T) {
		root := shortStateRoot(t)
		newTestInstance(t, root, "ffff00000005", "webapp", "")
		inst := &Instance{ID: "ffff00000005", Name: "webapp", KeySource: "directory", Workspace: filepath.Join(root, "gone")}
		state, _ := instanceState("ffff00000005", inst, nil, false)
		if state != "orphan" {
			t.Fatalf("state = %q, want orphan", state)
		}
	})

	t.Run("unreadable record with no daemon is stopped, not orphan", func(t *testing.T) {
		root := shortStateRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "sprout", "instances", "ffff00000006"), 0o700); err != nil {
			t.Fatal(err)
		}
		state, _ := instanceState("ffff00000006", nil, os.ErrNotExist, false)
		if state != "stopped" {
			t.Fatalf("state = %q, want stopped", state)
		}
	})
}
