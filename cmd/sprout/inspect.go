package main

import (
	"github.com/spf13/cobra"
)

type inspectOut struct {
	*Instance
	State string `json:"state"`
	// Not a property of the instance alone: a name two instances share
	// resolves to the ID instead.
	RouteLabel string  `json:"routeLabel"`
	UptimeSec  int64   `json:"uptimeSec"`
	CPUPct     float64 `json:"cpuPct"`
	MemBytes   int64   `json:"memBytes"`
	DiskBytes  int64   `json:"diskBytes"`
	// A count, not a size: a snapshot's blocks are shared with DiskBytes'
	// image until one side is written, so adding the two would double-count.
	Snapshots int `json:"snapshots"`
}

func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "inspect",
		Short:   "Full JSON record for one instance",
		GroupID: groupIntegration,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return cmdInspect(*selector) }
	return cmd
}

func cmdInspect(selector string) error {
	id, inst, dir, err := resolveAndLoad(selector)
	if err != nil {
		return err
	}

	state, info := instanceState(id.ID, inst, nil)
	label, _ := routeLabelFor(inst.ID, inst.Name)
	out := inspectOut{
		Instance:   inst,
		State:      state,
		RouteLabel: label,
		DiskBytes:  diskBytes(varImagePath(dir)),
		Snapshots:  countSnapshots(dir),
	}
	if info != nil {
		out.UptimeSec = int64(info.UptimeSec)
		out.MemBytes = info.MemBytes
		out.CPUPct = info.CPUPct
	}

	return printJSON(out)
}
