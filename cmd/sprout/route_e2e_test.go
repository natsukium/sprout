package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Stands in for a running instance's control socket, so the router's full
// handle→bridge→splice path runs without a VM. Every DIAL goes to one backend
// whatever address it named, letting a test point guest ":80" at a random port.
type fakeDaemon struct {
	backend string // host:port every DIAL is bridged to
	booting bool   // answer INFO with ready:false, as a daemon whose guest is still booting does
	pid     int    // reported in INFO; readiness waits compare it against a supersededPID
	// uptimeSec ages the daemon in INFO, which is what the router's start
	// grace reads.
	uptimeSec int
	// vanishAfterInfo stands in for a stop landing between a router's
	// readiness check and the dial that follows it.
	vanishAfterInfo bool
	// dropFirstDial hangs up on the first DIAL without a status line — the
	// transient failure class a daemon handover produces mid-request.
	dropFirstDial atomic.Bool
	// dialErrReply, when set, answers every DIAL with this ERR reason — a
	// daemon whose guest-side dial failed some way other than a refusal.
	dialErrReply string
	ln           net.Listener
	sawTrack     chan bool
	sawInfoArg   chan string
}

func (d *fakeDaemon) serve(t *testing.T, sockPath string) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	d.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handle(conn)
		}
	}()
}

func (d *fakeDaemon) handle(conn net.Conn) {
	defer conn.Close()
	line, err := readControlLine(conn)
	if err != nil {
		return
	}
	cmd, arg := parseControlLine(line)
	switch cmd {
	case "PING":
		fmt.Fprint(conn, "OK\n")
	case "INFO":
		select {
		case d.sawInfoArg <- arg:
		default:
		}
		fmt.Fprintf(conn, "OK {\"ready\":%t,\"name\":\"webapp\",\"guestIp\":\"127.0.0.1\",\"pid\":%d,\"uptimeSec\":%d}\n", !d.booting, d.pid, d.uptimeSec)
		// Close unlinks the socket, so the next dial fails as it would
		// against a daemon that has exited.
		if d.vanishAfterInfo {
			d.ln.Close()
		}
	case "DIAL":
		_, track := splitDialArg(arg)
		select {
		case d.sawTrack <- track:
		default:
		}
		if d.dropFirstDial.CompareAndSwap(true, false) {
			return
		}
		if d.dialErrReply != "" {
			fmt.Fprintf(conn, "ERR %s\n", d.dialErrReply)
			return
		}
		guest, err := net.Dial("tcp", d.backend)
		if err != nil {
			fmt.Fprintf(conn, "ERR %v\n", err)
			return
		}
		defer guest.Close()
		fmt.Fprint(conn, "OK\n")
		pipe(conn, guest)
	}
}

// Reads one line without buffering past it, so what the router sends after
// the DIAL stays on the socket instead of being swallowed by a bufio.Reader.
func readControlLine(conn net.Conn) (string, error) {
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			b = append(b, buf[0])
			if buf[0] == '\n' {
				return string(b), nil
			}
		}
		if err != nil {
			return string(b), err
		}
	}
}

// Echoes the Host it received, so a test can prove the header arrived
// untouched. Returns host:port.
func startEchoBackend(t *testing.T) string {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend saw Host=%s path=%s", r.Host, r.URL.Path)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); ln.Close() })
	go srv.Serve(ln)
	return ln.Addr().String()
}

// Seeds an instance long past its boot, which is what most tests mean by
// "running"; a boot-fresh one is built by hand (see the start grace).
//
// root must be short: the control socket lives under it (see shortStateRoot).
func seedInstance(t *testing.T, root, id, name, backend string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{backend: backend, uptimeSec: 3600, sawTrack: make(chan bool, 1), sawInfoArg: make(chan string, 1)}
	seedInstanceWith(t, root, id, name, d)
	return d
}

func seedInstanceWith(t *testing.T, root, id, name string, d *fakeDaemon) {
	t.Helper()
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Bundle must name a path that exists: the router stats it to tell an
	// instance it can start from one whose build is gone.
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: name, KeySource: "directory", GuestIP: "127.0.0.1", Bundle: dir,
	}); err != nil {
		t.Fatal(err)
	}
	d.serve(t, filepath.Join(dir, "control.sock"))
}

// Under /tmp, not t.TempDir(): macOS caps unix socket paths near 104 bytes,
// which /var/folders blows past once the instance dir is appended.
func shortStateRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "sproutrt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("XDG_STATE_HOME", root)
	return root
}

func startRouter(t *testing.T, r *router) string {
	t.Helper()
	lns, err := listenHostPort("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAll(lns) })
	ln := lns[0]
	r.port = ln.Addr().(*net.TCPAddr).Port
	go r.serve(ln)
	return ln.Addr().String()
}

// A request reaches the backend with its original Host and path, and the
// bridge marks the connection as idle-tracked activity.
func TestRouteEndToEnd(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	daemon := seedInstance(t, root, "abcd1234ef56", "webapp", backend)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	host := "webapp.sprout.localhost"
	body := httpGet(t, addr, host)
	if want := "backend saw Host=" + host + " path=/"; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	select {
	case track := <-daemon.sawTrack:
		if !track {
			t.Error("route DIAL was not marked as tracked activity")
		}
	case <-time.After(time.Second):
		t.Error("daemon never saw a DIAL")
	}
}

// The readiness check runs once per request, so it must ask for the form of
// INFO the daemon answers without forking `ps` and `footprint`.
func TestRouteReadinessCheckDoesNotAskForTheResourceSample(t *testing.T) {
	root := shortStateRoot(t)
	daemon := seedInstance(t, root, "abcd1234ef56", "webapp", startEchoBackend(t))

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)
	httpGet(t, addr, "webapp.sprout.localhost")

	select {
	case arg := <-daemon.sawInfoArg:
		if arg != "brief" {
			t.Errorf("router asked INFO %q, want the sample-free \"brief\" form", arg)
		}
	case <-time.After(time.Second):
		t.Error("daemon never saw an INFO")
	}
}

// Serving only the first listener would leave a socket accepted-but-never-read,
// which looks like a hang to a client.
func TestRouteServesEveryInheritedListener(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", backend)

	r := &router{domain: "sprout.localhost", port: 80, wake: true, waking: map[string]bool{}}

	var addrs []string
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go r.serve(ln)
		addrs = append(addrs, ln.Addr().String())
	}

	const host = "webapp.sprout.localhost"
	want := "backend saw Host=" + host + " path=/"
	for _, addr := range addrs {
		if body := httpGet(t, addr, host); body != want {
			t.Errorf("listener %s returned %q, want %q", addr, body, want)
		}
	}
}

// A POST whose body arrives in the same write as the head must reach the
// backend whole: those bytes sit in the router's bufio.Reader, so a splice
// from the raw conn would drop them.
func TestRoutePostBodyReplay(t *testing.T) {
	root := shortStateRoot(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got body=%s", body)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); ln.Close() })
	go srv.Serve(ln)
	seedInstance(t, root, "abcd1234ef56", "webapp", ln.Addr().String())

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// One write carrying head *and* body, so the body is buffered past the head
	// during the sniff — exactly the case the replay-from-reader guards.
	const payload = "hello-body"
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(payload), payload)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw, _ := io.ReadAll(conn)
	if _, body, _ := strings.Cut(string(raw), "\r\n\r\n"); body != "got body="+payload {
		t.Errorf("body = %q, want the POST body relayed intact", body)
	}
}

// A name two instances share is a 409 naming the IDs, not a 500 or a silent
// pick.
func TestRouteAmbiguousNameReturns409(t *testing.T) {
	root := shortStateRoot(t)
	seedInstance(t, root, "1111aaaa2222", "main", "127.0.0.1:1")
	seedInstance(t, root, "3333bbbb4444", "main", "127.0.0.1:1")

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)
	if status := httpStatus(t, addr, "main.sprout.localhost"); status != "409" {
		t.Errorf("status = %s, want 409", status)
	}
}

// Two instances running side by side are each reachable by their own name,
// with the request landing in the right one.
func TestRouteMultiInstance(t *testing.T) {
	root := shortStateRoot(t)
	backendMain := startEchoBackend(t)
	backendFeat := startEchoBackend(t)
	seedInstance(t, root, "1111aaaa2222", "main", backendMain)
	seedInstance(t, root, "3333bbbb4444", "feat/login", backendFeat)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	if body := httpGet(t, addr, "main.sprout.localhost"); !strings.Contains(body, "Host=main.sprout.localhost") {
		t.Errorf("main routed wrong: %q", body)
	}
	// "feat/login" sanitizes to "feat-login"; the browser sends the sanitized,
	// lowercased form.
	if body := httpGet(t, addr, "feat-login.sprout.localhost"); !strings.Contains(body, "Host=feat-login.sprout.localhost") {
		t.Errorf("feat-login routed wrong: %q", body)
	}
}

// A prefixed Host resolves by the label next to the domain and reaches the
// guest untouched, so an in-guest ingress can demux on the prefix.
func TestRouteVirtualHostPrefix(t *testing.T) {
	root := shortStateRoot(t)
	backendMain := startEchoBackend(t)
	backendFeat := startEchoBackend(t)
	seedInstance(t, root, "1111aaaa2222", "main", backendMain)
	seedInstance(t, root, "3333bbbb4444", "feat/login", backendFeat)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	host := "admin.feat-login.sprout.localhost"
	if body := httpGet(t, addr, host); !strings.Contains(body, "Host="+host) {
		t.Errorf("prefixed host routed wrong or was rewritten: %q", body)
	}
}

// A Host outside the route domain is a plain 404, never dialed anywhere.
func TestRouteForeignHostReturns404(t *testing.T) {
	shortStateRoot(t)
	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)
	if status := httpStatus(t, addr, "admin.dev.example.localhost"); status != "404" {
		t.Errorf("status = %s, want 404", status)
	}
}

// A name with no instance gets a 404, not a hang or a dial error.
func TestRouteUnknownNameReturns404(t *testing.T) {
	shortStateRoot(t)
	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)
	if status := httpStatus(t, addr, "ghost.sprout.localhost"); status != "404" {
		t.Errorf("status = %s, want 404", status)
	}
}

// A ready instance whose guest port refuses the dial gets a clean 502 page,
// not an empty reply: nothing has reached the client yet.
func TestRouteGuestDownReturns502(t *testing.T) {
	root := shortStateRoot(t)
	dead := deadBackend(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", dead)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)
	if status := httpStatus(t, addr, "webapp.sprout.localhost"); status != "502" {
		t.Errorf("status = %s, want 502", status)
	}
}

// A dev server bound to the guest's 127.0.0.1 refuses the router exactly as a
// closed port does, so the 502 has to name that trap next to the wrong-port one.
func TestRouteGuestDownHintNamesTheLoopbackTrap(t *testing.T) {
	root := shortStateRoot(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", deadBackend(t))

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	_, _, body := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	for _, want := range []string{"nothing answered on guest port 80", "0.0.0.0", "127.0.0.1", "sprout exec -i webapp"} {
		if !strings.Contains(body, want) {
			t.Errorf("502 page does not mention %q:\n%s", want, body)
		}
	}
}

// Readiness is SSH plus sprout-ready.target, which an ordinary guest reaches
// before its dev server binds, so a 502 at that moment is a dead end seconds
// before the port would have answered.
func TestRouteFreshBootReloadsUntilTheGuestPortListens(t *testing.T) {
	root := shortStateRoot(t)
	seedInstanceWith(t, root, "abcd1234ef56", "webapp",
		&fakeDaemon{backend: deadBackend(t), uptimeSec: 20, sawTrack: make(chan bool, 1)})

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, head, body := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "503" {
		t.Errorf("status = %s, want 503; body:\n%s", status, body)
	}
	if !strings.Contains(head, "Refresh: 2") {
		t.Errorf("response head lacks the Refresh header that carries the client over the gap:\n%s", head)
	}
	// A wrong port looks the same as a boot that is merely slow, so the page
	// that waits still has to say what to check.
	for _, want := range []string{"nothing is listening on guest port 80 yet", "0.0.0.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q:\n%s", want, body)
		}
	}
}

// Past the grace the reloading has to stop: no page should promise a port
// nothing will ever listen on.
func TestRouteLongRunningInstanceStopsWaitingForTheGuestPort(t *testing.T) {
	root := shortStateRoot(t)
	seedInstanceWith(t, root, "abcd1234ef56", "webapp",
		&fakeDaemon{backend: deadBackend(t), uptimeSec: int(routeStartGrace.Seconds()) + 1, sawTrack: make(chan bool, 1)})

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, head, _ := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "502" {
		t.Errorf("status = %s, want 502", status)
	}
	if strings.Contains(head, "Refresh:") {
		t.Errorf("502 still reloads:\n%s", head)
	}
}

// A stop landing between the readiness check and the dial is not the guest
// port being closed: the answer is the waking page, which recovers by itself.
// A transient control failure — the daemon handing over during an in-place
// `sprout up` — while the instance provably still answers INFO deserves one
// more dial, not an error page a manual reload would have cleared.
func TestRouteRetriesOnceAfterATransientControlFailure(t *testing.T) {
	root := shortStateRoot(t)
	d := &fakeDaemon{backend: startEchoBackend(t), sawTrack: make(chan bool, 1)}
	d.dropFirstDial.Store(true)
	seedInstanceWith(t, root, "abcd1234ef56", "webapp", d)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	if body := httpGet(t, addr, "webapp.sprout.localhost"); !strings.Contains(body, "backend saw") {
		t.Errorf("request did not reach the backend after the retry: %q", body)
	}
}

// The daemon answers ERR for every way its dial can fail; only a refusal
// proves a closed port. A timeout means the guest never answered at all, and
// the closed-port page would misdirect debugging into dev-server config.
func TestRouteGuestDialTimeoutIsNotBlamedOnThePort(t *testing.T) {
	root := shortStateRoot(t)
	seedInstanceWith(t, root, "abcd1234ef56", "webapp",
		&fakeDaemon{dialErrReply: "i/o timeout", sawTrack: make(chan bool, 1)})

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, _, body := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "502" {
		t.Errorf("status = %s, want 502", status)
	}
	if strings.Contains(body, "nothing answered on guest port") {
		t.Errorf("timeout page blames the guest port:\n%s", body)
	}
	if !strings.Contains(body, "i/o timeout") {
		t.Errorf("page does not carry the daemon's reason:\n%s", body)
	}
}

func TestRouteDaemonGoneMidRequestWakesInsteadOf502(t *testing.T) {
	root := shortStateRoot(t)
	seedInstanceWith(t, root, "abcd1234ef56", "webapp",
		&fakeDaemon{backend: startEchoBackend(t), vanishAfterInfo: true, sawTrack: make(chan bool, 1)})

	woken := make(chan string, 1)
	restore := wakeInstance
	wakeInstance = func(id string) error { woken <- id; return nil }
	t.Cleanup(func() { wakeInstance = restore })

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, head, body := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "503" {
		t.Errorf("status = %s, want 503; body:\n%s", status, body)
	}
	if !strings.Contains(head, "Refresh: 2") {
		t.Errorf("response head lacks the Refresh header that brings the client back:\n%s", head)
	}
	if !strings.Contains(body, "Waking") {
		t.Errorf("body is not the waking interstitial: %q", body)
	}
	select {
	case id := <-woken:
		if id != "abcd1234ef56" {
			t.Errorf("woke %q, want the instance that went away", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("no wake was started for the instance that went away")
	}
}

// The log carries the daemon's own reason, which is what separates a closed
// guest port from a control socket that never answered.
func TestRouteVerboseLogsWhyTheGuestDialFailed(t *testing.T) {
	root := shortStateRoot(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", deadBackend(t))

	r := &router{domain: "sprout.localhost", wake: true, verbose: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	logged := captureStderr(t, func() {
		if status := httpStatus(t, addr, "webapp.sprout.localhost"); status != "502" {
			t.Fatalf("status = %s, want 502", status)
		}
	})
	for _, want := range []string{"guest:80", "connection refused"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line does not mention %q:\n%s", want, logged)
		}
	}
}

// While the daemon is up but the guest is still booting, a request gets a 503
// carrying Refresh (browser reload, no script) and Retry-After.
func TestRouteBootingInstanceServesWakingInterstitial(t *testing.T) {
	root := shortStateRoot(t)
	d := &fakeDaemon{booting: true, sawTrack: make(chan bool, 1)}
	seedInstanceWith(t, root, "abcd1234ef56", "webapp", d)

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, head, body := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "503" {
		t.Errorf("status = %s, want 503", status)
	}
	if !strings.Contains(head, "Refresh: 2") {
		t.Errorf("response head lacks the Refresh header that drives the reload:\n%s", head)
	}
	if !strings.Contains(head, "Retry-After: 2") {
		t.Errorf("response head lacks Retry-After for non-browser clients:\n%s", head)
	}
	if !strings.Contains(body, "Waking") {
		t.Errorf("body is not the waking interstitial: %q", body)
	}
}

// Each smuggling shape gets a clean 400 and no dial, where TestSniffHost*
// covers only the pure sniff. No instance is seeded, so a dial would show up
// as a different status.
func TestRouteRejectsSmugglingShapesWith400(t *testing.T) {
	shortStateRoot(t)
	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	cases := map[string]string{
		"absolute-form target": "GET http://elsewhere.example/ HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n",
		"obs-fold Host line":   "GET / HTTP/1.1\r\nX-Foo: a\r\n Host: evil.sprout.localhost\r\nConnection: close\r\n\r\n",
		"duplicate Host":       "GET / HTTP/1.1\r\nHost: a.sprout.localhost\r\nHost: b.sprout.localhost\r\nConnection: close\r\n\r\n",
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if status, _, _ := sendRaw(t, addr, req); status != "400" {
				t.Errorf("status = %s, want 400", status)
			}
		})
	}
}

// A guest ingress answers 404 for a Host its own rules do not match, which by
// status alone is indistinguishable from the router's 404. Only pages the
// router wrote carry the Server header.
func TestRouteMarksItsOwnResponsesButNotTheGuests(t *testing.T) {
	root := shortStateRoot(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such vhost", http.StatusNotFound)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); ln.Close() })
	go srv.Serve(ln)
	seedInstance(t, root, "abcd1234ef56", "webapp", ln.Addr().String())

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	status, head, _ := sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: ghost.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "404" || !strings.Contains(head, "Server: "+routeServerHeader) {
		t.Errorf("the router's own 404 is unmarked, so it cannot be told from the guest's:\n%s", head)
	}

	status, head, _ = sendRaw(t, addr, "GET / HTTP/1.1\r\nHost: webapp.sprout.localhost\r\nConnection: close\r\n\r\n")
	if status != "404" {
		t.Fatalf("status = %s, want the backend's 404", status)
	}
	if strings.Contains(head, "Server: "+routeServerHeader) {
		t.Errorf("a 404 the guest produced is marked as the router's:\n%s", head)
	}
}

// --verbose names the instance and guest port a request was bridged to, which
// the response itself cannot say.
func TestRouteVerboseLogsTheResolvedTarget(t *testing.T) {
	root := shortStateRoot(t)
	backend := startEchoBackend(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", backend)

	r := &router{domain: "sprout.localhost", wake: true, verbose: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	logged := captureStderr(t, func() {
		if body := httpGet(t, addr, "admin.webapp.sprout.localhost"); !strings.Contains(body, "backend saw") {
			t.Fatalf("request did not reach the backend: %q", body)
		}
		// The request is answered before bridge logs, so the line needs a
		// moment to land before stderr is restored.
		time.Sleep(100 * time.Millisecond)
	})

	for _, want := range []string{"admin.webapp.sprout.localhost", "GET / HTTP/1.1", "webapp", "abcd1234ef56", "guest:80", "bridged"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line does not mention %q:\n%s", want, logged)
		}
	}
}

func TestRouteIsQuietWithoutVerbose(t *testing.T) {
	root := shortStateRoot(t)
	seedInstance(t, root, "abcd1234ef56", "webapp", startEchoBackend(t))

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
	addr := startRouter(t, r)

	logged := captureStderr(t, func() {
		httpGet(t, addr, "webapp.sprout.localhost")
		httpStatus(t, addr, "ghost.sprout.localhost")
		time.Sleep(100 * time.Millisecond)
	})
	if logged != "" {
		t.Errorf("router logged without --verbose:\n%s", logged)
	}
}

// Drained concurrently, so a log larger than the pipe buffer cannot deadlock
// the goroutine writing it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		done <- string(data)
	}()
	orig := os.Stderr
	os.Stderr = pw
	fn()
	os.Stderr = orig
	pw.Close()
	return <-done
}

func deadBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// A raw connection rather than http.Client, so the test can set an arbitrary
// Host the resolver would otherwise rewrite.
func httpGet(t *testing.T, addr, host string) string {
	t.Helper()
	_, body := rawRequest(t, addr, host)
	return body
}

func httpStatus(t *testing.T, addr, host string) string {
	t.Helper()
	status, _ := rawRequest(t, addr, host)
	return status
}

func rawRequest(t *testing.T, addr, host string) (status, body string) {
	t.Helper()
	status, _, body = sendRaw(t, addr, fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host))
	return status, body
}

// Writes req verbatim, so a test can send shapes http.Client refuses to
// produce (absolute-form targets, obs-fold headers).
func sendRaw(t *testing.T, addr, req string) (status, head, body string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, req)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ = strings.Cut(string(raw), "\r\n\r\n")
	if fields := strings.Fields(head); len(fields) >= 2 {
		status = fields[1]
	}
	return status, head, body
}
