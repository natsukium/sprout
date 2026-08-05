package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

func resolveDefinition(flakeRef, def string) (string, error) {
	// Checked even with an explicit --vm: selecting an attribute does not make
	// its source visible to Nix.
	if flakeRef == "." && !flakeNixPresent() {
		return "", fmt.Errorf("%s", initHint())
	}
	if flakeRef == "." && localFlakeUntracked() {
		return "", fmt.Errorf("flake.nix is untracked, so Nix cannot read it; stage it with:\n  git add flake.nix")
	}
	if def != "" {
		return def, nil
	}
	names, err := listDefinitions(flakeRef)
	if err != nil {
		return "", err
	}
	return pickDefinition(flakeRef, names)
}

// Does not stage the file: changing a developer's index is a version-control
// decision, and naming the command is enough to keep `init` then `up` from
// ending in a raw Nix error.
func localFlakeUntracked() bool {
	inside := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	inside.Stdout = nil
	inside.Stderr = nil
	if err := inside.Run(); err != nil {
		return false
	}
	tracked := exec.Command("git", "ls-files", "--error-unmatch", "--", "flake.nix")
	tracked.Stdout = nil
	tracked.Stderr = nil
	return tracked.Run() != nil
}

func listDefinitions(flakeRef string) ([]string, error) {
	attr := fmt.Sprintf("%s#sproutConfigurations", flakeRef)
	cmd := exec.Command("nix", "eval", "--json", attr, "--apply", "builtins.attrNames")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// A flake without the output has no VMs, which pickDefinition
		// explains; it is not a broken flake.
		if missingOutput(stderr.String()) {
			return nil, nil
		}
		os.Stderr.WriteString(stderr.String())
		return nil, fmt.Errorf("listing VM definitions in %s: %w", flakeRef, err)
	}
	var names []string
	if err := json.Unmarshal(out, &names); err != nil {
		return nil, fmt.Errorf("listing VM definitions in %s: %w", flakeRef, err)
	}
	sort.Strings(names)
	return names, nil
}

// Matched on the attribute name too, so a missing-attribute error from deeper
// in the flake still surfaces instead of reading as "no VMs".
func missingOutput(stderr string) bool {
	return strings.Contains(stderr, "does not provide attribute") &&
		strings.Contains(stderr, "'sproutConfigurations'")
}

func pickDefinition(flakeRef string, defs []string) (string, error) {
	switch {
	case len(defs) == 0:
		return "", fmt.Errorf("%s defines no VMs; add one under sprout.vms.<name> (see docs/tutorials/getting-started.md)", flakeRef)
	case len(defs) == 1:
		return defs[0], nil
	}
	for _, d := range defs {
		if d == "dev" {
			return d, nil
		}
	}
	return "", fmt.Errorf("%s defines several VMs (%s); pick one with --vm", flakeRef, strings.Join(defs, ", "))
}

func nixArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return runtime.GOARCH
}
