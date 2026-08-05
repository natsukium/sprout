package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// The single home of the release number: the Nix build parses this constant
// for its package version, so a plain `go build` and a Nix build report the
// same line.
const releaseVersion = "0.1.0"

// Injected at build time via -ldflags.
var version string

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the sprout version",
		GroupID: groupIntegration,
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println(versionString())
			return nil
		},
	}
}

func versionString() string {
	if version != "" {
		return version
	}
	return releaseVersion + "-" + vcsStamp()
}

func vcsStamp() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "-dirty"
	}
	return rev
}
