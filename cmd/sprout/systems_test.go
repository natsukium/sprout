package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"testing"
)

type nixHostEntry struct {
	Backends []string `json:"backends"`
}

func nixHostTable(t *testing.T) map[string]nixHostEntry {
	t.Helper()
	nixInstantiate, err := exec.LookPath("nix-instantiate")
	if err != nil {
		t.Skip("nix-instantiate is not available")
	}
	out, err := exec.Command(nixInstantiate, "--eval", "--strict", "--json",
		"--expr", "(import ../../nix/systems.nix).table").Output()
	if err != nil {
		t.Fatalf("evaluate nix/systems.nix: %v", err)
	}
	var table map[string]nixHostEntry
	if err := json.Unmarshal(out, &table); err != nil {
		t.Fatal(err)
	}
	return table
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The flake exposes bundles for exactly the hosts the CLI recognises, and on
// each host for exactly the backend kinds the CLI selects there.
func TestNixHostTableMatchesBackendRegistry(t *testing.T) {
	table := nixHostTable(t)
	nixHosts := sortedKeys(table)

	recognised := map[string]bool{}
	for _, goos := range []string{"darwin", "linux", "freebsd", "windows"} {
		for _, goarch := range []string{"amd64", "arm64", "386", "riscv64", "ppc64le", "s390x", "loong64"} {
			if host, err := hostNixSystemFor(goos, goarch); err == nil {
				recognised[host] = true
			}
		}
	}
	if got := sortedKeys(recognised); !slices.Equal(got, nixHosts) {
		t.Errorf("hostNixSystemFor recognises %v, nix/systems.nix supports %v", got, nixHosts)
	}

	registered := map[string]bool{}
	for _, k := range backendKinds {
		for _, h := range k.hosts {
			registered[h] = true
		}
	}
	if got := sortedKeys(registered); !slices.Equal(got, nixHosts) {
		t.Errorf("backendKinds cover hosts %v, nix/systems.nix supports %v", got, nixHosts)
	}

	for _, host := range nixHosts {
		var goKinds []string
		for _, k := range backendKindsFor(host) {
			goKinds = append(goKinds, k.name)
		}
		sort.Strings(goKinds)
		nixKinds := slices.Sorted(slices.Values(table[host].Backends))
		if !slices.Equal(goKinds, nixKinds) {
			t.Errorf("%s: backendKinds allow %v, nix/systems.nix allows %v", host, goKinds, nixKinds)
		}
	}
}

func flakeSystems(t *testing.T, what, text string) []string {
	t.Helper()
	block := regexp.MustCompile(`(?s)systems = \[(.*?)\]`).FindStringSubmatch(text)
	if block == nil {
		t.Fatalf("%s has no systems list", what)
	}
	var systems []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(block[1], -1) {
		systems = append(systems, m[1])
	}
	sort.Strings(systems)
	return systems
}

// sprout's own flake and the one `sprout init` writes both build for every
// supported host.
func TestFlakeSystemsCoverNixHosts(t *testing.T) {
	nixHosts := sortedKeys(nixHostTable(t))
	flake, err := os.ReadFile("../../flake.nix")
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"flake.nix": string(flake), "init template": initFlakeTemplate} {
		if systems := flakeSystems(t, what, text); !slices.Equal(systems, nixHosts) {
			t.Errorf("%s lists systems %v, nix/systems.nix supports %v", what, systems, nixHosts)
		}
	}
}
