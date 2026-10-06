package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The child *is* the daemon, so the reaping awaitBootOrReady does while racing
// its exit is what keeps a long-lived caller from collecting zombies.
func bootDetached(id string, childArgs []string, supersededPID int, what string, createDir bool, announce func(logPath string), watch *wakeWatch) error {
	dir, err := instanceDir(id)
	if err != nil {
		return err
	}
	logf, err := scaffoldDetached(id, dir, createDir)
	if err != nil {
		return err
	}
	defer logf.Close()
	logPath := upLogPath(dir)

	child, err := backgroundSelf(childArgs, logf)
	if err != nil {
		return err
	}
	wait := child.Wait
	if watch == nil {
		err = child.Start()
	} else {
		err = watch.launch(func(report *os.File) (*os.Process, error) {
			child.ExtraFiles = []*os.File{report}
			child.Env = append(os.Environ(), claimFDEnv+"=3")
			if err := child.Start(); err != nil {
				return nil, err
			}
			return child.Process, nil
		})
		wait = func() error {
			defer close(watch.exited)
			return child.Wait()
		}
	}
	if err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	if announce != nil {
		announce(logPath)
	}
	return awaitBootOrReady(wait, id, supersededPID, logPath, what)
}

// Scaffolding under the lifecycle lock keeps the dir and log from landing
// inside a directory a delete is renaming away. `up` is a create command;
// `start` is not, so for it a missing directory means no instance.
func scaffoldDetached(id, dir string, create bool) (*os.File, error) {
	lc, err := acquireLifecycleLock(id)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	logf, err := os.Create(upLogPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &instanceNotFoundError{selector: id}
		}
		return nil, err
	}
	return logf, nil
}

func launchDetached(id *Identity, selector string, childArgs []string, action, what string, createDir bool) error {
	// Captured before the child replaces it: an in-place `up` reboot keeps the
	// current VM answering through the rebuild, so readiness must wait for a
	// *different* daemon, not just any.
	supersededPID := runningPID(id.ID)
	err := bootDetached(id.ID, childArgs, supersededPID, what, createDir, func(logPath string) {
		fmt.Printf("%s %q in the background (log: %s) …\n", action, id.Display(), logPath)
	}, nil)
	if err != nil {
		return err
	}
	fmt.Printf("VM ready. Enter it with: %s\n", withSelector("sprout shell", selector))
	return nil
}

// The child runs in its own process group so a Ctrl-C aimed at the foreground
// command never reaches a daemon meant to outlive it, and neither does the
// SIGHUP of the terminal closing: a hangup signals only the session leader
// and the foreground group, so no new session is needed. It also starts without
// the caller's descriptors: a caller that serializes boots with `flock 9> lock`
// around `sprout up` would otherwise never get the lock back, since the lock
// belongs to the open file description the daemon and its runner inherited.
func backgroundSelf(args []string, logf *os.File) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dropInheritedDescriptors()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// Go opens its own files close-on-exec already, so what this marks is the
// caller's. The table is scanned rather than listed from /dev/fd: on darwin
// that directory yields 0-2 and then a readdir error, which would silently skip
// the descriptors this exists to catch. fcntl on an unopened one is a no-op.
func dropInheritedDescriptors() {
	for fd := 3; fd < descriptorTableSize(); fd++ {
		unix.CloseOnExec(fd)
	}
}

// Caps the scan: the RLIMIT_NOFILE soft limit can be raised into the millions
// and a boot should not spend that in fcntl, while the shell redirections this
// guards against use single-digit descriptors.
const maxScannedDescriptor = 1 << 16

func descriptorTableSize() int {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil || lim.Cur > maxScannedDescriptor {
		return maxScannedDescriptor
	}
	return int(lim.Cur)
}

func runningPID(id string) int {
	if info, err := queryInfoBrief(id); err == nil {
		return info.PID
	}
	return 0
}

// The child's exit is raced rather than joined: on the success path the child
// *is* the daemon and never exits; an error exit is a boot failure reported
// immediately rather than sitting out the readiness timeout.
//
// A clean exit before readiness means the child found a daemon already
// running and handed off to it, so readiness is then checked against *any*
// live daemon rather than the supersededPID filter — the superseded daemon is
// exactly the one still serving on this path.
func awaitBootOrReady(wait func() error, id string, supersededPID int, logPath, what string) error {
	exited := make(chan error, 1)
	go func() { exited <- wait() }()
	ready := make(chan error, 1)
	go func() { ready <- waitInstanceReady(id, supersededPID) }()

	select {
	case err := <-exited:
		if err != nil {
			if logPath == "" {
				return fmt.Errorf("%s failed: %w", what, err)
			}
			err = fmt.Errorf("%s failed, see %s: %w", what, logPath, err)
			// Quoted verbatim rather than classified: matching the log against
			// phase markers would couple this to nix's and vfkit's output.
			if line := lastLogLine(logPath); line != "" {
				err = fmt.Errorf("%w\n  %s", err, line)
			}
			return err
		}
		if supersededPID == 0 {
			return <-ready
		}
		return waitInstanceReady(id, 0)
	case err := <-ready:
		return err
	}
}

// The child's "sprout: " prefix is stripped because the parent adds its own.
func lastLogLine(path string) string {
	tail := strings.TrimSpace(runnerLogTail(path, runnerLogTailBytes))
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	return strings.TrimPrefix(line, "sprout: ")
}
