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
	} {
		if missingOutput(stderr) {
			t.Fatalf("%s misread as a flake with no VMs: %q", name, stderr)
		}
	}
}
