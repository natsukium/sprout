package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
// socket path, and the socket file lands in the instance directory's sock/.
func TestSocketDirBindsUnderDeepInstanceDir(t *testing.T) {
	instDir := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if direct := filepath.Join(instDir, "control.sock"); len(direct) <= maxSocketPathLen {
		t.Fatalf("test setup: %q must overflow sun_path to prove anything", direct)
	}

	sockDir, err := prepareSocketDir(testSocketBase(t), instDir)
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
	if _, err := os.Lstat(filepath.Join(instDir, socketSubdir, "control.sock")); err != nil {
		t.Errorf("socket file did not land in the instance directory: %v", err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial through the socket directory failed: %v", err)
	}
	conn.Close()
}

// Whatever already sits at the instance directory's link name is re-pointed
// at that directory, not reused.
func TestEnsureSocketDirRepointsStaleLink(t *testing.T) {
	base := testSocketBase(t)
	oldTarget, newTarget := t.TempDir(), t.TempDir()

	if err := os.Symlink(oldTarget, socketLinkIn(base, newTarget)); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSocketDir(base, newTarget); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(socketLinkIn(base, newTarget))
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
	cleanupSocketDir(t, id)

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

	(&fakeDaemon{}).serve(t, daemonSockets(t, dir, contractFor(t, "aarch64-darwin", "vfkit", nil)).control)

	if !instanceRunning(id) {
		t.Fatal("a daemon serving through the socket directory is unreachable to controlDial")
	}
}

func cleanupSocketDir(t *testing.T, id string) {
	t.Helper()
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeSocketDir(dir) })
}

// The sockets as bootInstanceLocked resolves them for a daemon of dir.
func daemonSockets(t *testing.T, dir string, m *Manifest) instanceSockets {
	t.Helper()
	sockDir, err := prepareSocketDir(socketDirBase(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeSocketDir(dir) })
	return socketsFor(t, sockDir, m)
}

func instanceDirUnder(t *testing.T, root, id string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", root)
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// One repository and branch under two state roots share an instance ID, and
// both daemons may run at once: each gets its own link, and booting or
// stopping one leaves the other's control socket bound and reachable.
func TestSocketDirSeparatesSameIDInstancesOfTwoStateRoots(t *testing.T) {
	const id = "5a3e1d000001"
	m := contractFor(t, "aarch64-darwin", "vfkit", nil)
	dirA := instanceDirUnder(t, shortStateRoot(t), id)
	dirB := instanceDirUnder(t, shortStateRoot(t), id)

	socksA := daemonSockets(t, dirA, m)
	ctxA, stopA := context.WithCancel(context.Background())
	defer stopA()
	if err := serveControl(ctxA, socksA.control, &controlServer{}); err != nil {
		t.Fatal(err)
	}

	socksB := daemonSockets(t, dirB, m)
	if socksA.dir == socksB.dir {
		t.Fatalf("both state roots resolved the socket directory %s", socksA.dir)
	}
	for dir, sockDir := range map[string]string{dirA: socksA.dir, dirB: socksB.dir} {
		if got, err := os.Readlink(filepath.Dir(sockDir)); err != nil || got != dir {
			t.Errorf("%s points at %q (%v), want %q", sockDir, got, err, dir)
		}
	}
	ctxB, stopB := context.WithCancel(context.Background())
	if err := serveControl(ctxB, socksB.control, &controlServer{}); err != nil {
		t.Fatal(err)
	}
	stopB()
	bSock := filepath.Join(dirB, socketSubdir, controlSocketName)
	if !pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		_, err := os.Lstat(bSock)
		return os.IsNotExist(err)
	}) {
		t.Fatal("test setup: the second daemon's teardown never unlinked its own socket")
	}

	if _, err := os.Lstat(filepath.Join(dirA, socketSubdir, controlSocketName)); err != nil {
		t.Fatalf("the other root's daemon boot and teardown removed this daemon's socket: %v", err)
	}
	conn, err := net.Dial("unix", socksA.control)
	if err != nil {
		t.Fatalf("this daemon's control socket is unreachable after the other root's daemon came and went: %v", err)
	}
	conn.Close()
}

// The orphan reaper of one state root's instance must not select the runner
// or sidecars of the same-ID instance of another root, whichever booted last.
func TestOrphanArgsOfOneStateRootIgnoreTheOtherRootsProcesses(t *testing.T) {
	const id = "5a3e1d000002"
	dirA := instanceDirUnder(t, shortStateRoot(t), id)
	dirB := instanceDirUnder(t, shortStateRoot(t), id)

	vfkit := contractFor(t, "aarch64-darwin", "vfkit", nil)
	qemu := contractFor(t, "x86_64-linux", "qemu", func(b map[string]any) {
		b["sidecars"] = []any{map[string]any{"name": "virtiofsd-workspace", "exec": []any{"/nix/store/x/bin/virtiofsd"}, "ready": map[string]any{"socket": "fs-workspace.sock"}}}
	})
	for name, m := range map[string]*Manifest{"vfkit": vfkit, "qemu": qemu} {
		t.Run(name, func(t *testing.T) {
			socksA := daemonSockets(t, dirA, m)
			socksB := daemonSockets(t, dirB, m)
			runner := func(s instanceSockets) string {
				if name == "vfkit" {
					return "/nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=" + s.net + ",mac=02:00:00:00:00:02"
				}
				return "/nix/store/q/bin/qemu-system-x86_64 -netdev stream,id=n0,addr.type=unix,addr.path=" + s.net
			}
			ps := "101 " + runner(socksA) + "\n" + "201 " + runner(socksB) + "\n"
			if name == "qemu" {
				ps += "102 /nix/store/v/bin/virtiofsd --socket-path=" + socksA.named["fs-workspace.sock"] + "\n" +
					"202 /nix/store/v/bin/virtiofsd --socket-path=" + socksB.named["fs-workspace.sock"] + "\n"
			}
			want := []int{101}
			if name == "qemu" {
				want = []int{101, 102}
			}
			if got := matchInstanceProcs(ps, m.contract.orphanArgs(socksA.dir, dirA), 0); !slices.Equal(got, want) {
				t.Errorf("reaping for the first root selects %v, want %v: only its own processes", got, want)
			}
		})
	}
}

// The socket directory has a fixed length, so the sun_path budget does not
// grow with the state root, even for the widest uid.
func TestSocketDirLengthIsIndependentOfTheInstanceDir(t *testing.T) {
	const base = "/tmp/sprout-4294967294"
	for _, dir := range []string{"/s", "/" + strings.Repeat("d", 500)} {
		got := socketDirIn(base, dir)
		if len(got) != len(base+"/0123456789ab/sock") {
			t.Errorf("socket directory for a %d-byte instance dir is %q, want %s/<12 hex>/sock", len(dir), got, base)
		}
		if _, err := socketPathIn(got, "virtiofsd-workspace.sock"); err != nil {
			t.Errorf("a typical socket under %s exceeds the limit: %v", got, err)
		}
	}
}

// A daemon started by an older sprout bound control.sock straight into the
// instance directory; a client of this sprout still reaches it, and first
// points the ID-named link, through which that daemon stops its VM, back at
// its instance, as the older client did before every dial.
func TestControlDialReachesAnOlderDaemonAndPointsTheIDLinkAtIt(t *testing.T) {
	const id = "5a3e1d000003"
	other := instanceDirUnder(t, shortStateRoot(t), id)
	legacy := legacyLink(t, id, other)
	root := shortStateRoot(t)
	cleanupSocketDir(t, id)
	dir := instanceDirUnder(t, root, id)

	(&fakeDaemon{}).serve(t, filepath.Join(dir, controlSocketName))

	if !instanceRunning(id) {
		t.Fatal("a daemon started by an older sprout is unreachable to this sprout's clients")
	}
	if got, err := os.Readlink(legacy); err != nil || got != dir {
		t.Errorf("the ID-named link points at %q (%v), want this instance %q", got, err, dir)
	}
}

// The ID-named link may meanwhile point at another state root's instance, so
// removing this instance's link leaves it alone.
func TestRemoveSocketDirLeavesTheLegacyIDLink(t *testing.T) {
	root := shortStateRoot(t)
	const id = "5a3e1d000004"
	dir := instanceDirUnder(t, root, id)
	legacy := legacyLink(t, id, dir)
	if _, err := ensureSocketDir(socketDirBase(), dir); err != nil {
		t.Fatal(err)
	}

	removeSocketDir(dir)

	if _, err := os.Lstat(socketLinkIn(socketDirBase(), dir)); !os.IsNotExist(err) {
		t.Errorf("the instance's own link survived removal: %v", err)
	}
	if _, err := os.Lstat(legacy); err != nil {
		t.Errorf("the ID-named link was removed: %v", err)
	}
}

func legacyLink(t *testing.T, id, dir string) string {
	t.Helper()
	if err := ensurePrivateDir(socketDirBase()); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(socketDirBase(), id)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	return link
}

// Runners an older sprout started name the ID-named link, which any older
// sprout of another state root may have re-pointed at its own instance, so
// such a runner is not provably this instance's and is never reaped by it;
// runners from before the socket directory still are, by the instance
// directory itself.
func TestOrphanArgsIgnoreRunnersNamingTheLegacyIDLink(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	m := contractFor(t, "aarch64-darwin", "vfkit", nil)
	sockDir := socketDirIn("/tmp/sprout-501", dir)

	ps := "" +
		"501 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=" + sockDir + "/net.sock,mac=02:00:00:00:00:02\n" +
		"502 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net.sock,mac=02:00:00:00:00:02\n" +
		"503 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=" + dir + "/net.sock,mac=02:00:00:00:00:02\n"

	if got := matchInstanceProcs(ps, m.contract.orphanArgs(sockDir, dir), 0); !slices.Equal(got, []int{501, 503}) {
		t.Fatalf("matchInstanceProcs found %v, want [501 503]: the runner naming the instance's own link and the pre-socket-directory runner, not the one naming the ID-named link", got)
	}
}

// Where a daemon of dir binds its control socket.
func daemonControlSocket(t *testing.T, dir string) string {
	t.Helper()
	sockDir, err := prepareSocketDir(socketDirBase(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeSocketDir(dir) })
	sock, err := socketPathIn(sockDir, controlSocketName)
	if err != nil {
		t.Fatal(err)
	}
	return sock
}

// An older daemon of another state root keeps tearing down and stopping its VM
// through the ID-named link, which an older sprout may have re-pointed at this
// instance: none of that reaches this sprout's daemon's sockets.
func TestOlderDaemonThroughARepointedIDLinkMissesThisDaemonsSockets(t *testing.T) {
	const id = "5a3e1d000005"
	m := contractFor(t, "aarch64-darwin", "vfkit", nil)
	dir := instanceDirUnder(t, shortStateRoot(t), id)
	legacy := legacyLink(t, id, dir)
	socks := daemonSockets(t, dir, m)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := serveControl(ctx, socks.control, &controlServer{}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socks.vmControl)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	for _, name := range []string{controlSocketName, filepath.Base(socks.net), filepath.Base(socks.vmControl)} {
		_ = os.Remove(filepath.Join(legacy, name))
	}
	if conn, err := net.Dial("unix", filepath.Join(legacy, filepath.Base(socks.vmControl))); err == nil {
		conn.Close()
		t.Error("the older daemon's graceful stop reached this daemon's VM control socket")
	}

	for _, sock := range []string{socks.control, socks.vmControl} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Errorf("%s is unreachable after the older daemon's teardown: %v", sock, err)
			continue
		}
		conn.Close()
	}
}

// The pre-upgrade fallback dials only through the instance's own link: with a
// dead older daemon's socket left in this instance directory and the ID-named
// link re-pointed at another root's live older daemon, the instance is not
// running, and the link is left where it was.
func TestControlDialNeverFallsBackThroughTheIDLink(t *testing.T) {
	const id = "5a3e1d000006"
	other := instanceDirUnder(t, shortStateRoot(t), id)
	legacy := legacyLink(t, id, other)
	(&fakeDaemon{}).serve(t, filepath.Join(other, controlSocketName))

	root := shortStateRoot(t)
	cleanupSocketDir(t, id)
	dir := instanceDirUnder(t, root, id)
	stale, err := net.Listen("unix", filepath.Join(dir, controlSocketName))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	if instanceRunning(id) {
		t.Fatal("this instance answered as running through another state root's older daemon")
	}
	if got, err := os.Readlink(legacy); err != nil || got != other {
		t.Errorf("a stale socket re-pointed the ID-named link at %q (%v), away from the live %q", got, err, other)
	}
}
