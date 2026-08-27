package main

// delete+recreate swaps the directory behind a pathname, so a daemon.lock
// flock alone may protect a dead inode. The lifecycle lock lives outside the
// deletable directory and serializes pathname create/claim/remove.
// Lock order is lifecycle → boot, never reversed.

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The lock file is never removed — deleting it would reintroduce the
// pathname-swap ambiguity it exists to close.
func acquireLifecycleLock(id string) (*os.File, error) {
	root, err := instancesRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".lifecycle-"+id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}

// The caller must hold the lifecycle lock: it keeps the pathname from being
// swapped between the open and the verification below.
func claimInstanceLocked(dir string, wait time.Duration, serving func() bool) (*os.File, error) {
	lock, err := lockInstance(dir, wait, serving)
	if err != nil {
		return nil, err
	}
	held, err := lock.Stat()
	if err != nil {
		lock.Close()
		return nil, err
	}
	current, err := os.Stat(filepath.Join(dir, "daemon.lock"))
	if err != nil || !os.SameFile(held, current) {
		lock.Close()
		return nil, fmt.Errorf("instance directory %s was replaced while claiming it; retry", dir)
	}
	return lock, nil
}

// Never creates the instance directory: only `up` and `fork` may.
func claimInstance(id string, wait time.Duration, serving func() bool) (*os.File, error) {
	lc, err := acquireLifecycleLock(id)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	dir, err := instanceDir(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, &instanceNotFoundError{selector: id}
		}
		return nil, err
	}
	return claimInstanceLocked(dir, wait, serving)
}

// Binds an unlocked user confirmation to the incarnation it was asked about.
// A held fd rather than a stored inode value: delete+recreate can reuse a
// freed inode.
type incarnationMarker struct{ f *os.File }

func openIncarnationMarker(dir string) (*incarnationMarker, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	return &incarnationMarker{f: f}, nil
}

func (m *incarnationMarker) verify(dir string) error {
	held, err := m.f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(dir)
	if err != nil || !os.SameFile(held, current) {
		return fmt.Errorf("instance changed since confirmation; re-run the command")
	}
	return nil
}

func (m *incarnationMarker) Close() error { return m.f.Close() }
