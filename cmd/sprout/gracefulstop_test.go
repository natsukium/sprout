package main

import (
	"encoding/json"
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
func startFakeRunner(t *testing.T, script string) (*exec.Cmd, *runnerExit) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, watchRunner(cmd)
}

// A stop path waiting on the runner and the daemon's main select must both
// see one exit: neither may consume it from the other.
func TestRunnerExitIsSeenByEveryObserver(t *testing.T) {
	_, exit := startFakeRunner(t, "exit 3")
	for i := range 2 {
		if !exit.within(5 * time.Second) {
			t.Fatalf("observer %d missed the runner exit", i)
		}
	}
	if exit.err == nil {
		t.Fatal("runner exit status was lost")
	}

	_, running := startFakeRunner(t, "sleep 300; :")
	if running.within(50 * time.Millisecond) {
		t.Fatal("within reported an exit that never happened")
	}
}

// The common daemon-crash shape: the runner never got as far as a REST
// endpoint, so the ladder moves on to SIGTERM and the runner ends up gone.
func TestGracefulStopFallsBackToSigterm(t *testing.T) {
	dir := t.TempDir()
	cmd, exit := startFakeRunner(t, "sleep 300; :")

	done := make(chan struct{})
	go func() {
		gracefulStop(vfkitREST{}, filepath.Join(dir, "vfkit-rest.sock"), cmd, exit)
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
	origRest, origTerm := controlStopWait, sigtermWait
	controlStopWait, sigtermWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { controlStopWait, sigtermWait = origRest, origTerm })

	cmd, exit := startFakeRunner(t, "trap '' TERM; sleep 300; :")

	done := make(chan struct{})
	go func() {
		gracefulStop(vfkitREST{}, sock, cmd, exit)
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
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner survived the whole ladder")
	}
}

// Serves vfkit's REST state endpoint on a socket short enough for macOS,
// handing each requested state to onState.
func serveFakeVfkit(t *testing.T, onState func(state string)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sprouths")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "vfkit-rest.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		onState(body.State)
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// The runner exiting on vfkit's HardStop ends the stop at once, without the
// signal ladder a runner ignoring SIGTERM would otherwise stretch out.
func TestHardStopAsksVfkitForHardStop(t *testing.T) {
	origTerm := sigtermWait
	sigtermWait = time.Minute
	t.Cleanup(func() { sigtermWait = origTerm })

	cmd, exit := startFakeRunner(t, "trap '' TERM; sleep 300; :")
	states := make(chan string, 4)
	sock := serveFakeVfkit(t, func(state string) {
		states <- state
		if state == "HardStop" {
			_ = cmd.Process.Kill()
		}
	})

	done := make(chan struct{})
	go func() {
		hardStop(vfkitREST{}, sock, cmd, exit)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hardStop did not return once vfkit powered the VM off")
	}
	if got := <-states; got != "HardStop" {
		t.Fatalf("vfkit was asked for state %q, want HardStop", got)
	}
}

// A vfkit that acknowledges HardStop but keeps running is still brought down
// by signals, so a hard stop cannot hang.
func TestHardStopFallsBackToSignals(t *testing.T) {
	origHard, origTerm := hardStopWait, sigtermWait
	hardStopWait, sigtermWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { hardStopWait, sigtermWait = origHard, origTerm })

	sock := serveFakeVfkit(t, func(string) {})
	cmd, exit := startFakeRunner(t, "trap '' TERM; sleep 300; :")

	hardStop(vfkitREST{}, sock, cmd, exit)
	if !exit.within(5 * time.Second) {
		t.Fatal("runner survived the hard stop's fallback")
	}
}
