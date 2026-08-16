package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustDeleteTarget(t *testing.T, id string) deleteTarget {
	t.Helper()
	target, err := newDeleteTarget(id)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// State survives while a daemon holds the lock but cannot answer control,
// which is what startup and shutdown look like.
func TestDeleteOneRefusesWhileLockHeld(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := newTestInstance(t, root, "aaaa00000001", "held", "var-data")

	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := deleteOne(mustDeleteTarget(t, "aaaa00000001")); err == nil {
		t.Fatal("delete should refuse while another process holds the instance lock")
	}
	if _, err := os.Stat(filepath.Join(dir, "var.img")); err != nil {
		t.Fatalf("var.img was deleted despite the held lock: %v", err)
	}
}

func TestDeleteOneRemovesStoppedInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := newTestInstance(t, root, "aaaa00000002", "stopped", "var-data")

	target := mustDeleteTarget(t, "aaaa00000002")
	out := captureStdout(t, func() error { return deleteOne(target) })
	if out != "instance \"stopped\" deleted\n" {
		t.Errorf("delete output = %q, want the instance name after state is removed", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("instance dir still present: %v", err)
	}
}

// Delete's internal stop must not duplicate the daemon's shutdown report: the
// deletion is the result the user asked for.
func TestDeleteRunningInstanceReportsOnlyDeletion(t *testing.T) {
	root := shortStateRoot(t)
	t.Setenv("XDG_STATE_HOME", root)
	const id = "aaaa00000003"
	dir := newTestInstance(t, root, id, "running", "var-data")
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			fmt.Fprintln(conn, "OK") //nolint:errcheck
			conn.Close()
			if strings.TrimSpace(line) == "STOP" {
				return
			}
		}
	}()

	target := mustDeleteTarget(t, id)
	out := captureStdout(t, func() error { return deleteOne(target) })
	if out != "instance \"running\" deleted\n" {
		t.Errorf("delete output = %q, want no client-side stopped line", out)
	}
}

// A `stop` losing the race to a concurrent stop — daemon alive for the
// running check, gone by the STOP request — must report the instance stopped,
// not fail.
func TestStopOneToleratesConcurrentStop(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000007"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "racer", KeySource: "directory", GuestIP: "127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// As a daemon torn down by a concurrent client leaves it: answering the
		// running check, gone by the follow-up STOP.
		ln.Close()
		os.Remove(sock)
		line, _ := bufio.NewReader(conn).ReadString('\n')
		if strings.TrimSpace(line) == "PING" {
			fmt.Fprintln(conn, "OK") //nolint:errcheck
		}
		conn.Close()
	}()

	out := captureStdout(t, func() error {
		return stopOne(id, stopBehavior{reportStopped: true})
	})
	if !strings.Contains(out, "stopped") {
		t.Errorf("stop losing to a concurrent stop should still report stopped, output:\n%s", out)
	}
}

// A piped stdin does not echo the answer as a terminal does, so status output
// must not end up appended to the prompt line.
func TestConfirmYesTerminatesAPipedPrompt(t *testing.T) {
	confirmIn = strings.NewReader("y\n")
	t.Cleanup(func() { confirmIn = os.Stdin })

	out := captureStdout(t, func() error {
		if !confirmYes("delete it?") {
			t.Fatal("yes answer was declined")
		}
		fmt.Println("deleted")
		return nil
	})
	if out != "delete it? [y/N] \ndeleted\n" {
		t.Errorf("prompt and status output are not separated:\n%q", out)
	}
}

// `delete --all` destroys every persistent volume on the host, so it asks once
// for the whole set and one "n" leaves every instance intact.
func TestDeleteAllAsksOnceForTheWholeSet(t *testing.T) {
	cases := []struct {
		what      string
		answer    string
		force     bool
		wantGone  bool
		wantError bool
	}{
		{what: "declined", answer: "n\n", wantGone: false, wantError: true},
		{what: "accepted", answer: "y\n", wantGone: true},
		{what: "no answer at all", answer: "", wantGone: false, wantError: true},
		{what: "forced, never asked", answer: "", force: true, wantGone: true},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_STATE_HOME", root)
			dirs := []string{
				newTestInstance(t, root, "cccc00000001", "one", "var-data"),
				newTestInstance(t, root, "cccc00000002", "two", "var-data"),
			}
			answered := strings.NewReader(c.answer)
			confirmIn = answered
			t.Cleanup(func() { confirmIn = os.Stdin })

			err := deleteInstances([]string{"cccc00000001", "cccc00000002"}, c.force)
			if c.wantError && err == nil {
				t.Error("delete succeeded, want it aborted")
			}
			if !c.wantError && err != nil {
				t.Errorf("delete: %v", err)
			}
			for _, dir := range dirs {
				_, statErr := os.Stat(dir)
				if c.wantGone && !os.IsNotExist(statErr) {
					t.Errorf("%s still present after an accepted delete", dir)
				}
				if !c.wantGone && statErr != nil {
					t.Errorf("%s was deleted despite the prompt not being accepted: %v", dir, statErr)
				}
			}
			if !c.force && answered.Len() != 0 {
				t.Errorf("%d byte(s) of the answer left unread; the prompt ran more than once", answered.Len())
			}
		})
	}
}

// The state root is host-global, so a per-clone cleanup that also swept
// another repository's /var volume would be the exact accident --project
// exists to prevent: only records that prove membership via RepoRoot are in
// scope.
func TestProjectScopeTouchesOnlyThisProjectsInstances(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	// repoContext resolves symlinks (macOS /var/folders is one), so the
	// RepoRoot to stamp is the realpath of the directory the test stands in.
	here, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	mine := newTestInstance(t, root, "dddd00000001", "mine", "var-data")
	foreign := newTestInstance(t, root, "dddd00000002", "foreign", "var-data")
	stampRepoRoot(t, "dddd00000001", here)
	stampRepoRoot(t, "dddd00000002", elsewhere)

	t.Chdir(here)

	quiet := captureStdout(t, func() error {
		_, err := runCLI(t, "list", "--project", "-q")
		return err
	})
	if strings.TrimSpace(quiet) != "dddd00000001" {
		t.Errorf("list --project -q printed %q, want only this project's ID", quiet)
	}

	out := captureStdout(t, func() error {
		_, err := runCLI(t, "delete", "--project", "--force")
		return err
	})
	if !strings.Contains(out, "deleted") {
		t.Errorf("delete --project reported nothing deleted:\n%s", out)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("this project's instance survived delete --project")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign instance was deleted by another project's --project scope: %v", err)
	}
}

// An empty --project scope is answered like delete's "no instances to
// delete", so running cleanup from the wrong directory is visible instead of
// a silent success.
func TestStopWithEmptyScopeSaysSo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, scope := range []string{"--project", "--all"} {
		t.Run(scope, func(t *testing.T) {
			out := captureStdout(t, func() error {
				_, err := runCLI(t, "stop", scope)
				return err
			})
			if !strings.Contains(out, "no instances to stop") {
				t.Errorf("stop %s on an empty scope printed %q", scope, out)
			}
		})
	}
}

func stampRepoRoot(t *testing.T, id, repoRoot string) {
	t.Helper()
	inst, _, err := loadInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.RepoRoot = repoRoot
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
}

// Prune never touches a live instance, even when the orphan classification
// raced a boot: the locked orphan survives, the unlocked one is removed.
func TestPruneSkipsLockedOrphan(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	lockedDir := newTestInstance(t, root, "bbbb00000001", "locked-orphan", "keep")
	freeDir := newTestInstance(t, root, "bbbb00000002", "free-orphan", "drop")
	for _, d := range []string{lockedDir, freeDir} {
		inst, _, err := loadInstance(filepath.Base(d))
		if err != nil {
			t.Fatal(err)
		}
		inst.Workspace = filepath.Join(root, "gone")
		if err := writeJSON(filepath.Join(d, "instance.json"), inst); err != nil {
			t.Fatal(err)
		}
	}

	lock, err := acquireInstanceLock(lockedDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := cmdPrune(true); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(lockedDir); err != nil {
		t.Fatalf("locked orphan was deleted: %v", err)
	}
	if _, err := os.Stat(freeDir); !os.IsNotExist(err) {
		t.Fatalf("unlocked orphan still present: %v", err)
	}
}
