package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// A process the daemon owns for its whole life (the runner, a sidecar) dies
// with the daemon, however the daemon dies, so nothing is left holding
// var.img or a share.
//
// PDEATHSIG fires when the thread that forked the child exits, not the
// process, and Go ends a thread whenever a goroutine exits while locked to it,
// which any code in the daemon may do to the thread that ran Start. So Start
// and Wait run on one locked thread that outlives the child.
//
// SIGTERM rather than SIGKILL: QEMU and virtiofsd exit cleanly on it, which
// leaves the disk image consistent; one that ignores it is still found by the
// next boot's orphan reaper.
//
// Its own process group keeps a terminal's Ctrl-C aimed at a foreground daemon
// from reaching the runner directly: QEMU would quit on SIGINT without the
// guest shutdown the daemon asks for.
func startManaged(cmd *exec.Cmd) (*runnerExit, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM

	exit := &runnerExit{done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		// Before Wait: an unreaped child keeps its pid, so the start time read
		// here cannot belong to a process that reused it.
		exit.proc = procIdentity{pid: cmd.Process.Pid}
		exit.proc.start, _ = procStartTime(cmd.Process.Pid)
		started <- nil
		exit.err = cmd.Wait()
		close(exit.done)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return exit, nil
}

// Each holder as "process <pid> (<comm>)". Only processes this user may
// inspect are seen, which covers anything a runner of this user left.
func varImageHolders(img string) []string {
	target, err := filepath.EvalSymlinks(img)
	if err != nil {
		return nil
	}
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	seen := map[string]bool{}
	var holders []string
	for _, fd := range fds {
		pid := strings.Split(fd, "/")[2]
		if seen[pid] {
			continue
		}
		if link, err := os.Readlink(fd); err != nil || link != target {
			continue
		}
		seen[pid] = true
		comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		holders = append(holders, fmt.Sprintf("process %s (%s)", pid, strings.TrimSpace(string(comm))))
	}
	return holders
}
