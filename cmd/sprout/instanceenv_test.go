package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A guest shell sources the file and sees the instance's identity, including a
// name with shell metacharacters: names come from git branches, and sprout
// allows anything git does.
func TestWriteInstanceEnvRoundTripsThroughAShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.env")
	if err := writeInstanceEnv(path, &Instance{
		ID:         "abc123",
		Name:       `feat/$(rm -rf)'quote"`,
		Definition: "dev",
	}, "feat-rm-rf-quote"); err != nil {
		t.Fatalf("writeInstanceEnv: %v", err)
	}

	out, err := exec.Command("/bin/sh", "-c", ". "+path+` && printf '%s' "$SPROUT_INSTANCE_NAME"`).Output()
	if err != nil {
		t.Fatalf("sourcing instance.env: %v", err)
	}
	if got := string(out); got != `feat/$(rm -rf)'quote"` {
		t.Errorf("name did not survive the shell round-trip: %q", got)
	}
}

// The variable names are the contract guests build on, so a rename is a
// breaking change this test makes deliberate.
func TestWriteInstanceEnvCarriesAllIdentityFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.env")
	if err := writeInstanceEnv(path, &Instance{ID: "abc123", Name: "main", Definition: "dev"}, "main"); err != nil {
		t.Fatalf("writeInstanceEnv: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"SPROUT_INSTANCE_ID='abc123'",
		"SPROUT_INSTANCE_NAME='main'",
		"SPROUT_INSTANCE_LABEL='main'",
		"SPROUT_DEFINITION='dev'",
	} {
		if !strings.Contains(string(data), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, data)
		}
	}
}
