package main

import (
	"sort"

	"github.com/spf13/cobra"
)

func addInstanceFlag(cmd *cobra.Command) *string {
	inst := cmd.Flags().StringP("instance", "i", "", "instance to act on (default: this worktree's branch)")
	_ = cmd.RegisterFlagCompletionFunc("instance", completeInstanceFlag)
	return inst
}

func completeInstanceFlag(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return instanceCandidates(), cobra.ShellCompDirectiveNoFileComp
}

func completeSnapshotArg(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	inst, _ := cmd.Flags().GetString("instance")
	return snapshotCandidates(inst), cobra.ShellCompDirectiveNoFileComp
}

func completeCacheArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cacheCandidates(), cobra.ShellCompDirectiveNoFileComp
}

// ID and display name, the two forms the selector accepts.
func instanceCandidates() []string {
	ids, err := instanceIDs()
	if err != nil {
		return nil
	}
	var out []string
	for _, id := range ids {
		out = append(out, id)
		if inst, _, err := loadInstance(id); err == nil && inst.Name != "" {
			out = append(out, inst.Name)
		}
	}
	return out
}

func snapshotCandidates(name string) []string {
	id, err := resolveExistingIdentity(name)
	if err != nil {
		return nil
	}
	dir, err := instanceDir(id.ID)
	if err != nil {
		return nil
	}
	snaps, err := listSnapshots(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range snaps {
		out = append(out, s.Name)
	}
	return out
}

// Deduplicated across the per-arch and per-project trees: `cache delete`
// deletes across all of them, so one entry per name is what to offer.
func cacheCandidates() []string {
	found, err := walkHostCaches()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range found {
		if seen[c.name] {
			continue
		}
		seen[c.name] = true
		out = append(out, c.name)
	}
	sort.Strings(out)
	return out
}

// Without it cobra falls back to file completion, offering the working
// directory's contents as if one of them were the answer.
func completeNothing(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}
