package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// The out-link is the build's GC root, registered inside the same nix
// invocation so there is no build-to-root window. Exactly one output path:
// with several, the root would silently not cover what gets booted.
func nixBuild(outLink, flakeRef, host, def string) (string, error) {
	attr := fmt.Sprintf("%s#sproutConfigurations.%s.%s", flakeRef, host, def)
	cmd := exec.Command("nix", "build", "--out-link", outLink, "--print-out-paths", attr)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build %s: %w", attr, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 1 {
		return "", fmt.Errorf("nix build %s printed %d output paths; a sprout configuration must produce exactly one bundle", attr, len(lines))
	}
	return lines[0], nil
}

func loadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	host, err := runningHostSystem()
	if err != nil {
		return nil, err
	}
	return parseManifest(data, host)
}

// The runner's shell quoting was fixed at Nix eval time, before any value
// existed, so a value that needs quoting cannot be escaped in — only rejected.
// The class is what nixpkgs' escapeShellArg leaves unquoted.
var runnerSafeValue = regexp.MustCompile(`^[[:alnum:],._+:@%/=-]+$`)

// Substituting text rather than re-deriving backend arguments keeps the
// runner a pure microvm.nix artifact shared by every instance. Sidecars are
// substituted the same way: under QEMU a share's source appears only in its
// virtiofsd argv, never in the runner.
//
// One pass, longest placeholder first: sequential replacement would corrupt a
// placeholder that is a prefix of another (".../credential/aws" vs
// ".../credential/aws-extra"), the shorter one hiding the longer.
func rewriteRunner(runnerPath string, m *Manifest, subs map[string]string, out string) ([]SidecarSpec, error) {
	content, err := os.ReadFile(runnerPath)
	if err != nil {
		return nil, err
	}
	// Validated against untouched content, so a prefix collision cannot make a
	// valid placeholder look missing.
	type pair struct{ placeholder, value string }
	pairs := make([]pair, 0, len(m.Substitutions))
	for _, s := range m.Substitutions {
		value, ok := subs[s.Value]
		if !ok {
			return nil, fmt.Errorf("manifest requests unknown substitution symbol %q (sprout too old for this flake?)", s.Value)
		}
		if !runnerSafeValue.MatchString(value) {
			return nil, fmt.Errorf("%s resolves to %q, which the runner script cannot carry; use a path of alphanumerics and ,._+:@%%/=-", s.Value, value)
		}
		if !bytes.Contains(content, []byte(s.Placeholder)) && !sidecarsMention(m.contract.sidecars, s.Placeholder) {
			return nil, fmt.Errorf("placeholder %q appears in neither the runner script nor any sidecar", s.Placeholder)
		}
		pairs = append(pairs, pair{s.Placeholder, value})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return len(pairs[i].placeholder) > len(pairs[j].placeholder)
	})
	oldnew := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		oldnew = append(oldnew, p.placeholder, p.value)
	}
	r := strings.NewReplacer(oldnew...)
	sidecars := make([]SidecarSpec, len(m.contract.sidecars))
	for i, s := range m.contract.sidecars {
		sidecars[i] = s
		sidecars[i].Exec = make([]string, len(s.Exec))
		for j, arg := range s.Exec {
			sidecars[i].Exec[j] = r.Replace(arg)
		}
	}
	if err := os.WriteFile(out, []byte(r.Replace(string(content))), 0o700); err != nil {
		return nil, err
	}
	return sidecars, nil
}

func sidecarsMention(sidecars []SidecarSpec, placeholder string) bool {
	for _, s := range sidecars {
		for _, arg := range s.Exec {
			if strings.Contains(arg, placeholder) {
				return true
			}
		}
	}
	return false
}
