package main

import (
	"os/exec"
	"runtime"
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
		started <- nil
		exit.err = cmd.Wait()
		close(exit.done)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return exit, nil
}
