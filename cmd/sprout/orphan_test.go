package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func contractFor(t *testing.T, host, kind string, mutate func(backend map[string]any)) *Manifest {
	t.Helper()
	doc := v2ManifestDoc(host, kind)
	if mutate != nil {
		mutate(doc["backend"].(map[string]any))
	}
	m, err := parseManifest(encodeDoc(t, doc), host)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func socketsFor(t *testing.T, sockDir string, m *Manifest) instanceSockets {
	t.Helper()
	socks, err := resolveInstanceSockets(sockDir, m)
	if err != nil {
		t.Fatal(err)
	}
	return socks
}

// Everything matchInstanceProcs returns gets killed, so it must find the
// instance's vfkit by the whole network argument the daemon substituted and
// nothing else: not another instance's vfkit, not a process merely mentioning
// the socket or the directory, not itself.
func TestMatchInstanceProcsFindsVfkitByItsNetworkArgument(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	const sockDir = "/tmp/sprout-501/6a7f9b51b885"
	m := contractFor(t, "aarch64-darwin", "vfkit", nil)
	args := m.contract.orphanArgs(sockDir, dir)

	ps := "" +
		"  1 /sbin/launchd\n" +
		"501 /nix/store/x/bin/vfkit --cpus 8 --device virtio-blk,path=var.img --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net.sock,mac=02:00:00:00:00:02\n" +
		"502 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/8b020ba20aef/net.sock,mac=02:00:00:00:00:02\n" +
		"503 tail -f /state/sprout/instances/6a7f9b51b885/runner.log\n" +
		"504 sprout up -i feat-login\n" +
		"505 socat - UNIX-CONNECT:/tmp/sprout-501/6a7f9b51b885/net.sock\n" +
		"506 grep unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net.sock run.sh\n" +
		"507 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net.sock.old,mac=02:00:00:00:00:02\n" +
		"508 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/state/sprout/instances/6a7f9b51b885/net.sock,mac=02:00:00:00:00:02\n" +
		"509 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net2.sock,mac=02:00:00:00:00:02\n" +
		"510 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/sub/net.sock\n" +
		"777 /nix/store/x/bin/vfkit --device virtio-net,unixSocketPath=/tmp/sprout-501/6a7f9b51b885/net.sock\n"

	got := matchInstanceProcs(ps, args, 777)
	want := []int{501, 508, 509}
	if !slices.Equal(got, want) {
		t.Fatalf("matchInstanceProcs found %v, want %v: the socket-dir runner, the legacy instance-dir runner, and a runner whose bundle named the socket differently (777 is self)", got, want)
	}
}

func TestMatchInstanceProcsFindsQemuAndSidecarsByTheirSocketArguments(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	const sockDir = "/tmp/sprout-1000/6a7f9b51b885"
	m := contractFor(t, "x86_64-linux", "qemu", func(b map[string]any) {
		b["sidecars"] = []any{map[string]any{"name": "virtiofsd-workspace", "exec": []any{"/nix/store/x/bin/virtiofsd"}, "ready": map[string]any{"socket": "fs-workspace.sock"}}}
	})
	args := m.contract.orphanArgs(sockDir, dir)

	ps := "" +
		"601 /nix/store/q/bin/qemu-system-x86_64 -M microvm -netdev stream,id=n0,server=off,addr.type=unix,addr.path=/tmp/sprout-1000/6a7f9b51b885/net.sock -device virtio-net-pci,netdev=n0\n" +
		"602 /nix/store/v/bin/virtiofsd --socket-path=/tmp/sprout-1000/6a7f9b51b885/fs-workspace.sock --shared-dir=/src/app --sandbox=namespace\n" +
		"603 /nix/store/q/bin/qemu-system-x86_64 -netdev stream,id=n0,addr.type=unix,addr.path=/tmp/sprout-1000/8b020ba20aef/net.sock\n" +
		"604 tail -f /state/sprout/instances/6a7f9b51b885/runner.log\n" +
		"605 socat - UNIX-CONNECT:/tmp/sprout-1000/6a7f9b51b885/vm-control.sock\n" +
		"606 ls /tmp/sprout-1000/6a7f9b51b885/fs-workspace.sock /tmp/sprout-1000/6a7f9b51b885/net.sock\n" +
		"607 /nix/store/v/bin/virtiofsd --socket-path=/tmp/sprout-1000/6a7f9b51b885/fs-workspace.sock.bak\n" +
		"608 /nix/store/q/bin/qemu-system-x86_64 -netdev socket,id=n0,addr.path=/tmp/sprout-1000/6a7f9b51b885/net.sock\n" +
		"609 /nix/store/v/bin/virtiofsd --socket-path=/tmp/sprout-1000/6a7f9b51b885/fs-old.sock --shared-dir=/src/app\n"

	got := matchInstanceProcs(ps, args, 0)
	if !slices.Equal(got, []int{601, 602, 609}) {
		t.Fatalf("matchInstanceProcs found %v, want [601 602 609]: the instance's qemu and its sidecars only", got)
	}
}

func TestMatchInstanceProcsIgnoresMalformedRows(t *testing.T) {
	ps := "\n   \n?? --socket-path=/s/a.sock\n501\nnotapid --socket-path=/s/a.sock\n"
	if got := matchInstanceProcs(ps, []argMatcher{socketArg("--socket-path", socketIn("/s"))}, 0); len(got) != 0 {
		t.Errorf("malformed ps rows produced pids %v, want none", got)
	}
}

// The lock is what proves an instance has no live daemon, so a second holder
// must be refused while the first is alive and admitted once it lets go.
func TestAcquireInstanceLockExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()

	first, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	if _, err := acquireInstanceLock(dir, 100*time.Millisecond); err == nil {
		t.Fatal("second acquire succeeded while the first was held, so a boot could race a live daemon")
	}

	first.Close()
	second, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	second.Close()
}

// Once the winner serves control, a losing booter's failure to acquire must
// read as a handoff rather than as the lock timeout.
func TestAcquireBootLockHandsOffOnceTheWinnerServes(t *testing.T) {
	root := shortStateRoot(t)
	const id = "bootlockserve"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	(&fakeDaemon{}).serve(t, daemonControlSocket(t, dir))

	if _, err := claimInstanceLocked(dir, 5*time.Second, func() bool { return instanceRunning(id) }); !errors.Is(err, errInstanceNowServing) {
		t.Fatalf("claimInstanceLocked returned %v, want errInstanceNowServing", err)
	}
}

// Contention with no daemon behind it (a crashed winner that never served, a
// snapshot restore holding the lock) must keep the timeout error: exiting
// clean there would make the detached parent wait on a boot nobody performs.
func TestAcquireBootLockTimesOutWithoutADaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "bootlocktimeout"
	cleanupSocketDir(t, id)
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	serving := func() bool { return instanceRunning(id) }
	if _, err := claimInstanceLocked(dir, 300*time.Millisecond, serving); err == nil || errors.Is(err, errInstanceNowServing) {
		t.Fatalf("want the lock-held timeout error, got %v", err)
	}

	held.Close()
	lock, err := claimInstanceLocked(dir, time.Second, serving)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	lock.Close()
}

// End to end over real processes: a survivor carrying the instance's network
// argument is gone once reapOrphans returns, an instance with nothing behind
// it is left alone. Two survivors, one per argument form: the socket-directory
// path current runners carry, and the instance-directory path an older
// sprout's carry. The stand-in answers no REST socket, so the fall-through
// from the graceful stop to signalling runs too.
func TestReapOrphansKillsTheSurvivingVM(t *testing.T) {
	dir, sockDir := t.TempDir(), t.TempDir()
	m := contractFor(t, "aarch64-darwin", "vfkit", nil)
	socks := socketsFor(t, sockDir, m)

	if err := reapOrphans(dir, socks, m); err != nil {
		t.Fatalf("reapOrphans with no leftovers failed: %v", err)
	}

	// A compound command keeps sh from exec'ing straight into sleep, which would
	// drop the trailing argument that stands in for vfkit's network device.
	startSurvivor := func(fingerprint string) chan struct{} {
		survivor := exec.Command("/bin/sh", "-c", "sleep 300; :", fingerprint)
		if err := survivor.Start(); err != nil {
			t.Fatal(err)
		}
		// Left unwaited the child lingers as a zombie, which still answers signal 0
		// and would read as "refused to exit".
		exited := make(chan struct{})
		go func() { _ = survivor.Wait(); close(exited) }()
		t.Cleanup(func() { _ = survivor.Process.Kill() })
		return exited
	}
	current := startSurvivor("virtio-net,unixSocketPath=" + filepath.Join(sockDir, "net.sock") + ",mac=02:00:00:00:00:02")
	legacy := startSurvivor("virtio-net,unixSocketPath=" + filepath.Join(dir, "net.sock") + ",mac=02:00:00:00:00:02")

	if err := reapOrphans(dir, socks, m); err != nil {
		t.Fatalf("reapOrphans failed: %v", err)
	}
	for name, exited := range map[string]chan struct{}{"current": current, "legacy": legacy} {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Errorf("the %s-fingerprint process survived reapOrphans, so the next boot would still find its disk held", name)
		}
	}
}

// Only the framework's storage-device wording is worth reinterpreting; anything
// else passes through untouched so the caller's report stays the whole story.
func TestVfkitRunnerFailureHint(t *testing.T) {
	const dir = "/state/sprout/instances/6a7f9b51b885"
	vzDiskInUse := `Error Domain=VZErrorDomain Code=2 Description="Invalid virtual machine configuration. The storage device attachment is invalid."`

	if got := vfkitRunnerFailureHint(vzDiskInUse, dir); !strings.Contains(got, filepath.Join(dir, "var.img")) {
		t.Errorf("storage-device failure translated to %q, want it to name the disk image", got)
	}

	unrelated := []string{
		"",
		"vfkit: unknown flag --nope",
		`Error Domain=VZErrorDomain Code=2 Description="Invalid virtual machine configuration. The memory size is invalid."`,
	}
	for _, out := range unrelated {
		if got := vfkitRunnerFailureHint(out, dir); got != "" {
			t.Errorf("vfkitRunnerFailureHint(%q) = %q, want no hint", out, got)
		}
	}
}

// The tail is read for a failure vfkit prints last, so it must survive logs
// longer and shorter than the window, and a missing log reads as "nothing to
// say" rather than failing the boot report.
func TestRunnerLogTail(t *testing.T) {
	dir := t.TempDir()

	short := filepath.Join(dir, "short.log")
	if err := os.WriteFile(short, []byte("boot ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runnerLogTail(short, 4096); got != "boot ok\n" {
		t.Errorf("short log read as %q, want the whole file", got)
	}

	long := filepath.Join(dir, "long.log")
	if err := os.WriteFile(long, []byte(strings.Repeat("chatter\n", 4000)+"the last words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runnerLogTail(long, 64)
	if !strings.HasSuffix(got, "the last words\n") {
		t.Errorf("long log tail = %q, want it to end with the final line", got)
	}
	if len(got) > 64 {
		t.Errorf("long log tail read %d bytes, want at most 64", len(got))
	}

	if got := runnerLogTail(filepath.Join(dir, "absent.log"), 4096); got != "" {
		t.Errorf("missing log read as %q, want empty", got)
	}
}

// An image the runner created but never finished formatting is moved aside so
// the next runner formats a fresh one; a formatted one stays where it is.
func TestSetAsideUnformattedImage(t *testing.T) {
	dir := t.TempDir()
	img := varImagePath(dir)
	if err := setAsideUnformattedImage(dir); err != nil {
		t.Fatalf("no image: %v", err)
	}

	formatted := make([]byte, 4096)
	formatted[1080], formatted[1081] = 0x53, 0xef
	for name, content := range map[string][]byte{
		"empty (created, never truncated)": nil,
		"sparse (truncated, never mkfs'd)": make([]byte, 1<<20),
	} {
		if err := os.WriteFile(img, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := setAsideUnformattedImage(dir); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(img); !os.IsNotExist(err) {
			t.Errorf("%s image left in place, where the runner would boot it unformatted", name)
		}
	}
	// Both interrupted images survive: a second set-aside must not replace
	// the first, which may be a real disk with a damaged superblock.
	if aside, _ := filepath.Glob(img + ".unformatted-*"); len(aside) != 2 {
		t.Errorf("images kept aside = %v, want both", aside)
	}

	if err := os.WriteFile(img, formatted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setAsideUnformattedImage(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(img); err != nil || len(got) != len(formatted) {
		t.Fatalf("formatted image was moved or altered: %v", err)
	}
}
