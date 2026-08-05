package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What makes `ssh sprout-<name>` reach a guest: the dial-stdio ProxyCommand
// keyed by instance ID, the per-instance known_hosts, and the guest's user.
func TestSSHConfigBlock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	// Pre-created so ensureSSHKey short-circuits instead of shelling out to
	// ssh-keygen.
	if err := os.MkdirAll(filepath.Join(root, "sprout"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sprout", "id_ed25519"), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := "abc123def456"
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID:      id,
		Name:    "feature/login",
		SSHUser: "dev",
	}); err != nil {
		t.Fatal(err)
	}

	block, err := sshConfigBlock(id)
	if err != nil {
		t.Fatalf("sshConfigBlock: %v", err)
	}

	// The Host alias and HostName use the sanitized name, not the raw branch
	// (which contains a "/" ssh would misread).
	if !strings.Contains(block, "Host sprout-feature-login\n") {
		t.Errorf("missing sanitized Host alias:\n%s", block)
	}
	// Routing must key on the unique ID, never the name, so two repos' "main"
	// branches don't collide onto one instance.
	if !strings.Contains(block, "dial-stdio --instance "+id+" ssh") {
		t.Errorf("ProxyCommand not keyed by instance ID:\n%s", block)
	}
	if !strings.Contains(block, "User dev\n") {
		t.Errorf("missing guest User:\n%s", block)
	}
	if !strings.Contains(block, filepath.Join(dir, "known_hosts")) {
		t.Errorf("missing per-instance known_hosts:\n%s", block)
	}
}
