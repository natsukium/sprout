package main

// AF_UNIX addresses are copied into sockaddr_un.sun_path, 104 bytes on macOS
// (108 on Linux), and a longer one fails as a bare EINVAL. Instance
// directories sit under the state root, whose depth is unbounded — a
// self-hosted CI runner's home already overflowed it — so every bind and dial
// goes through /tmp/sprout-<uid>/<hash of the instance directory>, a symlink
// to the instance directory, and the socket files themselves stay in its
// sock/ subdirectory. Not named by the instance ID, which repeats across state
// roots: one shared name would let each root's daemon unlink (Go's listener
// close included) and reap what the other bound through it. A symlink rather
// than a directory holding the sockets, because macOS purges /tmp entries left
// unused for a few days: a purged symlink is recreated by the next bind or
// dial, while a purged socket file would cut off a live daemon.
//
// Older sprouts bound through the ID-named link straight into the instance
// directory, and their daemons keep unlinking and dialing through that link
// after another state root's older sprout re-points it. Sockets in sock/ are
// out of their reach.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// macOS's 104-byte sun_path, the tighter of the two platforms, less the NUL.
const maxSocketPathLen = 103

const (
	netSocketName     = "net.sock"
	controlSocketName = "control.sock"
	socketSubdir      = "sock"
)

func socketDirBase() string {
	return fmt.Sprintf("/tmp/sprout-%d", os.Geteuid())
}

func socketPath(instDir, name string) (string, error) {
	dir, err := ensureSocketDir(socketDirBase(), instDir)
	if err != nil {
		return "", err
	}
	return socketPathIn(dir, name)
}

// Checked even though the short directory makes overflow implausible: an
// error naming the path and the limit beats bind's bare `invalid argument`.
func socketPathIn(sockDir, name string) (string, error) {
	p := filepath.Join(sockDir, name)
	if len(p) > maxSocketPathLen {
		return "", fmt.Errorf("socket path %s is %d bytes, over the %d-byte AF_UNIX path limit", p, len(p), maxSocketPathLen)
	}
	return p, nil
}

// The daemon binding and every client dialing must arrive at the same name, so
// all of them derive it here, from the instance directory alone.
func socketLinkIn(base, instDir string) string {
	return filepath.Join(base, hashID(instDir))
}

func socketDirIn(base, instDir string) string {
	return filepath.Join(socketLinkIn(base, instDir), socketSubdir)
}

// Leaves sock/ to the daemon (prepareSocketDir): a client dialing an instance
// that was never booted, or is being deleted, must not create directories in
// it.
func ensureSocketDir(base, instDir string) (string, error) {
	if err := ensurePrivateDir(base); err != nil {
		return "", err
	}
	if err := ensureSymlink(socketLinkIn(base, instDir), instDir); err != nil {
		return "", err
	}
	return socketDirIn(base, instDir), nil
}

func prepareSocketDir(base, instDir string) (string, error) {
	if err := os.Mkdir(filepath.Join(instDir, socketSubdir), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return ensureSocketDir(base, instDir)
}

// A daemon started by an older sprout bound control.sock in the instance
// directory itself. Reached through this instance's own link, never the
// ID-named one, which may lead to another state root's daemon.
func preUpgradeControlSocket(instDir string) (string, bool) {
	fi, err := os.Lstat(filepath.Join(instDir, controlSocketName))
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return "", false
	}
	p, err := socketPathIn(socketLinkIn(socketDirBase(), instDir), controlSocketName)
	return p, err == nil
}

// A daemon started by an older sprout reaches its own VM through the ID-named
// link, which its sprout re-pointed at the instance before every dial to it;
// left pointing at another state root, a STOP would power off that root's VM.
// Only for a daemon that answered, so a stale socket never re-points it.
func pointIDLinkAt(instDir string) error {
	return ensureSymlink(filepath.Join(socketDirBase(), filepath.Base(instDir)), instDir)
}

// /tmp is world-writable, so whatever sits at base cannot be trusted: another
// user may have pre-created it to redirect where sockets are bound. A loose
// mode is tightened rather than rejected, since only the owner can have set it.
func ensurePrivateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("%s exists but is not a directory; remove it and retry", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify the owner of %s", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not this user (uid %d); remove it and retry", path, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(path, 0o700)
	}
	return nil
}

// An existing link is re-pointed rather than trusted: whatever sits at the
// name in world-writable /tmp was not necessarily made for this directory.
// The rename keeps a concurrent dial from catching the name missing.
func ensureSymlink(link, target string) error {
	if err := os.Symlink(target, link); err == nil || !errors.Is(err, os.ErrExist) {
		return err
	}
	if existing, err := os.Readlink(link); err == nil && existing == target {
		return nil
	}
	tmp := fmt.Sprintf("%s.tmp.%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Best effort: a leftover link dangles harmlessly.
//
// The ID-named links an older sprout made are left alone: an older sprout of
// any state root may re-point one between checking its target and unlinking
// it, no lock spans state roots or versions, and an older daemon still dials
// its runner through it.
func removeSocketDir(instDir string) {
	_ = os.Remove(socketLinkIn(socketDirBase(), instDir))
}

// The paths ensureSocketDir would yield, computed without creating anything,
// so an over-long socket refuses a boot before it stops or rewrites anything.
func checkInstanceSocketPaths(instDir string, m *Manifest) error {
	_, err := resolveInstanceSockets(socketDirIn(socketDirBase(), instDir), m)
	return err
}
