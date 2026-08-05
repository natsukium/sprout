package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A child that dies before readiness is reported immediately, naming the log
// and using the caller's label: the build-failure path must not sit out the
// readiness timeout, nor be summarized as a boot failure when the child died
// building.
func TestAwaitBootOrReadyReportsChildFailure(t *testing.T) {
	shortStateRoot(t)
	boom := errors.New("exit status 1")
	err := awaitBootOrReady(func() error { return boom }, "dddd00000001", 0, "/tmp/up.log", "up")
	if err == nil || !strings.Contains(err.Error(), "up failed, see /tmp/up.log") {
		t.Fatalf("want up-failed error naming the log, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("child exit error not wrapped: %v", err)
	}
}

// The failure summary carries the log's final line, stripped of the child's
// own "sprout: " prefix. That line names the failed phase (nix build vs.
// runner exit), which the parent's view of the exit status cannot.
func TestAwaitBootOrReadyQuotesTheChildsLastLogLine(t *testing.T) {
	shortStateRoot(t)
	logPath := filepath.Join(t.TempDir(), "up.log")
	log := "building .#sproutConfigurations.dev …\n" +
		"error: syntax error, unexpected ')'\n" +
		"sprout: nix build .#sproutConfigurations.dev: exit status 1\n"
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	err := awaitBootOrReady(func() error { return errors.New("exit status 1") }, "dddd00000005", 0, logPath, "up")
	if err == nil || !strings.Contains(err.Error(), "\n  nix build .#sproutConfigurations.dev: exit status 1") {
		t.Fatalf("want the log's last line quoted without the sprout: prefix, got %v", err)
	}
}

// A clean child exit does not declare success by itself: the running daemon
// must actually answer SSH. Otherwise a detached `up` prints "VM ready" the
// moment its child exits zero, before the guest can accept anything.
func TestAwaitBootOrReadyCleanExitStillNeedsReadiness(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	d := &fakeDaemon{backend: backend, pid: 42, sawTrack: make(chan bool, 1)}
	seedInstanceWith(t, root, "dddd00000002", "handoff", d)

	if err := awaitBootOrReady(func() error { return nil }, "dddd00000002", 0, "", "boot"); err != nil {
		t.Fatalf("ready daemon after clean exit should succeed: %v", err)
	}
}

// The definition-unchanged handoff: the child exits clean because the daemon
// it was told to supersede is still the right one to keep. Readiness must then
// accept that daemon rather than wait for a different PID — which would stall
// until the daemon-start timeout for a VM that is already up.
func TestAwaitBootOrReadyCleanExitAcceptsSupersededDaemon(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	d := &fakeDaemon{backend: backend, pid: 42, sawTrack: make(chan bool, 1)}
	seedInstanceWith(t, root, "dddd00000003", "unchanged", d)

	done := make(chan error, 1)
	go func() {
		done <- awaitBootOrReady(func() error { return nil }, "dddd00000003", 42, "", "boot")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handoff to the still-running daemon should succeed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readiness stalled on the superseded daemon's PID")
	}
}

// The success path of a first boot: the child (which is the daemon) never
// exits, and readiness alone completes the wait.
func TestAwaitBootOrReadyReadinessWinsWhileChildRuns(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	d := &fakeDaemon{backend: backend, pid: 42, sawTrack: make(chan bool, 1)}
	seedInstanceWith(t, root, "dddd00000004", "booting", d)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	err := awaitBootOrReady(func() error { <-block; return nil }, "dddd00000004", 0, "", "boot")
	if err != nil {
		t.Fatalf("readiness should win while the daemon child keeps running: %v", err)
	}
}
