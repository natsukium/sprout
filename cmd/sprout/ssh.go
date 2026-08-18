package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// System ssh runs this as its ProxyCommand.
func newDialStdioCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dial-stdio [ADDR]",
		Short:  "Pipe stdio to a guest address through the daemon (internal)",
		Hidden: true,
		Args:   usageArgs(cobra.MaximumNArgs(1)),
	}
	selector := addInstanceFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		addr := "ssh"
		if len(args) > 0 {
			addr = args[0]
		}
		return cmdDialStdio(*selector, addr)
	}
	return cmd
}

func cmdDialStdio(selector, addr string) error {
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}

	conn, err := controlDial(id.ID)
	if err != nil {
		return fmt.Errorf("instance %q is not running: %w", id.Display(), err)
	}
	defer conn.Close()
	reader, err := dialHandshake(conn, addr)
	if err != nil {
		return err
	}

	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, os.Stdin); done <- struct{}{} }()    //nolint:errcheck
	go func() { io.Copy(os.Stdout, reader); done <- struct{}{} }() //nolint:errcheck
	<-done
	return nil
}

// Neither verb starts a stopped instance: wake-on-access is a router
// property, not a general lifecycle rule.

func newShellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "shell",
		Short:   "Open an interactive shell in the environment",
		GroupID: groupDaily,
		Args: usageArgs(func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("shell opens an interactive shell and takes no arguments; to run a command use `sprout exec -- %s`", strings.Join(args, " "))
			}
			return nil
		}),
	}
	selector := addInstanceFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return sshInto(*selector, true, nil)
	}
	return cmd
}

func newExecCmd() *cobra.Command {
	var forceTTY bool
	cmd := &cobra.Command{
		Use:     "exec -- CMD...",
		Short:   "Run a command in the environment",
		GroupID: groupDaily,
	}
	// A guest command's flags are not sprout's: parsing stops at the first
	// non-flag word, so a missing `--` gets the diagnosis below rather than
	// pflag's "unknown shorthand flag".
	cmd.Flags().SetInterspersed(false)
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVarP(&forceTTY, "tty", "t", false, "allocate a TTY (for full-screen commands like htop)")
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if err := requireGuestCommand(c, args); err != nil {
			return err
		}
		return sshInto(*selector, forceTTY, args)
	}
	return cmd
}

// The `--` is required unconditionally: a rule that applies only sometimes is
// one the user gets wrong exactly when the guest command grows its first flag.
func requireGuestCommand(cmd *cobra.Command, args []string) error {
	switch n := cmd.ArgsLenAtDash(); {
	case n < 0:
		if len(args) == 0 {
			return usagef("%s needs a command to run: %s -- CMD…", cmd.CommandPath(), cmd.CommandPath())
		}
		return usagef("a guest command must follow `--`: %s -- %s", cmd.CommandPath(), strings.Join(args, " "))
	case n > 0:
		return usagef("%s takes no arguments before `--`, got %v", cmd.CommandPath(), args[:n])
	case len(args) == 0:
		return usagef("%s needs a command after `--`", cmd.CommandPath())
	}
	return nil
}

func sshInto(selector string, tty bool, command []string) error {
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}
	if !instanceRunning(id.ID) {
		return stoppedError(id, selector)
	}
	// A stale instance is still reachable, but its /workspace shows the other
	// branch's files.
	if stale, ok := checkBranchStaleness(id.KeySource, id.Name, id.Worktree); ok && stale {
		fmt.Fprintf(os.Stderr, "warning: instance %q was booted for branch %q, but %s now has a different branch checked out\n", id.Display(), id.Name, id.Worktree)
	}
	sshPath, sshArgs, err := sshInvocation(id.ID, tty, command)
	if err != nil {
		return err
	}
	return syscall.Exec(sshPath, sshArgs, os.Environ())
}

// Values are pre-quoted for ssh_config; ssh's -o parser accepts the same.
type sshTarget struct {
	label   string
	user    string
	options [][2]string
	// Not part of options: a cd baked into an ssh_config block would break
	// scp/rsync over the same Host entry.
	workspaceMounted bool
}

func sshTargetFor(id string) (*sshTarget, error) {
	inst, dir, err := loadInstance(id)
	if err != nil {
		return nil, err
	}
	// Only has to be a well-formed ssh host argument, not unique: ProxyCommand
	// does the routing, this is what ssh and known_hosts call the instance.
	label, err := sanitizeName(inst.Name)
	if err != nil {
		label = inst.ID
	}
	keyPath, err := ensureSSHKey()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &sshTarget{
		label:            label,
		user:             inst.SSHUser,
		workspaceMounted: inst.WorkspaceMounted,
		options: [][2]string{
			{"IdentityFile", fmt.Sprintf("%q", keyPath)},
			{"IdentitiesOnly", "yes"},
			{"ProxyCommand", fmt.Sprintf("%q dial-stdio --instance %s ssh", exe, inst.ID)},
			{"StrictHostKeyChecking", "accept-new"},
			{"UserKnownHostsFile", fmt.Sprintf("%q", knownHostsPath(dir))},
			{"LogLevel", "ERROR"},
		},
	}, nil
}

func sshInvocation(id string, tty bool, command []string) (string, []string, error) {
	target, err := sshTargetFor(id)
	if err != nil {
		return "", nil, err
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return "", nil, err
	}
	sshArgs := []string{"ssh"}
	for _, o := range target.options {
		sshArgs = append(sshArgs, "-o", o[0]+"="+o[1])
	}
	if !tty {
		sshArgs = append(sshArgs, "-T")
	}
	sshArgs = append(sshArgs, fmt.Sprintf("%s@sprout-%s", target.user, target.label))
	if len(command) > 0 {
		sshArgs = append(sshArgs, remoteCommand(command, target.workspaceMounted))
	}
	return sshPath, sshArgs, nil
}

// One quoted string, because OpenSSH otherwise joins its argv with spaces
// before the remote shell sees it, losing the boundaries in `sh -c '…'`,
// empty arguments, and arguments containing spaces.
func remoteCommand(command []string, workspaceMounted bool) string {
	words := make([]string, len(command))
	for i, arg := range command {
		words[i] = shellQuote(arg)
	}
	prefix := ""
	if workspaceMounted {
		// Interactive shells get this from the guest's login init, which a
		// non-interactive command never reads.
		prefix = "cd /workspace 2>/dev/null; "
	}
	return prefix + strings.Join(words, " ")
}

func newSSHCmd() *cobra.Command {
	cmd := groupingCmd(&cobra.Command{
		Use:     "ssh",
		Short:   "SSH interoperability for other tools",
		GroupID: groupIntegration,
	})
	cmd.AddCommand(newSSHConfigCmd())
	return cmd
}

// The instance need not be running: the ProxyCommand simply fails to dial
// until `sprout up` boots it.
func newSSHConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print an ssh_config block (VS Code Remote-SSH, scp, …)",
		Args:  usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		id, err := resolveExistingIdentity(*selector)
		if err != nil {
			return err
		}
		block, err := sshConfigBlock(id.ID)
		if err != nil {
			return err
		}
		fmt.Print(block)
		return nil
	}
	return cmd
}

func sshConfigBlock(id string) (string, error) {
	target, err := sshTargetFor(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Host sprout-%s\n", target.label)
	fmt.Fprintf(&b, "\tHostName sprout-%s\n", target.label)
	fmt.Fprintf(&b, "\tUser %s\n", target.user)
	for _, o := range target.options {
		fmt.Fprintf(&b, "\t%s %s\n", o[0], o[1])
	}
	return b.String(), nil
}
