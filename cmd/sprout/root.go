package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

const (
	groupSetup       = "setup"
	groupDaily       = "daily"
	groupNetwork     = "network"
	groupData        = "data"
	groupIntegration = "integration"
)

const rootLong = `sprout — one disposable Linux microVM per git branch, on macOS

Every command acts on the branch checked out in the current worktree, scoped
to its repository. Outside a git repository, or on a detached HEAD, it falls
back to the worktree directory.

Select a different one with --instance / -i, which accepts a name or an
instance ID (or unambiguous prefix, as shown by ` + "`sprout list`" + `) and so reaches any
instance from any directory. Positional arguments are never instance
selectors: they are the command's own operands — a port, a snapshot name, a
guest command.

sprout dial-stdio is internal plumbing — system ssh runs it as its ProxyCommand
to reach a guest through the daemon. You never invoke it directly.`

// main exits 2 on these and 1 on everything else, so a caller can tell "you
// typed it wrong" from "the operation failed". path is the failing command's
// CommandPath, so the help pointer can name `sprout snapshot create --help`
// rather than the root; empty falls back to the root.
type usageError struct {
	err  error
	path string
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func (e *usageError) helpPath() string {
	if e.path != "" {
		return e.path
	}
	return "sprout"
}

func usagef(format string, a ...any) error { return &usageError{err: fmt.Errorf(format, a...)} }

func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return &usageError{err: err, path: cmd.CommandPath()}
		}
		return nil
	}
}

// Needed because cobra's own version of this runs only while Args is unset,
// which the root command cannot leave that way.
func rejectUnknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, s := range suggestions {
			msg += fmt.Sprintf("\t%s\n", s)
		}
	}
	return errors.New(msg)
}

// For a verb that only holds subcommands. Cobra's default prints help and
// exits 0 on an unknown one, which reads as success to a script.
func groupingCmd(cmd *cobra.Command) *cobra.Command {
	cmd.Args = usageArgs(rejectUnknownCommand)
	cmd.SuggestionsMinimumDistance = 2
	cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	return cmd
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sprout",
		Short: "One disposable Linux microVM per git branch, on macOS",
		Long:  rootLong,
		// main prints "sprout: <err>" and picks the exit code, so cobra must
		// not print the error or dump usage itself.
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(rejectUnknownCommand),
		Version:       versionString(),
		// Cobra sets this only inside the unknown-command path an explicit
		// Args validator replaces; at 0, SuggestionsFor never suggests.
		SuggestionsMinimumDistance: 2,
		// Someone who typed nothing is more often asking what state they are
		// in than for the command reference, which `sprout help` still prints.
		RunE: func(_ *cobra.Command, _ []string) error { return cmdStatus("", false) },
	}
	// Cobra's default template would prepend "sprout version ".
	root.SetVersionTemplate("{{.Version}}\n")
	// Registered by hand so it gets no -v shorthand: short options are for
	// arguments repeated interactively, which a version query is not.
	root.Flags().Bool("version", false, "print the sprout version")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err: err, path: cmd.CommandPath()}
	})

	root.AddGroup(
		&cobra.Group{ID: groupSetup, Title: "Setup:"},
		&cobra.Group{ID: groupDaily, Title: "Daily workflow:"},
		&cobra.Group{ID: groupNetwork, Title: "Network:"},
		&cobra.Group{ID: groupData, Title: "Persistent data:"},
		&cobra.Group{ID: groupIntegration, Title: "Integration & maintenance:"},
	)
	root.AddCommand(
		newInitCmd(),
		newDoctorCmd(),
		newUpCmd(),
		newStatusCmd(),
		newShellCmd(),
		newExecCmd(),
		newRunCmd(),
		newListCmd(),
		newStopCmd(),
		newDeleteCmd(),
		newLogsCmd(),
		newForwardCmd(),
		newRouteCmd(),
		newOpenCmd(),
		newSnapshotCmd(),
		newForkCmd(),
		newCacheCmd(),
		newSSHCmd(),
		newStartCmd(),
		newInspectCmd(),
		newPruneCmd(),
		newVersionCmd(),
		newDialStdioCmd(),
	)
	return root
}

// A fresh root per call keeps flag state from leaking between tests.
func execute(args []string) error {
	root := newRootCmd()
	root.SetArgs(args)
	return root.Execute()
}
