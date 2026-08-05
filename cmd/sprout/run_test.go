package main

import (
	"testing"
)

// A run name must be sanitizer-safe and unique per call: a stable state-dir
// path, and no collision between concurrent runs.
func TestEphemeralName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n, err := ephemeralName()
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := sanitizeName(n); got != n {
			t.Fatalf("ephemeralName() = %q, not sanitizer-stable (got %q)", n, got)
		}
		if seen[n] {
			t.Fatalf("ephemeralName() produced a duplicate: %q", n)
		}
		seen[n] = true
	}
}
