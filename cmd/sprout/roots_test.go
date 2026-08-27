package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRecord(t *testing.T, dir string) *Instance {
	t.Helper()
	data, err := os.ReadFile(instanceRecordPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		t.Fatal(err)
	}
	return &inst
}

func seedRecord(t *testing.T, dir, name, bundle string) *Instance {
	t.Helper()
	inst := &Instance{ID: filepath.Base(dir), Name: name, KeySource: "directory", Bundle: bundle}
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

type pinCall struct{ link, storePath string }

// Replaces the nix invocation with a recorder that still creates the out-link,
// so what the callers observe on disk keeps the shape a real pin leaves. fail
// reports the error for the n-th call, if any.
func recordPins(t *testing.T, fail func(n int) error) *[]pinCall {
	t.Helper()
	var calls []pinCall
	original := pinBundleRoot
	t.Cleanup(func() { pinBundleRoot = original })
	pinBundleRoot = func(link, storePath string) error {
		calls = append(calls, pinCall{link: link, storePath: storePath})
		if fail != nil {
			if err := fail(len(calls)); err != nil {
				return err
			}
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(storePath, link)
	}
	return &calls
}

// A record is resolved before any branch decision, so the store-backed cases
// need a store path that really exists on this host. nested names an entry
// inside it, standing in for a record that points below the store path.
func storeDirAndChild(t *testing.T) (storePath, nested string) {
	t.Helper()
	f, err := os.Open("/nix/store")
	if err != nil {
		t.Skip("no readable /nix/store on this host")
	}
	defer f.Close()
	var fallback string
	for {
		entries, readErr := f.ReadDir(64)
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			path := filepath.Join("/nix/store", e.Name())
			if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path {
				continue
			}
			if fallback == "" {
				fallback = path
			}
			children, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			for _, c := range children {
				child := filepath.Join(path, c.Name())
				if resolved, err := filepath.EvalSymlinks(child); err == nil && resolved == child {
					return path, child
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if fallback == "" {
		t.Skip("/nix/store has no entry usable as a bundle stand-in")
	}
	// No nested entry to be found: the store path stands in for itself, which
	// exercises the same branch.
	return fallback, fallback
}

func anyStoreDir(t *testing.T) string {
	t.Helper()
	storePath, _ := storeDirAndChild(t)
	return storePath
}

// The root to pin is the whole store path, whatever depth the record names.
func TestStoreRootOf(t *testing.T) {
	cases := []struct {
		canonical string
		want      string
		wantOK    bool
	}{
		{canonical: "/nix/store/abc-x", want: "/nix/store/abc-x", wantOK: true},
		{canonical: "/nix/store/abc-x/nested", want: "/nix/store/abc-x", wantOK: true},
		{canonical: "/nix/store/abc-x/nested/deeper", want: "/nix/store/abc-x", wantOK: true},
		{canonical: "/nix/store/"},
		{canonical: "/nix/store"},
		{canonical: "/nix/store/../x"},
		{canonical: "/nix/store/./x"},
		{canonical: "/nix/storeless/abc-x"},
		{canonical: "/home/u/bundle"},
		{canonical: "relative/bundle"},
		{canonical: ""},
	}
	for _, c := range cases {
		got, ok := storeRootOf(c.canonical)
		if ok != c.wantOK || got != c.want {
			t.Errorf("storeRootOf(%q) = (%q, %v), want (%q, %v)", c.canonical, got, ok, c.want, c.wantOK)
		}
	}
}

// A bundle outside the store gets no root: the record is canonicalized and the
// stale out-link dropped, but only when that link is a symlink sprout made.
func TestReconcileRootsAdoptsNonStoreBundle(t *testing.T) {
	t.Run("symlinked record is canonicalized and the link dropped", func(t *testing.T) {
		dir := t.TempDir()
		target, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		link := bundleLinkPath(dir)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		inst := seedRecord(t, dir, "legacy", link)
		calls := recordPins(t, nil)

		if err := reconcileRoots(dir, inst); err != nil {
			t.Fatalf("reconcileRoots: %v", err)
		}
		if inst.Bundle != target {
			t.Errorf("record in memory = %q, want the canonical %q", inst.Bundle, target)
		}
		if got := readRecord(t, dir).Bundle; got != target {
			t.Errorf("record on disk = %q, want the canonical %q", got, target)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("the out-link of a non-store bundle survived: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("a non-store bundle was pinned: %v", *calls)
		}
	})

	// A directory here can be the user's own prebuilt bundle, which sprout must
	// never delete.
	t.Run("a real directory at the link path is kept", func(t *testing.T) {
		dir := t.TempDir()
		bundle := bundleLinkPath(dir)
		if err := os.MkdirAll(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		canonical, err := filepath.EvalSymlinks(bundle)
		if err != nil {
			t.Fatal(err)
		}
		inst := seedRecord(t, dir, "prebuilt", bundle)

		if err := reconcileRoots(dir, inst); err != nil {
			t.Fatalf("reconcileRoots: %v", err)
		}
		if got := readRecord(t, dir).Bundle; got != canonical {
			t.Errorf("record on disk = %q, want the canonical %q", got, canonical)
		}
		fi, err := os.Stat(bundle)
		if err != nil || !fi.IsDir() {
			t.Fatalf("the user's own bundle directory was removed: %v", err)
		}
	})
}

// The invocation directory a relative record was written from is unknown, so
// resolving it against the current one would name an unrelated directory.
func TestReconcileRootsRefusesARelativeRecord(t *testing.T) {
	dir := t.TempDir()
	inst := seedRecord(t, dir, "relative", "relative/bundle")

	err := reconcileRoots(dir, inst)
	if err == nil {
		t.Fatal("reconcileRoots accepted a relative bundle path")
	}
	if !strings.Contains(err.Error(), "run `sprout up`") {
		t.Errorf("error = %v, want it to point at `sprout up`", err)
	}
	if got := readRecord(t, dir).Bundle; got != "relative/bundle" {
		t.Errorf("record on disk = %q, want the untouched relative path", got)
	}
}

// A collected bundle must not be adopted as a non-store path, and the failure
// returns before the sweep: retained staging may be the record's only
// remaining protection.
func TestReconcileRootsRejectsAVanishedBundleAndKeepsTokens(t *testing.T) {
	for _, c := range []struct {
		what   string
		record func(dir string) string
	}{
		{what: "missing path", record: func(dir string) string { return filepath.Join(dir, "collected") }},
		{what: "dangling symlink", record: func(dir string) string {
			link := filepath.Join(dir, "dangling")
			if err := os.Symlink(filepath.Join(dir, "collected"), link); err != nil {
				panic(err)
			}
			return link
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			dir := t.TempDir()
			dead, err := publishToken(dir)
			if err != nil {
				t.Fatal(err)
			}
			dead.Close()
			inst := seedRecord(t, dir, "collected", c.record(dir))

			err = reconcileRoots(dir, inst)
			if err == nil {
				t.Fatal("reconcileRoots accepted a bundle that is no longer there")
			}
			if !strings.Contains(err.Error(), "no longer in the store") {
				t.Errorf("error = %v, want it to say the build is gone", err)
			}
			if _, err := os.Stat(dead.dir); err != nil {
				t.Errorf("a failed reconcile swept staging anyway: %v", err)
			}
		})
	}
}

// A canonical store record pins the enclosing store path directly, and only
// then are dead tokens collected.
func TestReconcileRootsPinsACanonicalStoreRecord(t *testing.T) {
	storePath := anyStoreDir(t)
	dir := t.TempDir()
	dead, err := publishToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	inst := seedRecord(t, dir, "canonical", storePath)
	calls := recordPins(t, nil)

	if err := reconcileRoots(dir, inst); err != nil {
		t.Fatalf("reconcileRoots: %v", err)
	}
	want := []pinCall{{link: bundleLinkPath(dir), storePath: storePath}}
	if len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Fatalf("pins = %v, want %v", *calls, want)
	}
	if got := readRecord(t, dir).Bundle; got != storePath {
		t.Errorf("record on disk = %q, want it unchanged at %q", got, storePath)
	}
	if _, err := os.Stat(dead.dir); !os.IsNotExist(err) {
		t.Errorf("a dead token survived a successful reconcile: %v", err)
	}
}

// A resolving out-link proves nothing about the gcroots/auto registration,
// which lives outside the instance directory: every boot re-pins.
func TestReconcileRootsRepinsAnAlreadyCorrectLink(t *testing.T) {
	storePath := anyStoreDir(t)
	dir := t.TempDir()
	if err := os.Symlink(storePath, bundleLinkPath(dir)); err != nil {
		t.Fatal(err)
	}
	inst := seedRecord(t, dir, "repin", storePath)
	calls := recordPins(t, nil)

	if err := reconcileRoots(dir, inst); err != nil {
		t.Fatalf("reconcileRoots: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("pins = %v, want exactly one even though the link already resolved", *calls)
	}
}

// A legacy record whose own resolution passes through <dir>/bundle must be
// protected by a temporary root before the link is repointed: temp pin,
// rewrite, canonical pin, then retire the temp token.
func TestReconcileRootsMigratesALegacyRecordInOrder(t *testing.T) {
	storePath, nested := storeDirAndChild(t)
	dir := t.TempDir()
	link := bundleLinkPath(dir)
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	inst := seedRecord(t, dir, "legacy-store", link)
	calls := recordPins(t, nil)

	if err := reconcileRoots(dir, inst); err != nil {
		t.Fatalf("reconcileRoots: %v", err)
	}
	if got := readRecord(t, dir).Bundle; got != nested {
		t.Errorf("record on disk = %q, want the canonical %q", got, nested)
	}
	if len(*calls) != 2 {
		t.Fatalf("pins = %v, want a staging pin followed by the canonical one", *calls)
	}
	staged := (*calls)[0]
	if filepath.Base(staged.link) != "bundle" || filepath.Dir(filepath.Dir(staged.link)) != stagingDir(dir) {
		t.Errorf("first pin landed at %q, want a staging token's bundle link under %s", staged.link, stagingDir(dir))
	}
	if staged.storePath != storePath {
		t.Errorf("first pin covered %q, want the enclosing store path %q", staged.storePath, storePath)
	}
	if want := (pinCall{link: link, storePath: storePath}); (*calls)[1] != want {
		t.Errorf("second pin = %v, want %v", (*calls)[1], want)
	}
	if names := stagingEntries(t, dir); len(names) != 0 {
		t.Errorf("staging holds %v after a completed migration, want it empty", names)
	}
}

// The canonical pin is the last step, so its failure leaves a record already
// rewritten — and the temporary root is then the only thing protecting it.
func TestReconcileRootsKeepsTheTempRootWhenTheCanonicalPinFails(t *testing.T) {
	_, nested := storeDirAndChild(t)
	dir := t.TempDir()
	link := bundleLinkPath(dir)
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	inst := seedRecord(t, dir, "legacy-store", link)
	pinFailed := errors.New("nix build refused")
	calls := recordPins(t, func(n int) error {
		if n == 2 {
			return pinFailed
		}
		return nil
	})

	err := reconcileRoots(dir, inst)
	if !errors.Is(err, pinFailed) {
		t.Fatalf("reconcileRoots error = %v, want the pin failure", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("pins = %v, want the staging pin and the failing canonical one", *calls)
	}
	if got := readRecord(t, dir).Bundle; got != nested {
		t.Errorf("record on disk = %q, want the rewrite that preceded the failing pin (%q)", got, nested)
	}
	if names := stagingEntries(t, dir); len(names) != 1 {
		t.Errorf("staging holds %v, want the temporary root kept as the record's protection", names)
	}
}
