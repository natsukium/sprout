package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func newUpCmd() *cobra.Command {
	var (
		def        string
		flakeRef   string
		bundle     string
		foreground bool
	)
	cmd := &cobra.Command{
		Use:     "up",
		Short:   "Build & boot this checkout's VM",
		GroupID: groupDaily,
		Long: `Build or reconcile this checkout's environment, boot it, wait until it is
ready, and return. The VM keeps running in the background.

Starting a development environment should not occupy a terminal, so console
output is a separate, composable operation: ` + "`sprout logs --follow`" + `.

--foreground instead runs the daemon in this process, which is what a
supervisor (launchd, via services.sprout) needs: it must own a process that
lives as long as the VM. See docs/how-to/run-as-daemon.md.`,
		Args: usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().StringVar(&def, "vm", "", "VM definition name (sprout.vms.<name>; default: the flake's only definition, or \"dev\")")
	cmd.Flags().StringVar(&flakeRef, "flake", ".", "flake reference to build from")
	cmd.Flags().StringVar(&bundle, "bundle", "", "boot a prebuilt bundle directory instead of building from a flake")
	// Hidden, not removed: the nix-darwin module passes a store path here.
	_ = cmd.Flags().MarkHidden("bundle")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run the daemon in this process instead of returning once the VM is ready (for a supervisor)")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return cmdUp(*selector, def, flakeRef, bundle, foreground)
	}
	return cmd
}

func cmdUp(selector, def, flakeRef, bundle string, foreground bool) error {
	definition := def
	if bundle == "" {
		var err error
		definition, err = resolveDefinition(flakeRef, def)
		if err != nil {
			return err
		}
	}

	id, err := resolveIdentity(selector)
	if err != nil {
		return err
	}
	noteRunningSiblings(id)
	if !foreground {
		return upDetached(id, selector, definition, flakeRef, bundle)
	}
	return upForeground(id, definition, flakeRef, bundle)
}

// A note rather than a refusal: the instances keep separate /var volumes.
func noteRunningSiblings(id *Identity) {
	if hint := siblingHint(id); hint != "" {
		fmt.Fprintf(os.Stderr, "note: %s\n", hint)
	}
}

func upDetached(id *Identity, selector, def, flakeRef, bundle string) error {
	return launchDetached(id, selector, upChildArgs(selector, def, flakeRef, bundle), "building and booting", "up")
}

// --foreground is mandatory: without it the child would detach in turn and
// fork forever.
func upChildArgs(selector, def, flakeRef, bundle string) []string {
	args := []string{"up", "--foreground", "--flake", flakeRef}
	if def != "" {
		args = append(args, "--vm", def)
	}
	if bundle != "" {
		args = append(args, "--bundle", bundle)
	}
	if selector != "" {
		args = append(args, "--instance", selector)
	}
	return args
}

// The child *is* the daemon, so the reaping awaitBootOrReady does while racing
// its exit is what keeps a long-lived caller from collecting zombies.
func bootDetached(id string, childArgs []string, supersededPID int, what string, announce func(logPath string)) error {
	dir, err := instanceDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := upLogPath(dir)
	logf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logf.Close()

	child, err := backgroundSelf(childArgs, logf)
	if err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	if announce != nil {
		announce(logPath)
	}
	return awaitBootOrReady(child.Wait, id, supersededPID, logPath, what)
}

func launchDetached(id *Identity, selector string, childArgs []string, action, what string) error {
	// Captured before the child replaces it: an in-place `up` reboot keeps the
	// current VM answering through the rebuild, so readiness must wait for a
	// *different* daemon, not just any.
	supersededPID := runningPID(id.ID)
	err := bootDetached(id.ID, childArgs, supersededPID, what, func(logPath string) {
		fmt.Printf("%s %q in the background (log: %s) …\n", action, id.Display(), logPath)
	})
	if err != nil {
		return err
	}
	fmt.Printf("VM ready. Enter it with: %s\n", withSelector("sprout shell", selector))
	return nil
}

// The child runs in its own process group so a Ctrl-C aimed at the foreground
// command never reaches a daemon meant to outlive it. It also starts without
// the caller's descriptors: a caller that serializes boots with `flock 9> lock`
// around `sprout up` would otherwise never get the lock back, since the lock
// belongs to the open file description the daemon and vfkit inherited.
func backgroundSelf(args []string, logf *os.File) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dropInheritedDescriptors()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// Go opens its own files close-on-exec already, so what this marks is the
// caller's. The table is scanned rather than listed from /dev/fd: on darwin
// that directory yields 0-2 and then a readdir error, which would silently skip
// the descriptors this exists to catch. fcntl on an unopened one is a no-op.
func dropInheritedDescriptors() {
	for fd := 3; fd < descriptorTableSize(); fd++ {
		unix.CloseOnExec(fd)
	}
}

// Caps the scan: the RLIMIT_NOFILE soft limit can be raised into the millions
// and a boot should not spend that in fcntl, while the shell redirections this
// guards against use single-digit descriptors.
const maxScannedDescriptor = 1 << 16

func descriptorTableSize() int {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil || lim.Cur > maxScannedDescriptor {
		return maxScannedDescriptor
	}
	return int(lim.Cur)
}

func runningPID(id string) int {
	if info, err := queryInfo(id); err == nil {
		return info.PID
	}
	return 0
}

// The child's exit is raced rather than joined: on the success path the child
// *is* the daemon and never exits; an error exit is a boot failure reported
// immediately rather than sitting out the readiness timeout.
//
// A clean exit before readiness means the child found a daemon already
// running and handed off to it, so readiness is then checked against *any*
// live daemon rather than the supersededPID filter — the superseded daemon is
// exactly the one still serving on this path.
func awaitBootOrReady(wait func() error, id string, supersededPID int, logPath, what string) error {
	exited := make(chan error, 1)
	go func() { exited <- wait() }()
	ready := make(chan error, 1)
	go func() { ready <- waitInstanceReady(id, supersededPID) }()

	select {
	case err := <-exited:
		if err != nil {
			if logPath == "" {
				return fmt.Errorf("%s failed: %w", what, err)
			}
			err = fmt.Errorf("%s failed, see %s: %w", what, logPath, err)
			// Quoted verbatim rather than classified: matching the log against
			// phase markers would couple this to nix's and vfkit's output.
			if line := lastLogLine(logPath); line != "" {
				err = fmt.Errorf("%w\n  %s", err, line)
			}
			return err
		}
		if supersededPID == 0 {
			return <-ready
		}
		return waitInstanceReady(id, 0)
	case err := <-ready:
		return err
	}
}

// The child's "sprout: " prefix is stripped because the parent adds its own.
func lastLogLine(path string) string {
	tail := strings.TrimSpace(runnerLogTail(path, runnerLogTailBytes))
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	return strings.TrimPrefix(line, "sprout: ")
}

func upForeground(id *Identity, def, flakeRef, bundlePath string) error {
	dir, err := instanceDir(id.ID)
	if err != nil {
		return err
	}

	bundle := bundlePath
	if bundle == "" {
		fmt.Printf("building %s#sproutConfigurations.%s …\n", flakeRef, def)
		bundle, err = nixBuild(flakeRef, def)
		if err != nil {
			return err
		}
	}

	if instanceRunning(id.ID) {
		prev, _, err := loadInstance(id.ID)
		if err == nil && prev.Bundle == bundle {
			fmt.Printf("instance %q is already running\n", id.Display())
			return nil
		}
		fmt.Printf("instance %q definition changed, rebooting …\n", id.Display())
		if err := stopOne(id.ID, stopBehavior{reportStopped: true}); err != nil {
			return fmt.Errorf("stopping %q for reboot: %w", id.Display(), err)
		}
	}

	manifest, err := loadManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}

	if bundlePath != "" {
		def = manifest.Definition
	}

	inst := id.newInstance()
	inst.Definition, inst.Bundle = def, bundle
	inst.GuestIP, inst.SSHUser = manifest.Guest.IP, manifest.Guest.SSHUser
	return bootInstance(dir, inst, manifest)
}

// Shared by `up` and `start`, so a restart takes the same guest-setup path as
// a first boot and credentials are re-projected rather than frozen at the
// first `up`.
func bootInstance(dir string, inst *Instance, manifest *Manifest) error {
	inst.WorkspaceMounted = manifest.Workspace

	if err := os.MkdirAll(sshDataDir(dir), 0o700); err != nil {
		return err
	}

	// Claim the instance before touching anything under dir. The lock is held
	// for the daemon's whole life, so holding it proves no other daemon owns
	// this instance, which is what licenses reapOrphans to kill VM processes
	// rather than ask about them.
	lock, err := acquireInstanceLock(dir, instanceLockWait)
	if err != nil {
		return err
	}
	defer lock.Close()
	sockDir, err := ensureSocketDir(socketDirBase(), inst.ID, dir)
	if err != nil {
		return err
	}
	if err := reapOrphans(dir, sockDir, manifest); err != nil {
		return err
	}
	if remedy := guestGitRemedy(manifest, inst); remedy != "" {
		fmt.Fprintf(os.Stderr, "warning: %s keeps its git data in %s, which is outside the /workspace mount; git run inside the guest will report it as not a repository (%s)\n", inst.Workspace, inst.RepoRoot, remedy)
	}

	keyPath, err := ensureSSHKey()
	if err != nil {
		return err
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}
	if err := os.WriteFile(authorizedKeysPath(dir), pub, 0o600); err != nil {
		return err
	}
	// Per boot, not once at creation: the label depends on which other
	// instances exist.
	label, _ := routeLabelFor(inst.ID, inst.Name)
	if err := writeInstanceEnv(instanceEnvPath(dir), inst, label); err != nil {
		return err
	}
	// Per boot: a previous boot's marker would report a still-booting stack as
	// ready. Removed unconditionally, so a bundle switching away from
	// readiness gating cannot leave a stale one behind either.
	if err := os.Remove(readyFilePath(dir)); err != nil && !os.IsNotExist(err) {
		return err
	}

	socks, err := resolveInstanceSockets(sockDir, manifest)
	if err != nil {
		return err
	}
	subs := map[string]string{
		"netSocket":  socks.net,
		"restSocket": socks.rest,
		"dataDir":    dataDir(dir),
		"workspace":  inst.Workspace,
		"gitCommon":  inst.RepoRoot,
		"consolePty": "virtio-serial,pty",
	}
	// Start empty: a SIGKILLed daemon skips the cleanup below, and a credential
	// dropped from the definition would keep its stale file alive.
	if err := os.RemoveAll(credentialsDir(dir)); err != nil {
		return err
	}
	// Before boot: a missing mount source or a failing materialize command
	// should abort, not surface later as an empty mount or an unauthenticated
	// tool.
	if err := setupCredentials(manifest, subs, dataDir(dir)); err != nil {
		return err
	}
	// Materialized credentials are Keychain secrets copied to disk; leaving
	// them there after exit would defeat keeping them in the Keychain.
	defer os.RemoveAll(credentialsDir(dir))
	if err := addCacheSubstitutions(manifest, subs, inst.RepoRoot); err != nil {
		return err
	}
	runScript := filepath.Join(dir, "run.sh")
	if err := rewriteRunner(filepath.Join(inst.Bundle, "runner"), manifest, subs, runScript); err != nil {
		return err
	}

	inst.PID = os.Getpid()
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		return err
	}

	return runDaemon(dir, inst, manifest, runScript, socks)
}

// The short paths (see socketdir.go), resolved before anything boots so an
// over-long one aborts here by name instead of surfacing mid-boot as a bare
// EINVAL.
type instanceSockets struct {
	net     string
	control string
	// Substituted into the runner absolute: microvm.nix expands a relative
	// socket against the runner's cwd, the instance directory the short path
	// exists to avoid (see nix/bundle.nix).
	rest string
}

func resolveInstanceSockets(sockDir string, m *Manifest) (instanceSockets, error) {
	var s instanceSockets
	var err error
	if s.net, err = socketPathIn(sockDir, netSocketName); err != nil {
		return s, err
	}
	if s.control, err = socketPathIn(sockDir, controlSocketName); err != nil {
		return s, err
	}
	if s.rest, err = socketPathIn(sockDir, m.RestSocket); err != nil {
		return s, err
	}
	return s, nil
}

func nixBuild(flakeRef, def string) (string, error) {
	attr := fmt.Sprintf("%s#sproutConfigurations.%s", flakeRef, def)
	cmd := exec.Command("nix", "build", "--no-link", "--print-out-paths", attr)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build %s: %w", attr, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", fmt.Errorf("nix build %s produced no output path", attr)
	}
	return lines[len(lines)-1], nil
}

func loadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := jsonUnmarshalStrictVersion(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// The runner's shell quoting was fixed at Nix eval time, before any value
// existed, so a value that needs quoting cannot be escaped in — only rejected.
// The class is what nixpkgs' escapeShellArg leaves unquoted.
var runnerSafeValue = regexp.MustCompile(`^[[:alnum:],._+:@%/=-]+$`)

// Substituting text rather than re-deriving vfkit arguments keeps the runner
// a pure microvm.nix artifact shared by every instance.
//
// One pass, longest placeholder first: sequential replacement would corrupt a
// placeholder that is a prefix of another (".../credential/aws" vs
// ".../credential/aws-extra"), the shorter one hiding the longer.
func rewriteRunner(runnerPath string, m *Manifest, subs map[string]string, out string) error {
	content, err := os.ReadFile(runnerPath)
	if err != nil {
		return err
	}
	// Validated against untouched content, so a prefix collision cannot make a
	// valid placeholder look missing.
	type pair struct{ placeholder, value string }
	pairs := make([]pair, 0, len(m.Substitutions))
	for _, s := range m.Substitutions {
		value, ok := subs[s.Value]
		if !ok {
			return fmt.Errorf("manifest requests unknown substitution symbol %q (sprout too old for this flake?)", s.Value)
		}
		if !runnerSafeValue.MatchString(value) {
			return fmt.Errorf("%s resolves to %q, which the runner script cannot carry; use a path of alphanumerics and ,._+:@%%/=-", s.Value, value)
		}
		if !bytes.Contains(content, []byte(s.Placeholder)) {
			return fmt.Errorf("placeholder %q not found in runner script", s.Placeholder)
		}
		pairs = append(pairs, pair{s.Placeholder, value})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return len(pairs[i].placeholder) > len(pairs[j].placeholder)
	})
	oldnew := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		oldnew = append(oldnew, p.placeholder, p.value)
	}
	rewritten := strings.NewReplacer(oldnew...).Replace(string(content))
	return os.WriteFile(out, []byte(rewritten), 0o700)
}

var ptyPattern = regexp.MustCompile(`/dev/ttys[0-9]+`)

// Long enough to reassemble a "/dev/ttysNNN" split across two writes, short
// enough that watching an unbounded log never grows the buffer.
const ptyWatcherCarry = 64

// Scans runner output for the console PTY vfkit announces. The PTY has to be
// opened read-write and drained continuously: the kernel blocks boot until the
// device is opened, and a full buffer stalls the console.
type ptyWatcher struct {
	consoleLog string
	once       bool
	buf        []byte
}

func (w *ptyWatcher) Write(p []byte) (int, error) {
	if !w.once {
		w.buf = append(w.buf, p...)
		if m := ptyPattern.Find(w.buf); m != nil {
			w.once = true
			go w.attach(string(m))
		} else if len(w.buf) > ptyWatcherCarry {
			w.buf = w.buf[len(w.buf)-ptyWatcherCarry:]
		}
	}
	return len(p), nil
}

func (w *ptyWatcher) attach(path string) {
	pty, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console attach %s: %v\n", path, err)
		return
	}
	logf, err := os.Create(w.consoleLog)
	if err != nil {
		pty.Close()
		return
	}
	fmt.Printf("console: %s (log: %s)\n", path, w.consoleLog)
	go func() {
		defer pty.Close()
		defer logf.Close()
		io.Copy(logf, pty) //nolint:errcheck
	}()
}

func runDaemon(dir string, inst *Instance, m *Manifest, runScript string, socks instanceSockets) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vn, err := startNetwork(ctx, socks.net, m)
	if err != nil {
		return err
	}

	// Socket credentials need the running network stack, so they cannot go in
	// the pre-boot setupCredentials pass.
	if err := startSocketForwards(ctx, vn, m); err != nil {
		return err
	}

	logPath := runnerLogPath(dir)
	runnerLog, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer runnerLog.Close()

	watcher := &ptyWatcher{consoleLog: consoleLogPath(dir)}
	output := io.MultiWriter(runnerLog, watcher)

	cmd := exec.Command(runScript)
	cmd.Dir = dir
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("runner start: %w", err)
	}
	fmt.Printf("instance %q booting (runner pid %d) …\n", inst.Name, cmd.Process.Pid)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var stopRequested atomic.Bool
	stop := func() { stopRequested.Store(true); go gracefulStop(socks.rest, cmd, waitCh) }
	srv := &controlServer{vn: vn, inst: inst, started: time.Now(), stop: stop, sessions: newSessionTracker(time.Now()), runnerPID: cmd.Process.Pid}
	if err := serveControl(ctx, socks.control, srv); err != nil {
		// The runner is already up; returning without stopping it would strand
		// a vfkit holding var.img with no control socket, invisible to every
		// probe until the next boot's orphan reaper finds it.
		gracefulStop(socks.rest, cmd, waitCh)
		return err
	}

	go func() {
		waitReady(ctx, vn, inst, readyFilePath(dir), func() { srv.ready.Store(true) })
		// At readiness, not daemon start, so the ~30s boot never counts as
		// idle time.
		startIdleWatch(ctx, m, srv)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Printf("\nreceived %s, shutting down …\n", sig)
		srv.stopOnce.Do(stop)
		err = <-waitCh
	case err = <-waitCh:
	}

	// Normal shutdowns (vfkit REST stop, guest poweroff) also exit non-zero,
	// so only unexpected failures are reported.
	if err != nil {
		// Dying before SSH ever answered is a failed boot, and must exit
		// non-zero: a detached `up` reads a clean exit as a handoff to a
		// running daemon. After readiness, a late exit is just a report — a
		// guest that powered itself off is not a failure.
		if !srv.ready.Load() && !stopRequested.Load() {
			msg := fmt.Sprintf("runner exited before the VM became reachable: %v (see %s)", err, logPath)
			if hint := translateRunnerFailure(runnerLogTail(logPath, runnerLogTailBytes), dir); hint != "" {
				msg += "\n" + hint
			}
			return errors.New(msg)
		}
		fmt.Fprintf(os.Stderr, "runner exited: %v (see %s)\n", err, logPath)
	}
	fmt.Printf("instance %q stopped\n", inst.Name)
	return nil
}

// vfkit reports its errors on the last few lines; everything before them is
// boot chatter.
const runnerLogTailBytes = 8 << 10

// One budget serves both the daemon's own probe and the client-side wait, so
// a client never gives up on a boot the daemon is still allowing.
var (
	readyTimeout = 10 * time.Minute
	readyPoll    = time.Second
)

// The file check is a host-local stat, the data share being the same
// directory: no guest exec, no second SSH channel.
func waitReady(ctx context.Context, vn dialer, inst *Instance, readyFile string, onReady func()) {
	addr := net.JoinHostPort(inst.GuestIP, "22")
	timeout := readyTimeout
	deadline := time.Now().Add(timeout)
	sshUp := false
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if !sshUp {
			dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
			conn, err := vn.DialContextTCP(dctx, addr)
			dcancel()
			if err == nil {
				conn.Close()
				sshUp = true
			}
		}
		if sshUp && readyFileArrived(readyFile) {
			fmt.Printf("VM ready. Enter it with: sprout shell -i %s\n", inst.Name)
			onReady()
			return
		}
		time.Sleep(readyPoll)
	}
	if ctx.Err() == nil {
		what := "SSH"
		if sshUp {
			what = "the guest's sprout-ready signal"
		}
		fmt.Fprintf(os.Stderr, "warning: %s did not become reachable within %s; check `sprout logs`\n", what, timeout)
	}
}

func readyFileArrived(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type dialer interface {
	DialContextTCP(ctx context.Context, addr string) (net.Conn, error)
}

// Generous on purpose: killing too early orphans the Virtualization.framework
// XPC helper before it tears down.
var (
	restStopWait = 30 * time.Second
	sigtermWait  = 15 * time.Second
)

// restSock must be sun_path-safe (see socketdir.go): vfkit bound the same file
// relative to the instance directory.
func gracefulStop(restSock string, cmd *exec.Cmd, waitCh chan error) {
	if err := vfkitRestStop(restSock); err == nil {
		if waitExit(waitCh, restStopWait) {
			return
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if waitExit(waitCh, sigtermWait) {
		return
	}
	_ = cmd.Process.Kill()
}

// Re-queues the exit status, so the main select still observes it.
func waitExit(waitCh chan error, d time.Duration) bool {
	select {
	case err := <-waitCh:
		waitCh <- err
		return true
	case <-time.After(d):
		return false
	}
}

func vfkitRestStop(sock string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Post("http://vfkit/vm/state", "application/json",
		strings.NewReader(`{"state":"Stop"}`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vfkit REST stop: %s", resp.Status)
	}
	return nil
}
