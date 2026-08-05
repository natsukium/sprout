package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Drives a command line through the same root the binary builds, covering the
// parsing, dispatch, and validation the per-command tests never reach.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// Mirrors main's classification: a bad command line exits 2, anything else 1.
func isUsageError(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}

// An unrecognized verb must be a usage error naming what was typed, not a
// silent fall-through to the root command's own behavior.
func TestUnknownCommandIsAUsageError(t *testing.T) {
	_, err := runCLI(t, "bogus")
	if err == nil {
		t.Fatal("`sprout bogus` succeeded, want an unknown-command error")
	}
	if !isUsageError(err) {
		t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the unknown command", err)
	}
}

func TestUnknownCommandSuggestsTheNearMiss(t *testing.T) {
	_, err := runCLI(t, "sshh")
	if err == nil {
		t.Fatal("`sprout sshh` succeeded, want an unknown-command error")
	}
	if !strings.Contains(err.Error(), "ssh") {
		t.Errorf("error %q does not suggest `ssh`", err)
	}
}

// A malformed flag must come back as an error, never an os.Exit(2) that
// takes the calling process (including this test binary) with it.
func TestMalformedFlagReturnsInsteadOfExiting(t *testing.T) {
	cases := [][]string{
		{"list", "--nope"},
		{"logs", "--lines", "not-a-number"},
		{"route", "--port", "eighty"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("`sprout %s` succeeded, want a flag error", strings.Join(args, " "))
			}
			if !isUsageError(err) {
				t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
			}
		})
	}
}

// Every command answers -h without doing any work.
func TestHelpForEveryCommand(t *testing.T) {
	var walk func(prefix []string, cmd *cobra.Command)
	walk = func(prefix []string, cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			path := append(append([]string{}, prefix...), sub.Name())
			t.Run(strings.Join(path, " "), func(t *testing.T) {
				if _, err := runCLI(t, append(path, "-h")...); err != nil {
					t.Fatalf("`sprout %s -h` failed: %v", strings.Join(path, " "), err)
				}
				// Comparing the output against sub.Short would restate cobra's
				// template with its own input; what help can actually lack is a
				// description to print.
				if sub.Short == "" {
					t.Errorf("`sprout %s` has no Short description", strings.Join(path, " "))
				}
			})
			walk(path, sub)
		}
	}
	walk(nil, newRootCmd())
}

// `sprout --version` must print the bare version string, which scripts parse;
// cobra's default template would prepend "sprout version ".
func TestVersionFlagPrintsBareVersion(t *testing.T) {
	out, err := runCLI(t, "--version")
	if err != nil {
		t.Fatalf("`sprout --version` failed: %v", err)
	}
	if strings.TrimSpace(out) != versionString() {
		t.Errorf("`sprout --version` printed %q, want the bare %q", strings.TrimSpace(out), versionString())
	}
}

// Positionals are command-specific operands, never instance selectors, so a
// bare NAME must be rejected — and the error must say where the name goes
// instead, since that is what the user was reaching for.
func TestPositionalInstanceNamesAreRejected(t *testing.T) {
	for _, path := range [][]string{
		{"up"}, {"start"}, {"status"}, {"stop"}, {"delete"},
		{"logs"}, {"inspect"}, {"ssh", "config"}, {"snapshot", "list"},
	} {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			_, err := runCLI(t, append(append([]string{}, path...), "feature-x")...)
			if err == nil {
				t.Fatalf("`sprout %s feature-x` was accepted, want a rejected positional", strings.Join(path, " "))
			}
			if !isUsageError(err) {
				t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
			}
			if !strings.Contains(err.Error(), "-i") {
				t.Errorf("error %q does not point at the -i selector that replaced the positional", err)
			}
		})
	}
}

// -i must be the same flag as --instance, not a separate one that could drift.
func TestInstanceShorthandIsTheLongForm(t *testing.T) {
	for _, newCmd := range []func() *cobra.Command{newUpCmd, newStopCmd, newDeleteCmd, newLogsCmd, newShellCmd, newExecCmd, newSnapshotCreateCmd} {
		cmd := newCmd()
		t.Run(cmd.Name(), func(t *testing.T) {
			long := cmd.Flags().Lookup("instance")
			if long == nil {
				t.Fatal("no --instance flag")
			}
			if long.Shorthand != "i" {
				t.Errorf("--instance shorthand = %q, want i", long.Shorthand)
			}
		})
	}
}

// Rule 4 of the grammar: a guest command always begins after `--`. The
// boundary is unconditional, so it cannot be a thing the user re-evaluates
// per invocation and gets wrong the first time their command grows a flag.
func TestGuestCommandRequiresTheDashDashBoundary(t *testing.T) {
	rejected := [][]string{
		{"exec", "just", "test"},        // no boundary at all
		{"exec"},                        // no command either
		{"exec", "--"},                  // boundary, nothing after it
		{"exec", "extra", "--", "true"}, // an operand exec does not have
		{"run", "just", "test"},
		{"run", "--"},
	}
	for _, args := range rejected {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("`sprout %s` was accepted, want the missing `--` rejected", strings.Join(args, " "))
			}
			if !isUsageError(err) {
				t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
			}
			if !strings.Contains(err.Error(), "--") {
				t.Errorf("error %q does not mention the `--` boundary it is enforcing", err)
			}
		})
	}
}

// `shell` means an interactive shell and nothing else; a command belongs to
// `exec`, and someone who typed it here should be told which verb they want.
func TestShellRejectsACommand(t *testing.T) {
	_, err := runCLI(t, "shell", "just", "test")
	if err == nil {
		t.Fatal("`sprout shell just test` was accepted, want it rejected")
	}
	if !isUsageError(err) {
		t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
	}
	if !strings.Contains(err.Error(), "sprout exec -- just test") {
		t.Errorf("error %q does not point at the exec form that does this", err)
	}
}

// --foreground is the opt-in for the supervisor case, and its absence is the
// default — a flipped default here would make every launchd job exit
// immediately.
func TestBootDetachesUnlessForegroundIsAsked(t *testing.T) {
	for _, newCmd := range []func() *cobra.Command{newUpCmd, newStartCmd} {
		cmd := newCmd()
		t.Run(cmd.Name(), func(t *testing.T) {
			fg := cmd.Flags().Lookup("foreground")
			if fg == nil {
				t.Fatal("no --foreground flag")
			}
			if fg.DefValue != "false" {
				t.Errorf("--foreground defaults to %q; booting must detach unless asked", fg.DefValue)
			}
		})
	}
}

// Every re-exec'd child *is* the daemon and must not detach in turn — a
// missing --foreground there spawns a grandchild that does the same, forever.
// It parses too: a flag the child does not understand fails just as silently,
// with the error buried in the boot log.
func TestBackgroundedChildrenRunInTheForeground(t *testing.T) {
	cases := []struct {
		what string
		argv []string
	}{
		{"up", upChildArgs("", "dev", ".", "")},
		{"up with a bundle", upChildArgs("feature-x", "", ".", "/nix/store/bundle")},
		{"start", startChildArgs("aaaa00000001")},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			if len(c.argv) == 0 {
				t.Fatal("empty child argv")
			}
			root := newRootCmd()
			cmd, flags, err := root.Find(c.argv)
			if err != nil {
				t.Fatalf("child argv %v names no command: %v", c.argv, err)
			}
			if err := cmd.ParseFlags(flags); err != nil {
				t.Fatalf("child argv %v does not parse: %v", c.argv, err)
			}
			if fg, _ := cmd.Flags().GetBool("foreground"); !fg {
				t.Errorf("child argv %v does not pass --foreground; it would detach again", c.argv)
			}
		})
	}
}

// A guest command's own flags belong to the guest: parsing must stop at the
// first non-flag word so `sprout exec -- ls -la` does not fail on an unknown -l.
func TestGuestCommandFlagsAreNotParsedBySprout(t *testing.T) {
	for _, c := range []struct {
		verb string
		cmd  *cobra.Command
		args []string
		want []string
	}{
		{"exec", newExecCmd(), []string{"-i", "x", "--", "ls", "-la"}, []string{"ls", "-la"}},
		{"run", newRunCmd(), []string{"--", "ls", "-la"}, []string{"ls", "-la"}},
	} {
		t.Run(c.verb, func(t *testing.T) {
			flags := c.cmd.Flags()
			if err := flags.Parse(c.args); err != nil {
				t.Fatalf("parsing %v: %v", c.args, err)
			}
			got := flags.Args()
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("guest command = %v, want %v", got, c.want)
			}
		})
	}
}

// --all/--project and -i name a scope and a target, and a command line
// carrying both has no reading that is obviously what the user meant — least
// of all for delete. Two scopes at once are just as unreadable.
func TestAllAndInstanceAreMutuallyExclusive(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "--all", "-i", "main"},
		{"stop", "--all", "-i", "main"},
		{"delete", "--project", "-i", "main"},
		{"stop", "--project", "-i", "main"},
		{"delete", "--all", "--project"},
		{"stop", "--all", "--project"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("`sprout %s` was accepted, want the two selectors rejected", strings.Join(args, " "))
			}
			if !isUsageError(err) {
				t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
			}
		})
	}
}

// Options only a supervisor can satisfy stay out of help: nobody types a nix
// store path or a launchd socket name at a prompt, so listing them offers only
// a way to get it wrong. They must still parse — the nix-darwin module passes
// them — which is what separates hidden from removed.
func TestSupervisorOnlyFlagsAreHiddenButLive(t *testing.T) {
	for _, c := range []struct {
		cmd  *cobra.Command
		flag string
	}{
		{newUpCmd(), "bundle"},
		{newRouteServeCmd(), "launchd-socket"},
	} {
		t.Run(c.cmd.Name()+" --"+c.flag, func(t *testing.T) {
			f := c.cmd.Flags().Lookup(c.flag)
			if f == nil {
				t.Fatalf("--%s is gone; the nix-darwin module passes it", c.flag)
			}
			if !f.Hidden {
				t.Errorf("--%s appears in help", c.flag)
			}
		})
	}
}

// The nix-darwin module composes these command lines in Nix, where nothing
// type-checks them against the CLI. A flag renamed here and missed there fails
// at launchd start time, with the error in a log file nobody is watching.
func TestLaunchdCommandLinesStillParse(t *testing.T) {
	// Kept verbatim from nix/darwin-module.nix's `command =` strings, with the
	// store paths and names substituted.
	for _, c := range []struct {
		argv     []string
		wantPath string
	}{
		{[]string{"up", "--foreground", "--bundle", "/nix/store/x-sprout-vm-runner", "--instance", "runner"}, "sprout up"},
		{[]string{"route", "serve", "--launchd-socket", "Listeners", "--domain", "sprout.localhost", "--no-wake"}, "sprout route serve"},
	} {
		t.Run(strings.Join(c.argv, " "), func(t *testing.T) {
			root := newRootCmd()
			cmd, flags, err := root.Find(c.argv)
			if err != nil {
				t.Fatalf("names no command: %v", err)
			}
			if cmd.CommandPath() != c.wantPath {
				t.Fatalf("dispatched to %q, want %q", cmd.CommandPath(), c.wantPath)
			}
			if err := cmd.ParseFlags(flags); err != nil {
				t.Fatalf("does not parse: %v", err)
			}
			if args := cmd.Flags().Args(); len(args) > 0 {
				t.Errorf("left %v as positional arguments; a flag was dropped", args)
			}
		})
	}
}
