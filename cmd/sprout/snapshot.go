package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

type Snapshot struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// Live means the instance was running, so the image is crash-consistent
	// rather than clean.
	Live bool `json:"live"`
	// /etc is rebuilt from the store on every boot while /var survives it, so
	// a restore into a different build pairs mismatched /var and /etc. Usually
	// fine, hence a warning rather than a refusal.
	Definition string `json:"definition"`
	Bundle     string `json:"bundle"`
	// Absent from records written before restore checked it; see
	// loadSnapshotRecord.
	Version int `json:"version,omitempty"`
	Platform
}

func validateSnapshotName(name string) error {
	return validateNameComponent("snapshot", name)
}

func resolveSnapshotTarget(selector, snapName string) (*Identity, *Instance, string, error) {
	if err := validateSnapshotName(snapName); err != nil {
		return nil, nil, "", err
	}
	return resolveAndLoad(selector)
}

func requireSnapshot(path string, id *Identity, snapName string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("instance %q has no snapshot named %q", id.Display(), snapName)
	}
	return nil
}

// Inside the instance directory, so `sprout delete` is honest about deleting
// that directory and nothing else.
func snapshotsDir(instDir string) string { return filepath.Join(instDir, "snapshots") }

func snapshotDir(instDir, name string) string { return filepath.Join(snapshotsDir(instDir), name) }

func snapshotImage(instDir, name string) string {
	return varImagePath(snapshotDir(instDir, name))
}

func snapshotRecordPath(snapDir string) string { return filepath.Join(snapDir, "snapshot.json") }

// The directory is cleared when either step fails: a half-written one would
// list as a real snapshot or instance and restore into a broken /var.
func seedVolumeDir(dir, srcImg string, running bool, recordPath string, record any) (cow bool, err error) {
	cow, err = copyVarImage(srcImg, varImagePath(dir), running)
	if err == nil {
		err = writeJSON(recordPath, record)
	}
	if err != nil {
		os.RemoveAll(dir)
		return false, err
	}
	return cow, nil
}

func crashConsistentNote(what string) string {
	return fmt.Sprintf("note: %s, so its /var is crash-consistent — as if the VM had lost power", what)
}

func printBootHint(selector string) {
	fmt.Printf("boot it with: %s\n", withSelector("sprout start", selector))
}

// A directory whose record cannot be read still lists under its own name:
// losing the metadata does not lose the image.
func listSnapshots(instDir string) ([]Snapshot, error) {
	entries, err := os.ReadDir(snapshotsDir(instDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		snap := Snapshot{Name: e.Name()}
		if data, err := os.ReadFile(snapshotRecordPath(snapshotDir(instDir, e.Name()))); err == nil {
			_ = json.Unmarshal(data, &snap)
			snap.Name = e.Name()
		}
		snaps = append(snaps, snap)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Created.Before(snaps[j].Created) })
	return snaps, nil
}

// Best-effort: an unreadable snapshots directory means "none to mention", not
// a failed command.
func countSnapshots(instDir string) int {
	snaps, err := listSnapshots(instDir)
	if err != nil {
		return 0
	}
	return len(snaps)
}

// The runner creates the image on first boot, so an instance that has only
// ever been recorded has nothing to snapshot.
func liveImage(instDir, display string) (string, error) {
	img := varImagePath(instDir)
	if _, err := os.Stat(img); err != nil {
		return "", fmt.Errorf("instance %q has no /var volume yet; boot it once with `sprout start` first", display)
	}
	return img, nil
}

// Unlike a PING probe, the lock leaves no window for a boot to slip in under
// a var.img operation: a daemon holds it for its whole life.
func lockIdleInstance(id, display string) (*os.File, error) {
	lock, err := claimInstance(id, 0, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("instance %q is running, or another sprout process holds it; stop it first: %w", display, err)
	}
	return lock, nil
}

// The returned lock is nil when running, where the caller takes the
// crash-consistent CoW path. On the live path the caller must keep holding
// the lifecycle lock through the copy — nothing else pins the daemon.
func claimOrLive(id, instDir, what, verb string, live bool) (lock *os.File, running bool, err error) {
	lock, lockErr := claimInstanceLocked(instDir, 0, nil)
	if lockErr == nil {
		return lock, false, nil
	}
	if errors.Is(lockErr, os.ErrNotExist) {
		return nil, false, lockErr
	}
	if !live {
		return nil, false, fmt.Errorf("%s is running, or another sprout process holds it; stop it first, or pass --live to %s it as it runs: %w", what, verb, lockErr)
	}
	// Contention alone is not liveness: only an answering control socket
	// licenses copying under a running daemon.
	if !instanceRunning(id) {
		return nil, false, fmt.Errorf("%s is busy (another sprout process holds it, but no daemon answers yet); retry shortly", what)
	}
	return nil, true, nil
}

// A stopped instance's image is at rest, so either method is safe. A running
// one may only take the CoW path, which alone is atomic: the copy is what a
// power cut would have left, where a multi-second read against live writes
// yields no consistent state at all.
func copyVarImage(src, dst string, live bool) (cow bool, err error) {
	if !live {
		return cloneFile(src, dst)
	}
	if err := cowClone(src, dst); err != nil {
		if errors.Is(err, errCoWUnsupported) {
			if markedNoCoW(src) {
				return false, fmt.Errorf("%s is marked No_COW (chattr +C), which rules out copy-on-write clones, so a running instance's image cannot be copied atomically; stop the instance and retry without --live, then restore that snapshot once to replace the image with one that clones", src)
			}
			return false, fmt.Errorf("%s is on a filesystem without copy-on-write clones, so a running instance's image cannot be copied atomically; stop the instance and retry without --live", filepath.Dir(src))
		}
		return false, err
	}
	return true, nil
}

func copyNote(cow bool, src string) string {
	if cow {
		return "copy-on-write clone, no additional disk used yet"
	}
	if markedNoCoW(src) {
		return "full copy: " + src + " is marked No_COW (chattr +C), which rules out copy-on-write clones; restoring a snapshot once replaces it with an image that clones"
	}
	return "full copy: this filesystem has no copy-on-write clones"
}

func newSnapshotCmd() *cobra.Command {
	cmd := groupingCmd(&cobra.Command{
		Use:     "snapshot",
		Short:   "Save, list, delete, or roll back the /var volume",
		GroupID: groupData,
	})
	cmd.AddCommand(
		newSnapshotCreateCmd(),
		newSnapshotListCmd(),
		newSnapshotDeleteCmd(),
		newSnapshotRestoreCmd(),
	)
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var live bool
	cmd := &cobra.Command{
		Use:   "create SNAPSHOT",
		Short: "Save the /var volume (copy-on-write where the filesystem allows)",
		Args:  usageArgs(cobra.ExactArgs(1)),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&live, "live", false, "snapshot a running instance (crash-consistent; requires a copy-on-write filesystem)")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		return cmdSnapshotCreate(*selector, live, args[0])
	}
	return cmd
}

func cmdSnapshotCreate(selector string, live bool, snapName string) error {
	id, _, dir, err := resolveSnapshotTarget(selector, snapName)
	if err != nil {
		return err
	}
	lc, err := acquireLifecycleLock(id.ID)
	if err != nil {
		return err
	}
	defer lc.Close()
	lock, running, err := claimOrLive(id.ID, dir, fmt.Sprintf("instance %q", id.Display()), "snapshot", live)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
		// The held boot lock keeps delete out transitively; a full copy must
		// not block every lifecycle waiter.
		lc.Close()
	}
	inst, _, err := loadInstance(id.ID)
	if err != nil {
		return err
	}
	img, err := liveImage(dir, id.Display())
	if err != nil {
		return err
	}
	if _, err := os.Stat(snapshotDir(dir, snapName)); err == nil {
		return fmt.Errorf("instance %q already has a snapshot named %q", id.Display(), snapName)
	}

	target := snapshotDir(dir, snapName)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	snap := Snapshot{
		Name:       snapName,
		Created:    time.Now(),
		Live:       running,
		Definition: inst.Definition,
		Bundle:     inst.Bundle,
		Version:    snapshotSchemaVersion,
		Platform:   inst.Platform,
	}
	cow, err := seedVolumeDir(target, img, running, snapshotRecordPath(target), &snap)
	if err != nil {
		return err
	}

	fmt.Printf("snapshot %q of instance %q created (%s)\n", snapName, id.Display(), copyNote(cow, img))
	if running {
		fmt.Println(crashConsistentNote("taken while running"))
	}
	return nil
}

// Raw bytes and RFC3339 times, so --json stays machine-friendly.
type snapshotRow struct {
	Name      string `json:"name"`
	Created   string `json:"created,omitempty"`
	Live      bool   `json:"live"`
	DiskBytes int64  `json:"diskBytes"`
}

func newSnapshotListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List an instance's snapshots",
		Args:  usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	jsonOut := addJSONFlag(cmd, "snapshots as a JSON array")
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return cmdSnapshotList(*selector, *jsonOut) }
	return cmd
}

func cmdSnapshotList(selector string, jsonOut bool) error {
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}
	rows, err := gatherSnapshots(id.ID)
	if err != nil {
		return err
	}

	return listing[snapshotRow]{
		rows:   rows,
		empty:  fmt.Sprintf("instance %q has no snapshots", id.Display()),
		header: "NAME\tCREATED\tSTATE\tSIZE",
		row: func(w io.Writer, r snapshotRow) {
			created := r.Created
			if created == "" {
				created = "-"
			}
			state := "clean"
			if r.Live {
				state = "crash-consistent"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, created, state, humanBytes(r.DiskBytes))
		},
		footer: "SIZE counts blocks shared with the live image; a snapshot costs disk only as the two diverge.",
	}.render(jsonOut)
}

func gatherSnapshots(id string) ([]snapshotRow, error) {
	_, dir, err := loadInstance(id)
	if err != nil {
		return nil, err
	}
	snaps, err := listSnapshots(dir)
	if err != nil {
		return nil, err
	}
	// Non-nil so --json marshals an empty list to "[]", not "null".
	rows := []snapshotRow{}
	for _, s := range snaps {
		row := snapshotRow{Name: s.Name, Live: s.Live, DiskBytes: diskBytes(snapshotImage(dir, s.Name))}
		if !s.Created.IsZero() {
			row.Created = s.Created.Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func newSnapshotDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "delete SNAPSHOT",
		Short:             "Delete one of an instance's snapshots",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeSnapshotArg,
	}
	selector := addInstanceFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, args []string) error { return cmdSnapshotDelete(*selector, args[0]) }
	return cmd
}

func cmdSnapshotDelete(selector, snapName string) error {
	id, _, dir, err := resolveSnapshotTarget(selector, snapName)
	if err != nil {
		return err
	}
	target := snapshotDir(dir, snapName)
	if err := requireSnapshot(target, id, snapName); err != nil {
		return err
	}
	// A restore must not read this snapshot mid-removal.
	lock, err := lockIdleInstance(id.ID, id.Display())
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := requireSnapshot(target, id, snapName); err != nil {
		return err
	}
	// No confirmation, unlike `sprout delete`: the instance and its live /var
	// are untouched, so the worst case is a redo.
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	fmt.Printf("snapshot %q of instance %q removed\n", snapName, id.Display())
	return nil
}

func newSnapshotRestoreCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "restore SNAPSHOT",
		Short:             "Roll the /var volume back to a snapshot",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeSnapshotArg,
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		return cmdSnapshotRestore(*selector, force, args[0])
	}
	return cmd
}

func cmdSnapshotRestore(selector string, force bool, snapName string) error {
	id, _, dir, err := resolveSnapshotTarget(selector, snapName)
	if err != nil {
		return err
	}
	src := snapshotImage(dir, snapName)
	if err := requireSnapshot(src, id, snapName); err != nil {
		return err
	}

	// No --live escape hatch, unlike `create`: swapping the disk under a
	// running VM corrupts both the image and the guest's page cache.
	lock, err := lockIdleInstance(id.ID, id.Display())
	if err != nil {
		return err
	}
	defer lock.Close()
	// Reloaded under the claim: a concurrent restore or `up` may have
	// rewritten the record since the pre-claim reads.
	inst, _, err := loadInstance(id.ID)
	if err != nil {
		return err
	}
	if err := requireSnapshot(src, id, snapName); err != nil {
		return err
	}
	snap, err := loadSnapshotRecord(dir, snapName)
	if err != nil {
		return unrestorableSnapshotError(id.Display(), snapName, err)
	}
	if err := checkSnapshotPlatform(id.Display(), snapName, inst.Platform, snap); err != nil {
		return err
	}

	if !force && !confirmYes(fmt.Sprintf("replace instance %q's current /var with snapshot %q? the current /var is discarded", id.Display(), snapName)) {
		return errAborted
	}

	if snap.Bundle != "" && snap.Bundle != inst.Bundle {
		fmt.Fprintf(os.Stderr, "warning: snapshot %q was taken against a different build (%s); /etc comes from the current build on the next boot, so guest state written by the old system may not match it\n", snapName, snap.Bundle)
	}

	// Clone beside the live image and rename over it, so a failure partway
	// leaves the current /var intact.
	staging := varImagePath(dir) + ".restoring"
	os.Remove(staging)
	cow, err := cloneFile(src, staging)
	if err != nil {
		return err
	}
	if err := os.Rename(staging, varImagePath(dir)); err != nil {
		os.Remove(staging)
		return err
	}
	fmt.Printf("instance %q restored to snapshot %q (%s)\n", id.Display(), snapName, copyNote(cow, src))
	printBootHint(id.Display())
	return nil
}
