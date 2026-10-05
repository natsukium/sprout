package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSidecarStandIn is a helper process, not a test: a sidecar behaving as
// SPROUT_TEST_SIDECAR says ("<mode>:<socket>").
func TestSidecarStandIn(t *testing.T) {
	spec := os.Getenv("SPROUT_TEST_SIDECAR")
	if spec == "" {
		t.Skip("helper process only")
	}
	mode, sock, _ := strings.Cut(spec, ":")
	_ = os.WriteFile(sock+".standin", []byte(strconv.Itoa(os.Getpid())), 0o600)
	fmt.Println("starting")
	switch mode {
	case "serve", "stubborn":
		if mode == "stubborn" {
			signal.Ignore(syscall.SIGTERM)
		}
		if _, err := net.Listen("unix", sock); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	case "fail":
		fmt.Fprintln(os.Stderr, `Error entering sandbox: Unshare(Os { code: 1, kind: PermissionDenied })`)
		os.Exit(1)
	case "silent":
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

func sidecarStandIn(t *testing.T, name, mode string, socks instanceSockets) SidecarSpec {
	t.Helper()
	sock := name + ".sock"
	socks.named[sock] = filepath.Join(socks.dir, sock)
	return SidecarSpec{
		Name: name,
		Exec: []string{"/usr/bin/env", "SPROUT_TEST_SIDECAR=" + mode + ":" + socks.named[sock], os.Args[0], "-test.run=^TestSidecarStandIn$"},
		Ready: struct {
			Socket string `json:"socket"`
		}{Socket: sock},
	}
}

func sidecarSockets(t *testing.T) instanceSockets {
	return instanceSockets{dir: shortSocketDir(t), named: map[string]string{}}
}

func shortenSidecarWaits(t *testing.T) {
	prev := []time.Duration{sidecarReadyTimeout, sidecarExitWait, sidecarTermWait}
	sidecarReadyTimeout, sidecarExitWait, sidecarTermWait = 2*time.Second, 100*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { sidecarReadyTimeout, sidecarExitWait, sidecarTermWait = prev[0], prev[1], prev[2] })
}

func TestSidecarsAreReadyOnceTheirSocketsExistAndStopWithTheVM(t *testing.T) {
	shortenSidecarWaits(t)
	socks := sidecarSockets(t)
	dir := t.TempDir()
	specs := []SidecarSpec{sidecarStandIn(t, "a", "serve", socks), sidecarStandIn(t, "b", "serve", socks)}
	set, err := startSidecars(specs, socks, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range set.procs {
		if _, err := os.Stat(socks.named[p.name+".sock"]); err != nil {
			t.Errorf("%s reported ready without its socket: %v", p.name, err)
		}
	}
	set.stop()
	for _, p := range set.procs {
		select {
		case <-p.exit.done:
		default:
			t.Errorf("%s still running after stop", p.name)
		}
	}
	log, _ := os.ReadFile(sidecarLogPath(dir))
	for _, want := range []string{"[a] starting\n", "[b] starting\n"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("sidecars.log lacks %q:\n%s", want, log)
		}
	}
}

func TestSidecarDyingBeforeItsSocketFailsTheBootWithANamespaceHint(t *testing.T) {
	shortenSidecarWaits(t)
	fakeSysctls(t, map[string]string{"kernel.unprivileged_userns_clone": "0"})
	socks := sidecarSockets(t)
	specs := []SidecarSpec{sidecarStandIn(t, "ok", "serve", socks), sidecarStandIn(t, "broken", "fail", socks)}
	_, err := startSidecars(specs, socks, t.TempDir())
	if err == nil {
		t.Fatal("boot went ahead with a sidecar that exited")
	}
	for _, want := range []string{"broken exited before its socket appeared", "Error entering sandbox", "kernel.unprivileged_userns_clone = 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestFailedSidecarStartStopsTheOnesAlreadyRunning(t *testing.T) {
	shortenSidecarWaits(t)
	socks := sidecarSockets(t)
	first := sidecarStandIn(t, "first", "serve", socks)
	specs := []SidecarSpec{first, sidecarStandIn(t, "broken", "fail", socks)}
	if _, err := startSidecars(specs, socks, t.TempDir()); err == nil {
		t.Fatal("want a boot failure")
	}
	if alive := standInAlive(t, socks.named["first.sock"]); alive {
		t.Fatalf("the sidecar started before the failure is still running")
	}
}

func TestSidecarThatNeverBindsTimesOut(t *testing.T) {
	shortenSidecarWaits(t)
	sidecarReadyTimeout = 300 * time.Millisecond
	socks := sidecarSockets(t)
	_, err := startSidecars([]SidecarSpec{sidecarStandIn(t, "mute", "silent", socks)}, socks, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "mute did not create its socket within") {
		t.Fatalf("err = %v, want a readiness timeout naming the sidecar", err)
	}
	if alive := standInAlive(t, socks.named["mute.sock"]); alive {
		t.Fatalf("the timed-out sidecar is still running")
	}
}

func TestSidecarIgnoringSIGTERMIsKilled(t *testing.T) {
	shortenSidecarWaits(t)
	socks := sidecarSockets(t)
	set, err := startSidecars([]SidecarSpec{sidecarStandIn(t, "stubborn", "stubborn", socks)}, socks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { set.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return for a sidecar ignoring SIGTERM")
	}
}

func TestSidecarExitIsReported(t *testing.T) {
	shortenSidecarWaits(t)
	socks := sidecarSockets(t)
	set, err := startSidecars([]SidecarSpec{sidecarStandIn(t, "a", "serve", socks)}, socks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.stop)
	_ = set.procs[0].cmd.Process.Kill()
	select {
	case p := <-set.exited():
		if p.name != "a" {
			t.Fatalf("reported %q, want a", p.name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a killed sidecar was never reported")
	}
}

// Whether the stand-in for this socket still runs, by the pid it recorded at
// start, so a stop path that lost track of a process is caught.
func standInAlive(t *testing.T, sock string) bool {
	t.Helper()
	b, err := os.ReadFile(sock + ".standin")
	if err != nil {
		t.Fatalf("stand-in never started: %v", err)
	}
	pid, _ := strconv.Atoi(string(b))
	return syscall.Kill(pid, 0) == nil
}

// A sidecar dying under a running guest takes its share away, so the daemon
// must stop the VM and report the sidecar rather than wait for the guest.
func TestLostSidecarStopsTheRunnerAndIsReported(t *testing.T) {
	exit := &runnerExit{done: make(chan struct{})}
	lost := make(chan *sidecar, 1)
	lost <- &sidecar{name: "virtiofsd-workspace"}
	stopped := false
	got := awaitRunnerExit(nil, lost, exit, func() { stopped = true; close(exit.done) })
	if !stopped {
		t.Fatal("the runner was not asked to stop")
	}
	if got == nil || got.name != "virtiofsd-workspace" {
		t.Fatalf("lost sidecar = %v, want virtiofsd-workspace", got)
	}
}

// virtiofsd exits as soon as QEMU disconnects, which can be observed before
// QEMU's own exit; that ordinary shutdown is not a lost sidecar.
func TestSidecarExitingWithTheRunnerIsNotALoss(t *testing.T) {
	exit := &runnerExit{done: make(chan struct{})}
	lost := make(chan *sidecar, 1)
	lost <- &sidecar{name: "virtiofsd-workspace"}
	go func() { time.Sleep(50 * time.Millisecond); close(exit.done) }()
	stopped := false
	if got := awaitRunnerExit(nil, lost, exit, func() { stopped = true }); got != nil || stopped {
		t.Fatalf("reported %v (stop requested: %v) for a sidecar that exited with its runner", got, stopped)
	}
}
