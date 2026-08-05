package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRootHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	got, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/cache/sprout" {
		t.Fatalf("cacheRoot = %q, want /custom/cache/sprout", got)
	}
}

func TestCachePathFor(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/xdg")
	repo := "/src/myproj/.git"
	projTree := "/xdg/sprout/aarch64-linux/.projects/myproj-" + hashID(repo)

	t.Run("shared is keyed by arch under the cache root", func(t *testing.T) {
		got, err := cachePathFor("aarch64-linux", CacheSpec{Name: "sccache", Scope: "shared"}, repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != "/xdg/sprout/aarch64-linux/sccache" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("project is keyed by repo root inside the arch tree", func(t *testing.T) {
		got, err := cachePathFor("aarch64-linux", CacheSpec{Name: "cargo", Scope: "project"}, repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != projTree+"/cargo" {
			t.Fatalf("got %q, want %q", got, projTree+"/cargo")
		}
	})

	t.Run("project label comes from the repo, not the .git basename", func(t *testing.T) {
		got, err := cachePathFor("aarch64-linux", CacheSpec{Name: "cargo", Scope: "project"}, "/src/other")
		if err != nil {
			t.Fatal(err)
		}
		want := "/xdg/sprout/aarch64-linux/.projects/other-" + hashID("/src/other") + "/cargo"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("clones sharing a basename get separate trees", func(t *testing.T) {
		a, err := cachePathFor("aarch64-linux", CacheSpec{Name: "c", Scope: "project"}, "/a/myproj/.git")
		if err != nil {
			t.Fatal(err)
		}
		b, err := cachePathFor("aarch64-linux", CacheSpec{Name: "c", Scope: "project"}, "/b/myproj/.git")
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Fatalf("both clones resolved to %q", a)
		}
	})

	t.Run("project without a repo root errors", func(t *testing.T) {
		if _, err := cachePathFor("aarch64-linux", CacheSpec{Name: "cargo", Scope: "project"}, ""); err == nil {
			t.Fatal("want error when repoRoot is empty")
		}
	})

	t.Run("project without arch errors", func(t *testing.T) {
		if _, err := cachePathFor("", CacheSpec{Name: "cargo", Scope: "project"}, repo); err == nil {
			t.Fatal("want error when guestArch is empty")
		}
	})

	// The nix side accepts "instance" as a scope but backs it with the
	// guest's /var; a manifest naming it is malformed and must fail loudly.
	t.Run("instance scope never reaches the host", func(t *testing.T) {
		if _, err := cachePathFor("aarch64-linux", CacheSpec{Name: "target", Scope: "instance"}, repo); err == nil {
			t.Fatal("want error for instance scope in a manifest")
		}
	})

	t.Run("shared without arch errors", func(t *testing.T) {
		// A shared cache path must never fall back to an unkeyed directory that
		// could mix arch artifacts.
		if _, err := cachePathFor("", CacheSpec{Name: "sccache", Scope: "shared"}, repo); err == nil {
			t.Fatal("want error when guestArch is empty")
		}
	})

	t.Run("unknown scope errors", func(t *testing.T) {
		if _, err := cachePathFor("aarch64-linux", CacheSpec{Name: "x", Scope: "wat"}, repo); err == nil {
			t.Fatal("want error for unknown scope")
		}
	})

	// The nix side always stamps a scope (the option defaults to "project"), so
	// an empty scope means a manifest sprout did not produce.
	t.Run("empty scope errors", func(t *testing.T) {
		if _, err := cachePathFor("aarch64-linux", CacheSpec{Name: "cargo"}, repo); err == nil {
			t.Fatal("want error for empty scope")
		}
	})
}

// The segment holding project trees must stay unreachable as a cache name, or
// a cache called ".projects" would shadow every project tree in its arch.
func TestProjectCacheDirIsNotAValidCacheName(t *testing.T) {
	if err := validateCacheName(projectCacheDir); err == nil {
		t.Fatalf("%q is accepted as a cache name", projectCacheDir)
	}
}

func TestAddCacheSubstitutionsCreatesDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)

	repo := "/src/myproj/.git"
	m := &Manifest{
		GuestArch: "aarch64-linux",
		Caches: []CacheSpec{
			{Name: "sccache", Scope: "shared"},
			{Name: "cargo", Scope: "project"},
		},
	}
	subs := map[string]string{}
	if err := addCacheSubstitutions(m, subs, repo); err != nil {
		t.Fatal(err)
	}

	shared := filepath.Join(root, "sprout", "aarch64-linux", "sccache")
	if subs["cache:sccache"] != shared {
		t.Fatalf("cache:sccache = %q, want %q", subs["cache:sccache"], shared)
	}
	if fi, err := os.Stat(shared); err != nil || !fi.IsDir() {
		t.Fatalf("shared cache dir not created: %v", err)
	}
	project := filepath.Join(root, "sprout", "aarch64-linux", projectCacheDir, "myproj-"+hashID(repo), "cargo")
	if subs["cache:cargo"] != project {
		t.Fatalf("cache:cargo = %q, want %q", subs["cache:cargo"], project)
	}
	if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
		t.Fatalf("project cache dir not created: %v", err)
	}
}

// Removal spans every arch tree and every project holding the name, and
// reports a miss.
func TestCacheDelete(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	// Same cache name under two arches, mimicking a Rosetta-plus-native host.
	var trees []string
	for _, arch := range []string{"aarch64-linux", "x86_64-linux"} {
		trees = append(trees,
			filepath.Join(root, "sprout", arch, "sccache"),
			filepath.Join(root, "sprout", arch, projectCacheDir, "one-aaaa", "sccache"),
			filepath.Join(root, "sprout", arch, projectCacheDir, "two-bbbb", "sccache"),
		)
	}
	for _, dir := range trees {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := cmdCacheDelete("sccache"); err != nil {
		t.Fatalf("cache delete: %v", err)
	}
	for _, dir := range trees {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("cache still present at %s", dir)
		}
	}

	if err := cmdCacheDelete("sccache"); err == nil {
		t.Fatal("removing a missing cache should error")
	}
}

// A name resolving outside the per-arch cache directories is refused before
// anything is deleted: ".." joins to the cache root, where RemoveAll would
// erase every cache on the host.
func TestCacheDeleteRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	if err := os.MkdirAll(filepath.Join(root, "sprout", "aarch64-linux", "sccache"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..", "../..", "a/b", "/abs", ".hidden", ""} {
		if err := cmdCacheDelete(name); err == nil {
			t.Fatalf("cache delete %q should be rejected", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sprout", "aarch64-linux", "sccache")); err != nil {
		t.Fatalf("existing cache was deleted by a rejected name: %v", err)
	}
}

// Manifest-supplied cache names get the same validation as CLI input, since
// both end up as path components.
func TestCachePathForRejectsTraversal(t *testing.T) {
	for _, name := range []string{"..", "a/b", ""} {
		for _, scope := range []string{"shared", "project"} {
			if _, err := cachePathFor("aarch64-linux", CacheSpec{Name: name, Scope: scope}, "/src/p/.git"); err == nil {
				t.Fatalf("cachePathFor %q (scope %s) should be rejected", name, scope)
			}
		}
	}
}

// The same split every query command uses: the table formats, the JSON does
// not.
func TestCacheListJSONShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)

	dir := filepath.Join(root, "sprout", "aarch64-linux", "sccache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(root, "sprout", "aarch64-linux", projectCacheDir, "myproj-aaaa", "cargo")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rows, err := gatherCaches()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]cacheRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if len(rows) != 2 {
		t.Fatalf("gatherCaches returned %d rows, want 2: %+v", len(rows), rows)
	}

	r := byName["sccache"]
	if r.Arch != "aarch64-linux" {
		t.Errorf("row = %+v, want the sccache/aarch64-linux tree", r)
	}
	// A shared cache belongs to no project, so the field is omitted rather
	// than carrying a placeholder a consumer would have to special-case.
	if r.Project != "" {
		t.Errorf("shared row has project %q, want empty", r.Project)
	}
	if r.SizeBytes != int64(len("cached")) {
		t.Errorf("sizeBytes = %d, want %d (raw bytes, not a formatted string)", r.SizeBytes, len("cached"))
	}
	if _, err := time.Parse(time.RFC3339, r.LastUsed); err != nil {
		t.Errorf("lastUsed = %q, not RFC3339: %v", r.LastUsed, err)
	}
	if r.Path != dir {
		t.Errorf("path = %q, want %q", r.Path, dir)
	}

	p := byName["cargo"]
	if p.Arch != "aarch64-linux" || p.Project != "myproj-aaaa" {
		t.Errorf("project row = %+v, want arch aarch64-linux and project myproj-aaaa", p)
	}
	// The reason the field exists: a project cache sits under a key a caller
	// cannot spell without re-deriving it.
	if p.Path != projDir {
		t.Errorf("project path = %q, want %q", p.Path, projDir)
	}
}

// A host with no cache tree still produces "[]" rather than null, so a script
// can parse the output without checking for the tree first.
func TestCacheListJSONIsAnArrayWhenEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	rows, err := gatherCaches()
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "[]" {
		t.Errorf("empty cache list marshals to %s, want []", out)
	}
}
