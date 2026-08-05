package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// routeFlags parses given against the real `route serve` command's flags, so
// these tests cannot drift from the flag set the command actually defines.
func routeFlags(t *testing.T, given []string) *pflag.FlagSet {
	t.Helper()
	flags := newRouteServeCmd().Flags()
	if err := flags.Parse(given); err != nil {
		t.Fatal(err)
	}
	return flags
}

// launchd bound the socket before the router started, so --port and --bind
// cannot be honored. Ignoring them would leave the router answering somewhere
// other than where the command line says.
func TestRouteListenersRejectsBindFlagsWithLaunchdSocket(t *testing.T) {
	for _, given := range [][]string{{"--port", "8080"}, {"--bind", "0.0.0.0"}, {"--port", "8080", "--bind", "0.0.0.0"}} {
		_, _, err := routeListeners(routeFlags(t, given), "Listeners", "127.0.0.1", 80, "sprout.localhost")
		if err == nil {
			t.Fatalf("--launchd-socket with %v was accepted", given)
		}
		for _, name := range given {
			if strings.HasPrefix(name, "--") && !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the rejected flag %s", err, name)
			}
		}
	}
}

// A flag left at its default must not read as a contradiction, or every
// launchd-started router would refuse to start.
func TestFlagsGivenSeesOnlyWhatWasSpelledOut(t *testing.T) {
	got := flagsGiven(routeFlags(t, []string{"--bind", "127.0.0.1"}), "port", "bind")
	if len(got) != 1 || got[0] != "--bind" {
		t.Fatalf("flagsGiven = %v, want only --bind (--port was left at its default)", got)
	}
}

// URLs carry whatever port launchd actually bound, which the router can only
// learn from the socket it was handed.
func TestListenerPortReadsTheBoundSocket(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	want := tcp.Addr().(*net.TCPAddr).Port
	if got := listenerPort(tcp, 80); got != want {
		t.Errorf("listenerPort = %d, want the bound port %d", got, want)
	}

	// A Sockets entry can name a unix socket, which has no port, so the
	// configured value stands in rather than the type assertion panicking.
	dir, err := os.MkdirTemp("/tmp", "sproutport")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	unix, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	if got := listenerPort(unix, 8080); got != 8080 {
		t.Errorf("listenerPort on a unix socket = %d, want the fallback 8080", got)
	}
}

func occupiedPort(t *testing.T, respond func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				respond(conn)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// Through the router's own response writers, so the Server header and status
// the probe keys on come from the code a real router would run.
func routerIndexResponse(conn net.Conn) {
	(&router{domain: defaultRouteDomain, port: 80}).writePage(conn, http.StatusOK, nil, "sprout route", "")
}

func routerForeignHostResponse(conn net.Conn) {
	(&router{domain: "other.localhost", port: 80}).writeError(conn, http.StatusNotFound, "not a sprout route name", "")
}

// What holds the port decides whether a failed bind is a duplicate run or a
// real collision, and only the answering process can say which.
func TestProbeRouterIdentifiesWhatHoldsThePort(t *testing.T) {
	cases := []struct {
		name    string
		respond func(net.Conn)
		want    probeResult
	}{
		{"a router serving this domain", routerIndexResponse, probeRouterSame},
		{"a router serving another domain", routerForeignHostResponse, probeRouterOther},
		{"an HTTP server that is not ours", func(c net.Conn) {
			io.WriteString(c, "HTTP/1.1 200 OK\r\nServer: nginx\r\nContent-Length: 0\r\n\r\n")
		}, probeNotRouter},
		{"something that accepts and says nothing", func(net.Conn) {}, probeNotRouter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := occupiedPort(t, c.respond)
			if got := probeRouter("127.0.0.1", port, defaultRouteDomain); got != c.want {
				t.Errorf("probeRouter met %s and returned %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// The port can fall free between the failed bind and the probe, and a refused
// connection must not read as a router.
func TestProbeRouterOnANoLongerHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if got := probeRouter("127.0.0.1", port, defaultRouteDomain); got != probeNotRouter {
		t.Errorf("probeRouter on a free port = %v, want probeNotRouter", got)
	}
}

// A wildcard is not a destination; everything else is dialed back as written.
func TestProbeAddr(t *testing.T) {
	for _, c := range []struct{ bind, want string }{
		{"0.0.0.0", "127.0.0.1"},
		{"::", "::1"},
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"192.168.1.10", "192.168.1.10"},
	} {
		if got := probeAddr(c.bind); got != c.want {
			t.Errorf("probeAddr(%q) = %q, want %q", c.bind, got, c.want)
		}
	}
}

// Re-running the router is how a wrapper script asks for one to be up. One
// already being up is the state it wanted, not a failure.
func TestRouteListenReportsARouterAlreadyOnThePort(t *testing.T) {
	port := occupiedPort(t, routerIndexResponse)
	_, err := routeListen("127.0.0.1", port, defaultRouteDomain)
	if !errors.Is(err, errRouterAlreadyServing) {
		t.Fatalf("routeListen against a running router = %v, want errRouterAlreadyServing", err)
	}
}

// A router on another domain would 404 the names this one was asked to serve,
// so the collision stands as an error the user has to resolve.
func TestRouteListenRejectsARouterServingAnotherDomain(t *testing.T) {
	port := occupiedPort(t, routerForeignHostResponse)
	_, err := routeListen("127.0.0.1", port, defaultRouteDomain)
	if err == nil || errors.Is(err, errRouterAlreadyServing) {
		t.Fatalf("routeListen against another domain's router = %v, want a plain error", err)
	}
	if !strings.Contains(err.Error(), "--domain") {
		t.Errorf("error %q does not say the domain is what differs", err)
	}
}

func TestRouteListenKeepsTheBindErrorForAStranger(t *testing.T) {
	port := occupiedPort(t, func(c net.Conn) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nServer: nginx\r\nContent-Length: 0\r\n\r\n")
	})
	_, err := routeListen("127.0.0.1", port, defaultRouteDomain)
	if err == nil || errors.Is(err, errRouterAlreadyServing) {
		t.Fatalf("routeListen against a stranger = %v, want the bind error", err)
	}
	if !strings.Contains(err.Error(), "lsof") {
		t.Errorf("error %q no longer names the command that finds the holder", err)
	}
}

// The Host → (label, guest port) mapping: the default guest :80, the
// leading-digit port override, and the case-folding browsers force on us.
func TestParseRouteHost(t *testing.T) {
	const domain = "sprout.localhost"
	cases := []struct {
		host      string
		wantLabel string
		wantPort  int
		wantKind  routeKind
	}{
		{"feat-login.sprout.localhost", "feat-login", 80, routeInstance},
		{"feat-login.sprout.localhost:8080", "feat-login", 80, routeInstance},
		{"5173.feat-login.sprout.localhost", "feat-login", 5173, routeInstance},
		{"Feature-X.sprout.localhost", "feature-x", 80, routeInstance}, // browsers lowercase the host
		{"1234.sprout.localhost", "1234", 80, routeInstance},           // pure-digit *name*, not a port
		// A sanitized name is always one label, so the label next to the
		// domain is the whole instance name and anything left of it belongs
		// to the guest's own ingress.
		{"2024-q3.sprout.localhost", "2024-q3", 80, routeInstance},
		{"3000.feat-login.sprout.localhost", "feat-login", 3000, routeInstance},
		// Guest-side virtual hosts, however deep, resolve on the same label.
		{"admin.feat-login.sprout.localhost", "feat-login", 80, routeInstance},
		{"admin.login.internal.feat-login.sprout.localhost", "feat-login", 80, routeInstance},
		{"5173.api.feat-login.sprout.localhost", "feat-login", 5173, routeInstance}, // port label still leads
		{"api.5173.feat-login.sprout.localhost", "feat-login", 80, routeInstance},   // only the leftmost label is read as a port
		{"sprout.localhost", "", 0, routeIndex},
		{"sprout.localhost.", "", 0, routeIndex}, // trailing FQDN dot
		{"example.com", "", 0, routeForeign},
		{".sprout.localhost", "", 0, routeForeign},     // empty label
		{"foo..sprout.localhost", "", 0, routeForeign}, // empty instance label under a prefix
	}
	for _, c := range cases {
		got := parseRouteHost(c.host, domain)
		if got.kind != c.wantKind || got.label != c.wantLabel || (c.wantKind == routeInstance && got.gport != c.wantPort) {
			t.Errorf("parseRouteHost(%q) = {label:%q port:%d kind:%d}, want {label:%q port:%d kind:%d}",
				c.host, got.label, got.gport, got.kind, c.wantLabel, c.wantPort, c.wantKind)
		}
	}
}

// A trailing " track" opts a DIAL into idle accounting; without it the
// address is dialed without idle accounting.
func TestSplitDialArg(t *testing.T) {
	cases := []struct {
		in        string
		wantAddr  string
		wantTrack bool
	}{
		{"192.168.127.2:80 track", "192.168.127.2:80", true},
		{"192.168.127.2:80", "192.168.127.2:80", false},
		{"ssh", "ssh", false},
		{"", "", false},
	}
	for _, c := range cases {
		addr, track := splitDialArg(c.in)
		if addr != c.wantAddr || track != c.wantTrack {
			t.Errorf("splitDialArg(%q) = (%q, %v), want (%q, %v)", c.in, addr, track, c.wantAddr, c.wantTrack)
		}
	}
}

// TestSniffHost checks the router reads the Host header while preserving the
// exact bytes it consumed, so the guest receives the untouched request.
func TestSniffHost(t *testing.T) {
	req := "GET /app HTTP/1.1\r\nHost: feat-login.sprout.localhost\r\nUser-Agent: x\r\n\r\n"
	host, head, err := sniffHost(newHeadReader(req))
	if err != nil {
		t.Fatal(err)
	}
	if host != "feat-login.sprout.localhost" {
		t.Errorf("host = %q", host)
	}
	if string(head) != req {
		t.Errorf("head = %q, want the whole request head replayed verbatim", head)
	}
}

func TestSniffHostNoHost(t *testing.T) {
	if _, _, err := sniffHost(newHeadReader("GET / HTTP/1.1\r\n\r\n")); !errors.Is(err, errNoHost) {
		t.Errorf("want errNoHost, got %v", err)
	}
}

func TestSniffHostTooLarge(t *testing.T) {
	flood := "GET / HTTP/1.1\r\n" + strings.Repeat("X-Pad: "+strings.Repeat("a", 200)+"\r\n", 100)
	if _, _, err := sniffHost(newHeadReader(flood)); !errors.Is(err, errHeadTooLarge) {
		t.Errorf("want errHeadTooLarge, got %v", err)
	}
}

// A single never-terminated line is bounded by the reader, not accumulated in
// full before the size check runs.
func TestSniffHostUnterminatedBounded(t *testing.T) {
	flood := "GET / HTTP/1.1\r\nHost: " + strings.Repeat("a", maxRouteHead*2)
	if _, _, err := sniffHost(newHeadReader(flood)); !errors.Is(err, errHeadTooLarge) {
		t.Errorf("want errHeadTooLarge, got %v", err)
	}
}

func TestSniffHostBadHost(t *testing.T) {
	cases := map[string]string{
		"duplicate Host":     "GET / HTTP/1.1\r\nHost: a.sprout.localhost\r\nHost: b.sprout.localhost\r\n\r\n",
		"space before colon": "GET / HTTP/1.1\r\nHost : a.sprout.localhost\r\n\r\n",
		"obs-fold Host line": "GET / HTTP/1.1\r\nX-Foo: a\r\n Host: evil.sprout.localhost\r\n\r\n",
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := sniffHost(newHeadReader(req)); !errors.Is(err, errBadHost) {
				t.Errorf("want errBadHost, got %v", err)
			}
		})
	}
}

// An absolute-form target's authority overrides Host at an RFC-compliant
// guest but is invisible to a router keying on Host, so the two would
// disagree on the destination.
func TestSniffHostAbsoluteFormTarget(t *testing.T) {
	req := "GET http://elsewhere.example/ HTTP/1.1\r\nHost: feat-login.sprout.localhost\r\n\r\n"
	if _, _, err := sniffHost(newHeadReader(req)); !errors.Is(err, errBadTarget) {
		t.Errorf("want errBadTarget, got %v", err)
	}
}

// An origin-form target may carry a URL in its query, whose "://" must not
// read as an absolute-form authority.
func TestSniffHostOriginFormWithURLInQuery(t *testing.T) {
	req := "GET /redirect?to=http://x.example/ HTTP/1.1\r\nHost: feat-login.sprout.localhost\r\n\r\n"
	host, _, err := sniffHost(newHeadReader(req))
	if err != nil || host != "feat-login.sprout.localhost" {
		t.Errorf("got (%q, %v), want the request accepted with its Host", host, err)
	}
}

// The same maxRouteHead-sized reader handle() uses, so tests exercise the
// same bounded read.
func newHeadReader(req string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(req), maxRouteHead)
}

func writeRouteInstance(t *testing.T, root, id, name string) {
	t.Helper()
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID:        id,
		Name:      name,
		KeySource: "directory",
	}); err != nil {
		t.Fatal(err)
	}
}

// Every label handed out has to resolve back through this same router to the
// instance it came from, which is what a guest building an OIDC redirect URI
// depends on.
func TestRouteLabelForRoundTripsThroughTheRouter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	writeRouteInstance(t, root, "aaaa1111bbbb", "feat/login")
	writeRouteInstance(t, root, "99990000aaaa", "2024.q3")
	// Two clones on one branch: neither owns the name, so both are labelled by
	// ID, which a guest re-implementing sanitizeName cannot get right.
	writeRouteInstance(t, root, "cccc2222dddd", "main")
	writeRouteInstance(t, root, "eeee3333ffff", "main")

	for _, c := range []struct{ id, name, want string }{
		{"aaaa1111bbbb", "feat/login", "feat-login"},
		{"99990000aaaa", "2024.q3", "2024-q3"},
		{"cccc2222dddd", "main", "cccc2222dddd"},
		{"eeee3333ffff", "main", "eeee3333ffff"},
	} {
		t.Run(c.name+"/"+c.id, func(t *testing.T) {
			label, shared := routeLabelFor(c.id, c.name)
			if label != c.want {
				t.Fatalf("routeLabelFor(%q, %q) = %q, want %q", c.id, c.name, label, c.want)
			}
			if got, err := resolveRouteLabel(label); err != nil || got != c.id {
				t.Fatalf("the router resolves %q to (%q, %v), want %q", label, got, err, c.id)
			}
			if c.want == c.id && len(shared) != 2 {
				t.Errorf("shared = %v, want the two instances answering to %q", shared, c.name)
			}
		})
	}
}

// Name-match resolution is covered by the round-trip test above; here only the
// cases with no label to round-trip from: prefixes, misses, and collisions.
func TestResolveRouteLabel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	writeRouteInstance(t, root, "aaaa1111bbbb", "feat/login")
	writeRouteInstance(t, root, "cccc2222dddd", "main")
	writeRouteInstance(t, root, "eeee3333ffff", "main")

	t.Run("id prefix wins", func(t *testing.T) {
		id, err := resolveRouteLabel("aaaa1111")
		if err != nil || id != "aaaa1111bbbb" {
			t.Fatalf("got (%q, %v), want aaaa1111bbbb", id, err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if _, err := resolveRouteLabel("nope"); !errors.Is(err, errRouteNotFound) {
			t.Fatalf("want errRouteNotFound, got %v", err)
		}
	})
	t.Run("ambiguous name", func(t *testing.T) {
		_, err := resolveRouteLabel("main")
		var amb *routeAmbiguousError
		if !errors.As(err, &amb) || len(amb.ids) != 2 {
			t.Fatalf("want routeAmbiguousError with 2 ids, got %v", err)
		}
	})
}
