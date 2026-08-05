package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A base under /tmp like production's, but private to the test.
func testSocketBase(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "sproutsd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	// ensurePrivateDir must accept its own base on every later call, so start
	// from the state it would leave behind.
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	return base
}

// An instance directory too deep for sun_path still gets a bindable, dialable
// socket path, and the socket file lands in the instance directory.
func TestSocketDirBindsUnderDeepInstanceDir(t *testing.T) {
	instDir := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if direct := filepath.Join(instDir, "control.sock"); len(direct) <= maxSocketPathLen {
		t.Fatalf("test setup: %q must overflow sun_path to prove anything", direct)
	}

	sockDir, err := ensureSocketDir(testSocketBase(t), "abc123def456", instDir)
	if err != nil {
		t.Fatal(err)
	}
	sock, err := socketPathIn(sockDir, "control.sock")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("bind through the socket directory failed: %v", err)
	}
	defer ln.Close()
	if _, err := os.Lstat(filepath.Join(instDir, "control.sock")); err != nil {
		t.Errorf("socket file did not land in the instance directory: %v", err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial through the socket directory failed: %v", err)
	}
	conn.Close()
}

// The id is unique only per state root, so a link left by another root's
// instance must be re-pointed, not reused.
func TestEnsureSocketDirRepointsStaleLink(t *testing.T) {
	base := testSocketBase(t)
	oldTarget, newTarget := t.TempDir(), t.TempDir()

	if _, err := ensureSocketDir(base, "abc123def456", oldTarget); err != nil {
		t.Fatal(err)
	}
	sockDir, err := ensureSocketDir(base, "abc123def456", newTarget)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != newTarget {
		t.Errorf("link points at %q, want it re-pointed to %q", got, newTarget)
	}
}

// Something else sitting at the base must be refused, never adopted as the
// place sockets are bound.
func TestEnsurePrivateDirRejectsNonDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(base, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(base); err == nil {
		t.Fatal("a planted file at the base was accepted")
	}
}

func TestEnsurePrivateDirTightensLooseMode(t *testing.T) {
	base := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(base); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode after ensure = %v, want 0700", fi.Mode().Perm())
	}
}

// The guard replaces bind's bare EINVAL, so its error must name the limit.
func TestSocketPathInRejectsOverlongPath(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", maxSocketPathLen))
	_, err := socketPathIn(long, "control.sock")
	if err == nil {
		t.Fatal("an over-long socket path passed the length guard")
	}
	if !strings.Contains(err.Error(), "103") {
		t.Errorf("error %q does not name the limit", err)
	}
}

// The production shape of the CI failure this file exists for: a state root
// deep enough that the control socket's own path overflows sun_path.
func TestControlRoundTripUnderDeepStateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("r", 80))
	t.Setenv("XDG_STATE_HOME", root)
	const id = "feedfacecafe"
	t.Cleanup(func() { removeSocketDir(id) })

	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if direct := filepath.Join(dir, "control.sock"); len(direct) <= maxSocketPathLen {
		t.Fatalf("test setup: %q must overflow sun_path to prove anything", direct)
	}

	sock, err := socketPath(id, dir, "control.sock")
	if err != nil {
		t.Fatal(err)
	}
	(&fakeDaemon{}).serve(t, sock)

	if !instanceRunning(id) {
		t.Fatal("a daemon serving through the socket directory is unreachable to controlDial")
	}
}
