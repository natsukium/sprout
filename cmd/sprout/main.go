// sprout: disposable, declarative Linux microVMs for macOS development.
//
// Nix defines (runner + manifest.json), this binary executes: it owns
// instance state, the embedded guest network stack, and the vfkit runner
// process lifecycle.
package main

import (
	"errors"
	"fmt"
	"os"
)

// The three outcomes stay distinct: a wrapped command's own status
// (`sprout run`), a bad command line (2), and a failed operation (1).
func main() {
	err := execute(os.Args[1:])
	if err == nil {
		return
	}
	// No "sprout:" prefix here: the failure is the wrapped command's.
	var ec *exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}
	fmt.Fprintln(os.Stderr, "sprout:", err)
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", ue.helpPath())
		os.Exit(2)
	}
	os.Exit(1)
}

// Error() is only a fallback: main exits before printing it.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
