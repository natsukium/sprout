package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On-disk state for one stopped instance; varContent seeds the /var image the
// snapshot commands operate on.
func newTestInstance(t *testing.T, root, id, name, varContent string) string {
	t.Helper()
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID:        id,
		Name:      name,
		KeySource: "directory", // not "branch", so no git lookup is involved
		Workspace: root,
		Bundle:    "/nix/store/deadbeef-sprout-vm-dev",
		GuestIP:   "192.168.127.2",
		SSHUser:   "dev",
	}); err != nil {
		t.Fatal(err)
	}
	if varContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "var.img"), []byte(varContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidateSnapshotName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "before-upgrade", false},
		{"digits and dots", "v1.2.3", false},
		{"underscore", "clean_state", false},
		{"leading digit", "2026-07-31", false},
		{"empty", "", true},
		{"path separator", "a/b", true},
		{"parent directory", "..", true},
		{"current directory", ".", true},
		{"leading dot hides the directory", ".hidden", true},
		{"leading dash reads as a flag", "-force", true},
		{"space", "my snapshot", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSnapshotName(c.in)
			if c.wantErr && err == nil {
				t.Fatalf("validateSnapshotName(%q) = nil, want an error", c.in)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateSnapshotName(%q) unexpected error: %v", c.in, err)
			}
		})
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa1111bbbb"
	dir := newTestInstance(t, root, id, "feature", "original /var")

	if err := cmdSnapshotCreate(id, false, "before-upgrade"); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "var.img"), []byte("mutated /var"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdSnapshotRestore(id, true, "before-upgrade"); err != nil {
		t.Fatalf("snapshot restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "var.img"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original /var" {
		t.Fatalf("restored /var = %q, want %q", got, "original /var")
	}
	// The staging file the restore renames from must not survive as a second
	// multi-gigabyte image.
	if _, err := os.Stat(filepath.Join(dir, "var.img.restoring")); !os.IsNotExist(err) {
		t.Fatalf("staging image survived the restore (stat err = %v)", err)
	}
}

// The metadata a restore depends on to warn about a build mismatch.
func TestSnapshotCreateRecordsProvenance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa2222bbbb"
	dir := newTestInstance(t, root, id, "feature", "data")

	if err := cmdSnapshotCreate(id, false, "snap"); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(snapshotDir(dir, "snap"), "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Bundle != "/nix/store/deadbeef-sprout-vm-dev" {
		t.Errorf("bundle = %q, want the instance's build", snap.Bundle)
	}
	if snap.Live {
		t.Error("live = true for a snapshot of a stopped instance")
	}
	if snap.Created.IsZero() {
		t.Error("created timestamp is zero")
	}
}

// A second `create` must not overwrite the rollback point the user relies on.
func TestSnapshotCreateRefusesDuplicate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa3333bbbb"
	newTestInstance(t, root, id, "feature", "data")

	if err := cmdSnapshotCreate(id, false, "snap"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := cmdSnapshotCreate(id, false, "snap")
	if err == nil {
		t.Fatal("second create succeeded, want an already-exists error")
	}
	if !strings.Contains(err.Error(), "already has a snapshot") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Both halves of the safety rule: `create` refuses a running instance without
// --live, `restore` refuses it outright since no flag makes swapping a live
// VM's disk safe.
func TestSnapshotRefusesRunningInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa4444bbbb"
	dir := newTestInstance(t, root, id, "feature", "data")

	if err := cmdSnapshotCreate(id, false, "snap"); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}

	// Stand in for a running daemon: it holds this lock for its whole life.
	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	err = cmdSnapshotCreate(id, false, "while-running")
	if err == nil {
		t.Fatal("create of a running instance succeeded without --live")
	}
	if !strings.Contains(err.Error(), "--live") {
		t.Fatalf("error should point at --live, got: %v", err)
	}

	err = cmdSnapshotRestore(id, true, "snap")
	if err == nil {
		t.Fatal("restore of a running instance succeeded, want a refusal")
	}
	if strings.Contains(err.Error(), "--live") {
		t.Fatalf("restore must not offer --live as a way out, got: %v", err)
	}
}

// The listing reads oldest to newest, so the last line is the newest rollback
// point.
func TestSnapshotLsOrdersByCreation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa5555bbbb"
	dir := newTestInstance(t, root, id, "feature", "data")

	for i, name := range []string{"second", "first"} {
		target := snapshotDir(dir, name)
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "var.img"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// "first" is written second but timestamped earlier, so a listing that
		// leans on directory order instead of the record fails here.
		created := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Add(time.Duration(-i) * time.Hour)
		if err := writeJSON(filepath.Join(target, "snapshot.json"), &Snapshot{Name: name, Created: created}); err != nil {
			t.Fatal(err)
		}
	}

	snaps, err := listSnapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].Name != "first" || snaps[1].Name != "second" {
		t.Fatalf("listSnapshots = %v, want [first second]", snaps)
	}
}

// Removing a rollback point is not destructive to the VM itself, which is why
// `snapshot delete` has no prompt.
func TestSnapshotDeleteLeavesInstanceIntact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "aaaa6666bbbb"
	dir := newTestInstance(t, root, id, "feature", "live data")

	if err := cmdSnapshotCreate(id, false, "snap"); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	if err := cmdSnapshotDelete(id, "snap"); err != nil {
		t.Fatalf("snapshot delete: %v", err)
	}

	if _, err := os.Stat(snapshotDir(dir, "snap")); !os.IsNotExist(err) {
		t.Fatalf("snapshot survived delete (stat err = %v)", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "var.img"))
	if err != nil || string(got) != "live data" {
		t.Fatalf("live /var = %q (err %v), want it untouched", got, err)
	}
}

// Query commands report the same facts to a person and to a script, so the
// JSON shape is asserted per field rather than trusting the table.
func TestSnapshotListJSONShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "aaaa1111bbbb", "feature", "live data")

	if err := cmdSnapshotCreate("aaaa1111bbbb", false, "before-migration"); err != nil {
		t.Fatal(err)
	}
	rows, err := gatherSnapshots("aaaa1111bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("gatherSnapshots returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Name != "before-migration" {
		t.Errorf("name = %q, want before-migration", r.Name)
	}
	if r.Live {
		t.Error("live = true for a snapshot taken while stopped")
	}
	// Raw bytes, not "9B": the table formats, the JSON stays machine-readable.
	if r.DiskBytes <= 0 {
		t.Errorf("diskBytes = %d, want the snapshot's allocation", r.DiskBytes)
	}
	if _, err := time.Parse(time.RFC3339, r.Created); err != nil {
		t.Errorf("created = %q, not RFC3339: %v", r.Created, err)
	}
}

// An empty list has to marshal to "[]", so a consumer can parse
// unconditionally instead of special-casing "no snapshots".
func TestSnapshotListJSONIsAnArrayWhenEmpty(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "cccc1111dddd", "feature", "live data")

	rows, err := gatherSnapshots("cccc1111dddd")
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "[]" {
		t.Errorf("empty snapshot list marshals to %s, want []", out)
	}
}
