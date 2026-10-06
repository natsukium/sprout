package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
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
supervisor (launchd or systemd, via services.sprout) needs: it must own a
process that lives as long as the VM. See docs/how-to/run-as-daemon.md.`,
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
	if err := setAsideUnformattedImage(dir, manifest.VarFsType); err != nil {
		return err
	}
	// After the two above, which can leave var.img missing.
	if err := resetStaleHostTrust(dir); err != nil {
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
