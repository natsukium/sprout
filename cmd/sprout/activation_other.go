//go:build !(darwin && cgo) && !linux

package main

import (
	"fmt"
	"net"
	"runtime"
)

// launchd socket activation reaches libSystem through cgo, so a build without
// it keeps compiling and only refuses the one flag that needs the call — this
// path is for `CGO_ENABLED=0` builds and cross-compiles, where there is no
// launchd to check in with anyway.
func activatedListeners(name string) ([]net.Listener, error) {
	if runtime.GOOS == "darwin" {
		return nil, fmt.Errorf("--activated-socket %s needs a cgo-enabled darwin build", name)
	}
	return nil, fmt.Errorf("--activated-socket %s needs launchd (macOS) or systemd (Linux) to hand the socket over", name)
}
