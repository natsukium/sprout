package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A throwaway repository as the working directory, with its own state root.
func statusRepo(t *testing.T, branch string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := initTestRepo(t, branch)
	t.Chdir(repo)
	return repo
}

// The first thing anyone sees on typing `sprout`: an unbooted checkout says so
// and points at the command that creates the environment.
func TestStatusOnAnUnbootedCheckout(t *testing.T) {
	repo := statusRepo(t, "main")
	if err := os.WriteFile(filepath.Join(repo, "flake.nix"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := renderTestStatus(t, "")
	if !strings.Contains(out, "No sprout environment is configured") {
		t.Errorf("status does not report the missing environment:\n%s", out)
	}
	if !strings.Contains(out, "sprout up") {
		t.Errorf("status does not suggest `sprout up`:\n%s", out)
	}
}

// Without a flake there is nothing for `sprout up` to build, so pointing at it
// would send the user into a Nix error.
func TestStatusWithoutAFlakeSuggestsWritingOne(t *testing.T) {
	statusRepo(t, "main")

	out := renderTestStatus(t, "")
	if strings.Contains(out, "sprout up") {
		t.Errorf("status suggests `sprout up` with no flake.nix to build:\n%s", out)
	}
	if !strings.Contains(out, "flake.nix") {
		t.Errorf("status does not mention the missing flake.nix:\n%s", out)
	}
}

// A stopped instance reports what decides its fate: definition and disk held.
func TestStatusOnAStoppedInstance(t *testing.T) {
	repo := statusRepo(t, "main")
	writeStatusRecord(t, repo, "main", "dev")

	out := renderTestStatus(t, "")
	for _, want := range []string{"Environment: main", "State:       stopped", "Definition:  dev", "sprout up"} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
	// Uptime is meaningless while stopped and would read as "up for 0m".
	if strings.Contains(out, "Uptime") {
		t.Errorf("status reports an uptime for a stopped instance:\n%s", out)
	}
}

// The scripting surface: an object, raw units, and every field present whether
// or not the human block showed it.
func TestStatusJSON(t *testing.T) {
	repo := statusRepo(t, "main")
	writeStatusRecord(t, repo, "main", "dev")

	id, err := resolveExistingIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	got, err := gatherStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"instance", "id", "state", "definition", "uptimeSec", "diskBytes", "workspace"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON is missing the %q field: %s", key, data)
		}
	}
	if decoded["state"] != "stopped" {
		t.Errorf("state = %v, want stopped", decoded["state"])
	}
	if decoded["instance"] != "main" {
		t.Errorf("instance = %v, want main", decoded["instance"])
	}
}

// A bare `sprout` gives the same contextual status, so someone who remembers
// no command name still gets somewhere.
func TestBareInvocationShowsStatus(t *testing.T) {
	repo := statusRepo(t, "main")
	if err := os.WriteFile(filepath.Join(repo, "flake.nix"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		_, err := runCLI(t)
		return err
	})
	// The status block, not cobra's help text, which is what a RunE of
	// c.Help() would print instead.
	if !strings.Contains(out, "No sprout environment is configured") {
		t.Errorf("bare `sprout` did not print the status block:\n%s", out)
	}
}

func renderTestStatus(t *testing.T, name string) string {
	t.Helper()
	id, err := resolveExistingIdentity(name)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gatherStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	return renderStatus(s, name)
}

// A suggestion that dropped the selector would name a different instance than
// the one just described, which is worse than none because it looks right.
func TestStatusHintsCarryTheSelector(t *testing.T) {
	repo := statusRepo(t, "main")
	// The branch has to exist, or the instance reads as an orphan, whose one
	// useful action is the host-wide `prune` and takes no selector.
	runGit(t, repo, "branch", "feature-x")
	writeStatusRecord(t, repo, "feature-x", "dev")

	out := renderTestStatus(t, "feature-x")
	if !strings.Contains(out, "sprout up -i feature-x") {
		t.Errorf("hint does not carry the selector into the suggested command:\n%s", out)
	}
}

// An unreadable record is a failure, not an absence: "no environment is
// configured" with exit 0 would hide the state the user ran `status` to find.
func TestStatusReportsAnUnreadableRecord(t *testing.T) {
	repo := statusRepo(t, "main")
	writeStatusRecord(t, repo, "main", "dev")
	id, err := resolveExistingIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := instanceDir(id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := gatherStatus(id); err == nil {
		t.Fatal("a corrupt instance.json was reported as no environment at all")
	}
}

func writeStatusRecord(t *testing.T, repo, branch, definition string) {
	t.Helper()
	id, err := identityForKey(branch, "branch", repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := instanceDir(id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{
		ID: id.ID, Name: branch, KeySource: "branch",
		RepoRoot: id.RepoRoot, Definition: definition, Workspace: repo,
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
}
