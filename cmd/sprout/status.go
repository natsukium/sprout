package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type statusOut struct {
	Instance   string `json:"instance"`
	ID         string `json:"id"`
	State      string `json:"state"`
	Definition string `json:"definition"`
	UptimeSec  int64  `json:"uptimeSec"`
	DiskBytes  int64  `json:"diskBytes"`
	Workspace  string `json:"workspace"`
}

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Describe one environment and the next useful action",
		GroupID: groupDaily,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	jsonOut := addJSONFlag(cmd, "the environment as a JSON object")
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return cmdStatus(*selector, *jsonOut) }
	return cmd
}

func cmdStatus(selector string, jsonOut bool) error {
	// With no selector this is the identity `up` would create, record or no
	// record, which is exactly what `status` describes.
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}
	out, err := gatherStatus(id)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(out)
	}
	fmt.Print(renderStatus(out, selector))
	// "Nothing here" is what a branch switch produces while the previous
	// branch's instance still runs against this worktree, so this is the state
	// that has to name the instances -i can still reach.
	if out.State == stateAbsent {
		if hint := siblingHint(id); hint != "" {
			fmt.Printf("\n%s\n", hint)
		}
	}
	return nil
}

func gatherStatus(id *Identity) (*statusOut, error) {
	out := &statusOut{Instance: id.Display(), ID: id.ID, State: stateAbsent, DiskBytes: -1}
	inst, dir, loadErr := loadInstance(id.ID)
	if loadErr != nil {
		// Only a missing record means "no environment here". An unreadable one
		// is a failure to report: exiting 0 would hide exactly the problem
		// status was run to find.
		if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, loadErr
		}
		return out, nil
	}
	out.Definition, out.Workspace = inst.Definition, inst.Workspace
	state, info := instanceState(id.ID, inst, nil)
	out.State = state
	if info != nil {
		out.UptimeSec = int64(info.UptimeSec)
	}
	out.DiskBytes = diskBytes(varImagePath(dir))
	return out, nil
}

func renderStatus(s *statusOut, selector string) string {
	var b strings.Builder
	if s.State == stateAbsent {
		fmt.Fprintf(&b, "No sprout environment is configured for %q in this repository.\n", s.Instance)
	} else {
		w := tabwriter.NewWriter(&b, 0, 0, 1, ' ', 0)
		fmt.Fprintf(w, "Environment:\t%s\n", s.Instance)
		fmt.Fprintf(w, "State:\t%s\n", s.State)
		if s.Definition != "" {
			fmt.Fprintf(w, "Definition:\t%s\n", s.Definition)
		}
		if isLive(s.State) {
			fmt.Fprintf(w, "Uptime:\t%s\n", humanDuration(time.Duration(s.UptimeSec)*time.Second))
		} else if s.DiskBytes >= 0 {
			fmt.Fprintf(w, "Disk:\t%s\n", humanBytes(s.DiskBytes))
		}
		_ = w.Flush()
	}
	if hint := nextAction(s.State, selector); hint != "" {
		fmt.Fprintf(&b, "\n%s\n", hint)
	}
	return b.String()
}

func isLive(state string) bool {
	switch state {
	case stateRunning, stateBooting, stateStale:
		return true
	}
	return false
}

func nextAction(state, selector string) string {
	// prune is the exception below: host-wide, so it takes no selector.
	on := func(verb string) string { return withSelector(verb, selector) }
	switch state {
	case stateRunning, stateStale:
		return "Enter it with:\n  " + on("sprout shell")
	case stateBooting:
		return "It is still booting. Watch it with:\n  " + on("sprout logs") + " -f"
	case stateOrphan:
		return "Its worktree or branch is gone. Remove it with:\n  sprout prune"
	case stateAbsent:
		if !flakeNixPresent() {
			return initHint()
		}
		return "Create it with:\n  " + on("sprout up")
	default: // stopped
		return "Start it with:\n  " + on("sprout up")
	}
}

// A stat, never an evaluation: `status` must stay instant, and `sprout up` is
// where a broken flake should surface.
func flakeNixPresent() bool {
	_, err := os.Stat("flake.nix")
	return err == nil
}
