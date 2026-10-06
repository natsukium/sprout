package main

// delete+recreate swaps the directory behind a pathname, so a daemon.lock
// flock alone may protect a dead inode. The lifecycle lock lives outside the
// deletable directory and serializes pathname create/claim/remove.
// Lock order is lifecycle → boot, never reversed.

import (
	"errors"
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

// Canonical ID order whatever order the caller names them in: with blocking
// acquisition, a fixed global order is the only thing preventing two holders
// from waiting on each other forever.
func acquireLifecyclePair(a, b string) (lockA, lockB *os.File, err error) {
	first, second := a, b
	if second < first {
		first, second = second, first
	}
	lockFirst, err := acquireLifecycleLock(first)
	if err != nil {
		return nil, nil, err
	}
	lockSecond, err := acquireLifecycleLock(second)
	if err != nil {
		lockFirst.Close()
		return nil, nil, err
	}
	if first == a {
		return lockFirst, lockSecond, nil
	}
	return lockSecond, lockFirst, nil
}

// Covers an in-place reboot where the outgoing daemon is still unwinding.
// Short on purpose: past it, a held lock means a real concurrent owner.
const instanceLockWait = 15 * time.Second

// Held for as long as the daemon runs. The kernel drops a flock on process
// death however abrupt, so holding it proves no other daemon is alive here,
// and therefore that a matching runner belongs to a dead one.
func acquireInstanceLock(dir string, wait time.Duration) (*os.File, error) {
	return lockInstance(dir, wait, nil)
}

var errInstanceNowServing = errors.New("another daemon started serving this instance")

// The probe briefly takes a free lock, so the caller must hold the lifecycle
// lock that every claim is made under, or a claim could collide with it.
func daemonLockHeld(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

func lockInstance(dir string, wait time.Duration, serving func() bool) (*os.File, error) {
	path := filepath.Join(dir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if serving != nil && serving() {
			f.Close()
			return nil, errInstanceNowServing
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("another sprout process is already booting or running this instance (lock held on %s)", path)
		}
		time.Sleep(250 * time.Millisecond)
	}
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
