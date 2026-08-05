package main

import (
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type instanceRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	UptimeSec int64   `json:"uptimeSec"`
	CPUPct    float64 `json:"cpuPct"`
	MemBytes  int64   `json:"memBytes"`
	DiskBytes int64   `json:"diskBytes"`
	Workspace string  `json:"workspace"`
}

func newListCmd() *cobra.Command {
	var quiet, project bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List every environment on this host",
		GroupID: groupDaily,
		Args: usageArgs(func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("list takes no positional arguments, got %v (did you mean `sprout inspect`?)", args)
			}
			return nil
		}),
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only instance IDs, for scripting")
	cmd.Flags().BoolVar(&project, "project", false, "list only the current repository's instances")
	jsonOut := addJSONFlag(cmd, "instances as a JSON array")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if quiet && *jsonOut {
			return usagef("choose one of -q or --json, not both")
		}
		// Scoped before gathering, not filtered after: instanceRows pays a
		// live state query per instance, which foreign instances should not
		// cost, and the scope rule stays in projectInstanceIDs alongside the
		// stop/delete callers.
		ids, err := scopeIDs(project)
		if err != nil {
			return err
		}
		return cmdList(quiet, *jsonOut, ids)
	}
	return cmd
}

func cmdList(quiet, jsonOut bool, ids []string) error {
	rows := instanceRows(ids)

	if quiet {
		// No header, so `sprout list -q | xargs -n1 sprout stop` composes and
		// an empty list expands to nothing rather than a stray word.
		for _, r := range rows {
			fmt.Println(r.ID)
		}
		return nil
	}

	return listing[instanceRow]{
		rows:   rows,
		empty:  "no instances",
		header: "ID\tNAME\tSTATE\tUPTIME\tCPU\tMEM\tDISK\tWORKSPACE",
		row: func(w io.Writer, r instanceRow) {
			uptime, cpu, mem, disk := "-", "-", "-", "-"
			if isLive(r.State) {
				uptime = humanDuration(time.Duration(r.UptimeSec) * time.Second)
			}
			if r.CPUPct > 0 {
				cpu = fmt.Sprintf("%.0f%%", r.CPUPct)
			}
			if r.MemBytes > 0 {
				mem = humanBytes(r.MemBytes)
			}
			if r.DiskBytes >= 0 {
				disk = humanBytes(r.DiskBytes)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.State, uptime, cpu, mem, disk, r.Workspace)
		},
	}.render(jsonOut)
}

func instanceRows(ids []string) []instanceRow {
	// Non-nil so --json marshals an empty list to "[]", not "null".
	rows := []instanceRow{}
	for _, id := range ids {
		inst, dir, loadErr := loadInstance(id)
		r := instanceRow{ID: id, Name: id, State: stateStopped, Workspace: "?"}
		if loadErr == nil {
			r.Name, r.Workspace = inst.Name, inst.Workspace
		}

		state, info := instanceState(id, inst, loadErr)
		r.State = state
		if info != nil {
			r.UptimeSec = int64(info.UptimeSec)
			r.MemBytes = info.MemBytes
			r.CPUPct = info.CPUPct
		}
		r.DiskBytes = diskBytes(varImagePath(dir))
		rows = append(rows, r)
	}
	return rows
}

const (
	stateRunning = "running"
	stateBooting = "booting"
	stateStale   = "stale"
	stateOrphan  = "orphan"
	stateStopped = "stopped"
	// The one state `list` never reports, since it only walks instances that
	// exist. `status` describes the environment this worktree *would* address,
	// which on a fresh checkout has no state directory at all.
	stateAbsent = "absent"
)

// inst may be nil when loadErr is set.
func instanceState(id string, inst *Instance, loadErr error) (string, *controlInfo) {
	if info, err := queryInfo(id); err == nil {
		if !info.Ready {
			return stateBooting, info
		}
		if loadErr == nil {
			if stale, ok := checkBranchStaleness(inst.KeySource, inst.Name, inst.Workspace); ok && stale {
				return stateStale, info
			}
		}
		return stateRunning, info
	} else if instanceRunning(id) {
		return stateRunning, nil
	} else if loadErr == nil && isOrphaned(inst) {
		return stateOrphan, nil
	}
	return stateStopped, nil
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d == 0 {
		return "<1m"
	}
	return strings.TrimSuffix(d.String(), "0s")
}

// Allocated, not apparent, size: the volume is sparse, so apparent size would
// always read as the full diskSize. -1 when it cannot be stat'd.
func diskBytes(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return -1
	}
	return st.Blocks * 512
}
