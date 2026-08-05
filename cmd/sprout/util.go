package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func printJSON(v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func addJSONFlag(cmd *cobra.Command, what string) *bool {
	var jsonOut bool
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print "+what)
	return &jsonOut
}

type listing[T any] struct {
	rows   []T
	empty  string
	header string
	row    func(w io.Writer, r T)
	footer string
}

func (l listing[T]) render(jsonOut bool) error {
	if jsonOut {
		return printJSON(l.rows)
	}
	if len(l.rows) == 0 {
		fmt.Println(l.empty)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, l.header)
	for _, r := range l.rows {
		l.row(w, r)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if l.footer != "" {
		fmt.Println("\n" + l.footer)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// cond is always checked at least once.
func pollUntil(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// A leading alphanumeric rules out ".." and separators structurally, rather
// than by blocklisting them.
var componentNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Validated rather than sanitized, unlike a branch name: the user types this
// string back, so rewriting it would hand them a name that does not work.
func validateNameComponent(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if !componentNameRe.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: use letters, digits, dot, dash and underscore, starting with a letter or digit", kind, name)
	}
	return nil
}
