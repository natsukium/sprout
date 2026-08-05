package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Points at -i rather than cobra's generic "accepts 0 arg(s), received 1":
// what the user almost certainly typed is an instance name.
func noPositionals(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q: %s takes no positional arguments — select an instance with `%s -i %s`",
		args[0], cmd.CommandPath(), cmd.CommandPath(), args[0])
}

// A suggestion that dropped an explicit -i would name a different instance
// while looking correct.
func withSelector(command, selector string) string {
	if selector == "" {
		return command
	}
	return command + " -i " + selector
}
