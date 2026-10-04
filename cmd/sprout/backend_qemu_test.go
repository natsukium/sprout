package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const qmpGreeting = `{"QMP": {"version": {"qemu": {"micro": 3, "minor": 0, "major": 11}, "package": ""}, "capabilities": ["oob"]}}`

// A QMP monitor on a short socket path. answer receives each command after the
// greeting and returns the lines to send back; returning nil closes the
// connection without a reply, as QEMU can on quit.
type fakeQMP struct {
	sock     string
	greeting string
	answer   func(command string) []string
	commands chan string
}

func serveFakeQMP(t *testing.T, answer func(command string) []string) *fakeQMP {
	t.Helper()
	return serveFakeMonitor(t, qmpGreeting, answer)
}

// An empty greeting is a monitor that accepts and then says nothing.
func serveFakeMonitor(t *testing.T, greeting string, answer func(command string) []string) *fakeQMP {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sproutqmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	q := &fakeQMP{sock: filepath.Join(dir, "vm-control.sock"), greeting: greeting, answer: answer, commands: make(chan string, 16)}
	ln, err := net.Listen("unix", q.sock)
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
			go q.session(conn)
		}
	}()
	return q
}

func (q *fakeQMP) session(conn net.Conn) {
	defer conn.Close()
	if q.greeting == "" {
		time.Sleep(time.Minute)
		return
	}
	if _, err := conn.Write([]byte(q.greeting + "\r\n")); err != nil {
		return
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			Execute string `json:"execute"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			return
		}
		q.commands <- req.Execute
		reply := q.answer(req.Execute)
		if reply == nil {
			return
		}
		for _, line := range reply {
			if _, err := conn.Write([]byte(line + "\r\n")); err != nil {
				return
			}
		}
	}
}

func (q *fakeQMP) received(t *testing.T) []string {
	t.Helper()
	var got []string
	for {
		select {
		case c := <-q.commands:
			got = append(got, c)
		default:
			return got
		}
	}
}

func acknowledge(string) []string { return []string{`{"return": {}}`} }

func TestQMPGracefulStopNegotiatesThenPowersDown(t *testing.T) {
	q := serveFakeQMP(t, func(command string) []string {
		if command == "system_powerdown" {
			return []string{
				`{"timestamp": {"seconds": 1, "microseconds": 2}, "event": "POWERDOWN"}`,
				`{"return": {}}`,
			}
		}
		return acknowledge(command)
	})
	if err := (qmpControl{}).requestStop(q.sock, false); err != nil {
		t.Fatalf("graceful stop failed: %v", err)
	}
	if got := strings.Join(q.received(t), " "); got != "qmp_capabilities system_powerdown" {
		t.Fatalf("QMP commands = %q, want capabilities negotiation then system_powerdown", got)
	}
}

func TestQMPHardStopQuits(t *testing.T) {
	q := serveFakeQMP(t, acknowledge)
	if err := (qmpControl{}).requestStop(q.sock, true); err != nil {
		t.Fatalf("hard stop failed: %v", err)
	}
	if got := strings.Join(q.received(t), " "); got != "qmp_capabilities quit" {
		t.Fatalf("QMP commands = %q, want capabilities negotiation then quit", got)
	}
}

func TestQMPQuitCountsAConnectionClosedBeforeTheReplyAsDone(t *testing.T) {
	q := serveFakeQMP(t, func(command string) []string {
		if command == "quit" {
			return nil
		}
		return acknowledge(command)
	})
	if err := (qmpControl{}).requestStop(q.sock, true); err != nil {
		t.Fatalf("quit answered by QEMU exiting reported %v, want success", err)
	}
}

// Only quit is expected to end the connection: a powerdown that gets none is a
// monitor that went away, and the ladder must move on to signals.
func TestQMPPowerdownWithoutAReplyFails(t *testing.T) {
	q := serveFakeQMP(t, func(command string) []string {
		if command == "system_powerdown" {
			return nil
		}
		return acknowledge(command)
	})
	if err := (qmpControl{}).requestStop(q.sock, false); err == nil {
		t.Fatal("powerdown with the connection closed instead of a reply reported success")
	}
}

func TestQMPErrorReplyFailsWithItsDescription(t *testing.T) {
	q := serveFakeQMP(t, func(command string) []string {
		if command == "system_powerdown" {
			return []string{`{"error": {"class": "GenericError", "desc": "guest is not running"}}`}
		}
		return acknowledge(command)
	})
	err := (qmpControl{}).requestStop(q.sock, false)
	if err == nil || !strings.Contains(err.Error(), "guest is not running") {
		t.Fatalf("error = %v, want the monitor's description", err)
	}
}

func TestQMPRefusedCapabilitiesStopsBeforeTheCommand(t *testing.T) {
	q := serveFakeQMP(t, func(string) []string {
		return []string{`{"error": {"class": "CommandNotFound", "desc": "no"}}`}
	})
	if err := (qmpControl{}).requestStop(q.sock, true); err == nil {
		t.Fatal("refused capabilities negotiation reported success")
	}
	if got := q.received(t); len(got) != 1 {
		t.Fatalf("QMP commands = %v, want only qmp_capabilities", got)
	}
}

func TestQMPRejectsAPeerThatIsNotAMonitor(t *testing.T) {
	q := serveFakeMonitor(t, `{"hello": "world"}`, acknowledge)
	if err := (qmpControl{}).requestStop(q.sock, false); err == nil || !strings.Contains(err.Error(), "not a QMP monitor") {
		t.Fatalf("error = %v, want a not-a-monitor refusal", err)
	}
	if got := q.received(t); len(got) != 0 {
		t.Fatalf("sent %v to a peer that is not a monitor", got)
	}
}

// QEMU's monitor holds a second client without a greeting while the first is
// connected; the stop must give up rather than hang the ladder.
func TestQMPGivesUpOnAMonitorThatNeverGreets(t *testing.T) {
	orig := qmpTimeout
	qmpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { qmpTimeout = orig })

	q := serveFakeMonitor(t, "", acknowledge)
	start := time.Now()
	if err := (qmpControl{}).requestStop(q.sock, false); err == nil {
		t.Fatal("silent monitor reported success")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stop waited %v on a silent monitor", took)
	}
}

func TestQMPWithNoMonitorFails(t *testing.T) {
	if err := (qmpControl{}).requestStop(filepath.Join(t.TempDir(), "absent.sock"), false); err == nil {
		t.Fatal("stop with no monitor socket reported success")
	}
}

// A guest that ignores the power button still ends up stopped: powerdown, then
// SIGTERM, then SIGKILL for a runner deaf to it.
func TestGracefulStopOverQMPWalksTheWholeLadder(t *testing.T) {
	origStop, origTerm := controlStopWait, sigtermWait
	controlStopWait, sigtermWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { controlStopWait, sigtermWait = origStop, origTerm })

	q := serveFakeQMP(t, acknowledge)
	cmd, exit := startFakeRunner(t, "trap '' TERM; sleep 300; :")

	done := make(chan struct{})
	go func() {
		gracefulStop(qmpControl{}, q.sock, cmd, exit)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gracefulStop hung instead of escalating to SIGKILL")
	}
	if !exit.within(5 * time.Second) {
		t.Fatal("runner survived the whole ladder")
	}
	if got := strings.Join(q.received(t), " "); got != "qmp_capabilities system_powerdown" {
		t.Fatalf("QMP commands = %q, want the powerdown attempted first", got)
	}
}

// QEMU exiting on quit ends the hard stop at once, without the signal ladder
// a runner deaf to SIGTERM would otherwise stretch out.
func TestHardStopOverQMPEndsWhenQEMUQuits(t *testing.T) {
	origTerm := sigtermWait
	sigtermWait = time.Minute
	t.Cleanup(func() { sigtermWait = origTerm })

	cmd, exit := startFakeRunner(t, "trap '' TERM; sleep 300; :")
	quit := make(chan struct{})
	q := serveFakeQMP(t, func(command string) []string {
		if command == "quit" {
			close(quit)
			_ = cmd.Process.Kill()
			return nil
		}
		return acknowledge(command)
	})

	done := make(chan struct{})
	go func() {
		hardStop(qmpControl{}, q.sock, cmd, exit)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hardStop did not return once QEMU quit")
	}
	select {
	case <-quit:
	default:
		t.Fatal("QEMU was never asked to quit")
	}
}

// A QEMU orphan answering its monitor is powered down through it rather than
// signalled, the same way vfkit's REST socket serves the reaper.
func TestReapOrphansPowersAQEMUOrphanDownOverQMP(t *testing.T) {
	dir := t.TempDir()
	m := contractFor(t, "x86_64-linux", "qemu", nil)
	poweredDown := make(chan struct{}, 1)
	q := serveFakeQMP(t, func(command string) []string {
		if command == "system_powerdown" {
			poweredDown <- struct{}{}
		}
		return acknowledge(command)
	})
	sockDir := filepath.Dir(q.sock)
	socks := socketsFor(t, sockDir, m)
	if socks.vmControl != q.sock {
		t.Fatalf("control socket resolved to %s, want %s", socks.vmControl, q.sock)
	}

	survivor := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 300; :",
		"stream,id=n0,server=off,addr.type=unix,addr.path="+filepath.Join(sockDir, "net.sock"))
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = survivor.Wait(); close(exited) }()
	t.Cleanup(func() { _ = survivor.Process.Kill() })
	go func() {
		select {
		case <-poweredDown:
			_ = survivor.Process.Kill()
		case <-exited:
		}
	}()

	start := time.Now()
	if err := reapOrphans(dir, socks, m); err != nil {
		t.Fatalf("reapOrphans failed: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the QEMU orphan survived reapOrphans")
	}
	// The SIGTERM fallback would have waited out its 10s on a TERM-deaf orphan.
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("reap took %v, so it reached the signal fallback instead of the QMP powerdown", took)
	}
	if got := strings.Join(q.received(t), " "); got != "qmp_capabilities system_powerdown" {
		t.Fatalf("QMP commands = %q, want a powerdown", got)
	}
}

func TestStdioConsoleStartsEachBootEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte("previous boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := (stdioConsole{}).writer(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("[    0.000000] Linux version\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "[    0.000000] Linux version\n" {
		t.Fatalf("console.log = %q, want only this boot's output", got)
	}
}

func TestQemuRunnerFailureHint(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	locked := "qemu-system-x86_64: -drive id=vda,format=raw,file=var.img,if=none: Failed to get \"write\" lock\nIs another process using the image [var.img]?\n"
	if got := qemuRunnerFailureHint(locked, dir); !strings.Contains(got, filepath.Join(dir, "var.img")) {
		t.Errorf("image-lock failure hinted %q, want it to name the disk image", got)
	}
	kvm := "Could not access KVM kernel module: Permission denied\nqemu-system-x86_64: failed to initialize kvm: Permission denied\n"
	if got := qemuRunnerFailureHint(kvm, dir); !strings.Contains(got, "sprout doctor") {
		t.Errorf("KVM failure hinted %q, want a pointer to sprout doctor", got)
	}
	if got := qemuRunnerFailureHint("qemu-system-x86_64: -m 0: Invalid memory size\n", dir); got != "" {
		t.Errorf("unrelated failure hinted %q, want none", got)
	}
}

// With a stdio console runner.log is byte for byte console.log, so `logs`
// shows it once; vfkit's PTY console is a separate stream worth both.
func TestLogsShowTheRunnerLogOnlyWhenTheConsoleIsSeparate(t *testing.T) {
	for backend, want := range map[string]string{
		"qemu":  "console.log",
		"vfkit": "runner.log console.log",
		"":      "runner.log console.log",
	} {
		if got := strings.Join(logsToShow(backend), " "); got != want {
			t.Errorf("logs for %q backend = %q, want %q", backend, got, want)
		}
	}
}
