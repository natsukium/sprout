package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		def      string
		flakeRef string
	)
	cmd := &cobra.Command{
		Use:     "run -- CMD...",
		Short:   "Boot, run CMD, destroy (ephemeral)",
		GroupID: groupDaily,
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireGuestCommand(c, args); err != nil {
				return err
			}
			return cmdRun(def, flakeRef, args)
		},
	}
	// A guest command's flags are not sprout's: parsing stops at the first
	// non-flag word, so a missing `--` gets sprout's diagnosis rather than
	// pflag's "unknown shorthand flag".
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&def, "vm", "", "VM definition name (sprout.vms.<name>; default: the flake's only definition, or \"dev\")")
	cmd.Flags().StringVar(&flakeRef, "flake", ".", "flake reference to build from")
	return cmd
}

func cmdRun(def, flakeRef string, command []string) error {
	// Before the child `up` spawns, so an ambiguous flake fails here with the
	// candidate list instead of inside background boot chatter.
	definition, err := resolveDefinition(flakeRef, def)
	if err != nil {
		return err
	}

	name, err := ephemeralName()
	if err != nil {
		return err
	}
	// The child `up`/`delete` invocations re-resolve the same identity from
	// -i, cwd and repo context being unchanged.
	id, err := resolveIdentity(name)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	// The daemon gets its own process group, so a Ctrl-C reaches the command
	// rather than the daemon, which has to outlive the interrupt long enough
	// to stop the VM cleanly.
	up, err := backgroundSelf(upChildArgs(name, definition, flakeRef, ""), os.Stderr)
	if err != nil {
		return err
	}
	if err := up.Start(); err != nil {
		return fmt.Errorf("boot: %w", err)
	}

	defer func() {
		teardown := exec.Command(exe, "delete", "--force", "--instance", name)
		teardown.Stdout, teardown.Stderr = os.Stderr, os.Stderr
		_ = teardown.Run()
	}()

	if err := awaitBootOrReady(up.Wait, id.ID, 0, "", "up"); err != nil {
		return err
	}

	sshPath, sshArgs, err := sshInvocation(id.ID, false, command)
	if err != nil {
		return err
	}
	run := &exec.Cmd{
		Path:   sshPath,
		Args:   sshArgs,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	// Ignored in this parent while the command runs: the signal still reaches
	// the ssh child, so the command aborts and the deferred teardown runs
	// rather than the daemon being orphaned.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	err = run.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return &exitCodeError{code: exit.ExitCode()}
	}
	return err
}

// Random rather than a PID or timestamp, so concurrent runs cannot clash and
// no run reuses a just-removed instance's state directory.
func ephemeralName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b[:]), nil
}

// Well above any real build, so only a genuinely stuck one trips it.
const daemonStartTimeout = 30 * time.Minute

// Two phases, so a slow `nix build` is never counted as a boot that failed to
// become ready. Polling the daemon's judgment rather than dialing SSH is what
// makes every waiter agree with it on what "ready" means.
//
// supersededPID is the runner PID of a daemon this boot replaces, 0 on a
// fresh boot: an in-place reboot leaves the outgoing VM answering control
// through the rebuild, which would otherwise read as ready.
func waitInstanceReady(id string, supersededPID int) error {
	// The daemon serves control before it polls the guest, so its INFO marks
	// the end of the build phase. A build failure exits the child instead,
	// which callers race against this, so the cap only guards a hung build.
	started := pollUntil(daemonStartTimeout, 500*time.Millisecond, func() bool {
		info, err := queryInfoBrief(id)
		return err == nil && info.PID != supersededPID
	})
	if !started {
		return fmt.Errorf("instance %q daemon did not start within %s", displayForID(id), daemonStartTimeout)
	}
	ready := pollUntil(readyTimeout, 500*time.Millisecond, func() bool {
		info, err := queryInfoBrief(id)
		return err == nil && info.Ready
	})
	if !ready {
		return fmt.Errorf("instance %q did not become ready within %s", displayForID(id), readyTimeout)
	}
	return nil
}
