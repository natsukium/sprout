package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	ln, err := net.Listen("unix", daemonControlSocket(t, dir))
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
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "racer", KeySource: "directory", GuestIP: "127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	sock := daemonControlSocket(t, dir)
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

			err := deleteInstances([]string{"cccc00000001", "cccc00000002"}, c.force, false)
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

// Runs action the first time the prompt is read, then answers "y": the read is
// what marks the unlocked window between the confirmation and the removal, so
// an action placed here lands exactly where a racing command would.
type actingReader struct {
	once   sync.Once
	action func()
	answer io.Reader
}

func (r *actingReader) Read(p []byte) (int, error) {
	r.once.Do(r.action)
	return r.answer.Read(p)
}

func answerYesAfter(t *testing.T, action func()) {
	t.Helper()
	confirmIn = &actingReader{action: action, answer: strings.NewReader("y\n")}
	t.Cleanup(func() { confirmIn = os.Stdin })
}

// Stands in for a delete-and-recreate by another process: the pathname is the
// one that was confirmed, the directory behind it is a different instance.
func swapInstanceDir(t *testing.T, root, id, dir, newName string) {
	t.Helper()
	if err := os.Rename(dir, filepath.Join(root, "swapped-away-"+id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: newName, KeySource: "directory", Workspace: root,
	}); err != nil {
		t.Fatal(err)
	}
}

// KeySource stays "directory", so orphan classification turns purely on
// whether the workspace path exists.
func pointWorkspaceAt(t *testing.T, id, workspace string) {
	t.Helper()
	inst, dir, err := loadInstance(id)
	if err != nil {
		t.Fatal(err)
	}
	inst.Workspace = workspace
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
}

// The user confirmed the destruction of one instance's /var; if that instance
// is replaced at the same path before the removal runs, the delete must abort
// rather than destroy the newcomer's volume.
func TestDeleteAbortsOnASwappedIncarnation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "aaaa00000020"
	dir := newTestInstance(t, root, id, "confirmed", "var-data")

	answerYesAfter(t, func() { swapInstanceDir(t, root, id, dir, "recreated") })

	err := deleteInstances([]string{id}, false, false)
	if err == nil {
		t.Fatal("delete removed an instance that was replaced after the confirmation")
	}
	if !strings.Contains(err.Error(), "changed since confirmation") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "instance.json")); err != nil {
		t.Fatalf("the recreated instance was deleted anyway: %v", err)
	}
}

// The same swap under prune is one instance's problem, not the sweep's: the
// replaced instance is skipped and the command still succeeds.
func TestPruneSkipsASwappedIncarnation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "aaaa00000021"
	dir := newTestInstance(t, root, id, "orphan", "var-data")
	pointWorkspaceAt(t, id, filepath.Join(root, "workspace-gone"))

	answerYesAfter(t, func() { swapInstanceDir(t, root, id, dir, "recreated") })

	out := captureStdout(t, func() error { return cmdPrune(false) })
	if !strings.Contains(out, "removed 0 instance(s)") {
		t.Errorf("prune reported %q, want nothing removed", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "instance.json")); err != nil {
		t.Fatalf("the recreated instance was pruned: %v", err)
	}
}

// Orphan classification ran before the prompt; a branch that comes back while
// the user answers makes this a live instance again, and only the recheck
// under the claim can see it.
func TestPruneKeepsAnInstanceThatIsNoLongerOrphaned(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "aaaa00000022"
	dir := newTestInstance(t, root, id, "reclaimed", "var-data")
	workspace := filepath.Join(root, "workspace-gone")
	pointWorkspaceAt(t, id, workspace)

	answerYesAfter(t, func() {
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatal(err)
		}
	})

	out := captureStdout(t, func() error { return cmdPrune(false) })
	if !strings.Contains(out, "removed 0 instance(s)") {
		t.Errorf("prune reported %q, want nothing removed", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "var.img")); err != nil {
		t.Fatalf("an instance that stopped being orphaned was pruned: %v", err)
	}
}

// A fork or delete that died between steps leaves a directory without a
// record. Addressed by ID it must still resolve and be deletable — when the
// crash landed between a fork's GC-root pin and its record write, delete is
// the only way to reclaim the leaked root.
func TestDeleteReclaimsARecordlessGhostByID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "abcd00000099"
	cleanupSocketDir(t, id)
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExistingIdentity(id)
	if err != nil {
		t.Fatalf("a ghost addressed by ID did not resolve: %v", err)
	}
	if got.ID != id {
		t.Fatalf("resolved %q, want %q", got.ID, id)
	}

	if err := deleteInstances([]string{got.ID}, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("ghost directory survived delete")
	}
}

// An interrupted delete must never leave a readable instance: what survives is
// either the whole instance or a husk nothing looks at.
func TestRemoveInstanceDirLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := newTestInstance(t, root, "aaaa00000010", "husk-free", "var-data")

	if err := removeInstanceDir("aaaa00000010", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("instance dir still present: %v", err)
	}
	if _, err := os.Stat(huskPath("aaaa00000010", dir)); !os.IsNotExist(err) {
		t.Fatalf("husk still present: %v", err)
	}
}

// The husk name is derived, not unique per attempt, so a delete interrupted
// after its rename would otherwise wedge every later delete of the same ID.
func TestRemoveInstanceDirClearsAnAbandonedHusk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := newTestInstance(t, root, "aaaa00000011", "retry", "var-data")

	abandoned := huskPath("aaaa00000011", dir)
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "var.img"), []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeInstanceDir("aaaa00000011", dir); err != nil {
		t.Fatalf("delete was blocked by an earlier interrupted one: %v", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("husk still present: %v", err)
	}
}

// A husk carries a full instance directory, so anything walking instances by
// directory name would otherwise resurrect what delete removed.
func TestInstanceIDsSkipHusks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := newTestInstance(t, root, "aaaa00000012", "live", "var-data")
	newTestInstance(t, root, ".aaaa00000013.deleting", "husk", "var-data")

	ids, err := instanceIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "aaaa00000012" {
		t.Fatalf("instanceIDs() = %v, want only the live instance beside %s", ids, dir)
	}
}

// An ordered log shared by a fake daemon and the stubs a test installs.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// Only the events naming a stop or a sync, in order.
func (l *eventLog) stopsAndSyncs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if e == "sync" || strings.HasPrefix(e, "STOP") {
			out = append(out, e)
		}
	}
	return out
}

// Answers control requests on dir's socket, logging each request line. An
// answer with exit set tears the daemon down after it, as a daemon told to
// stop does.
func fakeControlDaemon(t *testing.T, dir string, log *eventLog, reply func(line string) (answer string, exit bool)) {
	t.Helper()
	sock := daemonControlSocket(t, dir)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			line = strings.TrimSpace(line)
			log.add(line)
			answer, exit := reply(line)
			fmt.Fprintln(conn, answer) //nolint:errcheck
			conn.Close()
			if exit {
				ln.Close()
				os.Remove(sock)
				return
			}
		}
	}()
}

func stubGuestSync(t *testing.T, fn func(id string) error) {
	t.Helper()
	orig := guestSync
	guestSync = fn
	t.Cleanup(func() { guestSync = orig })
}

// An older daemon takes `STOP hard` for a plain STOP, so --hard against one
// must fail before sending any stop, or syncing for one.
func TestHardStopRefusesADaemonThatPredatesIt(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000030"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "old-daemon", "var-data")
	log := &eventLog{}
	fakeControlDaemon(t, dir, log, func(line string) (string, bool) {
		if strings.HasPrefix(line, "INFO") {
			return `OK {"name":"old-daemon"}`, false
		}
		return "OK", strings.HasPrefix(line, "STOP")
	})
	stubGuestSync(t, func(string) error { log.add("sync"); return nil })

	err := hardStopLocked(id, "old-daemon")
	if err == nil || !strings.Contains(err.Error(), "predates --hard") {
		t.Fatalf("hard stop against an old daemon: err = %v", err)
	}
	if got := log.stopsAndSyncs(); len(got) != 0 {
		t.Fatalf("a refused hard stop still did %v", got)
	}
}

// The sync has to finish before the power cut, and the hard path must never
// send a plain STOP, which would start the graceful shutdown.
func TestHardStopSyncsTheGuestBeforeStopHard(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000031"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "hard", "var-data")
	log := &eventLog{}
	fakeControlDaemon(t, dir, log, func(line string) (string, bool) {
		switch {
		case strings.HasPrefix(line, "INFO"):
			return `OK {"name":"hard","hardStop":true}`, false
		case line == "STOP hard":
			return "OK hard", true
		}
		return "OK", line == "STOP"
	})
	stubGuestSync(t, func(string) error { log.add("sync"); return nil })

	if err := hardStopLocked(id, "hard"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log.stopsAndSyncs(), ","); got != "sync,STOP hard" {
		t.Fatalf("events = %s, want the guest synced and then only STOP hard", got)
	}
}

// A guest that cannot be synced is exactly the one --hard exists for, so the
// power cut still goes ahead.
func TestHardStopProceedsWhenGuestSyncFails(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000032"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "wedged", "var-data")
	fakeControlDaemon(t, dir, &eventLog{}, func(line string) (string, bool) {
		if strings.HasPrefix(line, "INFO") {
			return `OK {"name":"wedged","hardStop":true}`, false
		}
		if line == "STOP hard" {
			return "OK hard", true
		}
		return "OK", false
	})
	stubGuestSync(t, func(string) error { return errors.New("ssh: connect timed out") })

	if err := hardStopLocked(id, "wedged"); err != nil {
		t.Fatalf("hard stop gave up on an unreachable guest: %v", err)
	}
}

// While `stop --hard` waits for the lifecycle lock, the instance it selected
// can be deleted and re-created under the same ID; the power cut must not land
// on the newcomer.
func TestStopHardAbortsOnASwappedIncarnation(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000033"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "selected", "var-data")
	log := &eventLog{}
	stubGuestSync(t, func(string) error { log.add("sync"); return nil })

	lc, err := acquireLifecycleLock(id)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- stopOne(id, stopBehavior{hard: true}) }()
	// Long enough for stopOne to reach the lock wait holding its marker.
	time.Sleep(200 * time.Millisecond)

	swapInstanceDir(t, root, id, dir, "recreated")
	fakeControlDaemon(t, dir, log, func(line string) (string, bool) {
		if strings.HasPrefix(line, "INFO") {
			return `OK {"name":"recreated","hardStop":true}`, false
		}
		return "OK", false
	})
	lc.Close()

	err = <-result
	if err == nil || !strings.Contains(err.Error(), "changed since it was selected") {
		t.Fatalf("stop --hard on a swapped incarnation: err = %v", err)
	}
	if got := log.stopsAndSyncs(); len(got) != 0 {
		t.Fatalf("the re-created instance got %v", got)
	}
}

func shortBootingServeWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := bootingServeWait
	bootingServeWait = d
	t.Cleanup(func() { bootingServeWait = orig })
}

// A claimed daemon not yet serving control is booting, not stopped.
func TestStopWaitsForABootingDaemonAndStopsIt(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000040"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "booting", "var-data")
	shortBootingServeWait(t, 10*time.Second)
	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	sock := daemonControlSocket(t, dir)
	log := &eventLog{}
	go func() {
		time.Sleep(700 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			line = strings.TrimSpace(line)
			log.add(line)
			fmt.Fprintln(conn, "OK") //nolint:errcheck
			conn.Close()
			if line == "STOP" {
				os.Remove(sock)
				lock.Close()
				return
			}
		}
	}()

	out := captureStdout(t, func() error {
		return stopOne(id, stopBehavior{quietIfNotRunning: true, reportStopped: true})
	})
	if got := log.stopsAndSyncs(); len(got) != 1 || got[0] != "STOP" {
		t.Fatalf("stop sent %v to the booting daemon, want one STOP", got)
	}
	if !strings.Contains(out, `instance "booting" stopped`) {
		t.Errorf("output %q does not report the instance stopped", out)
	}
}

// A claim that never serves (a snapshot restore) ends in an error, not a hang
// or a reported success.
func TestStopGivesUpOnAClaimThatNeverServes(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000041"
	cleanupSocketDir(t, id)
	dir := newTestInstance(t, root, id, "restoring", "var-data")
	shortBootingServeWait(t, 500*time.Millisecond)
	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	err = stopOne(id, stopBehavior{quietIfNotRunning: true})
	if err == nil || !strings.Contains(err.Error(), "without serving") {
		t.Fatalf("stop against a claim that never serves: err = %v", err)
	}
}

func TestStopOfAStoppedInstanceDoesNotWait(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000042"
	cleanupSocketDir(t, id)
	newTestInstance(t, root, id, "stopped", "var-data")
	shortBootingServeWait(t, 10*time.Second)

	start := time.Now()
	if err := stopOne(id, stopBehavior{quietIfNotRunning: true}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stopping a stopped instance took %s", took)
	}
}

// Each call blocks until all have started, so a serial loop never finishes.
func TestStopAllStopsInstancesConcurrently(t *testing.T) {
	ids := []string{"a", "b", "c"}
	var started sync.WaitGroup
	started.Add(len(ids))
	done := make(chan error, 1)
	go func() {
		done <- forEachOf(ids, func(id string) error {
			started.Done()
			started.Wait()
			if id == "b" {
				return errors.New("b failed")
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "b failed") {
			t.Errorf("err = %v, want b's failure reported", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stops ran one at a time")
	}
}
