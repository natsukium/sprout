package main

import (
	"os"
	"path/filepath"
)

// Set by the Nix package to the bin directories of the tools sprout shells out
// to; empty in a plain `go build`.
var runtimePath string

// Prepended, not appended: the tool found first must be the packaged one, not a
// /usr/bin stub ahead of it in the user's PATH. Children, nix included,
// inherit the result.
func prependRuntimePath(extra, current string) string {
	switch {
	case extra == "":
		return current
	case current == "":
		return extra
	}
	return extra + string(filepath.ListSeparator) + current
}

func init() {
	if runtimePath != "" {
		_ = os.Setenv("PATH", prependRuntimePath(runtimePath, os.Getenv("PATH")))
	}
}
