package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A stand-in for the vfkit runner: a shell that stays alive until signaled.
// The compound command keeps sh from exec'ing the sleep away.
func startFakeRunner(t *testing.T, script string) (*exec.Cmd, chan error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return cmd, waitCh
}

// The exit status goes back on the channel, since runDaemon's main select
// still has to observe it after gracefulStop consumed it.
func TestWaitExitRequeues(t *testing.T) {
	waitCh := make(chan error, 1)
	waitCh <- os.ErrClosed
	if !waitExit(waitCh, time.Second) {
		t.Fatal("waitExit missed a queued exit")
	}
	select {
	case err := <-waitCh:
		if err != os.ErrClosed {
			t.Fatalf("requeued error changed: %v", err)
		}
	default:
		t.Fatal("exit status was consumed, not requeued")
	}

	if waitExit(waitCh, 50*time.Millisecond) {
		t.Fatal("waitExit reported an exit that never happened")
	}
}

// The common daemon-crash shape: the runner never got as far as a REST
// endpoint, so the ladder moves on to SIGTERM and the runner ends up gone.
func TestGracefulStopFallsBackToSigterm(t *testing.T) {
	dir := t.TempDir()
	cmd, waitCh := startFakeRunner(t, "sleep 300; :")

	done := make(chan struct{})
	go func() {
		gracefulStop(filepath.Join(dir, "vfkit-rest.sock"), cmd, waitCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gracefulStop did not return after SIGTERM killed the runner")
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("runner state after stop: %v", cmd.ProcessState)
	}
}

// A REST stop that is acknowledged but stops nothing, then a SIGTERM the
// runner ignores, still ends in SIGKILL rather than an early return or a hang.
func TestGracefulStopWalksTheWholeLadder(t *testing.T) {
	// Under /tmp, not t.TempDir(): the REST socket lives here and macOS caps
	// unix socket paths near 104 bytes (same constraint as shortStateRoot).
	dir, err := os.MkdirTemp("/tmp", "sproutgs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// Like a vfkit whose guest refuses to power off: acknowledges, does nothing.
	sock := filepath.Join(dir, "vfkit-rest.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	restCalled := make(chan struct{}, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case restCalled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// The ladder's order is what matters here, not the production grace periods.
	origRest, origTerm := restStopWait, sigtermWait
	restStopWait, sigtermWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { restStopWait, sigtermWait = origRest, origTerm })

	cmd, waitCh := startFakeRunner(t, "trap '' TERM; sleep 300; :")

	done := make(chan struct{})
	go func() {
		gracefulStop(sock, cmd, waitCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gracefulStop hung instead of escalating to SIGKILL")
	}
	select {
	case <-restCalled:
	default:
		t.Fatal("REST stop was never attempted")
	}
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runner survived the whole ladder")
	}
}
