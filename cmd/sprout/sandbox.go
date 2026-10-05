package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Set by the Linux Nix package; empty in a plain `go build`.
var virtiofsdPath string

// virtiofsd logs this whenever its namespace sandbox cannot be set up, and
// exits before binding its socket.
const sandboxFailureMarker = "entering sandbox"

var readSysctl = func(name string) (string, bool) {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(name, ".", "/"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// Names a cause only where a sysctl proves no user namespace can exist at
// all. kernel.apparmor_restrict_unprivileged_userns does not: AppArmor policy
// is per executable and its denials are in an audit log an unprivileged user
// cannot read, so a seccomp or container policy remains just as likely.
func explainSandboxFailure() string {
	const tail = "QEMU shares need it; sprout does not fall back to 9p or to an unsandboxed virtiofsd"
	if v, ok := readSysctl("kernel.unprivileged_userns_clone"); ok && v == "0" {
		return "hint: unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone = 0); set it to 1. " + tail
	}
	if v, ok := readSysctl("user.max_user_namespaces"); ok && v == "0" {
		return "hint: user namespaces are disabled (user.max_user_namespaces = 0); raise it. " + tail
	}
	candidates := []string{"a seccomp or container policy forbidding namespaces or mount syscalls"}
	if v, ok := readSysctl("kernel.apparmor_restrict_unprivileged_userns"); ok && v == "1" {
		candidates = append([]string{"AppArmor restricting unprivileged user namespaces for this executable (kernel.apparmor_restrict_unprivileged_userns = 1; a profile allowing `userns` for the store's virtiofsd, or that sysctl, lifts it)"}, candidates...)
	}
	return "hint: virtiofsd could not set up its user namespace sandbox, for a reason this host does not reveal; candidates: " + strings.Join(candidates, "; or ") + ". " + tail
}

// Runs virtiofsd the way a sidecar runs, so the namespaces, syscalls and
// per-executable policy are virtiofsd's own rather than an imitation's.
func probeVirtiofsd(command []string) error {
	// In the short socket base, as a sidecar's socket is: under a long TMPDIR the
	// path would overflow sun_path and read as a host restriction.
	if err := os.MkdirAll(socketDirBase(), 0o700); err != nil {
		return err
	}
	sockDir, err := os.MkdirTemp(socketDirBase(), "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(sockDir)
	sock, err := socketPathIn(sockDir, "probe.sock")
	if err != nil {
		return err
	}
	shared, err := os.MkdirTemp("", "sprout-probe-share-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(shared)

	args := append(append([]string{}, command[1:]...),
		"--socket-path="+sock,
		"--shared-dir="+shared,
		"--sandbox=namespace",
		"--translate-uid=squash-guest:0:"+strconv.Itoa(os.Getuid())+":4294967295",
		"--translate-gid=squash-guest:0:"+strconv.Itoa(os.Getgid())+":4294967295",
	)
	var out lockedBuffer
	cmd := exec.Command(command[0], args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return err
	}
	exit := watchRunner(cmd)
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		if !exit.within(sidecarTermWait) {
			_ = cmd.Process.Kill()
			<-exit.done
		}
	}()
	if err := awaitSidecarSocket(sock, exit); err != nil {
		msg := fmt.Sprintf("virtiofsd %s", err)
		if o := strings.TrimSpace(out.String()); o != "" {
			msg += "\n" + o
			if strings.Contains(o, sandboxFailureMarker) {
				msg += "\n" + explainSandboxFailure()
			}
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
