package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Holds an exclusive flock on path for the rest of the test, standing in for a
// live attempt's owner.
func holdFlock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("locking %s: %v", path, err)
	}
	return f
}

func stagingEntries(t *testing.T, instDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(stagingDir(instDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A published token verifies against its own live path, and stops verifying
// once that path is gone — the shape a delete leaves behind mid-build.
func TestPublishedTokenVerifiesUntilItsDirectoryIsGone(t *testing.T) {
	instDir := t.TempDir()

	tok, err := publishToken(instDir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()

	if filepath.Dir(tok.dir) != stagingDir(instDir) {
		t.Errorf("token published at %s, want a child of %s", tok.dir, stagingDir(instDir))
	}
	if _, err := os.Stat(tokenLockPath(tok.dir)); err != nil {
		t.Fatalf("published token has no lock file: %v", err)
	}
	if err := tok.verify(); err != nil {
		t.Fatalf("fresh token failed to verify: %v", err)
	}

	if err := os.RemoveAll(tok.dir); err != nil {
		t.Fatal(err)
	}
	err = tok.verify()
	if err == nil {
		t.Fatal("verify accepted a token whose directory was deleted")
	}
	if !strings.Contains(err.Error(), "deleted while building") {
		t.Errorf("verify error = %v, want it to name the deletion", err)
	}
}

// Only an attempt that dropped its flock is collectable; a live attempt's
// token is what protects its freshly built bundle from the GC.
func TestSweepDeadTokensSpareTheStillLockedOne(t *testing.T) {
	instDir := t.TempDir()

	dead, err := publishToken(instDir)
	if err != nil {
		t.Fatal(err)
	}
	live, err := publishToken(instDir)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	// The owner died without retiring its token: the flock is gone, the
	// directory is not.
	dead.Close()

	sweepDeadTokens(instDir)

	if _, err := os.Stat(dead.dir); !os.IsNotExist(err) {
		t.Errorf("token of a dead attempt survived the sweep: %v", err)
	}
	if _, err := os.Stat(live.dir); err != nil {
		t.Errorf("token of a live attempt was swept: %v", err)
	}
	if err := live.verify(); err != nil {
		t.Errorf("live token stopped verifying after the sweep: %v", err)
	}
}

// Publication also collects .tmp- entries an interrupted publisher left: one
// that never got its lock file, and one whose lock nobody holds. The .tmp-
// entry of a publisher still holding its lock is not ours to remove.
func TestPublishTokenSweepsEmbryonicTokens(t *testing.T) {
	instDir := t.TempDir()
	staging := stagingDir(instDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	noLock := filepath.Join(staging, ".tmp-dead")
	unlocked := filepath.Join(staging, ".tmp-dead2")
	held := filepath.Join(staging, ".tmp-live")
	for _, dir := range []string{noLock, unlocked, held} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tokenLockPath(unlocked), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	holdFlock(t, tokenLockPath(held))

	tok, err := publishToken(instDir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.remove()

	for _, dir := range []string{noLock, unlocked} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("embryonic token %s survived publication: %v", filepath.Base(dir), err)
		}
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("embryonic token of a live publisher was removed: %v", err)
	}
}

// remove retires the whole token directory, so a completed attempt leaves no
// staging root behind for the next sweep to reason about.
func TestTokenRemoveClearsTheStagingEntry(t *testing.T) {
	instDir := t.TempDir()

	tok, err := publishToken(instDir)
	if err != nil {
		t.Fatal(err)
	}
	tok.remove()

	if names := stagingEntries(t, instDir); len(names) != 0 {
		t.Errorf("staging holds %v after the token was retired, want it empty", names)
	}
}
