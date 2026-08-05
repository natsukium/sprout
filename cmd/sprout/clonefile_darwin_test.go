//go:build darwin

package main

import (
	"path/filepath"
	"testing"
)

// darwin-only because the property cannot hold everywhere: t.TempDir() is APFS
// here, but on a Linux runner it may be ext4, where the copy fallback is the
// honest result.
func TestCloneFileUsesCoWOnAPFS(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "var.img")
	dst := filepath.Join(dir, "clone.img")
	const size = 64 << 20
	writeImage(t, src, size, 0, []byte("apfs"))

	cow, err := cloneFile(src, dst)
	if err != nil {
		t.Fatalf("cloneFile: %v", err)
	}
	if !cow {
		t.Fatal("cloneFile fell back to a full copy on APFS, where clonefile(2) should have succeeded")
	}
	// A clone shares the source's extents, so equal allocation is what catches
	// a "clone" that silently became a real copy.
	if got, want := diskBytes(dst), diskBytes(src); got != want {
		t.Fatalf("clone allocation = %d, source = %d; want them equal (shared extents)", got, want)
	}
}
