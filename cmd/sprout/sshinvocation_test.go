package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// guestScript decodes the /bin/sh program in a remote command, failing if the
// login shell would see a quote or backslash.
func guestScript(t *testing.T, remote string) string {
	t.Helper()
	body, ok := strings.CutPrefix(remote, "exec /bin/sh -c '")
	body, ok2 := strings.CutSuffix(body, "'")
	if !ok || !ok2 || strings.ContainsAny(body, `'\`) {
		t.Fatalf("remote command is not a single inert quoted body: %q", remote)
	}
	m := regexp.MustCompile(`printf %s ([A-Za-z0-9+/=]+) \| base64 -d`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no base64 payload in %q", remote)
	}
	script, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(script)
}

// A remote command starts with a cd into /workspace when the instance's
// bundle mounts one: nearly every `sprout exec -- cmd` / `sprout run` is about
// the project checkout, and without it each of them starts in /root.
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
	if got, want := guestScript(t, args[len(args)-1]), "cd /workspace 2>/dev/null\nset -- 'just' 'build'\n"; !strings.HasPrefix(got, want) {
		t.Errorf("guest script starts %q, want prefix %q", got, want)
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
	if got, want := guestScript(t, args[len(args)-1]), "set -- 'just' 'build'\n"; !strings.HasPrefix(got, want) {
		t.Errorf("guest script starts %q, want prefix %q", got, want)
	}
}

// Only the non-pty path needs the guard: a pty's hangup already stops the
// command when the session ends.
func TestSSHInvocationGuardsOnlyTheNonPTYPath(t *testing.T) {
	writeSSHTestInstance(t, &Instance{
		ID:      "abc123def456",
		Name:    "main",
		SSHUser: "root",
	})

	for _, tty := range []bool{false, true} {
		_, args, err := sshInvocation("abc123def456", tty, []string{"true"})
		if err != nil {
			t.Fatalf("sshInvocation: %v", err)
		}
		if got := strings.Contains(args[len(args)-1], "setpriv --pdeathsig"); got == tty {
			t.Errorf("tty=%v: guarded=%v, want %v", tty, got, !tty)
		}
	}
}

// loginShells are the guest root login shells available here.
func loginShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	for _, name := range []string{"sh", "bash", "zsh", "fish"} {
		if path, err := exec.LookPath(name); err == nil {
			shells = append(shells, path)
		}
	}
	return shells
}

// The command arriving in the guest has the same empty, spaced, quoted, and
// backslashed arguments the user placed after `--`, whatever the login shell.
func TestSSHInvocationPreservesArgumentBoundaries(t *testing.T) {
	command := []string{"printf", "<%s>\\n", "two words", "", "it's quoted", `a\\b`}
	for _, guarded := range []bool{false, true} {
		remote := remoteCommand(command, false, guarded)
		for _, shell := range loginShells(t) {
			out, err := exec.Command(shell, "-c", remote).Output()
			if err != nil {
				t.Fatalf("%s (guarded=%v): %v", shell, guarded, err)
			}
			if got, want := string(out), "<two words>\n<>\n<it's quoted>\n<a\\\\b>\n"; got != want {
				t.Errorf("%s (guarded=%v): output = %q, want %q", shell, guarded, got, want)
			}
		}
	}
}

// Exit status, stdin, and shell builtins behave as if the command ran
// directly, with or without the guard.
func TestRemoteCommandPassesThroughStatusStdinAndBuiltins(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		for _, code := range []int{0, 1, 42, 143} {
			remote := remoteCommand([]string{"sh", "-c", fmt.Sprintf("exit %d", code)}, false, guarded)
			err := exec.Command("/bin/sh", "-c", remote).Run()
			got := 0
			if exit, ok := err.(*exec.ExitError); ok {
				got = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != code {
				t.Errorf("guarded=%v: exit %d came back as %d", guarded, code, got)
			}
		}

		cmd := exec.Command("/bin/sh", "-c", remoteCommand([]string{"cat"}, false, guarded))
		cmd.Stdin = strings.NewReader("from the host\n")
		out, err := cmd.Output()
		if err != nil || string(out) != "from the host\n" {
			t.Errorf("guarded=%v: stdin round trip = %q, %v", guarded, out, err)
		}

		if err := exec.Command("/bin/sh", "-c", remoteCommand([]string{":"}, false, guarded)).Run(); err != nil {
			t.Errorf("guarded=%v: builtin `:` failed: %v", guarded, err)
		}
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

// ssh's default RequestTTY=auto never allocates a pty when a remote command is
// given, so `exec --tty` has to force one; without --tty, ssh must not get one.
func TestSSHInvocationTTYFlag(t *testing.T) {
	writeSSHTestInstance(t, &Instance{
		ID:      "abc123def456",
		Name:    "main",
		SSHUser: "root",
	})

	for _, tc := range []struct {
		tty       bool
		want, not string
	}{
		{tty: true, want: "-tt", not: "-T"},
		{tty: false, want: "-T", not: "-tt"},
	} {
		_, args, err := sshInvocation("abc123def456", tc.tty, []string{"htop"})
		if err != nil {
			t.Fatalf("sshInvocation: %v", err)
		}
		if !slices.Contains(args, tc.want) || slices.Contains(args, tc.not) {
			t.Errorf("tty=%v: argv %q must contain %q and not %q", tc.tty, args, tc.want, tc.not)
		}
	}
}
