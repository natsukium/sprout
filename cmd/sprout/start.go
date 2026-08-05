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
	inst, dir, err := loadInstance(id.ID)
	if err != nil {
		return err
	}

	if instanceRunning(id.ID) {
		fmt.Printf("instance %q is already running\n", id.Display())
		return nil
	}

	if _, err := os.Stat(inst.Bundle); err != nil {
		return fmt.Errorf("build for %q is no longer in the store (%s); run `sprout up` to rebuild it", id.Display(), inst.Bundle)
	}
	manifest, err := loadManifest(filepath.Join(inst.Bundle, "manifest.json"))
	if err != nil {
		return err
	}
	return bootInstance(dir, inst, manifest)
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
	return launchDetached(id, selector, startChildArgs(id.ID), "starting", "boot")
}

// --foreground prevents each detached child from spawning another child.
func startChildArgs(id string) []string {
	return []string{"start", "--foreground", "--instance", id}
}
