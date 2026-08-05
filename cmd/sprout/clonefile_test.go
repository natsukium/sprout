package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Stands in for a disk image: mostly holes, some data.
func writeImage(t *testing.T, path string, size int64, offset int64, content []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(content, offset); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The destination is byte-for-byte the source, holes included. Which of the
// two copy paths ran depends on the filesystem underneath, so the assertion is
// on the outcome rather than the method.
func TestCloneFileReproducesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "var.img")
	dst := filepath.Join(dir, "clone.img")
	// Data past a hole and a hole past the data, so a copy that stops at the
	// last written byte or forgets to skip zeros is caught.
	const size = 8 << 20
	payload := bytes.Repeat([]byte("sprout"), 1000)
	writeImage(t, src, size, 5<<20, payload)

	if _, err := cloneFile(src, dst); err != nil {
		t.Fatalf("cloneFile: %v", err)
	}

	want, got := mustRead(t, src), mustRead(t, dst)
	if !bytes.Equal(want, got) {
		t.Fatalf("clone differs from source (len %d vs %d)", len(want), len(got))
	}
	if int64(len(got)) != size {
		t.Fatalf("clone length = %d, want %d", len(got), size)
	}
}

// Neither path overwrites: an existing name fails before any bytes move, which
// is what keeps a restore from reading a half-replaced image. On the CoW path
// this is clonefile(2)'s own precondition; the copy path has to uphold it
// itself.
func TestCloneFileRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "var.img")
	dst := filepath.Join(dir, "clone.img")
	writeImage(t, src, 1<<20, 0, []byte("source"))
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := cloneFile(src, dst); err == nil {
		t.Fatal("cloneFile overwrote an existing destination, want an error")
	}
	if got := mustRead(t, dst); !bytes.Equal(got, []byte("existing")) {
		t.Fatalf("destination was modified: %q", got)
	}
}

// Exercises the fallback directly: on macOS the CoW path always wins, so
// otherwise the fallback would only be tested on filesystems without reflinks.
func TestCopySparseSkipsHoles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "var.img")
	dst := filepath.Join(dir, "copy.img")
	const size = 32 << 20
	payload := []byte("data-in-the-middle")
	writeImage(t, src, size, 16<<20, payload)

	if err := copySparse(src, dst); err != nil {
		t.Fatalf("copySparse: %v", err)
	}

	if !bytes.Equal(mustRead(t, src), mustRead(t, dst)) {
		t.Fatal("sparse copy differs from source")
	}
	// The copy must stay sparse: writing every zero would turn a 150 GiB image
	// holding 40 GiB of data into a fully allocated 150 GiB file.
	if allocated := diskBytes(dst); allocated >= size {
		t.Fatalf("copy allocated %d bytes for a %d-byte sparse file; holes were materialized", allocated, size)
	}
}

// A failed copy leaves nothing behind: a truncated image at the destination
// would list as a usable snapshot.
func TestCopySparseCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "copy.img")

	if err := copySparse(filepath.Join(dir, "does-not-exist.img"), dst); err == nil {
		t.Fatal("copySparse succeeded with a missing source, want an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination survived a failed copy (stat err = %v)", err)
	}
}

func TestAllZero(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"all zero", make([]byte, 64), true},
		{"trailing non-zero", append(make([]byte, 63), 1), false},
		{"leading non-zero", append([]byte{1}, make([]byte, 63)...), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allZero(c.in); got != c.want {
				t.Fatalf("allZero(%d bytes) = %v, want %v", len(c.in), got, c.want)
			}
		})
	}
}
