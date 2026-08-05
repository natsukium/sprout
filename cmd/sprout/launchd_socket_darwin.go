//go:build darwin && cgo

package main

// Socket activation is the only way to serve loopback :80 without running as
// root: macOS refuses a non-root bind to a privileged port against a specific
// address, but launchd binds it as root and hands the descriptor to a job
// running as the user — which the router must be, since it reads per-user
// instance records.
//
// Checking in requires launch_activate_socket(3) from libSystem, with no
// stdlib or LISTEN_FDS-style support on macOS, so this is the one place sprout
// uses cgo — kept to a single call so everything above it stays ordinary Go.

/*
#include <launch.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// A key can name more than one socket, an entry resolving to both address
// families yielding one per family, so every descriptor is returned.
func launchdListeners(name string) ([]net.Listener, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var cfds *C.int
	var count C.size_t
	if rc := C.launch_activate_socket(cname, &cfds, &count); rc != 0 {
		return nil, launchdCheckinError(name, syscall.Errno(rc))
	}
	defer C.free(unsafe.Pointer(cfds))

	if count == 0 {
		return nil, fmt.Errorf("launchd Sockets entry %q bound no sockets", name)
	}

	lns := make([]net.Listener, 0, int(count))
	for _, fd := range unsafe.Slice((*C.int)(cfds), int(count)) {
		// net.FileListener dups the descriptor, so this wrapper must not stay
		// open: the listener owns the copy from here on.
		f := os.NewFile(uintptr(fd), fmt.Sprintf("launchd-socket:%s", name))
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			closeAll(lns)
			return nil, fmt.Errorf("adopting launchd socket %q: %w", name, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

func launchdCheckinError(name string, errno syscall.Errno) error {
	switch errno {
	case syscall.ENOENT:
		return fmt.Errorf("this launchd job declares no Sockets entry named %q", name)
	case syscall.ESRCH:
		return fmt.Errorf("--launchd-socket needs a socket launchd handed over, so it only works under launchd (services.sprout.route); bind one directly with --port/--bind instead")
	default:
		return fmt.Errorf("checking in for launchd socket %q: %w", name, errno)
	}
}
