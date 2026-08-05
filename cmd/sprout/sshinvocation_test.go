package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeSSHTestInstance sets up the minimal on-disk state sshInvocation needs:
// the client key (pre-created so ensureSSHKey never shells out to ssh-keygen)
// and one instance record.
func writeSSHTestInstance(t *testing.T, inst *Instance) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if err := os.MkdirAll(filepath.Join(root, "sprout"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sprout", "id_ed25519"), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sprout", "instances", inst.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
}

// A remote command is prefixed with a cd into /workspace when the instance's
// bundle mounts one: nearly every `sprout exec -- cmd` / `sprout run` is about
// the project checkout, and without the prefix each of them starts in /root.
func TestSSHInvocationDefaultsCommandsToWorkspace(t *testing.T) {
	writeSSHTestInstance(t, &Instance{
		ID:               "abc123def456",
		Name:             "main",
		SSHUser:          "root",
		WorkspaceMounted: true,
	})

	_, args, err := sshInvocation("abc123def456", false, []string{"just", "build"})
	if err != nil {
		t.Fatalf("sshInvocation: %v", err)
	}
	if got, want := args[len(args)-1], "cd /workspace 2>/dev/null; 'just' 'build'"; got != want {
		t.Errorf("remote command = %q, want %q", got, want)
	}
}

func TestSSHInvocationLeavesWorkspacelessGuestsAlone(t *testing.T) {
	writeSSHTestInstance(t, &Instance{
		ID:      "abc123def456",
		Name:    "main",
		SSHUser: "root",
	})

	_, args, err := sshInvocation("abc123def456", false, []string{"just", "build"})
	if err != nil {
		t.Fatalf("sshInvocation: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "cd /workspace") {
		t.Errorf("unexpected workspace cd for workspace-less instance:\n%s", joined)
	}
	if got, want := args[len(args)-1], "'just' 'build'"; got != want {
		t.Errorf("remote command = %q, want %q", got, want)
	}
}

// The command arriving at the remote shell has the same empty, spaced, and
// quoted arguments the user placed after `--`.
func TestSSHInvocationPreservesArgumentBoundaries(t *testing.T) {
	command := []string{"printf", "<%s>\\n", "two words", "", "it's quoted"}
	remote := remoteCommand(command, false)
	out, err := exec.Command("/bin/sh", "-c", remote).Output()
	if err != nil {
		t.Fatalf("running rendered command %q: %v", remote, err)
	}
	if got, want := string(out), "<two words>\n<>\n<it's quoted>\n"; got != want {
		t.Errorf("rendered command output = %q, want %q", got, want)
	}
}

// The interactive path stays a plain ssh (no injected command): the
// login-shell init inside the guest owns the /workspace default there, and an
// injected command would replace the shell entirely.
func TestSSHInvocationInteractiveGetsNoRemoteCommand(t *testing.T) {
	writeSSHTestInstance(t, &Instance{
		ID:               "abc123def456",
		Name:             "main",
		SSHUser:          "root",
		WorkspaceMounted: true,
	})

	_, args, err := sshInvocation("abc123def456", true, nil)
	if err != nil {
		t.Fatalf("sshInvocation: %v", err)
	}
	if last := args[len(args)-1]; last != "root@sprout-main" {
		t.Errorf("interactive argv must end at the destination, got %q", last)
	}
}
