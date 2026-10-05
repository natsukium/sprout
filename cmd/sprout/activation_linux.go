package main

// Socket activation is how a NixOS host serves :80 without running the router
// as root: Linux refuses a non-root bind below
// net.ipv4.ip_unprivileged_port_start on every address, but a systemd socket
// unit binds it and hands the descriptor to a service running as the user.
//
// This is sd_listen_fds(3) and sd_listen_fds_with_names(3) by hand: the
// protocol is three environment variables and a fixed first descriptor, which
// does not warrant a dependency on go-systemd.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const sdListenFDsStart = 3

var errNotActivated = errors.New("--activated-socket needs sockets systemd handed over, so it only works under a socket unit (services.sprout.route); bind one directly with --port/--bind instead")

func activatedListeners(name string) ([]net.Listener, error) {
	pidEnv, countEnv, namesEnv := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	// A router waking an instance runs `sprout start`, whose detached daemon
	// inherits this environment; left set, that daemon would read the
	// variables as addressed to a process that never received the sockets.
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(v)
	}

	all, fds, err := systemdListenFDs(pidEnv, countEnv, namesEnv, os.Getpid(), name)
	// systemd hands the descriptors over without close-on-exec, and one that
	// leaked into a woken instance's daemon would keep the port bound after
	// the router and its socket unit had both stopped.
	for _, fd := range all {
		syscall.CloseOnExec(fd)
	}
	if err != nil {
		return nil, err
	}

	lns := make([]net.Listener, 0, len(fds))
	for _, fd := range fds {
		f := os.NewFile(uintptr(fd), fmt.Sprintf("systemd-socket:%s", name))
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			closeAll(lns)
			return nil, fmt.Errorf("adopting systemd socket %q (fd %d): %w", name, fd, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// Returns every passed descriptor as well as the ones named name, so the
// caller can mark them all close-on-exec even when none matched.
func systemdListenFDs(pidEnv, countEnv, namesEnv string, pid int, name string) (all, named []int, err error) {
	if pidEnv == "" || countEnv == "" {
		return nil, nil, errNotActivated
	}
	listenPID, err := strconv.Atoi(pidEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("LISTEN_PID=%q is not a pid", pidEnv)
	}
	if listenPID != pid {
		return nil, nil, fmt.Errorf("LISTEN_PID=%d names another process than this one (%d), so the sockets were not handed to it", listenPID, pid)
	}
	count, err := strconv.Atoi(countEnv)
	if err != nil || count < 1 {
		return nil, nil, fmt.Errorf("LISTEN_FDS=%q passes no sockets", countEnv)
	}
	for i := range count {
		all = append(all, sdListenFDsStart+i)
	}

	names := strings.Split(namesEnv, ":")
	if namesEnv == "" || len(names) != count {
		return all, nil, fmt.Errorf("LISTEN_FDNAMES=%q does not name each of the %d sockets; set FileDescriptorName=%s on the socket unit", namesEnv, count, name)
	}
	for i, n := range names {
		if n == name {
			named = append(named, all[i])
		}
	}
	if len(named) == 0 {
		return all, nil, fmt.Errorf("this unit passes no sockets named %q (FileDescriptorName), only %s", name, strings.Join(names, ", "))
	}
	return all, named, nil
}
