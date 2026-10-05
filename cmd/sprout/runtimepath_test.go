package main

import "testing"

// The packaged git wins over a /usr/bin stub, the user's own ssh wins over the
// packaged one, and a plain `go build` leaves PATH alone.
func TestWithRuntimePath(t *testing.T) {
	for _, c := range []struct{ first, current, last, want string }{
		{"/nix/git/bin", "/usr/bin:/bin", "/nix/ssh/bin", "/nix/git/bin:/usr/bin:/bin:/nix/ssh/bin"},
		{"", "/usr/bin:/bin", "", "/usr/bin:/bin"},
		{"/nix/git/bin", "", "/nix/ssh/bin", "/nix/git/bin:/nix/ssh/bin"},
	} {
		if got := withRuntimePath(c.first, c.current, c.last); got != c.want {
			t.Errorf("withRuntimePath(%q, %q, %q) = %q, want %q", c.first, c.current, c.last, got, c.want)
		}
	}
}
