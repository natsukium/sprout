package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	var all, project, hard bool
	cmd := &cobra.Command{
		Use:     "stop",
		Short:   "Graceful shutdown, keep state",
		GroupID: groupDaily,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "stop every instance on this host")
	cmd.Flags().BoolVar(&project, "project", false, "stop every instance of the current repository")
	cmd.Flags().BoolVar(&hard, "hard", false, hardFlagUsage)
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
				return stopOne(id, stopBehavior{quietIfNotRunning: true, reportStopped: true, hard: hard})
			})
		}
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		return stopOne(id.ID, stopBehavior{reportStopped: true, addressed: id, hard: hard})
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
	hard              bool
}

func stopOne(id string, behavior stopBehavior) error {
	// Read once: every use below would otherwise re-read instance.json, and the
	// record is deleted out from under the last of them by `delete`.
	name := displayForID(id)
	dir, err := instanceDir(id)
	if err != nil {
		return err
	}
	// Opened before the lock wait, so a delete and re-create meanwhile is
	// caught before it can take the power cut meant for this incarnation.
	var marker *incarnationMarker
	if behavior.hard {
		if marker, err = openIncarnationMarker(dir); err == nil {
			defer marker.Close()
		}
	}
	lc, err := acquireLifecycleLock(id)
	if err != nil {
		return err
	}
	defer lc.Close()
	if !instanceRunning(id) {
		// Also the state a SIGKILLed daemon leaves behind, so this is where a
		// client sweeps the credentials its skipped defer stranded on disk.
		sweepStaleCredentialsLocked(dir, 0)
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
	stop := stopLocked
	if behavior.hard {
		if marker == nil || marker.verify(dir) != nil {
			return fmt.Errorf("instance %q changed since it was selected; re-run the command", name)
		}
		stop = hardStopLocked
	}
	if err := stop(id, name); err != nil {
		return err
	}
	sweepStaleCredentialsLocked(dir, 2*time.Second)
	if behavior.reportStopped {
		fmt.Printf("instance %q stopped\n", name)
	}
	return nil
}

// The caller holds the lifecycle lock, so this takes no locks and sweeps no
// credentials — either would self-deadlock.
func stopLocked(id, name string) error {
	// A concurrent stop can tear the daemon down between the running check and
	// this request, and a daemon already gone is the state STOP asked for.
	if _, err := controlRequest(id, "STOP"); err != nil && instanceRunning(id) {
		return fmt.Errorf("instance %q: stop failed: %w", name, err)
	}
	return waitStopped(id, name)
}

const hardFlagUsage = "power the VM off without a guest shutdown, after a best-effort guest sync"

// The caller holds the lifecycle lock, as for stopLocked.
func hardStopLocked(id, name string) error {
	info, err := queryInfoBrief(id)
	if err != nil {
		if !instanceRunning(id) {
			return nil
		}
		return fmt.Errorf("instance %q: hard stop failed: %w", name, err)
	}
	if !info.HardStop {
		return fmt.Errorf("instance %q runs a sprout daemon that predates --hard; stop it without --hard", name)
	}
	// Writes still in the guest's page cache reach the host's virtiofs
	// shares only through a sync; a guest that cannot answer loses them.
	if err := guestSync(id); err != nil {
		fmt.Fprintf(os.Stderr, "warning: instance %q: guest sync failed, powering off anyway: %v\n", name, err)
	}
	reply, err := controlRequest(id, "STOP hard")
	if err != nil {
		if !instanceRunning(id) {
			return nil
		}
		return fmt.Errorf("instance %q: hard stop failed: %w", name, err)
	}
	if reply != "OK hard" {
		return fmt.Errorf("instance %q: daemon answered %q to a hard stop", name, reply)
	}
	return waitStopped(id, name)
}

var guestSyncTimeout = 10 * time.Second

var guestSync = func(id string) error {
	sshPath, sshArgs, err := sshInvocation(id, false, []string{"sync"})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestSyncTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath, sshArgs[1:]...)
	// The ProxyCommand child can outlive a killed ssh and hold the output
	// pipe open, which would make the timeout wait for it.
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Polled until the control socket goes quiet, so `stop && rm` compositions
// are safe.
func waitStopped(id, name string) error {
	stopped := pollUntil(60*time.Second, 500*time.Millisecond, func() bool {
		return !instanceRunning(id)
	})
	if !stopped {
		return fmt.Errorf("instance %q did not stop within 60s", name)
	}
	return nil
}

func newDeleteCmd() *cobra.Command {
	var (
		force   bool
		all     bool
		project bool
		hard    bool
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
	cmd.Flags().BoolVar(&hard, "hard", false, hardFlagUsage)
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
			return deleteInstances(ids, force, hard)
		}
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		return deleteInstances([]string{id.ID}, force, hard)
	}
	return cmd
}

var errAborted = errors.New("aborted")

type deleteTarget struct {
	id        string
	dir       string
	snapshots int
	marker    *incarnationMarker
	hard      bool
}

func newDeleteTarget(id string) (deleteTarget, error) {
	dir, err := instanceDir(id)
	if err != nil {
		return deleteTarget{}, err
	}
	marker, err := openIncarnationMarker(dir)
	if err != nil {
		return deleteTarget{}, fmt.Errorf("instance %q has no state to delete", displayForID(id))
	}
	return deleteTarget{id: id, dir: dir, snapshots: countSnapshots(dir), marker: marker}, nil
}

func (t deleteTarget) listLine() string {
	snaps := ""
	if t.snapshots > 0 {
		snaps = fmt.Sprintf(", %d snapshot(s)", t.snapshots)
	}
	return fmt.Sprintf("  %s (%s%s)", displayForID(t.id), t.id, snaps)
}

// The flock rather than a PING: a booting daemon holds the lock before it
// answers control. The caller holds the lifecycle lock.
func (t deleteTarget) claim(wait time.Duration) (*os.File, error) {
	lock, err := claimInstanceLocked(t.dir, wait, nil)
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

func deleteInstances(ids []string, force, hard bool) error {
	targets := make([]deleteTarget, 0, len(ids))
	defer func() {
		for _, t := range targets {
			t.marker.Close()
		}
	}()
	for _, id := range ids {
		t, err := newDeleteTarget(id)
		if err != nil {
			return err
		}
		t.hard = hard
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

// Husks are hidden so instanceIDs skips them, and carry the id so a delete
// never collides with a concurrent one on another instance.
func huskPath(id, dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+id+".deleting")
}

// RemoveAll unlinks entries one at a time, so an interrupted delete can leave
// an instance that still resolves and boots but has lost its /var, or a
// directory that outlives its record and lists as a ghost. Renaming first
// leaves either an untouched instance or an unreferenced husk.
func removeInstanceDir(id, dir string) error {
	husk := huskPath(id, dir)
	// A husk from an interrupted delete would otherwise block the rename.
	if err := os.RemoveAll(husk); err != nil {
		return err
	}
	if err := os.Rename(dir, husk); err != nil {
		return err
	}
	return os.RemoveAll(husk)
}

// Confirmation is the caller's job, so a bulk delete asks once.
func deleteOne(t deleteTarget) error {
	name := displayForID(t.id)
	lc, err := acquireLifecycleLock(t.id)
	if err != nil {
		return err
	}
	defer lc.Close()
	// Before the stop, not after the claim: the marker, not the claim, binds
	// the stop to the incarnation the user confirmed.
	if err := t.marker.verify(t.dir); err != nil {
		return fmt.Errorf("instance %q: %w", name, err)
	}
	if instanceRunning(t.id) {
		stop := stopLocked
		if t.hard {
			stop = hardStopLocked
		}
		if err := stop(t.id, name); err != nil {
			return err
		}
	}
	// The stopped daemon's exit may trail its control socket going quiet.
	lock, err := t.claim(2 * time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Before the husk rename: a delete dying mid-removal must not strand
	// materialized secrets in a hidden husk nothing ever sweeps.
	if err := os.RemoveAll(credentialsDir(t.dir)); err != nil {
		return err
	}
	if err := removeInstanceDir(t.id, t.dir); err != nil {
		return err
	}
	// The socket directory symlink lives outside t.dir (see socketdir.go).
	removeSocketDir(t.dir)
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
	defer func() {
		for _, t := range orphans {
			t.marker.Close()
		}
	}()
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
		ok, err := pruneOne(t)
		if err != nil {
			return err
		}
		if ok {
			removed++
		}
	}
	fmt.Printf("removed %d instance(s)\n", removed)
	return nil
}

// Skips are not fatal: one contested instance should not strand the sweep.
func pruneOne(t deleteTarget) (bool, error) {
	skip := func(reason any) (bool, error) {
		fmt.Fprintln(os.Stderr, "skipping:", reason)
		return false, nil
	}
	lc, err := acquireLifecycleLock(t.id)
	if err != nil {
		return false, err
	}
	defer lc.Close()
	if err := t.marker.verify(t.dir); err != nil {
		return skip(fmt.Errorf("instance %q: %w", displayForID(t.id), err))
	}
	lock, err := t.claim(0)
	if err != nil {
		return skip(err)
	}
	defer lock.Close()
	// Reclassified after the claim: the confirmation ran unlocked.
	inst, _, err := loadInstance(t.id)
	if err != nil || !isOrphaned(inst) {
		return skip(fmt.Errorf("instance %q is no longer orphaned", displayForID(t.id)))
	}
	// Before the husk rename, as in deleteOne.
	if err := os.RemoveAll(credentialsDir(t.dir)); err != nil {
		return false, err
	}
	if err := removeInstanceDir(t.id, t.dir); err != nil {
		return false, err
	}
	removeSocketDir(t.dir)
	return true, nil
}
