package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// The URL open builds is the one the router demultiplexes on: the instance
// label sits next to the domain, a guest port in front of it, the router's
// host port as a suffix.
func TestRoutedURL(t *testing.T) {
	for _, c := range []struct {
		what       string
		prefix     string
		label      string
		domain     string
		guestPort  int
		routerPort int
		want       string
	}{
		{"bare", "", "feat-login", "sprout.localhost", 0, 80, "http://feat-login.sprout.localhost/"},
		{"guest port", "", "feat-login", "sprout.localhost", 3000, 80, "http://3000.feat-login.sprout.localhost/"},
		// Spelling the default ingress would make a second URL for one thing.
		{"guest port 80 is the default", "", "main", "sprout.localhost", 80, 80, "http://main.sprout.localhost/"},
		{"router on another port", "", "main", "sprout.localhost", 0, 8080, "http://main.sprout.localhost:8080/"},
		{"both ports", "", "main", "sprout.localhost", 5173, 8080, "http://5173.main.sprout.localhost:8080/"},
		{"custom domain", "", "main", "vm.test", 0, 80, "http://main.vm.test/"},
		// URLs are compared as strings, so one spelling keeps a bookmark and a
		// printed link identical.
		{"uppercase name", "", "Feat-Login", "sprout.localhost", 0, 80, "http://feat-login.sprout.localhost/"},
		// A guest ingress's own virtual host sits between the port label and
		// the instance: the router reads the port only in the leftmost slot.
		{"host prefix", "login", "feat-login", "sprout.localhost", 0, 80, "http://login.feat-login.sprout.localhost/"},
		{"multi-label host prefix", "admin.dev", "feat-login", "sprout.localhost", 0, 80, "http://admin.dev.feat-login.sprout.localhost/"},
		{"host prefix under a guest port", "admin.dev", "main", "sprout.localhost", 5173, 8080, "http://5173.admin.dev.main.sprout.localhost:8080/"},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := routedURL(c.prefix, c.label, c.domain, c.guestPort, c.routerPort); got != c.want {
				t.Errorf("routedURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRoutedURLWithAHostPrefixStillParsesToTheInstance(t *testing.T) {
	url := routedURL("admin.dev", "feat-login", defaultRouteDomain, 5173, 8080)
	host := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/")
	tgt := parseRouteHost(host, defaultRouteDomain)
	if tgt.kind != routeInstance || tgt.label != "feat-login" || tgt.gport != 5173 {
		t.Errorf("parseRouteHost(%q) = %+v, want instance feat-login on guest 5173", host, tgt)
	}
}

// The leftmost label is the router's guest-port slot, so a numeric prefix
// would quietly become a port instead of reaching the guest. The user meant
// the operand, and the error has to say so.
func TestCleanHostPrefix(t *testing.T) {
	ok := map[string]string{
		"":            "",
		"admin.dev":   "admin.dev",
		"Admin":       "admin",
		".login.":     "login",
		"api-gateway": "api-gateway",
	}
	for in, want := range ok {
		got, err := cleanHostPrefix(in)
		if err != nil || got != want {
			t.Errorf("cleanHostPrefix(%q) = (%q, %v), want (%q, nil)", in, got, err, want)
		}
	}
	for _, in := range []string{"3000", "3000.api", "admin..dev", "admin/dev", "-dev", "admin-.dev", "admin_dev"} {
		if got, err := cleanHostPrefix(in); err == nil {
			t.Errorf("cleanHostPrefix(%q) = %q, want it rejected", in, got)
		}
	}
	if _, err := cleanHostPrefix("3000"); !isUsageError(err) {
		t.Errorf("a bad --host-prefix is not classified as a usage error (would exit 1, want 2)")
	}
}

// A browser sent at a dead port shows a connection-refused page that never
// mentions sprout, so open has to name what is missing and how to supply it.
func TestOpenReportsAMissingRouter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	err = routerReachable(port)
	if err == nil {
		t.Fatal("routerReachable accepted a port with nothing on it")
	}
	if !strings.Contains(err.Error(), "sprout route serve") {
		t.Errorf("error %q does not name the command that starts a router", err)
	}
}

// On :80 the obvious fix does not work: macOS refuses a non-root loopback bind
// there, so "start a router" alone would send the user into a second failure.
func TestMissingRouterOnPort80NamesTheMacOSRestriction(t *testing.T) {
	if conn, err := net.Dial("tcp", "127.0.0.1:80"); err == nil {
		conn.Close()
		t.Skip("something is already serving 127.0.0.1:80")
	}
	err := routerReachable(80)
	if err == nil {
		t.Fatal("routerReachable accepted :80 with nothing on it")
	}
	if !strings.Contains(err.Error(), "--port 8080") {
		t.Errorf("error %q does not offer the unprivileged port that actually works", err)
	}
}

// open's operand is a guest port, not an instance name, so a word that is not
// a port has to say so rather than being silently routed to.
func TestOpenRejectsANonPortOperand(t *testing.T) {
	_, err := runCLI(t, "open", "feature-x")
	if err == nil {
		t.Fatal("`sprout open feature-x` was accepted, want the operand rejected")
	}
	if !isUsageError(err) {
		t.Errorf("error %v is not classified as a usage error (would exit 1, want 2)", err)
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("error %q does not say the operand is a port", err)
	}
}

func TestOpenLaunchesTheInstancesURL(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	newTestInstance(t, root, "aaaa0000bbbb", "feat/login", "var-data")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var opened string
	restore := openBrowser
	openBrowser = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { openBrowser = restore })

	if err := cmdOpen("aaaa0000bbbb", 3000, port, defaultRouteDomain, "", false); err != nil {
		t.Fatalf("open: %v", err)
	}
	want := "http://3000.feat-login.sprout.localhost:" + strconv.Itoa(port) + "/"
	if opened != want {
		t.Errorf("opened %q, want %q — the branch name has to reach the browser sanitized", opened, want)
	}
}
