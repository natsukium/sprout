package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParsePortSpec(t *testing.T) {
	ok := []struct {
		in   string
		want portForward
	}{
		{"8080", portForward{8080, 8080}},
		{"80:5432", portForward{80, 5432}},
		{"1:65535", portForward{1, 65535}},
	}
	for _, c := range ok {
		got, err := parsePortSpec(c.in)
		if err != nil || got != c.want {
			t.Fatalf("parsePortSpec(%q) = (%v, %v), want (%v, nil)", c.in, got, err, c.want)
		}
	}

	bad := []string{"", "0", "-1", "70000", "abc", "80:", ":80", "80:abc", "80:0"}
	for _, s := range bad {
		if _, err := parsePortSpec(s); err == nil {
			t.Fatalf("parsePortSpec(%q) = nil error, want failure", s)
		}
	}
}

// A forward binds loopback unless --bind names something else. Asserted on
// IsLoopback/IsUnspecified rather than a literal IP, since the kernel may
// report a wildcard bind as either 0.0.0.0 or [::].
func TestListenHostPortBindScope(t *testing.T) {
	cases := []struct {
		bind         string
		wantWildcard bool
	}{
		{bind: defaultBindAddress, wantWildcard: false},
		{bind: "127.0.0.1", wantWildcard: false},
		{bind: "0.0.0.0", wantWildcard: true},
	}
	for _, c := range cases {
		lns, err := listenHostPort(c.bind, 0)
		if err != nil {
			t.Fatalf("listenHostPort(%q, 0) failed: %v", c.bind, err)
		}
		for _, ln := range lns {
			addr := ln.Addr().(*net.TCPAddr)
			if c.wantWildcard {
				if !addr.IP.IsUnspecified() {
					t.Errorf("--bind %s bound %s, want a wildcard (all-interfaces) address", c.bind, addr.IP)
				}
			} else {
				if !addr.IP.IsLoopback() {
					t.Errorf("--bind %s bound %s, want a loopback address", c.bind, addr.IP)
				}
			}
			ln.Close()
		}
	}
}

// A browser may resolve `localhost` to ::1 and take the refusal there as the
// answer, so the default binds both families on one port.
func TestDefaultBindCoversBothLoopbackFamilies(t *testing.T) {
	lns, err := listenHostPort(defaultBindAddress, 0)
	if err != nil {
		t.Fatalf("listenHostPort(%q, 0) failed: %v", defaultBindAddress, err)
	}
	defer closeAll(lns)

	port := 0
	families := map[bool]bool{}
	for _, ln := range lns {
		addr := ln.Addr().(*net.TCPAddr)
		if port == 0 {
			port = addr.Port
		}
		if addr.Port != port {
			t.Errorf("one --bind %s bound two ports (%d and %d); a URL can only carry one", defaultBindAddress, port, addr.Port)
		}
		families[addr.IP.To4() != nil] = true
	}
	if !families[true] {
		t.Error("nothing bound 127.0.0.1")
	}
	// IPv6 loopback is skipped rather than fatal on a host without it
	// (bindListeners' EADDRNOTAVAIL case), so its absence is only worth
	// reporting when the host itself can bind ::1.
	if !families[false] {
		probe, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skip("this host has no IPv6 loopback to bind")
		}
		probe.Close()
		t.Error("nothing bound ::1, so a browser reaching for it gets a refusal")
	}
}

// The exposure warning fires on anything that is not loopback, and the
// default must not trip it — a warning printed on every plain `forward` is a
// warning no one reads.
func TestLoopbackBindAcceptsTheDefault(t *testing.T) {
	for _, addr := range []string{defaultBindAddress, "localhost", "LocalHost", "127.0.0.1", "::1"} {
		if !loopbackBind(addr) {
			t.Errorf("loopbackBind(%q) = false, want it treated as loopback", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0", "192.168.1.10", "example.test", ""} {
		if loopbackBind(addr) {
			t.Errorf("loopbackBind(%q) = true, want the exposure warning", addr)
		}
	}
}

// Stands in for the daemon's control socket, echoing the guest side so the
// bridge runs end to end without a VM. net.Pipe has no half-close, so the
// returned closer stands in for the daemon's own teardown; a caller wanting
// bridgeThroughDial to return has to invoke it.
func fakeDialServer(t *testing.T, wantAddr, reply string) (net.Conn, func()) {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			return
		}
		if line != "DIAL "+wantAddr+"\n" {
			t.Errorf("server got %q, want DIAL %s", line, wantAddr)
		}
		if _, err := fmt.Fprint(server, reply); err != nil {
			return
		}
		if reply == "OK\n" {
			buf := make([]byte, 256)
			for {
				n, err := server.Read(buf)
				if n > 0 {
					server.Write(buf[:n]) //nolint:errcheck
				}
				if err != nil {
					return
				}
			}
		}
	}()
	return client, func() { server.Close() }
}

// Bytes written by the host client must come back from the guest end, proving
// the stream reads through the handshake's buffered reader rather than the raw
// control connection.
func TestGuestStreamRoundTrip(t *testing.T) {
	ctl, closeServer := fakeDialServer(t, "192.168.127.2:80", "OK\n")
	defer closeServer()
	local, app := net.Pipe()

	guest, err := guestStream(ctl, "192.168.127.2:80", false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { pipe(local, guest); close(done) }()

	go func() { fmt.Fprint(app, "ping") }() //nolint:errcheck
	_ = app.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	if _, err := app.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("round trip = %q, want %q", buf, "ping")
	}

	// pipe waits for both directions, so it returns only once both ends are
	// closed. net.Pipe has no real CloseWrite, so closing both stands in for
	// the daemon and the client hanging up.
	app.Close()
	closeServer()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe did not return after both sides closed")
	}
}

func TestGuestStreamRejectsError(t *testing.T) {
	ctl, closeServer := fakeDialServer(t, "192.168.127.2:80", "ERR no route\n")
	defer closeServer()
	if _, err := guestStream(ctl, "192.168.127.2:80", false); err == nil {
		t.Fatal("want error when control replies ERR")
	}
}

// The suffix is what tells the daemon this connection counts as activity; a
// router that spelled it differently would idle-stop a VM in active use.
func TestGuestStreamTracksActivityOnRequest(t *testing.T) {
	ctl, closeServer := fakeDialServer(t, "192.168.127.2:80 track", "OK\n")
	defer closeServer()
	guest, err := guestStream(ctl, "192.168.127.2:80", true)
	if err != nil {
		t.Fatal(err)
	}
	guest.Close()
}

// A host port already taken is the everyday bind failure; the kernel names
// only the port, so the error has to name the way to find its occupier.
func TestBindListenerPointsAtThePortsOccupier(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	_, err = bindListener("127.0.0.1", port)
	if err == nil {
		t.Fatal("bindListener succeeded on a port already bound")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("lsof -nP -i :%d", port)) {
		t.Errorf("error %q does not name a way to find what holds the port", err)
	}
}

// A forward outlives the instance it points at, so the connection that fails
// after an idle auto-stop must say the target is gone and that the forward
// itself keeps working once it is booted again.
func TestForwardTargetGoneNamesTheInstanceAndTheRecovery(t *testing.T) {
	got := forwardTargetGone(&Identity{ID: "aaaa00000001", Name: "main"})
	for _, want := range []string{`"main"`, "is not running", "sprout up"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q does not mention %q", got, want)
		}
	}
}
