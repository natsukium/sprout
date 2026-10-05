package main

import (
	"os"
	"strings"
)

// Set by the Nix package to bin directories of tools sprout shells out to;
// empty in a plain `go build`. First goes ahead of the user's PATH, Last after
// it (see runtimeTools in nix/lib.nix for which tool goes where and why).
var runtimePathFirst, runtimePathLast string

func withRuntimePath(first, current, last string) string {
	var parts []string
	for _, p := range []string{first, current, last} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func init() {
	if runtimePathFirst != "" || runtimePathLast != "" {
		_ = os.Setenv("PATH", withRuntimePath(runtimePathFirst, os.Getenv("PATH"), runtimePathLast))
	}
}
