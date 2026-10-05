package main

import "testing"

// The packaged tools win over whatever the user's PATH would find first, and a
// plain `go build` leaves PATH alone.
func TestPrependRuntimePath(t *testing.T) {
	for _, c := range []struct{ extra, current, want string }{
		{"/nix/store/git/bin", "/usr/bin:/bin", "/nix/store/git/bin:/usr/bin:/bin"},
		{"", "/usr/bin:/bin", "/usr/bin:/bin"},
		{"/nix/store/git/bin", "", "/nix/store/git/bin"},
	} {
		if got := prependRuntimePath(c.extra, c.current); got != c.want {
			t.Errorf("prependRuntimePath(%q, %q) = %q, want %q", c.extra, c.current, got, c.want)
		}
	}
}
