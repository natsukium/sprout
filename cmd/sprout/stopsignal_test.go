package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sigtermSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
}

// Runs the calling test again in a child with mode set, returning its output
// and whether it exited cleanly rather than being killed by the signal.
func runSignalChild(t *testing.T, mode string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "SPROUT_TEST_SIGNAL_CHILD="+mode)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// A SIGTERM between the daemon's setup and the runner's start must neither
// kill sprout (PDEATHSIG would pass it straight to QEMU) nor be lost: it is
// held until the boot can act on it.
func TestStopSignalDuringSetupIsHeldForTheBoot(t *testing.T) {
	if os.Getenv("SPROUT_TEST_SIGNAL_CHILD") == "daemon" {
		sigCh := watchStopSignals()
		sigtermSelf()
		if err := stopBeforeBoot(sigCh, "setup"); err != nil {
			os.Stdout.WriteString(err.Error() + "\n")
		}
		os.Exit(0)
	}
	out, clean := runSignalChild(t, "daemon")
	if !clean || !strings.Contains(out, "not booting") {
		t.Fatalf("a SIGTERM during setup was not held for the boot (clean exit %v):\n%s", clean, out)
	}
}

func TestStopBeforeBootLetsAnUnsignalledBootGoAhead(t *testing.T) {
	if err := stopBeforeBoot(make(chan os.Signal, 1), "quiet"); err != nil {
		t.Fatal(err)
	}
}

// A signal that arrived while the runner was starting is already buffered
// when the daemon starts waiting, and must still become a stop.
func TestSignalBufferedBeforeTheWaitStillStopsTheRunner(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	exit, err := startManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM
	stopped := false
	done := make(chan struct{})
	go func() {
		awaitRunnerExit(sigCh, nil, exit, func() { stopped = true; _ = cmd.Process.Kill() })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the buffered signal was never acted on")
	}
	if !stopped {
		t.Error("the runner exited without the stop being requested")
	}
}

// The router serves wakes from its first accept, so its SIGTERM handler has
// to be in place by then, or a wake started before it would escape the
// shutdown's wait.
func TestRouterHandlesSIGTERMFromItsFirstAccept(t *testing.T) {
	if os.Getenv("SPROUT_TEST_SIGNAL_CHILD") == "router" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		beforeRouteServe = sigtermSelf
		root := newRootCmd()
		root.SetArgs([]string{"route", "serve", "--port", "0", "--bind", "127.0.0.1", "--no-wake"})
		if err := root.Execute(); err != nil {
			os.Stdout.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	out, clean := runSignalChild(t, "router")
	if !clean || !strings.Contains(out, "stopped routing") {
		t.Fatalf("a SIGTERM before the first accept did not take the router's shutdown path (clean exit %v):\n%s", clean, out)
	}
}
