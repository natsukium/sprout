package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newOpenCmd() *cobra.Command {
	var (
		routerPort int
		domain     string
		hostPrefix string
		printOnly  bool
	)
	cmd := &cobra.Command{
		Use:     "open [GUESTPORT]",
		Short:   "Open this environment's routed URL in your browser",
		GroupID: groupNetwork,
		Long: `Open http://<name>.sprout.localhost/ for the selected environment.

GUESTPORT reaches a port other than the guest's :80 — a dev server on 3000,
say — using the router's port-label form. --host-prefix puts labels in front
of the instance name for a guest that routes by hostname itself, so
--host-prefix admin.dev opens http://admin.dev.<name>.sprout.localhost/. --print
writes the URL instead of opening it, for piping into curl or a script.

The router has to be running: ` + "`sprout route serve`" + `, or the launchd job in
docs/how-to/run-as-daemon.md. --port must match the port it serves on.`,
		Args:              usageArgs(cobra.MaximumNArgs(1)),
		ValidArgsFunction: completeNothing,
	}
	selector := addInstanceFlag(cmd)
	// No backticks in flag usage: pflag reads a backticked word as the value
	// placeholder, so "`sprout route serve --port`" would rename the argument.
	cmd.Flags().IntVar(&routerPort, "port", 80, "host port the router serves on (the one given to route serve)")
	cmd.Flags().StringVar(&domain, "domain", defaultRouteDomain, "hostname suffix the router answers for")
	cmd.Flags().StringVar(&hostPrefix, "host-prefix", "", "hostname labels to put in front of the instance name, for a guest that routes by host itself (admin.dev)")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the URL instead of opening it")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		guestPort := 0
		if len(args) == 1 {
			p, err := parsePort(args[0])
			if err != nil {
				return usagef("%q is not a port; open takes the guest port to reach, as in `sprout open 3000`", args[0])
			}
			guestPort = p
		}
		return cmdOpen(*selector, guestPort, routerPort, domain, hostPrefix, printOnly)
	}
	return cmd
}

// A variable so tests can assert what URL was built without a browser window
// appearing on whoever runs them.
var openBrowser = func(url string) error {
	return exec.Command("open", url).Run()
}

func cmdOpen(selector string, guestPort, routerPort int, domain, hostPrefix string, printOnly bool) error {
	dom, err := cleanDomain(domain)
	if err != nil {
		return err
	}
	prefix, err := cleanHostPrefix(hostPrefix)
	if err != nil {
		return err
	}
	id, err := resolveExistingIdentity(selector)
	if err != nil {
		return err
	}
	label, shared := routeLabelFor(id.ID, id.Display())
	if len(shared) > 1 {
		fmt.Fprintf(os.Stderr, "warning: %d instances answer to the route label for %q, so this URL addresses this one by ID\n", len(shared), id.Display())
	}
	url := routedURL(prefix, label, dom, guestPort, routerPort)

	// Checked before opening: a browser pointed at a dead port shows its own
	// connection-refused page, saying nothing about sprout.
	if err := routerReachable(routerPort); err != nil {
		return err
	}
	if printOnly {
		fmt.Println(url)
		return nil
	}
	if err := openBrowser(url); err != nil {
		return fmt.Errorf("could not open %s: %w", url, err)
	}
	fmt.Println(url)
	return nil
}

// The guest-port label stays leftmost because that is the only position
// parseRouteHost reads it in, and :80 is left off because it is the router's
// default ingress.
func routedURL(prefix, label, domain string, guestPort, routerPort int) string {
	host := strings.ToLower(label) + "." + domain
	if prefix != "" {
		host = prefix + "." + host
	}
	if guestPort != 0 && guestPort != 80 {
		host = strconv.Itoa(guestPort) + "." + host
	}
	if routerPort != 80 {
		host = net.JoinHostPort(host, strconv.Itoa(routerPort))
	}
	return "http://" + host + "/"
}

func cleanHostPrefix(prefix string) (string, error) {
	prefix = strings.ToLower(strings.Trim(prefix, "."))
	if prefix == "" {
		return "", nil
	}
	labels := strings.Split(prefix, ".")
	for _, label := range labels {
		if label == "" {
			return "", usagef("--host-prefix %q has an empty label; write it as the hostname labels in front of the instance, like `admin.dev`", prefix)
		}
		if strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", usagef("--host-prefix %q is not a hostname: each label may hold letters, digits, and inner hyphens", prefix)
		}
	}
	if isAllDigits(labels[0]) {
		return "", usagef("--host-prefix %q starts with a number, which the router reads as a guest port; pass a port as the operand instead, as in `sprout open %s`", prefix, labels[0])
	}
	return prefix, nil
}

// routerReachable probes loopback rather than the routed hostname: the router
// binds a host port, and *.localhost resolution is a browser and resolver
// convention this check has no reason to depend on.
func routerReachable(routerPort int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(routerPort))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		return nil
	}
	msg := fmt.Sprintf("no router is listening on %s; start one with: sprout route serve", addr)
	if routerPort != 80 {
		msg += fmt.Sprintf(" --port %d", routerPort)
	} else {
		msg += "\nmacOS refuses a non-root bind of :80, so that needs either `--port 8080` (and `sprout open --port 8080`) or the launchd job in docs/how-to/run-as-daemon.md"
	}
	return fmt.Errorf("%s", msg)
}
