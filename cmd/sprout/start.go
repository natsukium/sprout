package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// `start` must not evaluate the flake: it boots the recorded build, while
// changed definitions remain `up`'s responsibility.
func newStartCmd() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:     "start",
		Short:   "Boot a stopped instance (no rebuild, any dir)",
		GroupID: groupIntegration,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run the daemon in this process instead of returning once the VM is ready (for a supervisor)")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if err := requireBootableHost(); err != nil {
			return err
		}
		if !foreground {
			return startDetached(*selector)
		}
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		return startForeground(id)
	}
	return cmd
}

func startForeground(id *Identity) error {
	if instanceRunning(id.ID) {
		fmt.Printf("instance %q is already running\n", id.Display())
		return nil
	}
	dir, err := instanceDir(id.ID)
	if err != nil {
		return err
	}
	// Retained through reconcileRoots: its migration path publishes a token,
	// which requires the lifecycle lock.
	lc, err := acquireLifecycleLock(id.ID)
	if err != nil {
		return err
	}
	defer lc.Close()
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return &instanceNotFoundError{selector: id.ID}
		}
		return err
	}
	lock, err := claimInstanceLocked(dir, instanceLockWait, func() bool { return instanceRunning(id.ID) })
	// `start` boots the bundle already on record, so any serving daemon is the
	// state it asked for; converging a changed definition is `up`'s job.
	if errors.Is(err, errInstanceNowServing) {
		fmt.Printf("instance %q is already running\n", id.Display())
		return nil
	}
	if err != nil {
		return err
	}
	// Swept here rather than left to the boot's cleanup, so a failed start
	// does not exit with a SIGKILLed daemon's secrets still on disk.
	if err := os.RemoveAll(credentialsDir(dir)); err != nil {
		lock.Close()
		return err
	}
	// Loaded under the claim: before it binds the incarnation, a concurrent
	// delete or `up` can replace the record.
	inst, _, err := loadInstance(id.ID)
	if err != nil {
		lock.Close()
		return err
	}
	// Lazy migration: a legacy record is resolved, pinned, and rewritten
	// before anything reads the bundle.
	if err := reconcileRoots(dir, inst); err != nil {
		lock.Close()
		return err
	}
	lc.Close()
	manifest, err := loadManifest(filepath.Join(inst.Bundle, "manifest.json"))
	if err != nil {
		lock.Close()
		return err
	}
	return bootInstanceLocked(dir, inst, manifest, lock)
}

func stoppedError(id *Identity, selector string) error {
	msg := fmt.Sprintf("instance %q is stopped; start it with: %s", id.Display(), withSelector("sprout up", selector))
	if hint := startHint(id.ID, selector); hint != "" {
		msg += "\n" + hint
	}
	if hint := siblingHint(id); hint != "" {
		msg += "\n" + hint
	}
	return errors.New(msg)
}

// Do not suggest `start` when only the preceding `up` hint can recover.
func startHint(id, selector string) string {
	inst, _, err := loadInstance(id)
	if err != nil {
		return ""
	}
	// A relative legacy path would stat against this caller's cwd; no hint
	// beats a wrong one.
	if !filepath.IsAbs(inst.Bundle) {
		return ""
	}
	if _, err := os.Stat(inst.Bundle); err != nil {
		return ""
	}
	return fmt.Sprintf("its build is still in the store, so %s boots it without rebuilding", withSelector("sprout start", selector))
}

func startDetached(selector string) error {
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}
	// Check before detaching so a missing record is not reported only in the log.
	if _, _, err := loadInstance(id.ID); err != nil {
		return err
	}
	return launchDetached(id, selector, startChildArgs(id.ID), "starting", "boot", false)
}

// --foreground prevents each detached child from spawning another child.
func startChildArgs(id string) []string {
	return []string{"start", "--foreground", "--instance", id}
}
