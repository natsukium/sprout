package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newForkCmd() *cobra.Command {
	var live bool
	cmd := &cobra.Command{
		Use:   "fork [NEWNAME]",
		Short: "New environment seeded with another's /var",
		Long: `Create a new environment seeded with an existing one's /var volume and build.

-i selects the source, as it does everywhere else. NEWNAME is the destination;
omitted, it is this worktree's branch, the same name ` + "`sprout up`" + ` would create.

  sprout fork experiment           this branch  ->  experiment
  sprout fork -i main              main         ->  this branch
  sprout fork -i main experiment   main         ->  experiment

The source must be stopped unless --live opts into a crash-consistent copy.

The destination must not already exist, and there is no --force: the state it
would overwrite is the persistent volume someone forked in order to keep.

Only the volume and the recorded build come across. Credentials, shared and
project caches, and ssh material are re-projected on the first boot like any
other environment's. Instance-scoped caches live on /var, so the fork starts
warm with a copy of them.`,
		GroupID:           groupData,
		Args:              usageArgs(cobra.MaximumNArgs(1)),
		ValidArgsFunction: completeNothing,
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().Lookup("instance").Usage = "environment to fork from (default: this worktree's branch)"
	cmd.Flags().BoolVar(&live, "live", false, "fork from a running instance (crash-consistent; requires a copy-on-write filesystem)")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		newName := ""
		if len(args) == 1 {
			newName = args[0]
		}
		return cmdFork(*selector, live, newName)
	}
	return cmd
}

func cmdFork(selector string, live bool, newName string) error {
	// Both defaults resolve to this worktree's branch, so a bare `sprout fork`
	// can only be a mistake.
	if selector == "" && newName == "" {
		return usagef("fork needs a source or a destination that is not this environment: select the source with -i, or name the new one — sprout fork NEWNAME")
	}
	src, _, srcDir, err := resolveAndLoad(selector)
	if err != nil {
		return err
	}

	// Not resolveExistingIdentity: the destination does not exist yet, and
	// repository scoping is what keeps `sprout fork -i x main` from adopting
	// another project's "main".
	dst, err := resolveIdentity(newName)
	if err != nil {
		return err
	}
	if dst.ID == src.ID {
		return usagef("instance %q would fork onto itself; select a different source with -i, or name the new environment: sprout fork NEWNAME", src.Display())
	}

	srcLC, dstLC, err := acquireLifecyclePair(src.ID, dst.ID)
	if err != nil {
		return err
	}
	defer srcLC.Close()
	defer dstLC.Close()

	dstDir, err := instanceDir(dst.ID)
	if err != nil {
		return err
	}
	// The atomic Mkdir claims the pathname: of two forks to one name, the
	// loser must not RemoveAll the winner's state.
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dstDir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("instance %q already exists here; fork under another name, or `sprout delete -i %s` first", dst.Display(), dst.Display())
		}
		return err
	}
	// Held across seeding and rollback: an unlocked half-seeded destination
	// would be claimable by a concurrent start or delete mid-copy.
	dstLock, err := claimInstanceLocked(dstDir, 0, nil)
	if err != nil {
		os.RemoveAll(dstDir)
		return err
	}
	defer dstLock.Close()

	srcLock, running, err := claimOrLive(src.ID, srcDir, fmt.Sprintf("source instance %q", src.Display()), "fork", live)
	if err != nil {
		os.RemoveAll(dstDir)
		return err
	}
	if srcLock != nil {
		defer srcLock.Close()
		// The held boot lock keeps delete out transitively; a full copy must
		// not block every lifecycle waiter. The live path has no boot lock,
		// so there the lifecycle lock stays held through the copy instead.
		srcLC.Close()
	}

	// The pre-lock record could predate a restore or a rebuild.
	srcInst, _, err := loadInstance(src.ID)
	if err != nil {
		os.RemoveAll(dstDir)
		return err
	}
	srcImg, err := liveImage(srcDir, src.Display())
	if err != nil {
		os.RemoveAll(dstDir)
		return err
	}
	// Pinned before seeding: a rollback must never publish a fork whose build
	// the next GC collects.
	bundle, err := canonicalBundlePath(srcInst.Bundle, src.Display())
	if err != nil {
		os.RemoveAll(dstDir)
		return err
	}
	if storeRoot, ok := storeRootOf(bundle); ok {
		// nix may create the link before failing, which the rollback removes.
		if err := pinBundleRoot(bundleLinkPath(dstDir), storeRoot); err != nil {
			os.RemoveAll(dstDir)
			return err
		}
	}

	inst := dst.newInstance()
	// From the source: the fork is that system with that /var. A later
	// `sprout up` rebuilds against this worktree's flake.
	inst.Definition, inst.Bundle = srcInst.Definition, bundle
	inst.GuestIP, inst.SSHUser = srcInst.GuestIP, srcInst.SSHUser
	cow, err := seedVolumeDir(dstDir, srcImg, running, instanceRecordPath(dstDir), inst)
	if err != nil {
		return err
	}

	fmt.Printf("instance %q forked from %q (%s)\n", dst.Display(), src.Display(), copyNote(cow))
	if running {
		fmt.Println(crashConsistentNote("forked while the source ran"))
	}
	printBootHint(dst.Display())
	return nil
}
