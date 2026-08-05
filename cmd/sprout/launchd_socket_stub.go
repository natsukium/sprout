//go:build !(darwin && cgo)

package main

import (
	"fmt"
	"net"
)

// launchd socket activation reaches libSystem through cgo, so a build without
// it keeps compiling and only refuses the one flag that needs the call — this
// path is for `CGO_ENABLED=0` builds and cross-compiles, where there is no
// launchd to check in with anyway.
func launchdListeners(name string) ([]net.Listener, error) {
	return nil, fmt.Errorf("--launchd-socket %s needs a cgo-enabled darwin build", name)
}
