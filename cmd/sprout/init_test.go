package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestInitWritesABootableFlake(t *testing.T) {
	t.Chdir(t.TempDir())

	out := captureStdout(t, func() error { return cmdInit("dev") })
	if !strings.Contains(out, "git add flake.nix") {
		t.Errorf("init does not explain how to make the new flake visible to Nix:\n%s", out)
	}
	got, err := os.ReadFile("flake.nix")
	if err != nil {
		t.Fatalf("init reported success but wrote no flake.nix: %v", err)
	}
	for _, want := range []string{
		`inputs.sprout.url = "github:natsukium/sprout"`,
		"inputs.sprout.flakeModules.default",
		"sprout.vms.dev = {",
		"workspace = true;",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("flake.nix is missing %q:\n%s", want, got)
		}
	}
}

// --vm names the definition, so a project whose environment is not called
// "dev" does not have to edit the file it was just given.
func TestInitHonorsTheDefinitionName(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := cmdInit("ci"); err != nil {
		t.Fatalf("init: %v", err)
	}
	got, err := os.ReadFile("flake.nix")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "sprout.vms.ci = {") {
		t.Errorf("flake.nix does not declare sprout.vms.ci:\n%s", got)
	}
}

// A name needing Nix quoting would produce a flake.nix that does not parse,
// and the failure would surface from `nix build` long after the typo.
func TestInitRejectsANameNixCannotSpell(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, name := range []string{"", "9lives", "my vm", "a.b"} {
		t.Run(name, func(t *testing.T) {
			if err := cmdInit(name); err == nil {
				t.Fatalf("init accepted %q as a definition name", name)
			}
			if _, err := os.Stat("flake.nix"); !os.IsNotExist(err) {
				t.Errorf("a rejected name still wrote flake.nix (stat err = %v)", err)
			}
		})
	}
}

// flake.nix pins every input a project builds from, so init must never clobber
// one, and must say where the sprout block goes instead.
func TestInitRefusesToOverwriteAFlake(t *testing.T) {
	t.Chdir(t.TempDir())

	const existing = "{ outputs = _: { }; }\n"
	if err := os.WriteFile("flake.nix", []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cmdInit("dev")
	if err == nil {
		t.Fatal("init overwrote an existing flake.nix")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error: %v", err)
	}
	got, err := os.ReadFile("flake.nix")
	if err != nil || string(got) != existing {
		t.Fatalf("flake.nix = %q (err %v), want it untouched", got, err)
	}
}

// init may well run before `git init`: identity falls back to the directory,
// and init itself needs no repository.
func TestInitWorksOutsideAGitRepository(t *testing.T) {
	// The test binary's own checkout is a repository, so the chdir to a
	// non-repository /tmp is what makes this meaningful.
	t.Chdir(t.TempDir())

	if err := cmdInit("dev"); err != nil {
		t.Fatalf("init outside a git repository: %v", err)
	}
	if _, err := os.Stat("flake.nix"); err != nil {
		t.Fatalf("no flake.nix: %v", err)
	}
}

// The template and the tutorial's copy-pasteable flake must stay the same
// text: once they drift, a reader cannot tell which one is current.
func TestInitTemplateMatchesTheTutorial(t *testing.T) {
	doc, err := os.ReadFile("../../docs/tutorials/getting-started.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "# flake.nix\n")
	if !ok {
		t.Fatal("the tutorial no longer contains a `# flake.nix` block to compare against")
	}
	snippet, _, ok := strings.Cut(after, "```")
	if !ok {
		t.Fatal("the tutorial's flake.nix block is unterminated")
	}
	want := fmt.Sprintf(initFlakeTemplate, "dev")
	if snippet != want {
		t.Errorf("`sprout init` writes a different flake than the tutorial tells people to paste.\n--- init ---\n%s\n--- tutorial ---\n%s", want, snippet)
	}
}
