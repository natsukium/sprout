package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A real repository, so identity resolution runs against git plumbing rather
// than a mock.
func initTestRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", branch, ".")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=sprout-test", "GIT_AUTHOR_EMAIL=sprout-test@example.com",
		"GIT_COMMITTER_NAME=sprout-test", "GIT_COMMITTER_EMAIL=sprout-test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Without the NUL separator, ("ab","c") and ("a","bc") would hash identically.
func TestHashIDPartsAreSeparated(t *testing.T) {
	if hashID("ab", "c") == hashID("a", "bc") {
		t.Fatal("hashID collides across a part boundary — separator not effective")
	}
	// IDs are displayed and stored at this width; a change silently invalidates
	// every existing instance directory name.
	if got := len(hashID("repo", "main")); got != 12 {
		t.Fatalf("hashID length = %d, want 12", got)
	}
}

func TestIdentityForKeyScopesByRepo(t *testing.T) {
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	idA, err := identityForKey("main", "branch", repoA)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := identityForKey("main", "branch", repoB)
	if err != nil {
		t.Fatal(err)
	}
	if idA.ID == idB.ID {
		t.Fatalf("same branch name in two different repos produced the same ID: %s", idA.ID)
	}

	again, err := identityForKey("main", "branch", repoA)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != idA.ID {
		t.Fatalf("identityForKey not stable across calls: %s != %s", again.ID, idA.ID)
	}
}

func TestIdentityForKeyDifferentKeysDiffer(t *testing.T) {
	repo := initTestRepo(t, "main")
	idMain, err := identityForKey("main", "branch", repo)
	if err != nil {
		t.Fatal(err)
	}
	idFeature, err := identityForKey("feature/foo", "branch", repo)
	if err != nil {
		t.Fatal(err)
	}
	if idMain.ID == idFeature.ID {
		t.Fatal("different branch names in the same repo produced the same ID")
	}
}

func TestResolveIdentityAtDefaultsToBranch(t *testing.T) {
	repo := initTestRepo(t, "feature-x")
	id, err := resolveIdentityAt("", repo)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeySource != "branch" || id.Name != "feature-x" {
		t.Fatalf("got KeySource=%q Name=%q, want branch/feature-x", id.KeySource, id.Name)
	}
	if id.Worktree != repo {
		t.Fatalf("Worktree = %q, want %q", id.Worktree, repo)
	}
}

func TestResolveIdentityAtDetachedHeadFallsBackToDirectory(t *testing.T) {
	repo := initTestRepo(t, "main")
	runGit(t, repo, "checkout", "-q", "--detach", "HEAD")

	id, err := resolveIdentityAt("", repo)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeySource != "directory" {
		t.Fatalf("KeySource = %q, want directory on a detached HEAD", id.KeySource)
	}
}

func TestResolveIdentityAtNonGitDirectory(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	id, err := resolveIdentityAt("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeySource != "directory" {
		t.Fatalf("KeySource = %q, want directory outside git", id.KeySource)
	}
	if id.Worktree != resolved {
		t.Fatalf("Worktree = %q, want %q", id.Worktree, resolved)
	}
}

func TestResolveIdentityAtExplicitNameIsScopedPerRepo(t *testing.T) {
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	idA, err := resolveIdentityAt("scratch", repoA)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := resolveIdentityAt("scratch", repoB)
	if err != nil {
		t.Fatal(err)
	}
	if idA.KeySource != "name" || idB.KeySource != "name" {
		t.Fatalf("KeySource = %q/%q, want name/name", idA.KeySource, idB.KeySource)
	}
	if idA.ID == idB.ID {
		t.Fatal("an explicit -i in two different repos produced the same ID")
	}
}

// `-i <branch>` resolves to an existing instance's recorded branch-keyed
// identity, not a fresh "name" key: a "name" key reads as never-stale, which
// would silently drop the stale-workspace warning.
func TestResolveIdentityAtNameAdoptsExistingBranchInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repo := initTestRepo(t, "feature-x")

	booted, err := identityForKey("feature-x", "branch", repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sprout", "instances", booted.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{ID: booted.ID, Name: "feature-x", KeySource: "branch", RepoRoot: booted.RepoRoot, Workspace: repo}
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}

	runGit(t, repo, "checkout", "-q", "-b", "other")

	got, err := resolveIdentityAt("feature-x", repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != booted.ID {
		t.Fatalf("ID = %q, want %q (should resolve to the existing instance)", got.ID, booted.ID)
	}
	if got.KeySource != "branch" {
		t.Fatalf("KeySource = %q, want branch (adopted from the existing record)", got.KeySource)
	}
	if stale, ok := checkBranchStaleness(got.KeySource, got.Name, got.Worktree); !ok || !stale {
		t.Fatalf("checkBranchStaleness = (%v, %v), want (true, true)", stale, ok)
	}
}

func TestResolveIdentityAtNameWithoutInstanceStaysNameKeyed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repo := initTestRepo(t, "main")

	got, err := resolveIdentityAt("scratch", repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeySource != "name" {
		t.Fatalf("KeySource = %q, want name for an -i with no existing instance", got.KeySource)
	}
}

// The predicate decides whether a boot warns that guest-side git will be
// broken, so it has to read real git plumbing: a primary worktree keeps .git
// inside itself and must stay silent, while a linked worktree points out of the
// mount and must not.
func TestLinkedWorktreeAgainstRealGit(t *testing.T) {
	primary := initTestRepo(t, "main")

	repoRoot, workspace, err := repoContext(primary)
	if err != nil {
		t.Fatal(err)
	}
	if linkedWorktree(repoRoot, workspace) {
		t.Errorf("primary worktree %s reported as linked (common dir %s)", workspace, repoRoot)
	}

	linked := filepath.Join(t.TempDir(), "wt")
	runGit(t, primary, "worktree", "add", "-q", "-b", "feature-x", linked)
	resolvedLinked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, workspace, err = repoContext(resolvedLinked)
	if err != nil {
		t.Fatal(err)
	}
	if !linkedWorktree(repoRoot, workspace) {
		t.Errorf("linked worktree %s not detected (common dir %s)", workspace, repoRoot)
	}

	// Outside git, repoContext returns the directory for both; that is not a
	// worktree and must not warn.
	if linkedWorktree("/tmp/plain", "/tmp/plain") {
		t.Error(`linkedWorktree("/tmp/plain", "/tmp/plain") = true, want false`)
	}
}

// recordInstance writes the minimal instance record the name resolvers read
// back, standing in for an instance a previous `up` booted from repoRoot.
func recordInstance(t *testing.T, stateHome, name, repoRoot string) string {
	t.Helper()
	key, err := identityForKey(name, "name", repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateHome, "sprout", "instances", key.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{ID: key.ID, Name: name, KeySource: "name", RepoRoot: key.RepoRoot, Workspace: repoRoot}
	if err := writeJSON(filepath.Join(dir, "instance.json"), inst); err != nil {
		t.Fatal(err)
	}
	return key.ID
}

// The repository in front of the user wins. Two checkouts of the same project
// both have a "main", and widening the search must never make `-i main`
// answer with the other one.
func TestResolveExistingIdentityAtPrefersTheLocalRepository(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	local := recordInstance(t, root, "main", repoA)
	recordInstance(t, root, "main", repoB)

	got, err := resolveExistingIdentityAt("main", repoA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != local {
		t.Fatalf("ID = %q, want this repository's instance %q", got.ID, local)
	}
}

// An instance `sprout list` shows must be addressable by the name it shows,
// from wherever the user happens to be standing.
func TestResolveExistingIdentityAtFindsAnotherRepositorysInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	remote := recordInstance(t, root, "main", repoB)

	got, err := resolveExistingIdentityAt("main", repoA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != remote {
		t.Fatalf("ID = %q, want the instance recorded in the other repository %q", got.ID, remote)
	}
	// The record's own identity comes along, so callers keep reporting the
	// instance as it was booted rather than as the local repo would key it.
	if got.RepoRoot == "" || got.Name != "main" {
		t.Fatalf("resolved identity = %+v, want the recorded name and repo root", got)
	}
}

// A branch name is reachable in the spelling it takes as a hostname label or ssh
// alias, matching how `route` and `ssh` already address it.
func TestResolveExistingIdentityAtMatchesTheSanitizedName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	want := recordInstance(t, root, "feat/login", repoB)

	got, err := resolveExistingIdentityAt("feat-login", repoA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want {
		t.Fatalf("ID = %q, want %q", got.ID, want)
	}
}

// Widening the search reintroduces the collision repository scoping exists to
// prevent, so the error has to name every candidate and the repository each came
// from — that is the difference the user is being asked to resolve.
func TestResolveExistingIdentityAtReportsAmbiguity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")
	elsewhere := initTestRepo(t, "main")

	recordInstance(t, root, "shared", repoA)
	recordInstance(t, root, "shared", repoB)

	_, err := resolveExistingIdentityAt("shared", elsewhere)
	var amb *instanceNameAmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
	if len(amb.ids) != 2 {
		t.Fatalf("ambiguity listed %v, want both instances", amb.ids)
	}
	for _, repo := range []string{repoA, repoB} {
		if !strings.Contains(err.Error(), repo) {
			t.Errorf("error %q does not name candidate repository %s", err, repo)
		}
	}
}

// A name that matches nothing must say which repository it was looked up in:
// an error that hides its scope leaves the user unable to tell whether the
// name or the place is wrong.
func TestResolveExistingIdentityAtNotFoundNamesTheScope(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repo := initTestRepo(t, "main")

	_, err := resolveExistingIdentityAt("absent", repo)
	if err == nil {
		t.Fatal("resolving a name that exists nowhere succeeded")
	}
	if !strings.Contains(err.Error(), repo) {
		t.Errorf("error %q does not name the repository it searched (%s)", err, repo)
	}
}

// SPROUT_NAME must never redirect a command implicitly, and the only way to
// keep that true is to assert it: an ambient target means a stray shell
// export silently points `sprout delete` at another instance, and exempting
// only the destructive commands would make the targeting rule per-command.
// Scripts pass their variable through visibly instead.
func TestEnvironmentDoesNotSelectAnInstance(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repo := initTestRepo(t, "main")
	other := recordInstance(t, root, "elsewhere", repo)
	t.Setenv("SPROUT_NAME", "elsewhere")

	got, err := resolveExistingIdentityAt("", repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == other {
		t.Fatal("SPROUT_NAME redirected the target; the selector must be the argument only")
	}
	if got.Name != "main" {
		t.Fatalf("Name = %q, want the checked-out branch main", got.Name)
	}
}

// With nothing selected the key came from the current branch or directory, which
// is a statement about here — it must not reach across the host for a same-named
// instance the user never asked about.
func TestResolveExistingIdentityAtBranchDefaultStaysLocal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	repoA := initTestRepo(t, "main")
	repoB := initTestRepo(t, "main")

	other := recordInstance(t, root, "main", repoB)

	got, err := resolveExistingIdentityAt("", repoA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == other {
		t.Fatal("the bare branch default resolved to another repository's instance")
	}
	if got.KeySource != "branch" {
		t.Fatalf("KeySource = %q, want branch", got.KeySource)
	}
}

// TestEnsureSSHKeyConcurrent pins the first-boot keygen race: many parallel
// boots with no shared key yet must produce exactly one key pair, with every
// caller returning the same path and no error. Without the lock, two callers
// pass the Stat and both run ssh-keygen against the same path, the second
// hitting keygen's non-interactive overwrite prompt (EOF -> failure).
func TestEnsureSSHKeyConcurrent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	const n = 8
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			paths[i], errs[i] = ensureSSHKey()
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatalf("goroutine %d returned path %q, want %q", i, paths[i], paths[0])
		}
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("private key missing after concurrent ensureSSHKey: %v", err)
	}
	if _, err := os.Stat(paths[0] + ".pub"); err != nil {
		t.Fatalf("public key missing after concurrent ensureSSHKey: %v", err)
	}
}

func TestMatchIDPrefix(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	instDir := filepath.Join(root, "sprout", "instances")
	for _, id := range []string{"aaaaaaaaaaaa", "aaaabbbbcccc", "ffffffffffff"} {
		if err := os.MkdirAll(filepath.Join(instDir, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("unique prefix resolves", func(t *testing.T) {
		got, err := matchIDPrefix("ffff")
		if err != nil {
			t.Fatal(err)
		}
		if got != "ffffffffffff" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("ambiguous prefix errors", func(t *testing.T) {
		if _, err := matchIDPrefix("aaaa"); err == nil {
			t.Fatal("want error for ambiguous prefix")
		}
	})
	t.Run("short input is not treated as an id", func(t *testing.T) {
		got, err := matchIDPrefix("aaa")
		if err != nil || got != "" {
			t.Fatalf("matchIDPrefix(short) = (%q, %v), want (\"\", nil)", got, err)
		}
	})
	t.Run("non-hex input is not treated as an id", func(t *testing.T) {
		got, err := matchIDPrefix("feature-x")
		if err != nil || got != "" {
			t.Fatalf("matchIDPrefix(non-hex) = (%q, %v), want (\"\", nil)", got, err)
		}
	})
	t.Run("no match returns empty", func(t *testing.T) {
		got, err := matchIDPrefix("deadbeef0000")
		if err != nil || got != "" {
			t.Fatalf("matchIDPrefix(no match) = (%q, %v), want (\"\", nil)", got, err)
		}
	})
}

func TestCheckBranchStaleness(t *testing.T) {
	repo := initTestRepo(t, "main")

	t.Run("fresh when branch matches", func(t *testing.T) {
		stale, ok := checkBranchStaleness("branch", "main", repo)
		if !ok || stale {
			t.Fatalf("checkBranchStaleness = (%v, %v), want (false, true)", stale, ok)
		}
	})

	t.Run("non-branch key source is never stale", func(t *testing.T) {
		stale, ok := checkBranchStaleness("name", "main", repo)
		if !ok || stale {
			t.Fatalf("checkBranchStaleness = (%v, %v), want (false, true)", stale, ok)
		}
	})

	runGit(t, repo, "checkout", "-q", "-b", "other")

	t.Run("stale once a different branch is checked out", func(t *testing.T) {
		stale, ok := checkBranchStaleness("branch", "main", repo)
		if !ok || !stale {
			t.Fatalf("checkBranchStaleness = (%v, %v), want (true, true)", stale, ok)
		}
	})

	t.Run("unreadable worktree reports ok=false, not fresh", func(t *testing.T) {
		stale, ok := checkBranchStaleness("branch", "main", filepath.Join(repo, "does-not-exist"))
		if ok || stale {
			t.Fatalf("checkBranchStaleness = (%v, %v), want (false, false)", stale, ok)
		}
	})
}

func TestIsOrphaned(t *testing.T) {
	repo := initTestRepo(t, "main")

	t.Run("existing branch is not orphaned", func(t *testing.T) {
		inst := &Instance{KeySource: "branch", Name: "main", Workspace: repo}
		if isOrphaned(inst) {
			t.Fatal("existing branch reported orphaned")
		}
	})

	t.Run("deleted branch is orphaned", func(t *testing.T) {
		inst := &Instance{KeySource: "branch", Name: "gone", Workspace: repo}
		if !isOrphaned(inst) {
			t.Fatal("nonexistent branch not reported orphaned")
		}
	})

	t.Run("missing workspace is orphaned regardless of key source", func(t *testing.T) {
		inst := &Instance{KeySource: "name", Name: "scratch", Workspace: filepath.Join(repo, "gone")}
		if !isOrphaned(inst) {
			t.Fatal("missing workspace not reported orphaned")
		}
	})

	t.Run("explicit name is never checked against branch existence", func(t *testing.T) {
		inst := &Instance{KeySource: "name", Name: "totally-not-a-branch", Workspace: repo}
		if isOrphaned(inst) {
			t.Fatal("custom-named instance incorrectly checked against branch existence")
		}
	})
}
