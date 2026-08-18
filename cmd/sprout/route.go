package main

import (
	"bufio"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Rides the `*.localhost → 127.0.0.1` convention, so no /etc/hosts or
// resolver edit is needed.
const defaultRouteDomain = "sprout.localhost"

const maxRouteHead = 8 << 10

const routeHeadReadTimeout = 10 * time.Second

func newRouteCmd() *cobra.Command {
	cmd := groupingCmd(&cobra.Command{
		Use:     "route",
		Short:   "Reach instances by name: http://<name>.sprout.localhost/",
		GroupID: groupNetwork,
	})
	cmd.AddCommand(newRouteServeCmd())
	return cmd
}

// HTTP-only by design: only a protocol carrying Host or SNI can be demuxed
// by name. Raw TCP stays on `sprout forward`.
func newRouteServeCmd() *cobra.Command {
	var (
		port          int
		bind          string
		domain        string
		noWake        bool
		verbose       bool
		launchdSocket string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the router in this terminal",
		Long: `Serve http://<name>.sprout.localhost/ for every instance on this host through one port.

Runs until interrupted, so it is a terminal (or a supervisor) of its own. A
request for a stopped instance starts it; --no-wake turns that off.

One router serves every instance. Where one already serves this address and
domain, this reports it and exits rather than starting a second.

To have launchd keep it running and own port 80 without root, see
docs/how-to/run-as-daemon.md.`,
		Args: usageArgs(func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("route serve takes no positional arguments, got %v (instances are addressed by hostname)", args)
			}
			return nil
		}),
		RunE: func(c *cobra.Command, _ []string) error {
			return cmdRoute(c.Flags(), port, bind, domain, noWake, verbose, launchdSocket)
		},
	}
	cmd.Flags().IntVar(&port, "port", 80, "host port to bind (URLs need an explicit :port when this isn't 80)")
	cmd.Flags().StringVar(&bind, "bind", defaultBindAddress, "address to bind (0.0.0.0 to reach it from your LAN — see the exposure warning)")
	cmd.Flags().StringVar(&domain, "domain", defaultRouteDomain, "hostname suffix to route (<label>.<domain>)")
	cmd.Flags().BoolVar(&noWake, "no-wake", false, "do not auto-start a stopped instance when a request arrives for it")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log every request's Host, the instance it resolved to, and the guest port")
	cmd.Flags().StringVar(&launchdSocket, "launchd-socket", "", "serve the socket launchd bound under this Sockets key instead of binding one")
	// Hidden, not removed: the nix-darwin module passes it, but no one at a
	// prompt can supply a launchd socket by hand.
	_ = cmd.Flags().MarkHidden("launchd-socket")
	return cmd
}

func cmdRoute(flags *pflag.FlagSet, port int, bind, domain string, noWake, verbose bool, launchdSocket string) error {
	dom, err := cleanDomain(domain)
	if err != nil {
		return err
	}

	lns, where, err := routeListeners(flags, launchdSocket, bind, port, dom)
	if err != nil {
		// One router serves every instance, so a second run of the wrapper
		// script that starts it is asking for a state that already holds.
		// Reporting it as a failure would make that script's exit code depend
		// on whether it had been run before.
		if errors.Is(err, errRouterAlreadyServing) {
			fmt.Printf("a sprout router already serves %s on %s:%d — leaving it running\n",
				routeURLTemplate(dom, port), bind, port)
			fmt.Println("(it keeps the flags it was started with; stop it first to change them)")
			return nil
		}
		return err
	}
	defer closeAll(lns)

	// launchd owns the address in the activated case, so the port comes from
	// the socket it bound, not --port.
	r := &router{domain: dom, port: listenerPort(lns[0], port), wake: !noWake, verbose: verbose, waking: map[string]bool{}}

	fmt.Printf("routing %s → instances on %s (Ctrl-C to stop)\n", routeURLTemplate(dom, r.port), where)

	for _, ln := range lns {
		go r.serve(ln)
	}

	awaitInterrupt(lns, "stopped routing")
	return nil
}

func awaitInterrupt(lns []net.Listener, stopped string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	closeAll(lns)
	fmt.Println("\n" + stopped)
}

func cleanDomain(domain string) (string, error) {
	dom := strings.Trim(domain, ".")
	if dom == "" {
		return "", usagef("--domain must not be empty")
	}
	return dom, nil
}

func routeListeners(flags *pflag.FlagSet, launchdSocket, bind string, port int, domain string) ([]net.Listener, string, error) {
	if launchdSocket == "" {
		lns, err := routeListen(bind, port, domain)
		if err != nil {
			return nil, "", err
		}
		if !loopbackBind(bind) {
			fmt.Fprintf(os.Stderr, "warning: --bind %s is not loopback; any host that can reach it can reach any instance's any port by sending a matching Host header\n", bind)
		}
		return lns, fmt.Sprintf("%s:%d", bind, port), nil
	}
	if given := flagsGiven(flags, "port", "bind"); len(given) > 0 {
		return nil, "", fmt.Errorf("--launchd-socket serves a socket launchd already bound, so it takes its address and port from the Sockets entry; drop %s", strings.Join(given, " and "))
	}
	lns, err := launchdListeners(launchdSocket)
	if err != nil {
		return nil, "", err
	}
	return lns, fmt.Sprintf("the launchd socket %q", launchdSocket), nil
}

func flagsGiven(flags *pflag.FlagSet, names ...string) []string {
	var given []string
	for _, n := range names {
		if flags.Changed(n) {
			given = append(given, "--"+n)
		}
	}
	return given
}

// launchd can hand over a unix socket, which has no port for URLs to carry.
func listenerPort(ln net.Listener, fallback int) int {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return fallback
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		ln.Close()
	}
}

func routeListen(bindHost string, port int, domain string) ([]net.Listener, error) {
	lns, err := bindListeners(bindHost, port)
	if errors.Is(err, errPrivilegedBind) {
		return nil, fmt.Errorf("cannot bind %s:%d: macOS forbids a non-root bind on a privileged port (<1024) against a specific address. Choose one:\n"+
			"  • an unprivileged port:  sprout route serve --port 8080     (URLs then carry it: http://<name>.%s:8080/)\n"+
			"  • all interfaces:        sprout route serve --bind 0.0.0.0  (allowed without root, but reachable from your LAN — every instance's every port, by Host header)\n"+
			"  • a launchd daemon that binds :%d for you (services.sprout.route, see docs/how-to/route.md)",
			bindHost, port, domain, port)
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		switch probeRouter(probeAddr(bindHost), port, domain) {
		case probeRouterSame:
			return nil, errRouterAlreadyServing
		case probeRouterOther:
			return nil, fmt.Errorf("cannot bind %s:%d: a sprout router already holds it, but it does not serve --domain %q; stop that router first, or run alongside it on another --port",
				bindHost, port, domain)
		}
	}
	return lns, err
}

var errRouterAlreadyServing = errors.New("a sprout router is already serving this address")

type probeResult int

const (
	probeNotRouter   probeResult = iota // a stranger, or nothing that answers HTTP
	probeRouterSame                     // a router serving the same --domain
	probeRouterOther                    // a router, but for some other --domain
)

const routeProbeTimeout = 2 * time.Second

// A router marks its own responses with Server: sprout-route and serves the
// index only for an exact --domain match, so one request separates a router
// from a stranger and the same domain from a different one. Anything
// unexpected reads as a stranger, which errs toward the bind error the caller
// already had.
func probeRouter(addr string, port int, domain string) probeResult {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(port)), routeProbeTimeout)
	if err != nil {
		return probeNotRouter
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(routeProbeTimeout))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", domain); err != nil {
		return probeNotRouter
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return probeNotRouter
	}
	defer resp.Body.Close()
	if resp.Header.Get("Server") != routeServerHeader {
		return probeNotRouter
	}
	// A bridged response carries no Server header of ours, so reaching here
	// means the router answered for itself: the index for its own domain,
	// some refusal for anyone else's.
	if resp.StatusCode != http.StatusOK {
		return probeRouterOther
	}
	return probeRouterSame
}

func probeAddr(bindHost string) string {
	ip := net.ParseIP(bindHost)
	if ip == nil || !ip.IsUnspecified() {
		return bindHost
	}
	if ip.To4() != nil {
		return "127.0.0.1"
	}
	return "::1"
}

func loopbackBind(addr string) bool {
	if strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

type router struct {
	domain  string
	port    int
	wake    bool
	verbose bool

	mu     sync.Mutex
	waking map[string]bool
}

// One Fprintf, so concurrent connections interleave whole lines.
func (r *router) logRequest(host string, head []byte, outcome string) {
	if !r.verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "route: %s %s -> %s\n", host, requestLine(head), outcome)
}

func instanceLog(id string) string {
	return fmt.Sprintf("%q (%s)", displayForID(id), id)
}

// Capped: a request-target is attacker-controlled and this line goes to a
// terminal.
func requestLine(head []byte) string {
	line, _, _ := strings.Cut(string(head), "\n")
	line = strings.TrimRight(line, "\r")
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return strconv.Quote(line)
}

func (r *router) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go r.handle(conn)
	}
}

// Splices byte-for-byte rather than reverse-proxying, so WebSockets and the
// true Host header pass through untouched.
func (r *router) handle(conn net.Conn) {
	defer conn.Close()
	// Buffer sized to the head cap, so an oversized header line surfaces as
	// ErrBufferFull (→ 431) instead of growing unbounded.
	br := bufio.NewReaderSize(conn, maxRouteHead)
	// Cleared below, so it never fires mid-splice on a long-lived connection.
	_ = conn.SetReadDeadline(time.Now().Add(routeHeadReadTimeout))
	host, head, err := sniffHost(br)
	if err != nil {
		r.logRequest(conn.RemoteAddr().String(), head, fmt.Sprintf("rejected: %v", err))
		if errors.Is(err, errHeadTooLarge) {
			r.writeError(conn, http.StatusRequestHeaderFieldsTooLarge, "Request header too large", "")
		} else {
			r.writeError(conn, http.StatusBadRequest, "Could not read an HTTP request with a single Host header", "")
		}
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	tgt := parseRouteHost(host, r.domain)
	switch tgt.kind {
	case routeIndex:
		r.logRequest(host, head, "the index page")
		r.writeIndex(conn)
		return
	case routeForeign:
		r.logRequest(host, head, fmt.Sprintf("not a .%s name (404)", r.domain))
		r.writeError(conn, http.StatusNotFound,
			fmt.Sprintf("Host %q is not a sprout route name", host),
			"Use "+html.EscapeString(routeURLTemplate(r.domain, r.port)))
		return
	}

	id, err := resolveRouteLabel(tgt.label)
	if err != nil {
		var amb *routeAmbiguousError
		switch {
		case errors.As(err, &amb):
			r.logRequest(host, head, fmt.Sprintf("label %q is ambiguous (409)", tgt.label))
			r.writeError(conn, http.StatusConflict,
				fmt.Sprintf("Name %q is ambiguous", tgt.label), amb.hint(r))
		case errors.Is(err, errRouteNotFound):
			r.logRequest(host, head, fmt.Sprintf("no instance answers to label %q (404)", tgt.label))
			r.writeError(conn, http.StatusNotFound,
				fmt.Sprintf("No instance named %q", tgt.label), r.knownNamesHint())
		default:
			r.logRequest(host, head, fmt.Sprintf("resolving label %q: %v (500)", tgt.label, err))
			r.writeError(conn, http.StatusInternalServerError, err.Error(), "")
		}
		return
	}

	r.serveInstance(conn, br, head, host, id, tgt.gport)
}

func (r *router) serveInstance(conn net.Conn, br *bufio.Reader, head []byte, host, id string, gport int) {
	state, info := r.ensureReady(id)
	if state != readyOK {
		r.writeNotReady(conn, head, host, id, "", state)
		return
	}
	r.bridge(conn, br, head, host, id, info, gport)
}

func (r *router) writeNotReady(conn net.Conn, head []byte, host, id, note string, state readyState) {
	switch state {
	case readyStopped:
		r.logRequest(host, head, note+instanceLog(id)+" is stopped (503)")
		r.writeError(conn, http.StatusServiceUnavailable,
			fmt.Sprintf("Instance %q is stopped", displayForID(id)),
			"Start it with <code>sprout start</code>, or drop <code>--no-wake</code> so the router boots it on demand.")
	case readyGone:
		r.logRequest(host, head, note+instanceLog(id)+" has no build to boot (503)")
		r.writeError(conn, http.StatusServiceUnavailable,
			fmt.Sprintf("The build for %q is no longer in the Nix store", displayForID(id)),
			"Run <code>sprout up</code> in its worktree to rebuild and boot it.")
	case readyWaking:
		r.logRequest(host, head, note+instanceLog(id)+" is still booting (503)")
		r.writeWaking(conn, displayForID(id))
	}
	// No default: a state this switch does not know closes the connection
	// with nothing written, where a default would dress it up as an
	// endlessly reloading "still booting".
}

func (r *router) bridge(client net.Conn, br *bufio.Reader, head []byte, host, id string, info *controlInfo, gport int) {
	guestAddr := net.JoinHostPort(info.GuestIP, strconv.Itoa(gport))
	guest, err := dialGuest(id, guestAddr, true)
	if err != nil {
		if guest = r.recoverDialFailure(client, head, host, id, info, guestAddr, gport, err); guest == nil {
			return
		}
	}
	defer guest.Close()
	// Before the splice: after it the guest owns the response and its status
	// is no longer visible here.
	r.logRequest(host, head, fmt.Sprintf("%s guest:%d bridged", instanceLog(id), gport))
	if _, err := guest.Write(head); err != nil {
		// The head is partly on the wire, so no error page can be written
		// cleanly now.
		fmt.Fprintf(os.Stderr, "route: %s: replay request head: %v\n", displayForID(id), err)
		return
	}
	// From br, not client: sniffHost may have buffered body bytes past the
	// head, which reading client directly would drop.
	pipe(bufferedConn{Conn: client, r: br}, guest)
}

// Only a daemon reply says anything about the guest; any other failure means
// the daemon did not answer, which is usually an instance stopped between the
// readiness check above and here. Answering that with the guest-port 502 both
// claims something untrue and strands the client on a page that never
// reloads, where the waking page would have brought it back by itself.
//
// Returns a live guest connection when a retry recovered one, nil after
// writing an error page.
func (r *router) recoverDialFailure(conn net.Conn, head []byte, host, id string, info *controlInfo, guestAddr string, gport int, err error) net.Conn {
	var rejected *controlRejectedError
	if errors.As(err, &rejected) {
		r.writeDialRejected(conn, head, host, id, info, gport, rejected)
		return nil
	}
	if state, _ := r.ensureReady(id); state != readyOK {
		r.writeNotReady(conn, head, host, id, fmt.Sprintf("control socket stopped answering mid-request (%v), ", err), state)
		return nil
	}
	// The recheck answered INFO over a connection as fresh as the one that
	// just failed, which proves the failure transient — typically a daemon
	// handover during an in-place `sprout up`. One more dial absorbs exactly
	// the class the recheck detected; a second failure falls through to the
	// error page, so this cannot loop.
	guest, retryErr := dialGuest(id, guestAddr, true)
	if retryErr == nil {
		return guest
	}
	if errors.As(retryErr, &rejected) {
		r.writeDialRejected(conn, head, host, id, info, gport, rejected)
		return nil
	}
	r.logRequest(host, head, fmt.Sprintf("%s control socket: %v (502)", instanceLog(id), retryErr))
	r.writeError(conn, http.StatusBadGateway,
		fmt.Sprintf("Could not reach %q through its control socket", displayForID(id)),
		"<code>"+html.EscapeString(retryErr.Error())+"</code><br>The instance answered a moment ago, so this is the host side of the connection failing, not the guest.")
	return nil
}

// The daemon replies ERR for every way its dial can fail, so only the reason
// naming a refusal proves a closed (or loopback-bound) port; a timeout means
// the guest never answered at all — a hung guest, a wedged network stack.
func (r *router) writeDialRejected(conn net.Conn, head []byte, host, id string, info *controlInfo, gport int, rejected *controlRejectedError) {
	name, reason := displayForID(id), rejected.reason()
	if !isConnectionRefused(reason) {
		r.logRequest(host, head, fmt.Sprintf("%s guest:%d dial failed: %s (502)", instanceLog(id), gport, reason))
		r.writeError(conn, http.StatusBadGateway,
			fmt.Sprintf("Dialing guest port %d of %q failed: %s", gport, name, reason),
			"Not a refusal, so the guest did not answer at all; <code>sprout logs</code> shows what state it is in.")
		return
	}
	if withinStartGrace(info) {
		r.logRequest(host, head, fmt.Sprintf("%s guest:%d not listening yet: %s (503)", instanceLog(id), gport, reason))
		r.writeStarting(conn, name, gport)
		return
	}
	r.logRequest(host, head, fmt.Sprintf("%s guest:%d refused the connection: %s (502)", instanceLog(id), gport, reason))
	r.writeError(conn, http.StatusBadGateway,
		fmt.Sprintf("%q is running but nothing answered on guest port %d", name, gport),
		guestPortHint(name, gport))
}

// Matched on text because the control protocol carries only the daemon's
// error string: gvisor's netstack says "connection was refused" where a
// kernel stack says "connection refused".
func isConnectionRefused(reason string) bool {
	return strings.Contains(strings.ToLower(reason), "refused")
}

// Readiness is SSH plus sprout-ready.target, which an unhooked guest reaches
// before its dev server binds, so a port refusing this soon after boot is
// still the boot. Bounded: a port nothing will ever listen on has to stop
// pretending.
const routeStartGrace = 2 * time.Minute

func withinStartGrace(info *controlInfo) bool {
	return time.Duration(info.UptimeSec)*time.Second < routeStartGrace
}

func guestPortHint(name string, gport int) string {
	return fmt.Sprintf("Is a server listening on port %d inside the VM, bound to <code>0.0.0.0</code> rather than <code>127.0.0.1</code>? "+
		"Check with <code>sprout exec -i %s -- ss -ltnp</code>, or add a <code>&lt;port&gt;.</code> prefix to the hostname to reach a different port.",
		gport, html.EscapeString(name))
}

// The returned conn owns the control connection: closing it closes both.
func dialGuest(id, guestAddr string, track bool) (net.Conn, error) {
	ctl, err := controlDial(id)
	if err != nil {
		return nil, err
	}
	return guestStream(ctl, guestAddr, track)
}

// The " track" suffix keeps a browser-only session from idle-stopping the VM.
func guestStream(ctl net.Conn, guestAddr string, track bool) (net.Conn, error) {
	addr := guestAddr
	if track {
		addr += " track"
	}
	reader, err := dialHandshake(ctl, addr)
	if err != nil {
		ctl.Close()
		return nil, err
	}
	return bufferedConn{Conn: ctl, r: reader}, nil
}

type readyState int

const (
	readyOK      readyState = iota // running and answering SSH
	readyWaking                    // booting, or a wake was just kicked off
	readyStopped                   // down, and --no-wake forbids starting it
	readyGone                      // down, and its recorded build is missing (can't start)
)

// The wake goes through `start`, not `up`: start re-boots a recorded bundle
// by ID, needing no flake or worktree. The request never blocks on it, so the
// caller can return a re-checking interstitial instead of holding the
// connection through a ~30s boot.
func (r *router) ensureReady(id string) (readyState, *controlInfo) {
	if info, err := queryInfo(id); err == nil {
		if info.Ready {
			return readyOK, info
		}
		return readyWaking, nil
	}
	if !r.wake {
		return readyStopped, nil
	}
	// A start with no build on disk can only fail; its own state keeps the
	// interstitial from spinning forever on something that will never boot.
	if inst, _, err := loadInstance(id); err == nil {
		if _, statErr := os.Stat(inst.Bundle); statErr != nil {
			return readyGone, nil
		}
	}
	r.startWake(id)
	return readyWaking, nil
}

// At most one wake per instance, so a burst of requests shares one boot. The
// entry is held for the whole boot: releasing it early would let the next
// refresh start a second daemon that clobbers the first's control socket.
func (r *router) startWake(id string) {
	r.mu.Lock()
	if r.waking[id] {
		r.mu.Unlock()
		return
	}
	r.waking[id] = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.waking, id)
			r.mu.Unlock()
		}()
		if err := wakeInstance(id); err != nil {
			fmt.Fprintf(os.Stderr, "route: waking %s: %v\n", displayForID(id), err)
		}
	}()
}

// Blocks until the VM answers, so startWake's entry covers the whole boot.
// Silent: nothing reads a router's stdout, and the failure a request cares
// about surfaces on the interstitial instead. A variable so a test can reach
// the pages a wake leads to without booting a VM.
var wakeInstance = func(id string) error {
	return bootDetached(id, startChildArgs(id), 0, "boot", nil)
}

type routeKind int

const (
	routeInstance routeKind = iota // <label>.<domain> — route to an instance
	routeIndex                     // exactly <domain> — serve the instance list
	routeForeign                   // some other host — not ours
)

// Labels left of the instance label (a guest-side virtual host) are not
// stored: the router replays the exact Host to the guest untouched.
type routeTarget struct {
	label string
	gport int
	kind  routeKind
}

// The label next to the domain is the instance, a leading all-digit label
// overrides the guest port (5173.<label>.<domain>), and anything else is a
// guest-side virtual host. Unambiguous only because sanitizeName collapses
// dots, so a sanitized name is never itself dotted.
func parseRouteHost(host, domain string) routeTarget {
	host = strings.ToLower(strings.TrimSuffix(stripHostPort(host), "."))
	domain = strings.ToLower(domain)
	if host == domain {
		return routeTarget{kind: routeIndex}
	}
	rest, ok := strings.CutSuffix(host, "."+domain)
	if !ok || rest == "" {
		return routeTarget{kind: routeForeign}
	}
	labels := strings.Split(rest, ".")
	gport := 80
	if len(labels) > 1 && isAllDigits(labels[0]) {
		if p, err := strconv.Atoi(labels[0]); err == nil && p >= 1 && p <= 65535 {
			gport = p
			labels = labels[1:]
		}
	}
	label := labels[len(labels)-1]
	if label == "" {
		return routeTarget{kind: routeForeign}
	}
	return routeTarget{label: label, gport: gport, kind: routeInstance}
}

func stripHostPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Returns the raw bytes too, so the head can be replayed to the guest
// byte-for-byte. br must be sized to maxRouteHead, so ReadSlice caps an
// unterminated line instead of buffering forever.
//
// The three rejected shapes are the ones where router and guest could disagree
// on which host is authoritative, exploitable the moment --bind opens the
// router past loopback:
//
//   - an absolute-form request-target (GET http://other/…), whose authority
//     overrides Host at the guest but is invisible here (errBadTarget);
//   - a second Host header, or whitespace before the Host colon (errBadHost);
//   - an obs-fold continuation line, which could smuggle a second "Host:" past
//     here as an ordinary value the guest then unfolds (errBadHost).
func sniffHost(br *bufio.Reader) (host string, head []byte, err error) {
	seenHost := false
	var buf []byte
	for lineNum := 0; ; lineNum++ {
		line, rerr := br.ReadSlice('\n')
		buf = append(buf, line...)
		if len(buf) > maxRouteHead || rerr == bufio.ErrBufferFull {
			return "", buf, errHeadTooLarge
		}
		if rerr != nil {
			return "", buf, fmt.Errorf("reading request head: %w", rerr)
		}
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			break
		}
		if lineNum == 0 {
			if isAbsoluteFormTarget(trimmed) {
				return "", buf, errBadTarget
			}
			continue
		}
		// SP/HTAB starts an obs-fold, not a new field: caught here before the
		// match below would read " Host: …" as one.
		if line[0] == ' ' || line[0] == '\t' {
			return "", buf, errBadHost
		}
		if name, value, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Host") {
			if seenHost || name != strings.TrimRight(name, " \t") {
				return "", buf, errBadHost
			}
			seenHost = true
			host = strings.TrimSpace(value)
		}
	}
	if host == "" {
		return "", buf, errNoHost
	}
	return host, buf, nil
}

// The leading-"/" check guards the "://" test, so an origin-form target with
// a URL in its query (/r?to=http://x) is not misread.
func isAbsoluteFormTarget(requestLine string) bool {
	fields := strings.Fields(requestLine)
	if len(fields) < 2 {
		return false
	}
	target := fields[1]
	if strings.HasPrefix(target, "/") || target == "*" {
		return false
	}
	return strings.Contains(target, "://")
}

var (
	errHeadTooLarge = errors.New("request head exceeds limit")
	errNoHost       = errors.New("request has no Host header")
	errBadHost      = errors.New("request has a duplicate or malformed Host header")
	errBadTarget    = errors.New("request-target is absolute-form, carrying its own authority")
)
