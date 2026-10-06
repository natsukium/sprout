package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The short paths (see socketdir.go), resolved before anything boots so an
// over-long one aborts here by name instead of surfacing mid-boot as a bare
// EINVAL.
type instanceSockets struct {
	dir string
	// The daemon's own control socket.
	control string
	net     string
	// Substituted into the runner absolute: microvm.nix expands a relative
	// socket against the runner's cwd, the instance directory the short path
	// exists to avoid (see nix/bundle.nix).
	vmControl string
	// Every backend socket by its manifest name.
	named map[string]string
}

func resolveInstanceSockets(sockDir string, m *Manifest) (instanceSockets, error) {
	s := instanceSockets{dir: sockDir, named: map[string]string{}}
	var err error
	if s.control, err = socketPathIn(sockDir, controlSocketName); err != nil {
		return s, err
	}
	for _, name := range m.contract.socketNames() {
		if s.named[name], err = socketPathIn(sockDir, name); err != nil {
			return s, err
		}
	}
	s.net = s.named[m.contract.networkSocket]
	s.vmControl = s.named[m.contract.controlSocket]
	return s, nil
}

// The sockets the runner and its sidecars bind, as opposed to the daemon's
// own and the network socket its transport serves. A runner that died
// unclean leaves its files behind, where a stale one would pass for a live
// endpoint.
func (s instanceSockets) runnerOwned(m *Manifest) []string {
	paths := []string{s.vmControl}
	for _, sc := range m.contract.sidecars {
		paths = append(paths, s.named[sc.Ready.Socket])
	}
	return paths
}

func removeSocketFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}
func runDaemon(dir string, inst *Instance, m *Manifest, runScript string, sidecarSpecs []SidecarSpec, socks instanceSockets) error {
	sigCh := watchStopSignals()
	defer signal.Stop(sigCh)
	defer removeSocketFiles([]string{socks.net, socks.control})
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

	console, err := m.contract.console.writer(consoleLogPath(dir))
	if err != nil {
		return err
	}
	defer console.Close()
	output := io.MultiWriter(runnerLog, console)

	kind := m.contract.kind.name
	runtimeSocks := socks.runnerOwned(m)
	removeSocketFiles(runtimeSocks)
	defer removeSocketFiles(runtimeSocks)

	if err := stopBeforeBoot(sigCh, inst.Name); err != nil {
		return err
	}
	sidecars, err := startSidecars(sidecarSpecs, socks, dir)
	if err != nil {
		return err
	}
	// After the runner has exited on every path below: a sidecar stopped
	// first would pull a share from under a running guest.
	defer sidecars.stop()

	if err := stopBeforeBoot(sigCh, inst.Name); err != nil {
		return err
	}
	cmd := exec.Command(runScript)
	cmd.Dir = dir
	cmd.Stdout = output
	cmd.Stderr = output
	exit, err := startManaged(cmd)
	if err != nil {
		return fmt.Errorf("%s runner start: %w", kind, err)
	}
	fmt.Printf("instance %q booting (%s runner pid %d) …\n", inst.Name, kind, cmd.Process.Pid)

	ctl := m.contract.control
	vmControl := func() (string, error) { return relinkedSocket(socketDirBase(), dir, socks.vmControl) }

	var stopRequested atomic.Bool
	stop := func() {
		stopRequested.Store(true)
		go gracefulStop(ctl, vmControl, cmd, exit)
	}
	var hardOnce sync.Once
	hard := func() {
		stopRequested.Store(true)
		hardOnce.Do(func() { go hardStop(ctl, vmControl, cmd, exit) })
	}
	srv := &controlServer{vn: vn, inst: inst, started: time.Now(), stop: stop, hardStop: hard, sessions: newSessionTracker(time.Now()), measured: append([]procIdentity{exit.proc}, sidecars.identities()...)}
	if err := serveControl(ctx, socks.control, srv); err != nil {
		// The runner is already up; returning without stopping it would strand
		// a runner holding var.img with no control socket, invisible to every
		// probe until the next boot's orphan reaper finds it.
		gracefulStop(ctl, vmControl, cmd, exit)
		<-exit.done
		return err
	}

	go func() {
		waitReady(ctx, vn, inst, readyFilePath(dir), func() { srv.ready.Store(true) })
		// At readiness, not daemon start, so the ~30s boot never counts as
		// idle time.
		startIdleWatch(ctx, m, srv)
	}()

	lost := awaitRunnerExit(sigCh, sidecars.exited(), exit, func() { srv.stopOnce.Do(stop) })
	err = exit.err

	if lost != nil {
		failure := sidecars.failure(lost, fmt.Sprintf("exited while the VM was running: %v", lost.exit.err))
		if !srv.ready.Load() {
			return failure
		}
		fmt.Fprintln(os.Stderr, failure)
		fmt.Printf("instance %q stopped\n", inst.Name)
		return nil
	}

	// Normal shutdowns (a control-socket stop, guest poweroff) also exit
	// non-zero, so only unexpected failures are reported.
	if err != nil {
		// Dying before SSH ever answered is a failed boot, and must exit
		// non-zero: a detached `up` reads a clean exit as a handoff to a
		// running daemon. After readiness, a late exit is just a report — a
		// guest that powered itself off is not a failure.
		if !srv.ready.Load() && !stopRequested.Load() {
			msg := fmt.Sprintf("%s runner exited before the VM became reachable: %v (see %s)", kind, err, logPath)
			if hint := m.contract.runnerFailureHint(runnerLogTail(logPath, runnerLogTailBytes), dir); hint != "" {
				msg += "\n" + hint
			}
			return errors.New(msg)
		}
		fmt.Fprintf(os.Stderr, "%s runner exited: %v (see %s)\n", kind, err, logPath)
	}
	fmt.Printf("instance %q stopped\n", inst.Name)
	return nil
}

// Runners report their errors on the last few lines; everything before them is
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

// Generous on purpose: killing vfkit too early orphans the
// Virtualization.framework XPC helper before it tears down.
var (
	controlStopWait = 30 * time.Second
	sigtermWait     = 15 * time.Second
)

// vmControl's path must be sun_path-safe (see socketdir.go): the runner bound the same
// file relative to the instance directory.
func gracefulStop(ctl controlProtocol, vmControl func() (string, error), cmd *exec.Cmd, exit *runnerExit) {
	if requestStopVia(ctl, vmControl, false) && exit.within(controlStopWait) {
		return
	}
	killRunner(cmd, exit)
}

// The guest gets no shutdown, but the backend cuts the power itself
// (Virtualization.framework for vfkit, quit for QEMU), so the runner still
// exits on its own.
var hardStopWait = 5 * time.Second

func hardStop(ctl controlProtocol, vmControl func() (string, error), cmd *exec.Cmd, exit *runnerExit) {
	if requestStopVia(ctl, vmControl, true) && exit.within(hardStopWait) {
		return
	}
	killRunner(cmd, exit)
}

func killRunner(cmd *exec.Cmd, exit *runnerExit) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if exit.within(sigtermWait) {
		return
	}
	_ = cmd.Process.Kill()
}

// A single reaper that every stop path observes: with a channel of the exit
// status instead, whichever path received first would hide the exit from the
// others.
type runnerExit struct {
	done chan struct{}
	err  error
	proc procIdentity
}

func watchRunner(cmd *exec.Cmd) *runnerExit {
	exit := &runnerExit{done: make(chan struct{})}
	go func() {
		exit.err = cmd.Wait()
		close(exit.done)
	}()
	return exit
}

func (e *runnerExit) within(d time.Duration) bool {
	select {
	case <-e.done:
		return true
	case <-time.After(d):
		return false
	}
}

// Installed before the daemon starts anything: under Go's default action a
// SIGTERM ends sprout, and PDEATHSIG then hands QEMU a SIGTERM it quits on
// without a guest poweroff.
func watchStopSignals() chan os.Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return sigCh
}

// An error, not a clean exit: a detached `up` reads a clean exit before
// readiness as a handoff to a running daemon.
func stopBeforeBoot(sigCh <-chan os.Signal, name string) error {
	select {
	case sig := <-sigCh:
		return fmt.Errorf("received %s before instance %q booted; not booting it", sig, name)
	default:
		return nil
	}
}

// Blocks until the runner has exited, asking it to stop on a signal or on a
// lost sidecar, and returns the sidecar when one was lost.
func awaitRunnerExit(sigCh <-chan os.Signal, lostSidecar <-chan *sidecar, exit *runnerExit, stop func()) *sidecar {
	select {
	case sig := <-sigCh:
		fmt.Printf("\nreceived %s, shutting down …\n", sig)
		stop()
		<-exit.done
	case p := <-lostSidecar:
		// virtiofsd also exits the moment QEMU disconnects, which can be seen
		// before QEMU's own exit: only a sidecar the runner outlives is lost.
		if exit.within(sidecarLostGrace) {
			return nil
		}
		stop()
		<-exit.done
		return p
	case <-exit.done:
	}
	return nil
}

func requestStopVia(ctl controlProtocol, vmControl func() (string, error), hard bool) bool {
	sock, err := vmControl()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach the VM's control socket (%v); stopping its runner instead\n", err)
		return false
	}
	return ctl.requestStop(sock, hard) == nil
}
