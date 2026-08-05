package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func resolveAndLoad(selector string) (*Identity, *Instance, string, error) {
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return nil, nil, "", err
	}
	inst, dir, err := loadInstance(id.ID)
	if err != nil {
		var nf *instanceNotFoundError
		if errors.As(err, &nf) {
			nf.selector = id.Display()
		}
		return nil, nil, "", err
	}
	return id, inst, dir, nil
}

// The dot is excluded on purpose: the router splits a hostname on dots
// (<gport>.<label>.<domain>), so a branch "2024.q3" would resolve as guest
// port 2024 of an instance "q3".
var nameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// sanitizeName has no role in identity, which is keyed by ID. It exists
// because ssh addresses an instance by a pseudo-hostname (sprout-<label>),
// where a branch like "feature/foo" would land a literal "/".
func sanitizeName(s string) (string, error) {
	s = strings.Trim(nameSanitizer.ReplaceAllString(s, "-"), "-")
	if s == "" {
		return "", fmt.Errorf("cannot derive a usable instance name")
	}
	return s, nil
}

// ID is a stable hash, also used for docker-style addressing by prefix; Name
// is the raw, unsanitized label shown to the user.
type Identity struct {
	ID        string
	Name      string
	KeySource string
	// RepoRoot is the git-common-dir realpath, shared by every worktree of the
	// same clone, so it scopes the ID rather than Worktree does.
	RepoRoot string
	// Worktree is the realpath of the directory mounted at /workspace.
	Worktree string
}

func (id *Identity) Display() string {
	if id.Name != "" {
		return id.Name
	}
	return id.ID
}

func (id *Identity) newInstance() *Instance {
	return &Instance{
		ID:        id.ID,
		Name:      id.Name,
		KeySource: id.KeySource,
		RepoRoot:  id.RepoRoot,
		Workspace: id.Worktree,
	}
}

// The selector is only ever the argument, never an ambient env var: that would
// let a shell export silently redirect a destructive command.
func resolveIdentity(flagName string) (*Identity, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return resolveIdentityAt(flagName, cwd)
}

func resolveIdentityAt(flagName, cwd string) (*Identity, error) {
	if flagName != "" {
		id, err := matchIDPrefix(flagName)
		if err != nil {
			return nil, err
		}
		if id != "" {
			return loadIdentityByID(id)
		}
		key, err := identityForKey(flagName, "name", cwd)
		if err != nil {
			return nil, err
		}
		// Adopt the existing record's identity (KeySource above all), so
		// addressing an instance by name gets the same staleness check as
		// addressing it by ID.
		if instanceRecordExists(key.ID) {
			return loadIdentityByID(key.ID)
		}
		return key, nil
	}
	if branch, ok := gitCurrentBranch(cwd); ok {
		return identityForKey(branch, "branch", cwd)
	}
	return directoryIdentity(cwd)
}

// Unlike `up`/`run`, which create and stay scoped to this repository, the
// search widens host-wide: a name `sprout list` shows but `-i` cannot reach
// would read as a bug.
func resolveExistingIdentity(flagName string) (*Identity, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return resolveExistingIdentityAt(flagName, cwd)
}

func resolveExistingIdentityAt(flagName, cwd string) (*Identity, error) {
	id, err := resolveIdentityAt(flagName, cwd)
	if err != nil {
		return nil, err
	}
	// This repository wins: two checkouts both have a "main", and the one in
	// front of the user is the one they mean.
	if instanceRecordExists(id.ID) {
		return id, nil
	}
	// An unselected key came from the current branch or directory, a statement
	// about *here*; widening it host-wide would answer a question nobody asked.
	if flagName == "" {
		return id, nil
	}
	matches, err := instancesNamed(flagName)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no instance named %q in %s, or anywhere else on this host (see `sprout list`)", flagName, id.RepoRoot)
	case 1:
		return loadIdentityByID(matches[0])
	default:
		return nil, &instanceNameAmbiguousError{name: flagName, ids: matches}
	}
}

type instanceNameAmbiguousError struct {
	name string
	ids  []string
}

func (e *instanceNameAmbiguousError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "several instances are named %q; address one by its ID:", e.name)
	for _, id := range e.ids {
		inst, _, err := loadInstance(id)
		if err != nil {
			fmt.Fprintf(&b, "\n  %s", id)
			continue
		}
		fmt.Fprintf(&b, "\n  %s  (%s)", id, inst.RepoRoot)
	}
	return b.String()
}

func instancesNamed(name string) ([]string, error) {
	ids, err := instanceIDs()
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(name)
	var matches []string
	for _, id := range ids {
		inst, _, err := loadInstance(id)
		if err != nil {
			continue
		}
		if inst.Name == name {
			matches = append(matches, id)
			continue
		}
		if s, err := sanitizeName(inst.Name); err == nil && strings.ToLower(s) == want {
			matches = append(matches, id)
		}
	}
	return matches, nil
}

func identityForKey(key, source, cwd string) (*Identity, error) {
	repoRoot, worktree, err := repoContext(cwd)
	if err != nil {
		return nil, err
	}
	return &Identity{
		ID:        hashID(repoRoot, key),
		Name:      key,
		KeySource: source,
		RepoRoot:  repoRoot,
		Worktree:  worktree,
	}, nil
}

func directoryIdentity(cwd string) (*Identity, error) {
	repoRoot, worktree, err := repoContext(cwd)
	if err != nil {
		return nil, err
	}
	return &Identity{
		ID:        hashID(worktree),
		Name:      filepath.Base(worktree),
		KeySource: "directory",
		RepoRoot:  repoRoot,
		Worktree:  worktree,
	}, nil
}

// A missing or corrupt instance.json still yields a usable, if sparse,
// Identity: path resolution only needs the ID.
func loadIdentityByID(id string) (*Identity, error) {
	inst, _, err := loadInstance(id)
	if err != nil {
		return &Identity{ID: id}, nil
	}
	return &Identity{
		ID:        id,
		Name:      inst.Name,
		KeySource: inst.KeySource,
		RepoRoot:  inst.RepoRoot,
		Worktree:  inst.Workspace,
	}, nil
}

func instanceRecordExists(id string) bool {
	dir, err := instanceDir(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(instanceRecordPath(dir))
	return err == nil
}

func instanceIDs() ([]string, error) {
	root, err := stateRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "instances"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Membership is proven by the recorded RepoRoot (the same git-common-dir
// resolveIdentity hashes into the ID), not by a workspace path prefix, which
// would misfile nested checkouts and moved worktrees.
func projectInstanceIDs() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	repoRoot, _, err := repoContext(cwd)
	if err != nil {
		return nil, err
	}
	ids, err := instanceIDs()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, id := range ids {
		inst, _, err := loadInstance(id)
		if err != nil {
			// An unreadable record cannot prove it belongs to this project, and
			// the callers are bulk stop/delete, where guessing an instance into
			// scope is the destructive direction.
			continue
		}
		if inst.RepoRoot == repoRoot {
			matches = append(matches, id)
		}
	}
	return matches, nil
}

// Returns ("", nil) when prefix is not a plausible ID, so the caller falls
// through to treating it as a custom key.
func matchIDPrefix(prefix string) (string, error) {
	// A pasted ID can come back uppercased. Folding keeps it an ID lookup
	// rather than a name key, which would mint a fresh instance.
	prefix = strings.ToLower(prefix)
	if len(prefix) < 4 || !isHex(prefix) {
		return "", nil
	}
	ids, err := instanceIDs()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", &idPrefixAmbiguousError{prefix: prefix, ids: matches}
	}
}

type idPrefixAmbiguousError struct {
	prefix string
	ids    []string
}

func (e *idPrefixAmbiguousError) Error() string {
	return fmt.Sprintf("instance id %q is ambiguous, matches: %s", e.prefix, strings.Join(e.ids, ", "))
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Parts are joined by a NUL byte, which cannot appear in a path or branch
// name, so no input can be crafted to collide across a part boundary.
func hashID(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func repoContext(cwd string) (repoRoot, worktree string, err error) {
	common, commonErr := gitCommonDir(cwd)
	if commonErr != nil {
		wd := cwd
		if rp, evalErr := filepath.EvalSymlinks(wd); evalErr == nil {
			wd = rp
		}
		return wd, wd, nil
	}
	top, topErr := gitToplevel(cwd)
	if topErr != nil {
		// A bare repository has a common-dir but no toplevel; falling back to
		// it beats failing `up` outright.
		return common, common, nil
	}
	return common, top, nil
}

// True when /workspace keeps its git data outside itself (a linked worktree
// or a submodule checkout): `.git` then points at a host path outside the
// share, which git inside the guest follows to nothing.
func linkedWorktree(repoRoot, workspace string) bool {
	// Equal paths mean repoContext found no repository and fell back to the
	// directory.
	if repoRoot == workspace {
		return false
	}
	return repoRoot != filepath.Join(workspace, ".git")
}

// The same test as nix/guest/worktree-git.sh, so the host warning cannot
// drift from guest behavior: only a linked worktree's admin dir,
// <common>/worktrees/<name>, is reachable through the shared common dir. A
// submodule checkout looks the same but lives at <super>/.git/modules/<name>.
func bridgeableWorktree(workspace string) bool {
	data, err := os.ReadFile(filepath.Join(workspace, ".git"))
	if err != nil {
		return false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir: ")
	// A relative gitdir already resolves inside the share.
	if !ok || !filepath.IsAbs(target) {
		return false
	}
	return filepath.Base(filepath.Dir(target)) == "worktrees"
}

// A warning rather than a refusal: only guest-side git tooling breaks, the VM
// is still useful.
func guestGitRemedy(m *Manifest, inst *Instance) string {
	if !m.Workspace || !linkedWorktree(inst.RepoRoot, inst.Workspace) {
		return ""
	}
	if !bridgeableWorktree(inst.Workspace) {
		return "a submodule checkout cannot be shared this way; use a full clone per branch"
	}
	return ""
}

func gitQuery(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("git %s: empty output", strings.Join(args, " "))
	}
	return s, nil
}

func gitCommonDir(dir string) (string, error) {
	common, err := gitQuery(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.EvalSymlinks(common)
}

func gitToplevel(dir string) (string, error) {
	top, err := gitQuery(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(top)
}

// ok=false outside a repository or on a detached HEAD, neither of which has a
// branch for an instance to bind to.
func gitCurrentBranch(dir string) (branch string, ok bool) {
	b, err := gitQuery(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || b == "HEAD" {
		return "", false
	}
	return b, true
}

// An unreadable worktree reports ok=false: that is an orphan question, not a
// staleness one.
func checkBranchStaleness(keySource, name, worktree string) (stale, ok bool) {
	if keySource != "branch" {
		return false, true
	}
	cur, err := gitQuery(worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, false
	}
	return cur != name, true
}

// Siblings exist because identity binds to the branch: switching branches in
// place leaves the previous branch's instance running against this same
// directory, where no worktree-scoped command addresses it.
func runningSiblings(id *Identity) []*Instance {
	ids, err := instanceIDs()
	if err != nil {
		return nil
	}
	var siblings []*Instance
	for _, other := range ids {
		if other == id.ID {
			continue
		}
		inst, _, err := loadInstance(other)
		if err != nil || inst.Workspace != id.Worktree {
			continue
		}
		if !instanceRunning(other) {
			continue
		}
		siblings = append(siblings, inst)
	}
	return siblings
}

func siblingHint(id *Identity) string {
	siblings := runningSiblings(id)
	if len(siblings) == 0 {
		return ""
	}
	names := make([]string, 0, len(siblings))
	for _, inst := range siblings {
		names = append(names, fmt.Sprintf("%q", inst.Name))
	}
	subject := fmt.Sprintf("instance %s is", names[0])
	if len(names) > 1 {
		subject = fmt.Sprintf("instances %s are", strings.Join(names, ", "))
	}
	return fmt.Sprintf("%s already running for this worktree; reach it with %s, or stop it with %s",
		subject,
		withSelector("sprout shell", siblings[0].Name),
		withSelector("sprout stop", siblings[0].Name))
}

// Branch renames create a separate identity because Git has no rename history.
func isOrphaned(inst *Instance) bool {
	if _, err := os.Stat(inst.Workspace); err != nil {
		return true
	}
	if inst.KeySource != "branch" {
		return false
	}
	cmd := exec.Command("git", "-C", inst.Workspace, "show-ref", "--verify", "--quiet", "refs/heads/"+inst.Name)
	return cmd.Run() != nil
}

func displayForID(id string) string {
	if inst, _, err := loadInstance(id); err == nil && inst.Name != "" {
		return inst.Name
	}
	return id
}
