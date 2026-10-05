package main

// macOS has no PDEATHSIG, and on Linux a runner may ignore the signal it
// delivers, so a crashed daemon can leave its runner running, still holding
// var.img with no control socket to reach it by.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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

// The probe itself briefly holds the lock when it is free, so the caller must
// hold the lifecycle lock: every claim is made under it, so none can collide
// with the probe and fail.
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

// A process is matched by a whole argument the daemon substituted into it, so
// it is identified by what it is rather than by a reusable pid. Not a
// substring of the command line: that would also match a pager on the runner
// log or anything else naming the socket, and this decides what gets killed.
func matchInstanceProcs(psOut string, args []argMatcher, self int) []int {
	var pids []int
	for _, line := range strings.Split(psOut, "\n") {
		pidField, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidField)
		if err != nil || pid == self {
			continue
		}
		if commandHasArg(command, args) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ps joins argv with spaces; every argument matched here was substituted
// from runnerSafeValue, which has none.
func commandHasArg(command string, args []argMatcher) bool {
	for _, field := range strings.Fields(command) {
		for _, match := range args {
			if match(field) {
				return true
			}
		}
	}
	return false
}

// Only valid while holding the instance lock, which is what proves the
// processes found are orphans and not a live sibling boot. Reclaimed rather
// than refused: no VM here is worth preserving, and refusing would only ask
// the user to run the kill sprout is already positioned to run.
func reapOrphans(dir string, socks instanceSockets, m *Manifest) error {
	psOut, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return fmt.Errorf("scanning for leftover VM processes: %w", err)
	}
	pids := matchInstanceProcs(string(psOut), m.contract.orphanArgs(socks.dir, dir), os.Getpid())
	if len(pids) == 0 {
		return nil
	}
	kind := m.contract.kind.name
	fmt.Fprintf(os.Stderr, "reclaiming %s VM process %s left by a previous daemon …\n", kind, formatPIDs(pids))

	// gracefulStop's ladder, with shorter waits: a user is watching a boot and
	// the VM being drained is already unreachable.
	if ctl := m.contract.control; ctl != nil {
		if err := ctl.requestStop(socks.vmControl, false); err == nil {
			if waitProcsGone(pids, 15*time.Second) {
				return nil
			}
		}
	}
	signalProcs(pids, syscall.SIGTERM)
	if waitProcsGone(pids, 10*time.Second) {
		return nil
	}
	signalProcs(pids, syscall.SIGKILL)
	if waitProcsGone(pids, 5*time.Second) {
		return nil
	}
	return fmt.Errorf("leftover %s VM process %s would not exit; it still holds this instance's disk image, so kill it manually and retry", kind, formatPIDs(alivePIDs(pids)))
}

func signalProcs(pids []int, sig syscall.Signal) {
	for _, pid := range pids {
		_ = syscall.Kill(pid, sig)
	}
}

func waitProcsGone(pids []int, d time.Duration) bool {
	return pollUntil(d, 250*time.Millisecond, func() bool {
		return len(alivePIDs(pids)) == 0
	})
}

func alivePIDs(pids []int) []int {
	var alive []int
	for _, pid := range pids {
		// EPERM means the process exists but belongs to someone else, which
		// still counts as alive here.
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			alive = append(alive, pid)
		}
	}
	return alive
}

func formatPIDs(pids []int) string {
	s := make([]string, len(pids))
	for i, pid := range pids {
		s[i] = strconv.Itoa(pid)
	}
	return strings.Join(s, ", ")
}

func runnerLogTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if off := info.Size() - n; off > 0 {
		if _, err := f.Seek(off, 0); err != nil {
			return ""
		}
	}
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	return string(buf[:read])
}

// Bounds the wait for a leftover holder of var.img, long enough for the
// mkfs a first boot runs to finish on its own.
var diskReleaseWait = 30 * time.Second

// A runner's own children are neither the runner nor fingerprinted, so
// neither PDEATHSIG nor reapOrphans reaches them: microvm.nix's runner
// formats var.img with mkfs before it execs the VM, and a daemon killed
// meanwhile leaves mkfs writing the image. The next runner skips formatting
// an image that exists and would boot it under the writer. Waited for rather
// than killed, since a holder that is not a runner child is not sprout's to
// kill.
func awaitDiskReleased(dir string) error {
	img := varImagePath(dir)
	holders := varImageHolders(img)
	if len(holders) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "waiting for %s to close %s …\n", strings.Join(holders, ", "), img)
	if pollUntil(diskReleaseWait, 250*time.Millisecond, func() bool {
		holders = varImageHolders(img)
		return len(holders) == 0
	}) {
		return nil
	}
	return fmt.Errorf("%s still has %s open after %s; booting now would share the disk with it, so stop it and retry", strings.Join(holders, ", "), img, diskReleaseWait)
}

const (
	ext4MagicOffset = 1080
	ext4Magic       = 0xef53
)

// microvm.nix's runner creates var.img and only then runs mkfs.ext4 on it,
// skipping both whenever the file exists, so a first boot killed in between
// leaves an image every later boot accepts and none can mount. mke2fs writes
// the primary superblock last, so its magic marks a finished format. Moved
// aside rather than deleted: a real disk whose superblock was damaged also
// lacks it, and fsck can repair that from a backup copy. Any filesystem
// other than ext4, or a bundle too old to declare one, has no such marker.
func setAsideUnformattedImage(dir, fsType string) error {
	if fsType != "ext4" {
		return nil
	}
	img := varImagePath(dir)
	f, err := os.Open(img)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var magic [2]byte
	_, err = f.ReadAt(magic[:], ext4MagicOffset)
	f.Close()
	if err == nil && binary.LittleEndian.Uint16(magic[:]) == ext4Magic {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	// Linked, not renamed: a rename would replace an image an earlier
	// interrupted boot set aside.
	aside := fmt.Sprintf("%s.unformatted-%s", img, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Link(img, aside); err != nil {
		return err
	}
	if err := os.Remove(img); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "warning: %s held no filesystem, which is what a first boot interrupted while formatting it leaves; moved it to %s so this boot formats a fresh one\n", img, aside)
	return nil
}
