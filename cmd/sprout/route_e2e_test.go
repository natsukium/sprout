package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stands in for a running instance's control socket, so the router's full
// handle→bridge→splice path runs without a VM. Every DIAL goes to one backend
// whatever address it named, letting a test point guest ":80" at a random port.
type fakeDaemon struct {
	backend  string // host:port every DIAL is bridged to
	booting  bool   // answer INFO with ready:false, as a daemon whose guest is still booting does
	pid      int    // reported in INFO; readiness waits compare it against a supersededPID
	sawTrack chan bool
}

func (d *fakeDaemon) serve(t *testing.T, sockPath string) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
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
		fmt.Fprintf(conn, "OK {\"ready\":%t,\"name\":\"webapp\",\"guestIp\":\"127.0.0.1\",\"pid\":%d}\n", !d.booting, d.pid)
	case "DIAL":
		_, track := splitDialArg(arg)
		select {
		case d.sawTrack <- track:
		default:
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

// root must be short: the control socket lives under it (see shortStateRoot).
func seedInstance(t *testing.T, root, id, name, backend string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{backend: backend, sawTrack: make(chan bool, 1)}
	seedInstanceWith(t, root, id, name, d)
	return d
}

func seedInstanceWith(t *testing.T, root, id, name string, d *fakeDaemon) {
	t.Helper()
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: name, KeySource: "directory", GuestIP: "127.0.0.1",
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
