package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

type portForward struct {
	hostPort, guestPort int
}

func parsePortSpec(s string) (portForward, error) {
	if h, g, ok := strings.Cut(s, ":"); ok {
		hp, err := parsePort(h)
		if err != nil {
			return portForward{}, err
		}
		gp, err := parsePort(g)
		if err != nil {
			return portForward{}, err
		}
		return portForward{hp, gp}, nil
	}
	p, err := parsePort(s)
	if err != nil {
		return portForward{}, err
	}
	return portForward{p, p}, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q (want 1-65535)", s)
	}
	return p, nil
}

func newForwardCmd() *cobra.Command {
	var bind string
	cmd := &cobra.Command{
		Use:   "forward PORT[:GUESTPORT]...",
		Short: "Forward host ports into the VM",
		Long: `Forward host ports into the VM for as long as the command runs.

Without --instance the target is re-resolved on every new connection, so a
forward left running across a branch switch follows the checkout. Selecting an
instance pins it for the process's whole life, which is what side-by-side
comparison across branches needs.`,
		GroupID: groupNetwork,
		Args: usageArgs(func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("usage: sprout forward [-i INSTANCE] [--bind ADDR] PORT[:GUESTPORT]…")
			}
			return nil
		}),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().StringVar(&bind, "bind", defaultBindAddress, "address to bind (0.0.0.0 for all interfaces, reachable from your LAN; also lets you bind privileged ports <1024 without root on macOS)")
	cmd.RunE = func(_ *cobra.Command, args []string) error { return cmdForward(*selector, bind, args) }
	return cmd
}

func cmdForward(selector, bind string, args []string) error {
	pinned := selector != ""
	resolve, err := forwardResolver(selector)
	if err != nil {
		return err
	}
	id0, err := resolve()
	if err != nil {
		return err
	}
	if !instanceRunning(id0.ID) {
		return stoppedError(id0, selector)
	}

	// Every spec is parsed before anything binds, so a typo in the last
	// argument does not leave earlier ports half-forwarded.
	specs := make([]portForward, 0, len(args))
	for _, a := range args {
		pf, err := parsePortSpec(a)
		if err != nil {
			return err
		}
		specs = append(specs, pf)
	}

	var listeners []net.Listener
	if !loopbackBind(bind) {
		fmt.Fprintf(os.Stderr, "warning: --bind %s is not loopback; these forwards are reachable from that network\n", bind)
	}
	var descs []string
	for _, pf := range specs {
		lns, err := listenHostPort(bind, pf.hostPort)
		if err != nil {
			closeAll(listeners)
			return err
		}
		listeners = append(listeners, lns...)
		for _, ln := range lns {
			go forwardAccept(ln, resolve, pf.guestPort)
		}
		descs = append(descs, fmt.Sprintf("%s:%d → vm:%d", bind, pf.hostPort, pf.guestPort))
	}
	target := "this worktree's current branch"
	if pinned {
		target = fmt.Sprintf("instance %q", id0.Display())
	}
	fmt.Printf("forwarding %s to %s (Ctrl-C to stop)\n", strings.Join(descs, ", "), target)

	awaitInterrupt(listeners, "stopped forwarding")
	return nil
}

func listenHostPort(bindHost string, port int) ([]net.Listener, error) {
	lns, err := bindListeners(bindHost, port)
	if errors.Is(err, errPrivilegedBind) {
		return nil, fmt.Errorf("%w — rerun with --bind 0.0.0.0 to bind all interfaces instead", err)
	}
	return lns, err
}

// The name rather than 127.0.0.1: a browser resolving `localhost` may try ::1
// first and take the refusal there as the answer, where curl falls back to the
// other family. An IPv4-only listener then looks dead to a browser and live to
// a script.
const defaultBindAddress = "localhost"

// Only "localhost" expands, naming loopback rather than an address; binding
// more than the user named would widen the exposure they chose.
func bindAddrs(bindHost string) []string {
	if strings.EqualFold(bindHost, "localhost") {
		return []string{"127.0.0.1", "::1"}
	}
	return []string{bindHost}
}

// The whole set is undone if one address fails: a half-bound --bind that
// answers on one family and refuses on another is what this prevents.
func bindListeners(bindHost string, port int) ([]net.Listener, error) {
	var lns []net.Listener
	for _, addr := range bindAddrs(bindHost) {
		// Port 0 lets the kernel choose, and it would choose per family. The
		// first choice becomes the port the rest bind, so one --bind cannot
		// answer on two different ports.
		p := port
		if p == 0 && len(lns) > 0 {
			p = listenerPort(lns[0], 0)
		}
		ln, err := bindListener(addr, p)
		if err != nil {
			// A host with IPv6 disabled has no ::1, where the other family is
			// the whole of loopback. Only tolerable once something is bound,
			// or nothing would be listening.
			if len(lns) > 0 && errors.Is(err, syscall.EADDRNOTAVAIL) {
				continue
			}
			closeAll(lns)
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

func bindListener(bindHost string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(port)))
	if err == nil {
		return ln, nil
	}
	if bindHost != "0.0.0.0" && port < 1024 && errors.Is(err, syscall.EACCES) {
		return nil, fmt.Errorf("%w (%s:%d)", errPrivilegedBind, bindHost, port)
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("listen %s:%d: %w\nfind what holds the port with: lsof -nP -i :%d", bindHost, port, err, port)
	}
	return nil, fmt.Errorf("listen %s:%d: %w", bindHost, port, err)
}

var errPrivilegedBind = errors.New("macOS forbids a non-root bind on a privileged port (<1024) against a specific address")

func forwardResolver(selector string) (func() (*Identity, error), error) {
	if selector != "" {
		id, err := resolveExistingIdentity(selector)
		if err != nil {
			return nil, err
		}
		return func() (*Identity, error) { return id, nil }, nil
	}
	return func() (*Identity, error) { return resolveIdentity("") }, nil
}

func forwardAccept(ln net.Listener, resolve func() (*Identity, error), guestPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			if err := forwardConn(conn, resolve, guestPort); err != nil {
				fmt.Fprintf(os.Stderr, "forward: %v\n", err)
			}
		}()
	}
}

func forwardConn(local net.Conn, resolve func() (*Identity, error), guestPort int) error {
	id, err := resolve()
	if err != nil {
		return fmt.Errorf("resolve target instance: %w", err)
	}
	inst, _, err := loadInstance(id.ID)
	if err != nil {
		return fmt.Errorf("%s: %w", forwardTargetGone(id), err)
	}
	guestAddr := net.JoinHostPort(inst.GuestIP, strconv.Itoa(guestPort))
	// Untracked: reaching a forwarded port is not by itself a reason to keep
	// the VM awake, unlike an SSH session.
	guest, err := dialGuest(id.ID, guestAddr, false)
	if err != nil {
		// A dial fails either because the daemon is gone or because nothing
		// listens on the guest port; only the first is the instance's fault,
		// and blaming it for the second would send the user looking in the
		// wrong place.
		if !instanceRunning(id.ID) {
			return fmt.Errorf("%s: %w", forwardTargetGone(id), err)
		}
		return err
	}
	defer guest.Close()
	pipe(local, guest)
	return nil
}

func forwardTargetGone(id *Identity) string {
	return fmt.Sprintf("instance %q is not running, so this connection has nowhere to go; boot it with `sprout up` and the forward reaches it again", id.Display())
}
