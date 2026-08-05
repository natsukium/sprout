//go:build darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// With the instance held as a running daemon holds it, --live still copies,
// and records the result as crash-consistent so a later restore is not
// mistaken for a clean one. darwin-only: without copy-on-write clones --live
// is unavailable, and the honest outcome there is the refusal.
func TestSnapshotLiveSucceedsOnCoW(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "cccc1111dddd"
	dir := newTestInstance(t, root, id, "feature", "running /var")

	// Stand in for the daemon, which holds this lock for its whole life.
	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := cmdSnapshotCreate(id, true, "hot"); err != nil {
		t.Fatalf("snapshot create --live: %v", err)
	}

	got, err := os.ReadFile(snapshotImage(dir, "hot"))
	if err != nil || string(got) != "running /var" {
		t.Fatalf("snapshot image = %q (err %v), want the live volume's contents", got, err)
	}

	data, err := os.ReadFile(filepath.Join(snapshotDir(dir, "hot"), "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Live {
		t.Error("live = false for a snapshot taken with --live; a restore would read as clean")
	}
}
