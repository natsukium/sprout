package main

import (
	"os"
	"strings"
	"testing"
)

// The Git state `sprout init` produces: Nix ignores a new flake until it
// enters the index, while a tracked file and a non-repository directory pass
// through to normal flake evaluation.
func TestLocalFlakeUntracked(t *testing.T) {
	t.Run("new file in Git", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		if err := os.WriteFile(dir+"/flake.nix", []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if !localFlakeUntracked() {
			t.Fatal("untracked flake.nix was not recognized")
		}
		if _, err := resolveDefinition(".", "dev"); err == nil || !strings.Contains(err.Error(), "git add flake.nix") {
			t.Fatalf("explicit --vm did not get the staging remedy: %v", err)
		}
		runGit(t, dir, "add", "flake.nix")
		if localFlakeUntracked() {
			t.Fatal("staged flake.nix still reported as untracked")
		}
	})

	t.Run("outside Git", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/flake.nix", []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if localFlakeUntracked() {
			t.Fatal("a non-Git directory reported an untracked flake")
		}
	})
}

func TestPickDefinitionSelectsTheOnlyDefinition(t *testing.T) {
	got, err := pickDefinition(".", []string{"todo"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "todo" {
		t.Fatalf("picked %q, want %q", got, "todo")
	}
}

// Among several definitions, "dev" is the documented default and must keep
// booting without --vm.
func TestPickDefinitionPrefersDevAmongSeveral(t *testing.T) {
	got, err := pickDefinition(".", []string{"ci", "dev", "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "dev" {
		t.Fatalf("picked %q, want %q", got, "dev")
	}
}

// With several definitions and no "dev" there is nothing to guess from, so the
// error names every candidate and the fix is copy-pasteable.
func TestPickDefinitionListsCandidatesWhenAmbiguous(t *testing.T) {
	_, err := pickDefinition(".", []string{"api", "worker"})
	if err == nil {
		t.Fatal("expected an error for an ambiguous definition set")
	}
	for _, want := range []string{"api", "worker", "--vm"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// A flake with no definitions at all should point at the fix (define
// sprout.vms.<name>), not fail later inside `nix build`.
func TestPickDefinitionExplainsAnEmptyFlake(t *testing.T) {
	_, err := pickDefinition(".", nil)
	if err == nil {
		t.Fatal("expected an error for a flake without VM definitions")
	}
	if !strings.Contains(err.Error(), "sprout.vms") {
		t.Fatalf("error %q does not mention sprout.vms", err)
	}
}

// Only the supported host systems map to a bundle slice.
func TestHostNixSystemFor(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		want         string
		wantOK       bool
	}{
		{"darwin", "arm64", "aarch64-darwin", true},
		{"linux", "arm64", "aarch64-linux", true},
		{"linux", "amd64", "x86_64-linux", true},
		{"darwin", "amd64", "", false},
		{"linux", "riscv64", "", false},
		{"windows", "amd64", "", false},
	} {
		got, err := hostNixSystemFor(c.goos, c.goarch)
		if c.wantOK {
			if err != nil || got != c.want {
				t.Errorf("hostNixSystemFor(%q, %q) = (%q, %v), want (%q, nil)", c.goos, c.goarch, got, err, c.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("hostNixSystemFor(%q, %q) = %q, want it rejected", c.goos, c.goarch, got)
		}
	}
}

// Unqualified definition names must not be read as host systems.
func TestCheckOutputShape(t *testing.T) {
	for _, c := range []struct {
		name    string
		shape   map[string]bool
		wantErr []string
	}{
		{"qualified, host present", map[string]bool{"aarch64-darwin": false, "x86_64-linux": false}, nil},
		{"qualified, host missing", map[string]bool{"aarch64-darwin": false}, []string{"not for x86_64-linux", "aarch64-darwin", `add "x86_64-linux" to systems`}},
		{"unqualified", map[string]bool{"dev": true, "ci": true}, []string{"ci, dev", "nix flake update sprout", "sproutConfigurations.x86_64-linux = sprout.lib.mkVMs"}},
		{"unqualified definition named like a host", map[string]bool{"x86_64-linux": true}, []string{"nix flake update sprout"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkOutputShape(".", "x86_64-linux", c.shape)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejected, got nil")
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// Linux hosts are refused until a Linux backend boots guests.
func TestBootableHostFor(t *testing.T) {
	if err := bootableHostFor("darwin"); err != nil {
		t.Errorf("darwin rejected: %v", err)
	}
	if err := bootableHostFor("linux"); err == nil || !strings.Contains(err.Error(), "QEMU/KVM") {
		t.Errorf("linux = %v, want a not-yet-implemented refusal naming the backend", err)
	}
}

// Both failure modes share nix's "does not provide attribute" wording, but a
// missing sproutConfigurations output means "no VMs defined" while a missing
// attribute deeper in the flake is a real error the user must see.
func TestMissingOutputDistinguishesNoVMsFromEvalErrors(t *testing.T) {
	noOutput := "error: flake 'git+file:///p' does not provide attribute " +
		"'packages.aarch64-darwin.sproutConfigurations', 'legacyPackages.aarch64-darwin.sproutConfigurations' or 'sproutConfigurations'"
	if !missingOutput(noOutput) {
		t.Fatalf("missing sproutConfigurations output not recognized: %q", noOutput)
	}
	for name, stderr := range map[string]string{
		"unrelated missing attribute": "error: flake 'git+file:///p' does not provide attribute 'devShells.aarch64-darwin.foo'",
		"eval error inside a bundle":  "error: attribute 'nixpkgs' missing",
		"flake targeting other hosts": "error: flake 'git+file:///p' does not provide attribute 'sproutConfigurations.x86_64-linux'",
	} {
		if missingOutput(stderr) {
			t.Fatalf("%s misread as a flake with no VMs: %q", name, stderr)
		}
	}
}
