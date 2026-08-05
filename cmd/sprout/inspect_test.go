package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The reason inspect exists over `ls --json`: the persisted fields ls omits,
// alongside a computed state.
func TestInspectEmitsFullRecord(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	id := "abc123def456"
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID:        id,
		Name:      "feature",
		KeySource: "directory", // not "branch", so isOrphaned skips the git check
		Workspace: root,        // an existing dir, so the instance reads as stopped not orphan
		Bundle:    "/nix/store/deadbeef-sprout-vm-dev",
		GuestIP:   "192.168.127.2",
		SSHUser:   "dev",
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return cmdInspect(id)
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v\n%s", err, out)
	}
	if got["bundle"] != "/nix/store/deadbeef-sprout-vm-dev" {
		t.Errorf("missing/wrong bundle: %v", got["bundle"])
	}
	if got["guestIp"] != "192.168.127.2" {
		t.Errorf("missing/wrong guestIp: %v", got["guestIp"])
	}
	if got["state"] != "stopped" {
		t.Errorf("state = %v, want stopped", got["state"])
	}
}

// A branch name holds characters a hostname cannot, so a script composing a
// route URL reads the label here instead of re-implementing the sanitizer.
func TestInspectReportsTheRouteLabel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "abc123def456", "feat/login", "")

	out := captureStdout(t, func() error { return cmdInspect("abc123def456") })

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v\n%s", err, out)
	}
	if got["routeLabel"] != "feat-login" {
		t.Errorf("routeLabel = %v, want feat-login (the name is %v)", got["routeLabel"], got["name"])
	}
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = orig
	w.Close()
	data, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("fn returned error: %v", runErr)
	}
	return string(data)
}
