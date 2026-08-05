package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasBuilderEntriesAcceptsLiteralBuilder(t *testing.T) {
	if !hasBuilderEntries("ssh-ng://linux-builder aarch64-linux") {
		t.Fatal("a literal builder entry was not recognized")
	}
}

// nix-darwin installs the machines file unconditionally, so `builders = @file`
// only counts when the file actually names a machine.
func TestHasBuilderEntriesReadsThroughAtFileIndirection(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# no machines\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasBuilderEntries("@" + empty) {
		t.Fatal("a machines file with only comments was counted as a builder")
	}

	populated := filepath.Join(dir, "machines")
	if err := os.WriteFile(populated, []byte("ssh-ng://linux-builder aarch64-linux\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasBuilderEntries("@" + populated) {
		t.Fatal("a populated machines file was not recognized")
	}
}

// A missing machines file means no builder, not an error the user has to
// decode.
func TestHasBuilderEntriesTreatsMissingFileAsNoBuilder(t *testing.T) {
	if hasBuilderEntries("@/nonexistent/machines") {
		t.Fatal("a dangling @file reference was counted as a builder")
	}
}
