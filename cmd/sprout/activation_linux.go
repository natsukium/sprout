package main

// sd_listen_fds(3) by hand: three environment variables and a fixed first
// descriptor do not warrant a dependency on go-systemd.

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
	// A woken instance's daemon inherits this environment and would read the
	// variables as addressed to itself.
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(v)
	}

	all, fds, err := systemdListenFDs(pidEnv, countEnv, namesEnv, os.Getpid(), name)
	// systemd passes them without close-on-exec; one leaked into a woken
	// instance's daemon keeps the port bound after the socket unit stops.
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
