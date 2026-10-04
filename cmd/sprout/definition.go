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

func hostNixSystem() (string, error) {
	return hostNixSystemFor(runtime.GOOS, runtime.GOARCH)
}

func hostNixSystemFor(goos, goarch string) (string, error) {
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			return "aarch64-darwin", nil
		}
	case "linux":
		switch goarch {
		case "arm64":
			return "aarch64-linux", nil
		case "amd64":
			return "x86_64-linux", nil
		}
	}
	return "", fmt.Errorf("sprout runs on aarch64-darwin, aarch64-linux, and x86_64-linux hosts (this is %s/%s); anything else is out of scope", goos, goarch)
}

func requireBootableHost() error {
	return bootableHostFor(runtime.GOOS)
}

func bootableHostFor(goos string) error {
	if goos == "darwin" {
		return nil
	}
	return fmt.Errorf("sprout cannot boot VMs on %s yet: Linux bundles build, but the QEMU/KVM backend that boots them is not implemented", goos)
}

func listDefinitions(flakeRef string) ([]string, error) {
	host, err := hostNixSystem()
	if err != nil {
		return nil, err
	}
	shape, found, err := evalOutputShape(flakeRef)
	if err != nil || !found {
		// A flake without the output has no VMs, which pickDefinition
		// explains; it is not a broken flake.
		return nil, err
	}
	if err := checkOutputShape(flakeRef, host, shape); err != nil {
		return nil, err
	}
	var names []string
	if _, err := evalFlakeJSON(fmt.Sprintf("%s#sproutConfigurations.%s", flakeRef, host), "builtins.attrNames", flakeRef, &names); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func evalOutputShape(flakeRef string) (map[string]bool, bool, error) {
	var shape map[string]bool
	found, err := evalFlakeJSON(fmt.Sprintf("%s#sproutConfigurations", flakeRef),
		`cfgs: builtins.mapAttrs (_: v: (v.type or null) == "derivation") cfgs`, flakeRef, &shape)
	return shape, found, err
}

func checkOutputShape(flakeRef, host string, shape map[string]bool) error {
	var keys, unqualified []string
	for k, isBundle := range shape {
		keys = append(keys, k)
		if isBundle {
			unqualified = append(unqualified, k)
		}
	}
	sort.Strings(keys)
	sort.Strings(unqualified)
	if len(unqualified) > 0 {
		return fmt.Errorf("%s exposes VMs as sproutConfigurations.<name> (%s), but this sprout reads sproutConfigurations.<system>.<name>; update the flake's sprout input (nix flake update sprout), or nest a lib.mkVMs result under the host: sproutConfigurations.%s = sprout.lib.mkVMs { ... }",
			flakeRef, strings.Join(unqualified, ", "), host)
	}
	if _, ok := shape[host]; !ok {
		return fmt.Errorf("%s defines VMs but not for %s (it targets %s); add %q to systems", flakeRef, host, strings.Join(keys, ", "), host)
	}
	return nil
}

// Nil when the shape is fine or unreadable, so the build's own error stands.
func diagnoseOutputShape(flakeRef, host string) error {
	shape, found, err := evalOutputShape(flakeRef)
	if err != nil || !found {
		return nil
	}
	return checkOutputShape(flakeRef, host, shape)
}

func evalFlakeJSON(attr, apply, flakeRef string, out any) (bool, error) {
	cmd := exec.Command("nix", "eval", "--json", attr, "--apply", apply)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		if missingOutput(stderr.String()) {
			return false, nil
		}
		os.Stderr.WriteString(stderr.String())
		return false, fmt.Errorf("listing VM definitions in %s: %w", flakeRef, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("listing VM definitions in %s: %w", flakeRef, err)
	}
	return true, nil
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
