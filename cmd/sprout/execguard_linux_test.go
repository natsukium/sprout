package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processesMatching(marker string) []string {
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !strings.Contains(string(cmdline), marker) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		if fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:])); len(fields) > 0 && fields[0] == "Z" {
			continue
		}
		found = append(found, e.Name()+": "+strings.ReplaceAll(string(cmdline), "\x00", " "))
	}
	return found
}

// A dropped session takes the whole command tree with it, even a command that
// ignores TERM, wherever the disconnect lands in the guard's startup.
func TestExecGuardStopsCommandTreeWhenSessionDies(t *testing.T) {
	requireGuardTools(t)
	// A disconnect before the login shell reads $PPID leaves it pointing at the
	// subreaper rather than init, which the guard cannot tell from a live sshd.
	// The guest has no subreaper; only the afterStart cases hold on such hosts.
	subreaped := !orphansReparentToInit(t)

	n := 0
	delays := []time.Duration{-1, 0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond}
	for _, shell := range loginShells(t) {
		for _, delay := range delays {
			for _, termDeaf := range []bool{false, true} {
				n++
				seq := n
				name := fmt.Sprintf("%s/delay=%v/termDeaf=%v", filepath.Base(shell), delay, termDeaf)
				if delay < 0 {
					name = fmt.Sprintf("%s/afterStart/termDeaf=%v", filepath.Base(shell), termDeaf)
				}
				t.Run(name, func(t *testing.T) {
					if subreaped && delay >= 0 {
						t.Skip("orphans here are adopted by a subreaper, not init as in the guest")
					}
					t.Parallel()
					marker := fmt.Sprintf("4242.%d%03d", time.Now().UnixNano()%1_000_000_000, seq)
					ready := filepath.Join(t.TempDir(), "ready")
					trap := ""
					if termDeaf {
						trap = `trap "" TERM; `
					}
					body := trap + `sleep ` + marker + ` & touch ` + ready + `; while :; do sleep 1; done`
					remote := remoteCommand([]string{"sh", "-c", body, "guest-" + marker}, false, true)
					t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", marker).Run() })

					sshd := exec.Command("/bin/sh", "-c", `"$@"; :`, "sshd-stand-in", shell, "-c", remote)
					if err := sshd.Start(); err != nil {
						t.Fatal(err)
					}
					if delay < 0 {
						waitForFile(t, ready)
					} else {
						time.Sleep(delay)
					}
					_ = sshd.Process.Signal(syscall.SIGKILL)
					_ = sshd.Wait()

					// TERM grace (5 s) plus slack.
					deadline := time.Now().Add(8 * time.Second)
					for {
						left := processesMatching(marker)
						if len(left) == 0 {
							return
						}
						if time.Now().After(deadline) {
							t.Fatalf("command outlived its session:\n%s", strings.Join(left, "\n"))
						}
						time.Sleep(100 * time.Millisecond)
					}
				})
			}
		}
	}
}

func orphansReparentToInit(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "sleep 5 </dev/null >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(out))
	defer exec.Command("kill", pid).Run() //nolint:errcheck
	stat, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 1 && fields[1] == "1"
}

// A login shell that starts after sshd-session died must not start the command.
func TestExecGuardRefusesSessionThatDiedBeforeShellStarted(t *testing.T) {
	requireGuardTools(t)
	if !orphansReparentToInit(t) {
		t.Skip("orphans here are adopted by a subreaper, not init as in the guest")
	}

	dir := t.TempDir()
	gate, waiting, passed, ran := filepath.Join(dir, "gate"), filepath.Join(dir, "waiting"), filepath.Join(dir, "passed"), filepath.Join(dir, "ran")
	remote := remoteCommand([]string{"touch", ran}, false, true)
	sshd := exec.Command("/bin/sh", "-c", `"$@"; :`, "sshd-stand-in",
		"/bin/sh", "-c", `echo $$ >"$1"; while [ ! -e "$0" ]; do sleep 0.01; done; touch "$2"; shift 2; exec "$@"`, gate, waiting, passed, "/bin/sh", "-c", remote)
	if err := sshd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, waiting)
	_ = sshd.Process.Signal(syscall.SIGKILL)
	_ = sshd.Wait()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	waitForFile(t, passed)
	// The gated shell execs its way into the guard, so its pid is the guard's.
	guard, err := os.ReadFile(waiting)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(guard)), "stat"))
		if err != nil || strings.Contains(string(stat[strings.LastIndexByte(string(stat), ')')+1:]), " Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command started although its session was already gone")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitUntilGone(t *testing.T, marker string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := processesMatching(marker)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running:\n%s", strings.Join(left, "\n"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSSHDStandIn is a helper process, not a test: like sshd-session it holds
// the read end of the command's output pipe.
func TestSSHDStandIn(t *testing.T) {
	remote := os.Getenv("SPROUT_TEST_SSHD_REMOTE")
	if remote == "" {
		t.Skip("helper process only")
	}
	// PDEATHSIG follows the forking thread, not the process.
	runtime.LockOSThread()
	r, w, err := os.Pipe()
	if err != nil {
		os.Exit(2)
	}
	cmd := exec.Command("/bin/sh", "-c", remote)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	w.Close()
	io.Copy(io.Discard, r) //nolint:errcheck
	_ = cmd.Wait()
	os.Exit(0)
}

func startWithSessionOutput(t *testing.T, remote string) *exec.Cmd {
	t.Helper()
	sshd := exec.Command(os.Args[0], "-test.run=^TestSSHDStandIn$")
	sshd.Env = append(os.Environ(), "SPROUT_TEST_SSHD_REMOTE="+remote)
	if err := sshd.Start(); err != nil {
		t.Fatal(err)
	}
	return sshd
}

func requireGuardTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"setpriv", "setsid", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available; the guard runs unguarded without it", tool)
		}
	}
}

// A backgrounded job still writing to the session is stopped on disconnect.
func TestExecGuardStopsLeftoverJobHoldingSessionOutput(t *testing.T) {
	requireGuardTools(t)
	marker := fmt.Sprintf("4343.%d", time.Now().UnixNano()%1_000_000_000)
	ready := filepath.Join(t.TempDir(), "ready")
	t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", marker).Run() })

	sshd := startWithSessionOutput(t, remoteCommand([]string{"sh", "-c", "sleep " + marker + " & touch " + ready}, false, true))
	waitForFile(t, ready)
	// Let the command exit so the guard is waiting on the leftover job.
	time.Sleep(1500 * time.Millisecond)
	_ = sshd.Process.Signal(syscall.SIGKILL)
	_ = sshd.Wait()

	waitUntilGone(t, marker, 8*time.Second)
}

// A job that let go of the session's output keeps running, and the session
// ends with the command.
func TestExecGuardLeavesDetachedJobAlone(t *testing.T) {
	requireGuardTools(t)
	marker := fmt.Sprintf("4444.%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", marker).Run() })

	start := time.Now()
	sshd := startWithSessionOutput(t, remoteCommand([]string{"sh", "-c", "sleep " + marker + " </dev/null >/dev/null 2>&1 &"}, false, true))
	if err := sshd.Wait(); err != nil {
		t.Fatalf("session: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("session took %v to end after the command exited", took)
	}
	time.Sleep(time.Second)
	if len(processesMatching(marker)) == 0 {
		t.Error("the detached job was killed")
	}
}
