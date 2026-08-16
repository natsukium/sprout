package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything matchInstanceProcs returns gets killed, so it must find the
// instance's vfkit by the socket path in its argv and nothing else: not another
// instance's vfkit, not a process merely mentioning the directory, not itself.
func TestMatchInstanceProcs(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	fingerprint := instanceProcFingerprint(dir)

	ps := "" +
		"  1 /sbin/launchd\n" +
		"501 /nix/store/x/bin/vfkit --cpus 8 --device 'virtio-blk,path=var.img' --device 'virtio-net,unixSocketPath=/state/sprout/instances/6a7f9b51b885/net.sock,mac=02:00:00:00:00:02'\n" +
		"502 /nix/store/x/bin/vfkit --device 'virtio-net,unixSocketPath=/state/sprout/instances/8b020ba20aef/net.sock,mac=02:00:00:00:00:02'\n" +
		"503 tail -f /state/sprout/instances/6a7f9b51b885/runner.log\n" +
		"504 sprout up -i feat-login\n" +
		"777 /nix/store/x/bin/vfkit --device 'virtio-net,unixSocketPath=/state/sprout/instances/6a7f9b51b885/net.sock'\n"

	got := matchInstanceProcs(ps, fingerprint, 777)
	if len(got) != 1 || got[0] != 501 {
		t.Fatalf("matchInstanceProcs found %v, want only the instance's own vfkit [501] (777 is self)", got)
	}
}

func TestMatchInstanceProcsIgnoresMalformedRows(t *testing.T) {
	ps := "\n   \n?? /state/net.sock\n501\nnotapid /state/net.sock\n"
	if got := matchInstanceProcs(ps, "/state/net.sock", 0); len(got) != 0 {
		t.Errorf("malformed ps rows produced pids %v, want none", got)
	}
}

// The lock is what proves an instance has no live daemon, so a second holder
// must be refused while the first is alive and admitted once it lets go.
func TestAcquireInstanceLockExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()

	first, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	if _, err := acquireInstanceLock(dir, 100*time.Millisecond); err == nil {
		t.Fatal("second acquire succeeded while the first was held, so a boot could race a live daemon")
	}

	first.Close()
	second, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	second.Close()
}

// Once the winner serves control, a losing booter's failure to acquire must
// read as a handoff rather than as the lock timeout.
func TestAcquireBootLockHandsOffOnceTheWinnerServes(t *testing.T) {
	root := shortStateRoot(t)
	const id = "bootlockserve"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	(&fakeDaemon{}).serve(t, filepath.Join(dir, "control.sock"))

	if _, err := acquireBootLock(dir, id, 5*time.Second); !errors.Is(err, errInstanceNowServing) {
		t.Fatalf("acquireBootLock returned %v, want errInstanceNowServing", err)
	}
}

// Contention with no daemon behind it (a crashed winner that never served, a
// snapshot restore holding the lock) must keep the timeout error: exiting
// clean there would make the detached parent wait on a boot nobody performs.
func TestAcquireBootLockTimesOutWithoutADaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "bootlocktimeout"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := acquireBootLock(dir, id, 300*time.Millisecond); err == nil || errors.Is(err, errInstanceNowServing) {
		t.Fatalf("want the lock-held timeout error, got %v", err)
	}

	held.Close()
	lock, err := acquireBootLock(dir, id, time.Second)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	lock.Close()
}

// End to end over real processes: a survivor carrying the instance's socket is
// gone once reapOrphans returns, an instance with nothing behind it is left
// alone. Two survivors, one per fingerprint: the socket-directory path current
// runners carry, and the instance-directory path an older sprout's carry. The
// stand-in answers no REST socket, so the fall-through from the graceful stop
// to signalling runs too.
func TestReapOrphansKillsTheSurvivingVM(t *testing.T) {
	dir, sockDir := t.TempDir(), t.TempDir()
	m := &Manifest{RestSocket: "vfkit-rest.sock"}

	if err := reapOrphans(dir, sockDir, m); err != nil {
		t.Fatalf("reapOrphans with no leftovers failed: %v", err)
	}

	// A compound command keeps sh from exec'ing straight into sleep, which would
	// drop the trailing argument that stands in for vfkit's socket path.
	startSurvivor := func(fingerprint string) chan struct{} {
		survivor := exec.Command("/bin/sh", "-c", "sleep 300; :", fingerprint)
		if err := survivor.Start(); err != nil {
			t.Fatal(err)
		}
		// Left unwaited the child lingers as a zombie, which still answers signal 0
		// and would read as "refused to exit".
		exited := make(chan struct{})
		go func() { _ = survivor.Wait(); close(exited) }()
		t.Cleanup(func() { _ = survivor.Process.Kill() })
		return exited
	}
	current := startSurvivor(instanceProcFingerprint(sockDir))
	legacy := startSurvivor(instanceProcFingerprint(dir))

	if err := reapOrphans(dir, sockDir, m); err != nil {
		t.Fatalf("reapOrphans failed: %v", err)
	}
	for name, exited := range map[string]chan struct{}{"current": current, "legacy": legacy} {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Errorf("the %s-fingerprint process survived reapOrphans, so the next boot would still find its disk held", name)
		}
	}
}

// Only the framework's storage-device wording is worth reinterpreting; anything
// else passes through untouched so the caller's report stays the whole story.
func TestTranslateRunnerFailure(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	vzDiskInUse := `Error Domain=VZErrorDomain Code=2 Description="Invalid virtual machine configuration. The storage device attachment is invalid."`

	if got := translateRunnerFailure(vzDiskInUse, dir); !strings.Contains(got, filepath.Join(dir, "var.img")) {
		t.Errorf("storage-device failure translated to %q, want it to name the disk image", got)
	}

	unrelated := []string{
		"",
		"vfkit: unknown flag --nope",
		`Error Domain=VZErrorDomain Code=2 Description="Invalid virtual machine configuration. The memory size is invalid."`,
	}
	for _, out := range unrelated {
		if got := translateRunnerFailure(out, dir); got != "" {
			t.Errorf("translateRunnerFailure(%q) = %q, want no hint", out, got)
		}
	}
}

// The tail is read for a failure vfkit prints last, so it must survive logs
// longer and shorter than the window, and a missing log reads as "nothing to
// say" rather than failing the boot report.
func TestRunnerLogTail(t *testing.T) {
	dir := t.TempDir()

	short := filepath.Join(dir, "short.log")
	if err := os.WriteFile(short, []byte("boot ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runnerLogTail(short, 4096); got != "boot ok\n" {
		t.Errorf("short log read as %q, want the whole file", got)
	}

	long := filepath.Join(dir, "long.log")
	if err := os.WriteFile(long, []byte(strings.Repeat("chatter\n", 4000)+"the last words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runnerLogTail(long, 64)
	if !strings.HasSuffix(got, "the last words\n") {
		t.Errorf("long log tail = %q, want it to end with the final line", got)
	}
	if len(got) > 64 {
		t.Errorf("long log tail read %d bytes, want at most 64", len(got))
	}

	if got := runnerLogTail(filepath.Join(dir, "absent.log"), 4096); got != "" {
		t.Errorf("missing log read as %q, want empty", got)
	}
}
