package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func newUpCmd() *cobra.Command {
	var (
		def            string
		flakeRef       string
		bundle         string
		foreground     bool
		expectInstance string
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
	cmd.Flags().StringVar(&expectInstance, "expect-instance", "", "abort unless the selector resolves to this instance ID (internal, set by the detached parent)")
	_ = cmd.Flags().MarkHidden("expect-instance")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return cmdUp(*selector, def, flakeRef, bundle, foreground, expectInstance)
	}
	return cmd
}

func cmdUp(selector, def, flakeRef, bundle string, foreground bool, expect string) error {
	if err := requireBootableHost(); err != nil {
		return err
	}
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
	if err := refuseDiskWithoutRecord(id.ID, id.Display()); err != nil {
		return err
	}
	// The detached parent resolved the selector once; if its instance was
	// deleted since, this child's re-resolution would mint a different
	// instance than the one the parent waits on — the pinned ID turns that
	// into an abort. An empty KeySource is a recordless re-scaffolded
	// directory: booting it would publish a broken record.
	if expect != "" && (id.ID != expect || id.KeySource == "") {
		return fmt.Errorf("instance %s was deleted while `up` was starting; re-run `sprout up`", expect)
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
	return launchDetached(id, selector, upChildArgs(id.ID, selector, def, flakeRef, bundle), "building and booting", "up", true)
}

// --foreground is mandatory: without it the child would detach in turn and
// fork forever.
func upChildArgs(id, selector, def, flakeRef, bundle string) []string {
	args := []string{"up", "--foreground", "--flake", flakeRef, "--expect-instance", id}
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
func bootDetached(id string, childArgs []string, supersededPID int, what string, createDir bool, announce func(logPath string)) error {
	dir, err := instanceDir(id)
	if err != nil {
		return err
	}
	logf, err := scaffoldDetached(id, dir, createDir)
	if err != nil {
		return err
	}
	defer logf.Close()
	logPath := upLogPath(dir)

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

// Scaffolding under the lifecycle lock keeps the dir and log from landing
// inside a directory a delete is renaming away. `up` is a create command;
// `start` is not, so for it a missing directory means no instance.
func scaffoldDetached(id, dir string, create bool) (*os.File, error) {
	lc, err := acquireLifecycleLock(id)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	logf, err := os.Create(upLogPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &instanceNotFoundError{selector: id}
		}
		return nil, err
	}
	return logf, nil
}

func launchDetached(id *Identity, selector string, childArgs []string, action, what string, createDir bool) error {
	// Captured before the child replaces it: an in-place `up` reboot keeps the
	// current VM answering through the rebuild, so readiness must wait for a
	// *different* daemon, not just any.
	supersededPID := runningPID(id.ID)
	err := bootDetached(id.ID, childArgs, supersededPID, what, createDir, func(logPath string) {
		fmt.Printf("%s %q in the background (log: %s) …\n", action, id.Display(), logPath)
	})
	if err != nil {
		return err
	}
	fmt.Printf("VM ready. Enter it with: %s\n", withSelector("sprout shell", selector))
	return nil
}

// The child runs in its own process group so a Ctrl-C aimed at the foreground
// command never reaches a daemon meant to outlive it, and neither does the
// SIGHUP of the terminal closing: a hangup signals only the session leader
// and the foreground group, so no new session is needed. It also starts without
// the caller's descriptors: a caller that serializes boots with `flock 9> lock`
// around `sprout up` would otherwise never get the lock back, since the lock
// belongs to the open file description the daemon and its runner inherited.
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
	if info, err := queryInfoBrief(id); err == nil {
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
	tok, err := publishAttempt(id.ID, dir)
	if err != nil {
		return err
	}
	// A committed record whose canonical pin failed may have the staging root
	// as its only GC protection; every other exit retires the token.
	keepToken := false
	defer func() {
		if keepToken {
			tok.Close()
		} else {
			tok.remove()
		}
	}()

	bundle := bundlePath
	if bundle == "" {
		host, err := hostNixSystem()
		if err != nil {
			return err
		}
		fmt.Printf("building %s#sproutConfigurations.%s.%s …\n", flakeRef, host, def)
		bundle, err = nixBuild(stagingBundleLink(tok.dir), flakeRef, host, def)
		if err != nil {
			if shapeErr := diagnoseOutputShape(flakeRef, host); shapeErr != nil {
				return shapeErr
			}
			return err
		}
	} else {
		bundle, err = intakeBundleArg(bundlePath, tok)
		if err != nil {
			return err
		}
	}

	manifest, err := loadManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	if err := manifest.contract.bootable(); err != nil {
		return err
	}

	if bundlePath != "" {
		def = manifest.Definition
	}

	// Bounded rather than looping until the check settles: two `up`s carrying
	// different bundles can keep stopping each other's boot, and a bound turns
	// that ping-pong into an error instead of an endless reboot cycle.
	for attempt := 0; ; attempt++ {
		lock, inst, err := prepareUpBoot(id, dir, tok, def, bundle, manifest, attempt, &keepToken)
		if errors.Is(err, errUpConverged) {
			return nil
		}
		if errors.Is(err, errInstanceNowServing) {
			fmt.Printf("another sprout process booted %q first, rechecking …\n", id.Display())
			continue
		}
		if err != nil {
			return err
		}
		return bootInstanceLocked(dir, inst, manifest, lock)
	}
}

var errUpConverged = errors.New("instance already runs this bundle")

// The token is verified before the stop and around the claim: a stale `up`
// whose instance was deleted mid-build must neither stop the replacement's
// daemon nor drop a daemon.lock into its directory. The record is committed
// before reconcileRoots, so a crash between the two leaves the staging root
// protecting it until the next boot re-pins.
func prepareUpBoot(id *Identity, dir string, tok *attemptToken, def, bundle string, manifest *Manifest, attempt int, keepToken *bool) (*os.File, *Instance, error) {
	lc, err := acquireLifecycleLock(id.ID)
	if err != nil {
		return nil, nil, err
	}
	defer lc.Close()
	if err := tok.verify(); err != nil {
		return nil, nil, err
	}
	// Before anything acts on the instance, so a refusal leaves a running VM
	// running and the record still describing the disk.
	if err := checkUpCompatible(id, dir, manifest); err != nil {
		return nil, nil, err
	}
	if err := checkInstanceSocketPaths(dir, manifest); err != nil {
		return nil, nil, err
	}
	if instanceRunning(id.ID) {
		prev, _, loadErr := loadInstance(id.ID)
		if loadErr == nil && sameBundle(prev.Bundle, bundle) {
			fmt.Printf("instance %q is already running\n", id.Display())
			return nil, nil, errUpConverged
		}
		if attempt >= 3 {
			if loadErr != nil {
				return nil, nil, fmt.Errorf("instance %q is running but its record cannot be read: %w", id.Display(), loadErr)
			}
			return nil, nil, fmt.Errorf("another sprout process keeps booting %q with a different definition; retry once the concurrent `up`s agree", id.Display())
		}
		fmt.Printf("instance %q definition changed, rebooting …\n", id.Display())
		if err := stopLocked(id.ID, id.Display()); err != nil {
			return nil, nil, fmt.Errorf("stopping %q for reboot: %w", id.Display(), err)
		}
		fmt.Printf("instance %q stopped\n", id.Display())
	} else if attempt >= 3 {
		return nil, nil, fmt.Errorf("booting %q keeps losing to other sprout processes; retry once the concurrent boots settle", id.Display())
	}

	lock, err := claimInstanceLocked(dir, instanceLockWait, func() bool { return instanceRunning(id.ID) })
	if err != nil {
		return nil, nil, err
	}
	if err := tok.verify(); err != nil {
		lock.Close()
		return nil, nil, err
	}
	// Swept here rather than left to the boot's cleanup, so a failed attempt
	// does not exit with a SIGKILLed daemon's secrets still on disk.
	if err := os.RemoveAll(credentialsDir(dir)); err != nil {
		lock.Close()
		return nil, nil, err
	}
	inst := id.newInstance()
	inst.Definition, inst.Bundle = def, bundle
	inst.GuestIP, inst.SSHUser = manifest.Guest.IP, manifest.Guest.SSHUser
	inst.Platform = manifest.platform()
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		lock.Close()
		return nil, nil, err
	}
	if err := reconcileRoots(dir, inst); err != nil {
		*keepToken = true
		lock.Close()
		return nil, nil, err
	}
	tok.remove()
	return lock, inst, nil
}

// `up` is a create command, so no record and no disk is a new instance.
func checkUpCompatible(id *Identity, dir string, manifest *Manifest) error {
	prev, _, err := loadInstance(id.ID)
	if err != nil {
		if _, statErr := os.Stat(varImagePath(dir)); os.IsNotExist(statErr) {
			return nil
		}
		return diskWithoutRecordError(id.Display(), err)
	}
	return checkInstancePlatform(id.Display(), prev.Platform, manifest.platform())
}

// Convergence compares builds, not spellings: a legacy record is resolved,
// and an unresolvable one counts as different — the reboot then migrates it.
func sameBundle(recorded, canonical string) bool {
	if recorded == canonical {
		return true
	}
	if !filepath.IsAbs(recorded) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(recorded)
	return err == nil && resolved == canonical
}

// Creation happens under the lifecycle lock so a delete completing first
// serializes into a clean fresh creation instead of racing the removal.
func publishAttempt(id, dir string) (*attemptToken, error) {
	lc, err := acquireLifecycleLock(id)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return publishToken(dir)
}

// Canonicalized at intake so records never carry relative or symlinked paths;
// a store-backed argument gets a staging root before any GC could collect it,
// a non-store directory stays intentionally rootless.
func intakeBundleArg(arg string, tok *attemptToken) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if storeRoot, ok := storeRootOf(canonical); ok {
		if err := pinBundleRoot(stagingBundleLink(tok.dir), storeRoot); err != nil {
			return "", err
		}
	}
	return canonical, nil
}

// The guest's host key lives only in /var (nix/guest/base.nix), so a missing
// var.img means the runner mkfs's a fresh volume and sshd generates a new key.
// A surviving known_hosts turns that into ssh's "REMOTE HOST IDENTIFICATION
// HAS CHANGED" — a possible-attack warning for what is really lost state.
//
// known_hosts rather than instance.json is the discriminator: the record is
// rewritten every boot, while known_hosts exists only if a past boot's SSH
// succeeded, which implies a var.img was there to hold the key.
func resetStaleHostTrust(dir string) error {
	if _, err := os.Stat(varImagePath(dir)); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(knownHostsPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fmt.Fprintln(os.Stderr, "warning: this instance's /var volume is gone; the guest boots from an empty /var with a new SSH host key, so its persistent guest state was lost")
	return nil
}

// Shared by `up` and `start`, so a restart takes the same guest-setup path as
// a first boot and credentials are re-projected rather than frozen at the
// first `up`. Ownership of the caller's claimed lock fd transfers here; held
// for the daemon's whole life, it is what licenses reapOrphans to kill VM
// processes rather than ask about them.
func bootInstanceLocked(dir string, inst *Instance, manifest *Manifest, lock *os.File) error {
	defer lock.Close()
	inst.WorkspaceMounted = manifest.Workspace

	if err := os.MkdirAll(sshDataDir(dir), 0o700); err != nil {
		return err
	}
	if err := resetStaleHostTrust(dir); err != nil {
		return err
	}
	sockDir, err := prepareSocketDir(socketDirBase(), dir)
	if err != nil {
		return err
	}
	socks, err := resolveInstanceSockets(sockDir, manifest)
	if err != nil {
		return err
	}
	if err := reapOrphans(dir, socks, manifest); err != nil {
		return err
	}
	if err := awaitDiskReleased(dir); err != nil {
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

	subs := map[string]string{
		"netSocket":  socks.net,
		"restSocket": socks.vmControl,
		"dataDir":    dataDir(dir),
		"workspace":  inst.Workspace,
		"gitCommon":  inst.RepoRoot,
		"consolePty": "virtio-serial,pty",
		"hostUid":    strconv.Itoa(os.Getuid()),
		"hostGid":    strconv.Itoa(os.Getgid()),
	}
	for name, path := range socks.named {
		subs["socket:"+name] = path
	}
	// Start empty: a SIGKILLed daemon skips the cleanup below, and a credential
	// dropped from the definition would keep its stale file alive.
	if err := os.RemoveAll(credentialsDir(dir)); err != nil {
		return err
	}
	// Registered before setup, not after: a setup failing on its second
	// credential has already written its first, which needs the same cleanup.
	defer os.RemoveAll(credentialsDir(dir))
	// Before boot: a missing mount source or a failing materialize command
	// should abort, not surface later as an empty mount or an unauthenticated
	// tool.
	if err := setupCredentials(manifest, subs, dataDir(dir)); err != nil {
		return err
	}
	if err := addCacheSubstitutions(manifest, subs, inst.RepoRoot); err != nil {
		return err
	}
	runScript := filepath.Join(dir, "run.sh")
	sidecars, err := rewriteRunner(filepath.Join(inst.Bundle, "runner"), manifest, subs, runScript)
	if err != nil {
		return err
	}

	inst.PID = os.Getpid()
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		return err
	}

	return runDaemon(dir, inst, manifest, runScript, sidecars, socks)
}

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

// The out-link is the build's GC root, registered inside the same nix
// invocation so there is no build-to-root window. Exactly one output path:
// with several, the root would silently not cover what gets booted.
func nixBuild(outLink, flakeRef, host, def string) (string, error) {
	attr := fmt.Sprintf("%s#sproutConfigurations.%s.%s", flakeRef, host, def)
	cmd := exec.Command("nix", "build", "--out-link", outLink, "--print-out-paths", attr)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build %s: %w", attr, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 1 {
		return "", fmt.Errorf("nix build %s printed %d output paths; a sprout configuration must produce exactly one bundle", attr, len(lines))
	}
	return lines[0], nil
}

func loadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	host, err := runningHostSystem()
	if err != nil {
		return nil, err
	}
	return parseManifest(data, host)
}

// The runner's shell quoting was fixed at Nix eval time, before any value
// existed, so a value that needs quoting cannot be escaped in — only rejected.
// The class is what nixpkgs' escapeShellArg leaves unquoted.
var runnerSafeValue = regexp.MustCompile(`^[[:alnum:],._+:@%/=-]+$`)

// Substituting text rather than re-deriving backend arguments keeps the
// runner a pure microvm.nix artifact shared by every instance. Sidecars are
// substituted the same way: under QEMU a share's source appears only in its
// virtiofsd argv, never in the runner.
//
// One pass, longest placeholder first: sequential replacement would corrupt a
// placeholder that is a prefix of another (".../credential/aws" vs
// ".../credential/aws-extra"), the shorter one hiding the longer.
func rewriteRunner(runnerPath string, m *Manifest, subs map[string]string, out string) ([]SidecarSpec, error) {
	content, err := os.ReadFile(runnerPath)
	if err != nil {
		return nil, err
	}
	// Validated against untouched content, so a prefix collision cannot make a
	// valid placeholder look missing.
	type pair struct{ placeholder, value string }
	pairs := make([]pair, 0, len(m.Substitutions))
	for _, s := range m.Substitutions {
		value, ok := subs[s.Value]
		if !ok {
			return nil, fmt.Errorf("manifest requests unknown substitution symbol %q (sprout too old for this flake?)", s.Value)
		}
		if !runnerSafeValue.MatchString(value) {
			return nil, fmt.Errorf("%s resolves to %q, which the runner script cannot carry; use a path of alphanumerics and ,._+:@%%/=-", s.Value, value)
		}
		if !bytes.Contains(content, []byte(s.Placeholder)) && !sidecarsMention(m.contract.sidecars, s.Placeholder) {
			return nil, fmt.Errorf("placeholder %q appears in neither the runner script nor any sidecar", s.Placeholder)
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
	r := strings.NewReplacer(oldnew...)
	sidecars := make([]SidecarSpec, len(m.contract.sidecars))
	for i, s := range m.contract.sidecars {
		sidecars[i] = s
		sidecars[i].Exec = make([]string, len(s.Exec))
		for j, arg := range s.Exec {
			sidecars[i].Exec[j] = r.Replace(arg)
		}
	}
	if err := os.WriteFile(out, []byte(r.Replace(string(content))), 0o700); err != nil {
		return nil, err
	}
	return sidecars, nil
}

func sidecarsMention(sidecars []SidecarSpec, placeholder string) bool {
	for _, s := range sidecars {
		for _, arg := range s.Exec {
			if strings.Contains(arg, placeholder) {
				return true
			}
		}
	}
	return false
}

func runDaemon(dir string, inst *Instance, m *Manifest, runScript string, sidecarSpecs []SidecarSpec, socks instanceSockets) error {
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

	sidecars, err := startSidecars(sidecarSpecs, socks, dir)
	if err != nil {
		return err
	}
	// After the runner has exited on every path below: a sidecar stopped
	// first would pull a share from under a running guest.
	defer sidecars.stop()

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

	var stopRequested atomic.Bool
	stop := func() { stopRequested.Store(true); go gracefulStop(ctl, socks.vmControl, cmd, exit) }
	var hardOnce sync.Once
	hard := func() {
		stopRequested.Store(true)
		hardOnce.Do(func() { go hardStop(ctl, socks.vmControl, cmd, exit) })
	}
	srv := &controlServer{vn: vn, inst: inst, started: time.Now(), stop: stop, hardStop: hard, sessions: newSessionTracker(time.Now()), runnerPID: cmd.Process.Pid}
	if err := serveControl(ctx, socks.control, srv); err != nil {
		// The runner is already up; returning without stopping it would strand
		// a runner holding var.img with no control socket, invisible to every
		// probe until the next boot's orphan reaper finds it.
		gracefulStop(ctl, socks.vmControl, cmd, exit)
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

	var lost *sidecar
	select {
	case sig := <-sigCh:
		fmt.Printf("\nreceived %s, shutting down …\n", sig)
		srv.stopOnce.Do(stop)
		<-exit.done
	case p := <-sidecars.exited():
		// virtiofsd also exits the moment QEMU disconnects, which can be seen
		// before QEMU's own exit: only a sidecar the runner outlives is lost.
		if !exit.within(sidecarLostGrace) {
			lost = p
			srv.stopOnce.Do(stop)
			<-exit.done
		}
	case <-exit.done:
	}
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

// sock must be sun_path-safe (see socketdir.go): the runner bound the same
// file relative to the instance directory.
func gracefulStop(ctl controlProtocol, sock string, cmd *exec.Cmd, exit *runnerExit) {
	if err := ctl.requestStop(sock, false); err == nil && exit.within(controlStopWait) {
		return
	}
	killRunner(cmd, exit)
}

// The guest gets no shutdown, but the backend cuts the power itself
// (Virtualization.framework for vfkit, quit for QEMU), so the runner still
// exits on its own.
var hardStopWait = 5 * time.Second

func hardStop(ctl controlProtocol, sock string, cmd *exec.Cmd, exit *runnerExit) {
	if err := ctl.requestStop(sock, true); err == nil && exit.within(hardStopWait) {
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
