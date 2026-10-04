package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A zombie still answers signal 0, so the state field decides.
func processAlive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(stat)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

func waitProcessGone(t *testing.T, pid int, what string) {
	t.Helper()
	if !pollUntil(10*time.Second, 20*time.Millisecond, func() bool { return !processAlive(pid) }) {
		t.Fatalf("%s (pid %d) is still running", what, pid)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	waitForFile(t, path)
	var pid int
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool {
		data, _ := os.ReadFile(path)
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		pid = n
		return err == nil
	}) {
		t.Fatalf("no pid in %s", path)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func writePIDFile(dir, name string, pid int) {
	tmp := filepath.Join(dir, name+".tmp")
	if os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600) != nil || os.Rename(tmp, filepath.Join(dir, name)) != nil {
		os.Exit(2)
	}
}

// Go never ends the main thread, so a goroutine that exits locked to it leaves
// the thread running; keeping the main goroutine there puts the stand-in's
// goroutine on a thread that does end.
func init() {
	if os.Getenv("SPROUT_TEST_MANAGED_PARENT") != "" {
		runtime.LockOSThread()
	}
}

// TestManagedParentStandIn is a helper process, not a test: a daemon that
// starts a managed child from a goroutine whose locked thread then exits.
func TestManagedParentStandIn(t *testing.T) {
	dir := os.Getenv("SPROUT_TEST_MANAGED_PARENT")
	if dir == "" {
		t.Skip("helper process only")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Never unlocked, so this thread ends with the goroutine.
		runtime.LockOSThread()
		if syscall.Gettid() == os.Getpid() {
			os.Exit(3)
		}
		naive := exec.Command("sleep", "300")
		naive.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := naive.Start(); err != nil {
			os.Exit(2)
		}
		managed := exec.Command("sleep", "300")
		if _, err := startManaged(managed); err != nil {
			os.Exit(2)
		}
		writePIDFile(dir, "naive", naive.Process.Pid)
		writePIDFile(dir, "managed", managed.Process.Pid)
	}()
	<-done
	time.Sleep(time.Hour)
}

// The runner must not outlive a crashed daemon, and must not die with
// whichever daemon thread happened to start it: PDEATHSIG tracks the thread.
func TestManagedProcessDiesWithTheDaemonNotWithAThread(t *testing.T) {
	dir := t.TempDir()
	parent := exec.Command(os.Args[0], "-test.run=^TestManagedParentStandIn$")
	parent.Env = append(os.Environ(), "SPROUT_TEST_MANAGED_PARENT="+dir)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })

	naive := readPIDFile(t, filepath.Join(dir, "naive"))
	managed := readPIDFile(t, filepath.Join(dir, "managed"))

	// The naive child going proves the starting goroutine's thread has exited.
	waitProcessGone(t, naive, "the child started directly on the exited thread")
	if !processAlive(managed) {
		t.Fatal("the managed child died with the thread that asked for it, while its daemon still runs")
	}

	if err := parent.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	waitProcessGone(t, managed, "the managed child of a SIGKILLed daemon")
}

// TestDetachedChildStandIn is a helper process, not a test: a daemon, or the
// foreground command it stands beside.
func TestDetachedChildStandIn(t *testing.T) {
	if os.Getenv("SPROUT_TEST_DETACHED_CHILD") == "" {
		t.Skip("helper process only")
	}
	time.Sleep(time.Hour)
}

// TestTerminalSessionStandIn is a helper process, not a test: the session
// leader of a terminal, from which `sprout up` detaches its daemon.
func TestTerminalSessionStandIn(t *testing.T) {
	dir := os.Getenv("SPROUT_TEST_TERMINAL_SESSION")
	if dir == "" {
		t.Skip("helper process only")
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		os.Exit(2)
	}
	os.Setenv("SPROUT_TEST_DETACHED_CHILD", "1")
	args := []string{"-test.run=^TestDetachedChildStandIn$"}
	daemon, err := backgroundSelf(args, devnull)
	if err != nil || daemon.Start() != nil {
		os.Exit(2)
	}
	foreground := exec.Command(os.Args[0], args...)
	foreground.Stdout, foreground.Stderr = devnull, devnull
	if foreground.Start() != nil {
		os.Exit(2)
	}
	writePIDFile(dir, "daemon", daemon.Process.Pid)
	writePIDFile(dir, "foreground", foreground.Process.Pid)
	time.Sleep(time.Hour)
}

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// Closing a terminal hangs up its session: the leader and the foreground
// process group get SIGHUP. A detached daemon is in neither and keeps running.
func TestDetachedDaemonSurvivesTerminalHangup(t *testing.T) {
	master, slave := openPTY(t)
	dir := t.TempDir()
	leader := exec.Command(os.Args[0], "-test.run=^TestTerminalSessionStandIn$")
	leader.Env = append(os.Environ(), "SPROUT_TEST_TERMINAL_SESSION="+dir)
	leader.Stdin, leader.Stdout, leader.Stderr = slave, slave, slave
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	t.Cleanup(func() { _ = leader.Process.Kill(); _ = leader.Wait() })

	daemon := readPIDFile(t, filepath.Join(dir, "daemon"))
	foreground := readPIDFile(t, filepath.Join(dir, "foreground"))

	master.Close()
	err := leader.Wait()
	if st, ok := leader.ProcessState.Sys().(syscall.WaitStatus); !ok || !st.Signaled() || st.Signal() != syscall.SIGHUP {
		t.Fatalf("session leader ended with %v, want death by SIGHUP from the hangup", err)
	}
	// The foreground group's SIGHUP is sent as the leader exits, so once it
	// is gone the daemon has had every signal the hangup produces.
	waitProcessGone(t, foreground, "the foreground process of the hung-up terminal")
	if !processAlive(daemon) {
		t.Fatal("the detached daemon died with its terminal")
	}
}

// A leftover writer of var.img (a runner's mkfs outliving its daemon) holds
// the boot until it lets go, and a holder that never does fails the boot by
// name instead of sharing the disk.
func TestAwaitDiskReleasedWaitsForALeftoverHolder(t *testing.T) {
	dir := t.TempDir()
	img := varImagePath(dir)
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := awaitDiskReleased(dir); err != nil {
		t.Fatalf("unheld disk: %v", err)
	}

	orig := diskReleaseWait
	t.Cleanup(func() { diskReleaseWait = orig })
	startHolder := func() *exec.Cmd {
		f, err := os.Open(img)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		holder := exec.Command("sleep", "300")
		holder.Stdin = f
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
		return holder
	}

	stuck := startHolder()
	diskReleaseWait = 300 * time.Millisecond
	err := awaitDiskReleased(dir)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("process %d (sleep)", stuck.Process.Pid)) {
		t.Fatalf("held disk: error = %v, want the holder named", err)
	}
	_ = stuck.Process.Kill()
	_ = stuck.Wait()

	finishing := startHolder()
	diskReleaseWait = time.Minute
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = finishing.Process.Kill()
	}()
	if err := awaitDiskReleased(dir); err != nil {
		t.Fatalf("holder that exits: %v", err)
	}
}
