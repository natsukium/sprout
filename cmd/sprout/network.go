package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// The gvproxy convention for reaching the host's loopback from a guest.
const hostAlias = "192.168.127.254"

// The stack runs in-process rather than as a separate gvproxy: nothing to
// babysit, and DialContextTCP gets a direct line into the guest network.
func startNetwork(ctx context.Context, netSock string, m *Manifest) (*virtualnetwork.VirtualNetwork, error) {
	cfg := types.Configuration{
		MTU:               1500,
		Subnet:            m.Guest.Subnet,
		GatewayIP:         m.Guest.GatewayIP,
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		DHCPStaticLeases: map[string]string{
			m.Guest.IP: m.Guest.MAC,
		},
		Protocol: types.VfkitProtocol,
	}
	// Opt-in: the alias exposes every 127.0.0.1 listener on the host, so
	// wiring it unconditionally would undercut the VM as a boundary.
	if m.HostLoopback {
		cfg.GatewayVirtualIPs = []string{hostAlias}
		cfg.NAT = map[string]string{hostAlias: "127.0.0.1"}
	}
	cfg.DNS = wildcardZones(m.DNS.WildcardDomains)
	vn, err := virtualnetwork.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("virtual network: %w", err)
	}

	_ = os.Remove(netSock)
	ln, err := transport.ListenUnixgram("unixgram://" + netSock)
	if err != nil {
		return nil, fmt.Errorf("vfkit socket listen: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(netSock)
	}()
	go func() {
		// One connection per vfkit process; the loop covers VM restarts
		// within the daemon's lifetime.
		for {
			conn, err := transport.AcceptVfkit(ln)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("vfkit accept: %v", err)
				}
				return
			}
			go func() {
				if err := vn.AcceptVfkit(ctx, conn); err != nil && ctx.Err() == nil {
					log.Printf("vfkit network session ended: %v", err)
				}
			}()
		}
	}()

	return vn, nil
}

// Wildcard domains resolve here, at the gateway's resolver, rather than in a
// guest-side resolver on loopback: kubelet and docker copy the guest's
// /etc/resolv.conf into other network namespaces, where a loopback nameserver
// addresses the copier instead of the resolver.
//
// A zone answers its subdomains, not the domain itself, matching the option's
// contract. The trailing dot is what makes the match work at all: the resolver
// tests a query's FQDN against "."+name.
func wildcardZones(domains []string) []types.Zone {
	var zones []types.Zone
	for _, d := range domains {
		zones = append(zones, types.Zone{
			Name:      strings.TrimSuffix(d, ".") + ".",
			DefaultIP: net.IPv4(127, 0, 0, 1),
		})
	}
	return zones
}

// The listener is bound inside the virtual network, not on a real host port,
// so a bridged host socket ($SSH_AUTH_SOCK) is reachable only from this guest.
func startSocketForwards(ctx context.Context, vn socketListener, m *Manifest) error {
	for _, c := range m.Credentials {
		if c.Strategy != "socket" {
			continue
		}
		hostSock := os.ExpandEnv(c.Source)
		if hostSock == "" {
			return fmt.Errorf("credential %q: socket source %q resolved empty (is SSH_AUTH_SOCK set?)", c.Name, c.Source)
		}
		if c.GuestPort == 0 {
			return fmt.Errorf("credential %q: socket strategy requires a guest port", c.Name)
		}
		addr := net.JoinHostPort(m.Guest.GatewayIP, strconv.Itoa(c.GuestPort))
		ln, err := vn.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("credential %q: listen %s: %w", c.Name, addr, err)
		}
		go func() {
			<-ctx.Done()
			ln.Close()
		}()
		go socketBridge(ln, hostSock)
	}
	return nil
}

type socketListener interface {
	Listen(network, addr string) (net.Listener, error)
}

func socketBridge(ln net.Listener, hostSock string) {
	for {
		guest, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer guest.Close()
			host, err := net.Dial("unix", hostSock)
			if err != nil {
				log.Printf("socket forward: dial %s: %v", hostSock, err)
				return
			}
			defer host.Close()
			pipe(guest, host)
		}()
	}
}

// controlServer answers the line-oriented protocol on control.sock:
//
//	PING            -> OK
//	INFO [brief]    -> OK <json>, "brief" skipping the host-side resource sample
//	DIAL ssh        -> OK, then raw bidirectional stream to guest:22
//	DIAL ip:port    -> OK, then raw bidirectional stream
//	STOP            -> OK, then triggers graceful daemon shutdown
type controlServer struct {
	vn       *virtualnetwork.VirtualNetwork
	inst     *Instance
	started  time.Time
	stopOnce sync.Once
	stop     func()
	sessions *sessionTracker
	// runnerPID is the vfkit child, whose CPU/memory INFO reports. inst.PID is
	// the daemon, not the hypervisor, so it cannot stand in here.
	runnerPID int
	ready     atomic.Bool
}

func serveControl(ctx context.Context, sockPath string, srv *controlServer) error {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(sockPath)
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(ctx, conn)
		}
	}()
	return nil
}

func (s *controlServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	cmd, arg := parseControlLine(line)

	switch cmd {
	case "PING":
		fmt.Fprintln(conn, "OK")
	case "INFO":
		// The sample forks `ps` and `footprint` per call, so "brief" skips it
		// for the callers that never read it. Opt-out rather than opt-in, so
		// a CLI predating the argument keeps its CPU/MEM columns against this
		// daemon. A failed sample only omits the metrics, rendering as "-" in
		// `list`; it must never fail the whole INFO response.
		var stats procStats
		if arg != "brief" {
			stats, _ = sampleProcTree(s.runnerPID)
		}
		info, _ := json.Marshal(controlInfo{
			Name:       s.inst.Name,
			Definition: s.inst.Definition,
			GuestIP:    s.inst.GuestIP,
			PID:        s.inst.PID,
			UptimeSec:  int(time.Since(s.started).Seconds()),
			Ready:      s.ready.Load(),
			MemBytes:   stats.MemBytes,
			CPUPct:     stats.CPUPct,
		})
		fmt.Fprintf(conn, "OK %s\n", info)
	case "DIAL":
		addr, track := splitDialArg(arg)
		sshAddr := net.JoinHostPort(s.inst.GuestIP, "22")
		// SSH always counts as activity; a plain `forward` to another port
		// does not. The router opts in with "track", so a browser-only
		// workflow does not idle-stop mid-use.
		isSSH := addr == "ssh" || addr == sshAddr
		if addr == "ssh" {
			addr = sshAddr
		}
		guest, err := s.vn.DialContextTCP(ctx, addr)
		if err != nil {
			fmt.Fprintf(conn, "ERR %v\n", err)
			return
		}
		defer guest.Close()
		fmt.Fprintln(conn, "OK")
		if (isSSH || track) && s.sessions != nil {
			s.sessions.begin()
			defer s.sessions.end(time.Now())
		}
		pipe(conn, guest)
	case "STOP":
		fmt.Fprintln(conn, "OK")
		s.stopOnce.Do(s.stop)
	default:
		fmt.Fprintln(conn, "ERR unknown command")
	}
}

func splitDialArg(arg string) (addr string, track bool) {
	if rest, ok := strings.CutSuffix(arg, " track"); ok {
		return strings.TrimSpace(rest), true
	}
	return arg, false
}

func parseControlLine(line string) (cmd, arg string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexFunc(line, unicode.IsSpace); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

type halfCloser interface {
	CloseWrite() error
}

// Waits for both directions, not the first: returning on one EOF would cut
// off the other, still-flowing direction and drop bytes in transit.
func pipe(a, b io.ReadWriter) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyHalf(a, b) }()
	go func() { defer wg.Done(); copyHalf(b, a) }()
	wg.Wait()
}

func copyHalf(dst io.Writer, src io.Reader) {
	io.Copy(dst, src) //nolint:errcheck
	if hc, ok := dst.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}

// Reads go through the bufio.Reader already wrapped around the conn: the DIAL
// handshake may have buffered guest bytes past the status line. CloseWrite is
// forwarded explicitly because the embedded net.Conn interface hides the
// implementation's method from pipe's type assertion.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c bufferedConn) CloseWrite() error {
	if hc, ok := c.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return nil
}
