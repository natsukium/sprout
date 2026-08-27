package main

// Every `up` attempt publishes a token dir under <instanceDir>/staging: its
// flocked lock file is the attempt's incarnation marker, and a built attempt
// parks its staging GC root beside it. Flocks die with the process however
// abrupt, so a dead attempt is recognizable by an acquirable flock — no PID
// reuse ambiguity.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func stagingDir(instDir string) string { return filepath.Join(instDir, "staging") }

func tokenLockPath(tokenDir string) string { return filepath.Join(tokenDir, "lock") }

func stagingBundleLink(tokenDir string) string { return filepath.Join(tokenDir, "bundle") }

type attemptToken struct {
	dir  string
	lock *os.File
}

var errInstanceDeletedWhileBuilding = errors.New("instance was deleted while building; re-run `sprout up`")

// The caller holds the lifecycle lock: the .tmp- rename keeps a token dir
// from being observable without its flocked lock file, and no compliant
// publisher can be mid-publication while we hold it, which is what makes
// sweeping embryonic .tmp- entries here safe.
func publishToken(instDir string) (*attemptToken, error) {
	staging := stagingDir(instDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	sweepEmbryonicTokens(staging)
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw[:])
	tmp := filepath.Join(staging, ".tmp-"+token)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(tmp, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		os.RemoveAll(tmp)
		return nil, err
	}
	final := filepath.Join(staging, token)
	if err := os.Rename(tmp, final); err != nil {
		lock.Close()
		os.RemoveAll(tmp)
		return nil, err
	}
	return &attemptToken{dir: final, lock: lock}, nil
}

// Held fd vs live path, as in claimInstanceLocked: a stale attempt must never
// touch (or recreate) a replacement incarnation.
func (t *attemptToken) verify() error {
	held, err := t.lock.Stat()
	if err != nil {
		return errInstanceDeletedWhileBuilding
	}
	current, err := os.Stat(tokenLockPath(t.dir))
	if err != nil || !os.SameFile(held, current) {
		return errInstanceDeletedWhileBuilding
	}
	return nil
}

// The flock is still held through the RemoveAll.
func (t *attemptToken) remove() {
	os.RemoveAll(t.dir)
	t.Close()
}

func (t *attemptToken) Close() { t.lock.Close() }

func sweepEmbryonicTokens(staging string) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".tmp-") {
			probeAndRemoveToken(filepath.Join(staging, e.Name()))
		}
	}
}

// Callers pin the canonical root first, so a dead attempt's staging root only
// ever covers a bundle that is canonical-covered or unreferenced.
func sweepDeadTokens(instDir string) {
	staging := stagingDir(instDir)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			probeAndRemoveToken(filepath.Join(staging, e.Name()))
		}
	}
}

// Only an acquirable flock (or a missing lock file) proves the owner is dead;
// the probe flock is held through the RemoveAll so no new owner can adopt the
// dir mid-removal.
func probeAndRemoveToken(tokenDir string) {
	f, err := os.OpenFile(tokenLockPath(tokenDir), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.RemoveAll(tokenDir)
		}
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	_ = os.RemoveAll(tokenDir)
}
