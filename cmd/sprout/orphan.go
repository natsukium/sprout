package main

// macOS has no PDEATHSIG, so a crashed daemon leaves its vfkit child running,
// still holding var.img with no control socket to reach it by.

import (
	"errors"
	"fmt"
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
// and therefore that a matching vfkit belongs to a dead one.
func acquireInstanceLock(dir string, wait time.Duration) (*os.File, error) {
	return lockInstance(dir, wait, nil)
}

var errInstanceNowServing = errors.New("another daemon started serving this instance")

// A winning daemon holds the lock from the start of its boot but answers
// control only once its runner is up, so a booter that lost the race would
// wait out the whole lock timeout and fail against a healthy winner. Checking
// PING once before the wait would not close that window — only checking
// *during* it does.
func acquireBootLock(dir, id string, wait time.Duration) (*os.File, error) {
	return lockInstance(dir, wait, func() bool { return instanceRunning(id) })
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

// The runner bakes this path into vfkit's --device virtio-net argument, so it
// identifies the process by what it is rather than by a reusable pid. Not the
// runner script's path: that would also match a pager opened on the script,
// and this fingerprint decides what gets killed.
func instanceProcFingerprint(sockDir string) string {
	return filepath.Join(sockDir, netSocketName)
}

func matchInstanceProcs(psOut, fingerprint string, self int) []int {
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
		if strings.Contains(command, fingerprint) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Only valid while holding the instance lock, which is what proves the
// processes found are orphans and not a live sibling boot. Reclaimed rather
// than refused: no VM here is worth preserving, and refusing would only ask
// the user to run the kill sprout is already positioned to run.
func reapOrphans(dir, sockDir string, m *Manifest) error {
	psOut, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return fmt.Errorf("scanning for leftover VM processes: %w", err)
	}
	pids := matchInstanceProcs(string(psOut), instanceProcFingerprint(sockDir), os.Getpid())
	// Runners from a sprout that predates the socket directory carry the
	// instance directory's path instead; matched too, or an upgraded sprout
	// could not reclaim what an older one's crash left behind.
	pids = append(pids, matchInstanceProcs(string(psOut), instanceProcFingerprint(dir), os.Getpid())...)
	if len(pids) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "reclaiming VM process %s left by a previous daemon …\n", formatPIDs(pids))

	// gracefulStop's ladder, with shorter waits: a user is watching a boot and
	// the VM being drained is already unreachable.
	if restSock, err := socketPathIn(sockDir, m.RestSocket); err == nil {
		if err := vfkitRestStop(restSock); err == nil {
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
	return fmt.Errorf("leftover VM process %s would not exit; it still holds this instance's disk image, so kill it manually and retry", formatPIDs(alivePIDs(pids)))
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

// The framework's "storage device attachment is invalid" gives no hint that
// another process holds var.img, and reaching this message means reapOrphans
// found nothing, so the holder is unrecognized.
func translateRunnerFailure(runnerOutput, dir string) string {
	if strings.Contains(runnerOutput, "VZErrorDomain") && strings.Contains(runnerOutput, "storage device attachment is invalid") {
		img := varImagePath(dir)
		return fmt.Sprintf("hint: another process still has %s open, which is what the framework is refusing; find it with `lsof %s`, stop it, and retry", img, img)
	}
	return ""
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
