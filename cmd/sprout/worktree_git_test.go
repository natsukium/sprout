package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guest-side bridging script, run against a simulated guest filesystem.
// It ships as a plain shell file so this test can exercise it: a Nix-embedded
// heredoc could only be checked by booting a VM.
const worktreeGitScript = "../../nix/guest/worktree-git.sh"

// Unlike runGit this hands the error back: half of what these tests assert is
// that git fails before the bridging and succeeds after it.
func gitOutput(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return target
}

func containsLine(text, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func runWorktreeGitScript(t *testing.T, workspace, gitCommon string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", worktreeGitScript, workspace, gitCommon)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree-git.sh %s %s: %v\n%s", workspace, gitCommon, err, out)
	}
}

// Stages what the guest sees: a real worktree and its main repository's admin
// dir at the two mount points, with the host tree they name deleted. Before
// bridging `git rev-parse HEAD` fails; after the script runs it resolves.
func TestWorktreeGitScriptMakesALinkedWorktreeUsable(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "host", "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, proj, "init", "-q", "-b", "main", ".")
	runGit(t, proj, "commit", "-q", "--allow-empty", "-m", "initial")
	hostWorktree := filepath.Join(base, "host", "wt", "feat-x")
	runGit(t, proj, "worktree", "add", "-q", "-b", "feat-x", hostWorktree)

	guest := filepath.Join(base, "guest")
	if err := os.MkdirAll(guest, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(guest, "workspace")
	gitCommon := filepath.Join(guest, "gitcommon")
	if err := os.Rename(hostWorktree, workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(proj, ".git"), gitCommon); err != nil {
		t.Fatal(err)
	}
	// Everything the two shares point at is now gone, exactly as it is from
	// inside a VM that mounts only those two directories.
	if err := os.RemoveAll(filepath.Join(base, "host")); err != nil {
		t.Fatal(err)
	}

	if out, err := gitOutput(t, workspace, "rev-parse", "HEAD"); err == nil {
		t.Fatalf("workspace resolved before bridging (%q); the test no longer reproduces the bug", out)
	}

	runWorktreeGitScript(t, workspace, gitCommon)

	if got := readLink(t, filepath.Join(base, "host", "proj", ".git")); got != gitCommon {
		t.Errorf("common dir link = %q, want %q", got, gitCommon)
	}
	if got := readLink(t, filepath.Join(base, "host", "wt", "feat-x")); got != workspace {
		t.Errorf("worktree back-link = %q, want %q", got, workspace)
	}

	head, err := gitOutput(t, workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD in the bridged workspace: %v\n%s", err, head)
	}
	branch, err := gitOutput(t, workspace, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "feat-x" {
		t.Errorf("branch = %q, want feat-x", branch)
	}
	// Unlike rev-parse, merge-base reads the common dir's object store.
	if _, err := gitOutput(t, workspace, "merge-base", "HEAD", "main"); err != nil {
		t.Errorf("git merge-base HEAD main: %v", err)
	}
	// The reverse pointer decides whether git considers this worktree live;
	// a dangling one makes the guest's own checkout look prunable.
	if _, err := gitOutput(t, workspace, "status", "--porcelain"); err != nil {
		t.Errorf("git status: %v", err)
	}
	list, err := gitOutput(t, workspace, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !containsLine(list, "worktree "+filepath.Join(base, "host", "wt", "feat-x")) {
		t.Errorf("worktree list does not carry the bridged path:\n%s", list)
	}

	// Re-running must be a no-op: a repository under /var lands on the volume
	// that survives `stop`, so the unit meets its own symlinks on the next boot.
	runWorktreeGitScript(t, workspace, gitCommon)
	if got := readLink(t, filepath.Join(base, "host", "proj", ".git")); got != gitCommon {
		t.Errorf("common dir link after re-run = %q, want %q", got, gitCommon)
	}
	if _, err := gitOutput(t, workspace, "rev-parse", "HEAD"); err != nil {
		t.Errorf("git broken by a second run: %v", err)
	}
}

// A primary checkout carries its whole repository inside the share, so the
// script must leave the guest alone rather than inventing paths for it.
func TestWorktreeGitScriptLeavesAPrimaryCheckoutAlone(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "init", "-q", "-b", "main", ".")
	runGit(t, workspace, "commit", "-q", "--allow-empty", "-m", "initial")

	runWorktreeGitScript(t, workspace, filepath.Join(workspace, ".git"))

	if _, err := gitOutput(t, workspace, "rev-parse", "HEAD"); err != nil {
		t.Errorf("primary checkout broken by the script: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "workspace" {
		t.Errorf("script created paths outside the workspace: %v", entries)
	}
}

// A submodule's .git file has a linked worktree's shape but names
// <super>/.git/modules/<name>, found through core.worktree rather than the
// reverse pointer the script writes. Bridging it would produce a repository
// pointed at the wrong tree, so the shape is rejected.
func TestWorktreeGitScriptSkipsNonWorktreeGitdirs(t *testing.T) {
	for _, c := range []struct{ name, gitdir string }{
		{"submodule", "gitdir: /elsewhere/super/.git/modules/lib\n"},
		{"relative", "gitdir: ./somewhere\n"},
		{"not a gitdir file", "not a git file at all\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(base, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte(c.gitdir), 0o644); err != nil {
				t.Fatal(err)
			}

			runWorktreeGitScript(t, workspace, filepath.Join(base, "gitcommon"))

			if _, err := os.Lstat("/elsewhere"); err == nil {
				t.Fatal("script materialized a host path for a non-worktree gitdir")
			}
			entries, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "workspace" {
				t.Errorf("script created paths for a non-worktree gitdir: %v", entries)
			}
		})
	}
}

func bridgingManifest() *Manifest {
	m := &Manifest{Workspace: true}
	m.Substitutions = append(m.Substitutions, struct {
		Placeholder string `json:"placeholder"`
		Value       string `json:"value"`
	}{Placeholder: "/sprout/placeholder/gitcommon", Value: "gitCommon"})
	return m
}

// The warning tracks what the guest can actually reconnect, which is narrower
// than "the git data is outside the mount": a submodule checkout satisfies
// that and still ends up without git.
func TestGuestGitRemedy(t *testing.T) {
	// writeWorkspace stages a workspace with the given .git file contents;
	// "" leaves the real directory a primary checkout has.
	writeWorkspace := func(t *testing.T, dotGit string) string {
		t.Helper()
		ws := filepath.Join(t.TempDir(), "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if dotGit == "" {
			if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ws
		}
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte(dotGit), 0o644); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	for _, c := range []struct {
		name       string
		dotGit     string
		repoRoot   func(ws string) string
		manifest   *Manifest
		wantRemedy bool
		wantSays   string
	}{
		{
			name:     "linked worktree on a bundle that shares the common dir",
			dotGit:   "gitdir: /p/.git/worktrees/feat-x\n",
			repoRoot: func(string) string { return "/p/.git" },
			manifest: bridgingManifest(),
		},
		{
			name:       "submodule on a bundle that shares the common dir",
			dotGit:     "gitdir: /p/.git/modules/lib\n",
			repoRoot:   func(string) string { return "/p/.git/modules/lib" },
			manifest:   bridgingManifest(),
			wantRemedy: true,
			wantSays:   "submodule",
		},
		{
			name:     "primary checkout",
			repoRoot: func(ws string) string { return filepath.Join(ws, ".git") },
			manifest: bridgingManifest(),
		},
		{
			name:     "workspace not mounted at all",
			dotGit:   "gitdir: /p/.git/modules/lib\n",
			repoRoot: func(string) string { return "/p/.git/modules/lib" },
			manifest: &Manifest{Workspace: false},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := writeWorkspace(t, c.dotGit)
			got := guestGitRemedy(c.manifest, &Instance{RepoRoot: c.repoRoot(ws), Workspace: ws})
			if c.wantRemedy && got == "" {
				t.Fatal("no warning for a workspace the guest cannot use git in")
			}
			if !c.wantRemedy && got != "" {
				t.Fatalf("warned about a usable workspace: %q", got)
			}
			if c.wantSays != "" && !strings.Contains(got, c.wantSays) {
				t.Errorf("remedy %q does not mention %q", got, c.wantSays)
			}
		})
	}
}
