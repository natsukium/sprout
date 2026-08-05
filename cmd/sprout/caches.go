package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// Applied to manifest names too: a flake typo should fail loudly rather than
// create a directory outside the cache root.
func validateCacheName(name string) error {
	return validateNameComponent("cache", name)
}

// Not a valid cache name — validateCacheName requires a leading letter or
// digit — so a shared cache can never collide with it and no separate
// reservation is needed.
const projectCacheDir = ".projects"

func projectCacheKey(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("project-scoped cache: instance record has no repoRoot")
	}
	base := filepath.Base(repoRoot)
	// A non-worktree clone's git-common-dir is <repo>/.git, whose basename
	// labels every repository alike.
	if base == ".git" {
		base = filepath.Base(filepath.Dir(repoRoot))
	}
	label, err := sanitizeName(base)
	if err != nil {
		return "", fmt.Errorf("project-scoped cache: %w", err)
	}
	return label + "-" + hashID(repoRoot), nil
}

// Host-side caches are keyed by guest arch, so an x86_64 guest cannot poison
// the aarch64 tree.
func cachePathFor(guestArch string, c CacheSpec, repoRoot string) (string, error) {
	if err := validateCacheName(c.Name); err != nil {
		return "", err
	}
	archRoot := func() (string, error) {
		if guestArch == "" {
			return "", fmt.Errorf("cache %q: manifest is missing guestArch", c.Name)
		}
		root, err := cacheRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, guestArch), nil
	}
	switch c.Scope {
	case "project":
		root, err := archRoot()
		if err != nil {
			return "", err
		}
		key, err := projectCacheKey(repoRoot)
		if err != nil {
			return "", fmt.Errorf("cache %q: %w", c.Name, err)
		}
		return filepath.Join(root, projectCacheDir, key, c.Name), nil
	case "shared":
		root, err := archRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, c.Name), nil
	default:
		return "", fmt.Errorf("cache %q: unknown scope %q", c.Name, c.Scope)
	}
}

func addCacheSubstitutions(m *Manifest, subs map[string]string, repoRoot string) error {
	for _, c := range m.Caches {
		path, err := cachePathFor(m.GuestArch, c, repoRoot)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		subs["cache:"+c.Name] = path
	}
	return nil
}

func newCacheCmd() *cobra.Command {
	cmd := groupingCmd(&cobra.Command{
		Use:     "cache",
		Short:   "List or delete host-side build caches",
		GroupID: groupData,
	})
	cmd.AddCommand(newCacheListCmd(), newCacheDeleteCmd())
	return cmd
}

func newCacheListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List host-side build caches",
		Args:  usageArgs(cobra.NoArgs),
	}
	jsonOut := addJSONFlag(cmd, "caches as a JSON array")
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return cmdCacheList(*jsonOut) }
	return cmd
}

type cacheRow struct {
	Name      string `json:"name"`
	Arch      string `json:"arch"`
	Project   string `json:"project,omitempty"`
	SizeBytes int64  `json:"sizeBytes"`
	LastUsed  string `json:"lastUsed"`
	Path      string `json:"path"`
}

type hostCache struct {
	name    string
	arch    string
	project string
	path    string
}

// Instance caches never reach the host tree.
func walkHostCaches() ([]hostCache, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}
	arches, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []hostCache
	for _, arch := range arches {
		if !arch.IsDir() {
			continue
		}
		archDir := filepath.Join(root, arch.Name())
		entries, err := os.ReadDir(archDir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if e.Name() != projectCacheDir {
				found = append(found, hostCache{
					name: e.Name(),
					arch: arch.Name(),
					path: filepath.Join(archDir, e.Name()),
				})
				continue
			}
			projects, err := os.ReadDir(filepath.Join(archDir, projectCacheDir))
			if err != nil {
				return nil, err
			}
			for _, p := range projects {
				if !p.IsDir() {
					continue
				}
				projDir := filepath.Join(archDir, projectCacheDir, p.Name())
				names, err := os.ReadDir(projDir)
				if err != nil {
					return nil, err
				}
				for _, n := range names {
					if !n.IsDir() {
						continue
					}
					found = append(found, hostCache{
						name:    n.Name(),
						arch:    arch.Name(),
						project: p.Name(),
						path:    filepath.Join(projDir, n.Name()),
					})
				}
			}
		}
	}
	return found, nil
}

func cmdCacheList(jsonOut bool) error {
	rows, err := gatherCaches()
	if err != nil {
		return err
	}
	return listing[cacheRow]{
		rows:   rows,
		empty:  "no caches",
		header: "NAME\tARCH\tPROJECT\tSIZE\tLAST USED",
		row: func(w io.Writer, r cacheRow) {
			project := r.Project
			if project == "" {
				project = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Arch, project, humanBytes(r.SizeBytes), r.LastUsed)
		},
	}.render(jsonOut)
}

func gatherCaches() ([]cacheRow, error) {
	found, err := walkHostCaches()
	if err != nil {
		return nil, err
	}
	// Non-nil so --json marshals an empty list to "[]", not "null".
	rows := []cacheRow{}
	for _, c := range found {
		size, mtime := dirStats(c.path)
		rows = append(rows, cacheRow{
			Name:      c.name,
			Arch:      c.arch,
			Project:   c.project,
			SizeBytes: size,
			LastUsed:  mtime.Format(time.RFC3339),
			Path:      c.path,
		})
	}
	return rows, nil
}

func newCacheDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "delete NAME",
		Short:             "Delete a host-side build cache",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeCacheArg,
		RunE:              func(_ *cobra.Command, args []string) error { return cmdCacheDelete(args[0]) },
	}
}

// Instances hold no lock on caches, so removal only makes the next build for
// that cache cold.
func cmdCacheDelete(name string) error {
	if err := validateCacheName(name); err != nil {
		return err
	}
	found, err := walkHostCaches()
	if err != nil {
		return err
	}
	removed := false
	for _, c := range found {
		if c.name != name {
			continue
		}
		if err := os.RemoveAll(c.path); err != nil {
			return err
		}
		if c.project == "" {
			fmt.Printf("removed cache %q (%s)\n", name, c.arch)
		} else {
			fmt.Printf("removed cache %q (%s, project %s)\n", name, c.arch, c.project)
		}
		removed = true
	}
	if !removed {
		return fmt.Errorf("cache %q not found", name)
	}
	return nil
}

func dirStats(path string) (int64, time.Time) {
	var total int64
	var newest time.Time
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			total += info.Size()
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return total, newest
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
