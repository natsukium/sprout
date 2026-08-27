package main

// Bundle GC-root maintenance. Promotion is a fresh pin at <instanceDir>/bundle,
// never a symlink rename: gcroots/auto registers the link *path*, so renaming
// would silently drop the registration.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// nix registers the out-link as a GC root in the same invocation, so there is
// no build-to-root window. A package-level seam so tests can pin fictitious
// store paths without nix.
var pinBundleRoot = func(link, storePath string) error {
	// ENOENT only: an EACCES or EIO misreported as "collected" would send the
	// user rebuilding instead of looking at the disk.
	if _, err := os.Stat(storePath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("build %s is no longer in the store; run `sprout up` to rebuild it", storePath)
		}
		return fmt.Errorf("checking %s: %w", storePath, err)
	}
	out, err := exec.Command("nix", "build", "--out-link", link, storePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("registering GC root %s for %s: %w\n%s", link, storePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func bundleLinkPath(instDir string) string { return filepath.Join(instDir, "bundle") }

// A record may name a directory nested inside a store path; the root must pin
// the whole store path.
func storeRootOf(canonical string) (string, bool) {
	const store = "/nix/store/"
	if !strings.HasPrefix(canonical, store) {
		return "", false
	}
	name, _, _ := strings.Cut(canonical[len(store):], "/")
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	return store + name, true
}

// A relative recorded path is unrecoverable: the invocation cwd was never
// recorded, and resolving against the current one would name an unrelated
// directory.
func canonicalBundlePath(recorded, display string) (string, error) {
	if !filepath.IsAbs(recorded) {
		return "", fmt.Errorf("the recorded build path for %q (%s) is relative and its original directory is unknown; run `sprout up` to rebuild", display, recorded)
	}
	canonical, err := filepath.EvalSymlinks(recorded)
	if err != nil {
		// A dangling link and a collected store path land here alike; the
		// non-store branch must never adopt either.
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("build for %q is no longer in the store (%s); run `sprout up` to rebuild it", display, recorded)
		}
		return "", err
	}
	return canonical, nil
}

// Runs under the boot lock with the lifecycle lock still held (migration
// publishes a token, which requires it). Every boot pins unconditionally: a
// resolving symlink is no proof of registration. Any failure returns before
// the token sweep — retained staging may be the record's only protection.
func reconcileRoots(dir string, inst *Instance) error {
	canonical, err := canonicalBundlePath(inst.Bundle, inst.Name)
	if err != nil {
		return err
	}
	if storeRoot, ok := storeRootOf(canonical); ok {
		err = pinStoreBundle(dir, inst, canonical, storeRoot)
	} else {
		err = adoptNonStoreBundle(dir, inst, canonical)
	}
	if err != nil {
		return err
	}
	sweepDeadTokens(dir)
	return nil
}

func pinStoreBundle(dir string, inst *Instance, canonical, storeRoot string) error {
	link := bundleLinkPath(dir)
	if inst.Bundle == canonical {
		return pinBundleRoot(link, storeRoot)
	}
	// A legacy record's symlink chain may pass through <dir>/bundle itself, so
	// pinning the canonical link first would repoint the very link the record's
	// meaning depends on. Hence: temporary root at a path no legacy chain can
	// name, record rewrite, then the canonical pin — a crash between any two
	// steps leaves the record protected and resolving correctly.
	tok, err := publishToken(dir)
	if err != nil {
		return err
	}
	defer tok.Close()
	if err := pinBundleRoot(stagingBundleLink(tok.dir), storeRoot); err != nil {
		return err
	}
	inst.Bundle = canonical
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		return err
	}
	if err := pinBundleRoot(link, storeRoot); err != nil {
		return err
	}
	tok.remove()
	return nil
}

func adoptNonStoreBundle(dir string, inst *Instance, canonical string) error {
	// Rewrite before removing the out-link: a legacy record may name
	// <dir>/bundle itself, whose meaning the removal would destroy.
	if inst.Bundle != canonical {
		inst.Bundle = canonical
		if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
			return err
		}
	}
	// Only ever a symlink: a real directory at this path could be a user's
	// actual bundle and is never deleted.
	link := bundleLinkPath(dir)
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return nil
}
