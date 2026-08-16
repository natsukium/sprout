package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	var all, project bool
	cmd := &cobra.Command{
		Use:     "stop",
		Short:   "Graceful shutdown, keep state",
		GroupID: groupDaily,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "stop every instance on this host")
	cmd.Flags().BoolVar(&project, "project", false, "stop every instance of the current repository")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if all || project {
			if all && project {
				return usagef("--all and --project name different scopes; choose one")
			}
			if *selector != "" {
				return usagef("--all/--project stop a scope of instances; do not also select one with -i")
			}
			ids, err := scopeIDs(project)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				fmt.Println("no instances to stop")
				return nil
			}
			return forEachOf(ids, func(id string) error {
				return stopOne(id, stopBehavior{quietIfNotRunning: true, reportStopped: true})
			})
		}
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		return stopOne(id.ID, stopBehavior{reportStopped: true, addressed: id})
	}
	return cmd
}

func scopeIDs(projectOnly bool) ([]string, error) {
	if projectOnly {
		return projectInstanceIDs()
	}
	return instanceIDs()
}

type stopBehavior struct {
	quietIfNotRunning bool
	reportStopped     bool
	addressed         *Identity
}

func stopOne(id string, behavior stopBehavior) error {
	// Read once: every use below would otherwise re-read instance.json, and the
	// record is deleted out from under the last of them by `delete`.
	name := displayForID(id)
	if !instanceRunning(id) {
		// Also the state a SIGKILLed daemon leaves behind, so this is where a
		// client sweeps the credentials its skipped defer stranded on disk.
		sweepStaleCredentials(id)
		if behavior.quietIfNotRunning {
			return nil
		}
		msg := fmt.Sprintf("instance %q is not running", name)
		if addressed := behavior.addressed; addressed != nil {
			msg = fmt.Sprintf("instance %q is not running", addressed.Display())
			if hint := siblingHint(addressed); hint != "" {
				msg += "\n" + hint
			}
		}
		return errors.New(msg)
	}
	// A concurrent stop can tear the daemon down between the running check and
	// this request, and a daemon already gone is the state STOP asked for.
	if _, err := controlRequest(id, "STOP"); err != nil && instanceRunning(id) {
		return fmt.Errorf("instance %q: stop failed: %w", name, err)
	}
	// Polled until the control socket goes quiet, so `stop && rm`
	// compositions are safe.
	stopped := pollUntil(60*time.Second, 500*time.Millisecond, func() bool {
		return !instanceRunning(id)
	})
	if !stopped {
		return fmt.Errorf("instance %q did not stop within 60s", name)
	}
	// The daemon's own exit path already deleted them; this only matters when
	// it died between dropping control and running its defers.
	sweepStaleCredentials(id)
	if behavior.reportStopped {
		fmt.Printf("instance %q stopped\n", name)
	}
	return nil
}

func newDeleteCmd() *cobra.Command {
	var (
		force   bool
		all     bool
		project bool
	)
	cmd := &cobra.Command{
		Use:     "delete",
		Short:   "Stop the environment and delete its state",
		GroupID: groupDaily,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation")
	cmd.Flags().BoolVar(&all, "all", false, "delete every instance on this host")
	cmd.Flags().BoolVar(&project, "project", false, "delete every instance of the current repository")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if all || project {
			if all && project {
				return usagef("--all and --project name different scopes; choose one")
			}
			if *selector != "" {
				return usagef("--all/--project delete a scope of instances; do not also select one with -i")
			}
			ids, err := scopeIDs(project)
			if err != nil {
				return err
			}
			return deleteInstances(ids, force)
		}
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		return deleteInstances([]string{id.ID}, force)
	}
	return cmd
}

var errAborted = errors.New("aborted")

type deleteTarget struct {
	id        string
	dir       string
	snapshots int
}

func newDeleteTarget(id string) (deleteTarget, error) {
	dir, err := instanceDir(id)
	if err != nil {
		return deleteTarget{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return deleteTarget{}, fmt.Errorf("instance %q has no state to delete", displayForID(id))
	}
	return deleteTarget{id: id, dir: dir, snapshots: countSnapshots(dir)}, nil
}

func (t deleteTarget) listLine() string {
	snaps := ""
	if t.snapshots > 0 {
		snaps = fmt.Sprintf(", %d snapshot(s)", t.snapshots)
	}
	return fmt.Sprintf("  %s (%s%s)", displayForID(t.id), t.id, snaps)
}

// The flock rather than a PING: a daemon holds it from the start of its boot
// but answers control only once the runner is up, so gating on PING could pull
// var.img out from under a booting daemon.
func (t deleteTarget) lock(wait time.Duration) (*os.File, error) {
	lock, err := acquireInstanceLock(t.dir, wait)
	if err != nil {
		return nil, fmt.Errorf("instance %q is booting, or another sprout process holds it; stop it first: %w", displayForID(t.id), err)
	}
	return lock, nil
}

func listTargets(targets []deleteTarget) {
	for _, t := range targets {
		fmt.Println(t.listLine())
	}
}

func deleteInstances(ids []string, force bool) error {
	targets := make([]deleteTarget, 0, len(ids))
	for _, id := range ids {
		t, err := newDeleteTarget(id)
		if err != nil {
			return err
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		fmt.Println("no instances to delete")
		return nil
	}
	if !force && !confirmDeletion(targets) {
		return errAborted
	}
	var errs []error
	for _, t := range targets {
		errs = append(errs, deleteOne(t))
	}
	return errors.Join(errs...)
}

func confirmDeletion(targets []deleteTarget) bool {
	if len(targets) == 1 {
		t := targets[0]
		also := ""
		if t.snapshots > 0 {
			also = fmt.Sprintf(" and its %d snapshot(s)", t.snapshots)
		}
		return confirmYes(fmt.Sprintf("delete instance %q including its persistent /var volume%s?", displayForID(t.id), also))
	}
	listTargets(targets)
	return confirmYes(fmt.Sprintf("delete the %d instance(s) above, including their persistent /var volumes?", len(targets)))
}

// Confirmation is the caller's job, so a bulk delete asks once.
func deleteOne(t deleteTarget) error {
	name := displayForID(t.id)
	if instanceRunning(t.id) {
		if err := stopOne(t.id, stopBehavior{}); err != nil {
			return err
		}
	}
	// The wait covers the moment after stopOne where the daemon's exit trails
	// its control socket going quiet.
	lock, err := t.lock(2 * time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := os.RemoveAll(t.dir); err != nil {
		return err
	}
	// The socket directory symlink lives outside t.dir (see socketdir.go).
	removeSocketDir(t.id)
	fmt.Printf("instance %q deleted\n", name)
	return nil
}

var confirmIn io.Reader = os.Stdin

// Only an explicit leading y/Y accepts: EOF, an empty line, and a read error
// all land on the declining side.
func confirmYes(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	var answer string
	fmt.Fscanln(confirmIn, &answer) //nolint:errcheck
	// A terminal echoed the Enter and already advanced; pipes and test readers
	// did not.
	if !readerEchoesInput(confirmIn) {
		fmt.Println()
	}
	return strings.HasPrefix(strings.ToLower(answer), "y")
}

func readerEchoesInput(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func forEachOf(ids []string, fn func(id string) error) error {
	var errs []error
	for _, id := range ids {
		errs = append(errs, fn(id))
	}
	return errors.Join(errs...)
}

func newPruneCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "prune",
		Short:   "Delete orphaned instances (worktree/branch gone)",
		GroupID: groupIntegration,
		Args:    usageArgs(cobra.NoArgs),
		RunE:    func(_ *cobra.Command, _ []string) error { return cmdPrune(force) },
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation")
	return cmd
}

// Running instances are never touched, even when orphaned: that is a bug to
// fix, not something prune should paper over.
func cmdPrune(force bool) error {
	ids, err := instanceIDs()
	if err != nil {
		return err
	}
	var orphans []deleteTarget
	for _, id := range ids {
		if instanceRunning(id) {
			continue
		}
		inst, _, err := loadInstance(id)
		if err != nil || !isOrphaned(inst) {
			continue
		}
		t, err := newDeleteTarget(id)
		if err != nil {
			return err
		}
		orphans = append(orphans, t)
	}

	if len(orphans) == 0 {
		fmt.Println("nothing to prune")
		return nil
	}
	listTargets(orphans)
	if !force && !confirmYes(fmt.Sprintf("delete %d orphaned instance(s) above, including their /var volumes?", len(orphans))) {
		return errAborted
	}
	removed := 0
	for _, t := range orphans {
		// Skipped rather than fatal: one instance a boot has claimed since
		// classification should not strand the rest of the sweep.
		lock, err := t.lock(0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skipping:", err)
			continue
		}
		if err := os.RemoveAll(t.dir); err != nil {
			lock.Close()
			return err
		}
		removeSocketDir(t.id)
		lock.Close()
		removed++
	}
	fmt.Printf("removed %d instance(s)\n", removed)
	return nil
}
