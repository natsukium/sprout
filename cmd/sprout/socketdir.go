package main

// AF_UNIX addresses are copied into sockaddr_un.sun_path, 104 bytes on macOS
// (108 on Linux), and a longer one fails as a bare EINVAL. Instance
// directories sit under the state root, whose depth is unbounded — a
// self-hosted CI runner's home already overflowed it — so every bind and dial
// goes through /tmp/sprout-<uid>/<id>, a symlink to the instance directory
// where the socket files themselves stay. A symlink rather than a directory
// holding the sockets, because macOS purges /tmp entries left unused for a few
// days: a purged symlink is recreated by the next bind or dial, while a purged
// socket file would cut off a live daemon.

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
)

func socketDirBase() string {
	return fmt.Sprintf("/tmp/sprout-%d", os.Geteuid())
}

func socketPath(id, instDir, name string) (string, error) {
	dir, err := ensureSocketDir(socketDirBase(), id, instDir)
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

func ensureSocketDir(base, id, instDir string) (string, error) {
	if err := ensurePrivateDir(base); err != nil {
		return "", err
	}
	link := filepath.Join(base, id)
	if err := ensureSymlink(link, instDir); err != nil {
		return "", err
	}
	return link, nil
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

// An existing link is re-pointed rather than trusted: the id is unique only
// within one state root, so after an XDG_STATE_HOME change it may still point
// at another root's instance. The rename keeps a concurrent dial from catching
// the name missing.
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

// Best effort: a leftover link dangles harmlessly and the next boot of a
// same-id instance re-points it.
func removeSocketDir(id string) {
	_ = os.Remove(filepath.Join(socketDirBase(), id))
}
