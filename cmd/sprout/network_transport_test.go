package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestManifestTransportSelectsItsWireProtocol(t *testing.T) {
	for _, c := range []struct {
		host, kind string
		impl       networkTransport
		protocol   types.Protocol
	}{
		{"aarch64-darwin", "vfkit", vfkitUnixgram{}, types.VfkitProtocol},
		{"x86_64-linux", "qemu", qemuStream{}, types.QemuProtocol},
		{"aarch64-linux", "qemu", qemuStream{}, types.QemuProtocol},
	} {
		m, err := parseManifest(encodeDoc(t, v2ManifestDoc(c.host, c.kind)), c.host)
		if err != nil {
			t.Fatal(err)
		}
		if m.contract.network != c.impl || m.contract.network.protocol() != c.protocol {
			t.Errorf("%s on %s: transport %T speaking %q, want %T speaking %q", c.kind, c.host, m.contract.network, m.contract.network.protocol(), c.impl, c.protocol)
		}
	}
}

const (
	testGuestIP  = "192.168.127.2"
	testGuestMAC = "5a:94:ef:e4:0c:ee"
)

// A NIC as the runner's side of the backend socket presents it: one Ethernet
// frame per call, in whatever framing the transport uses.
type guestNIC interface {
	send(frame []byte) error
	recv() ([]byte, error)
	Close() error
}

// QEMU's stream netdev: a stream client, each frame behind a 4-byte
// big-endian length.
type qemuNIC struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialQemuNIC(t *testing.T, sock string) guestNIC {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	return &qemuNIC{conn: conn, r: bufio.NewReader(conn)}
}

func (n *qemuNIC) send(frame []byte) error {
	buf := binary.BigEndian.AppendUint32(nil, uint32(len(frame)))
	_, err := n.conn.Write(append(buf, frame...))
	return err
}

func (n *qemuNIC) recv() ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(n.r, size[:]); err != nil {
		return nil, err
	}
	frame := make([]byte, binary.BigEndian.Uint32(size[:]))
	_, err := io.ReadFull(n.r, frame)
	return frame, err
}

func (n *qemuNIC) Close() error { return n.conn.Close() }

// vfkit's virtio-net sends one frame per datagram from a bound, connected socket.
type vfkitNIC struct {
	conn *net.UnixConn
}

func dialVfkitNIC(t *testing.T, sock string) guestNIC {
	t.Helper()
	local := filepath.Join(filepath.Dir(sock), "vfkit-"+strconv.Itoa(os.Getpid())+".sock")
	conn, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: local, Net: "unixgram"},
		&net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(local) })
	return &vfkitNIC{conn: conn}
}

func (n *vfkitNIC) send(frame []byte) error {
	_, err := n.conn.Write(frame)
	return err
}

func (n *vfkitNIC) recv() ([]byte, error) {
	buf := make([]byte, 65536)
	size, err := n.conn.Read(buf)
	return buf[:size], err
}

func (n *vfkitNIC) Close() error { return n.conn.Close() }

// A userspace TCP/IP stack standing in for the guest kernel behind nic, so
// what crosses the socket is exactly what a VM would exchange.
func startFakeGuest(t *testing.T, ctx context.Context, nic guestNIC) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	mac, err := net.ParseMAC(testGuestMAC)
	if err != nil {
		t.Fatal(err)
	}
	link := channel.New(256, 1500, tcpip.LinkAddress(mac))
	if err := s.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	addr := tcpip.AddrFromSlice(net.ParseIP(testGuestIP).To4())
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr.WithPrefix()}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{192, 168, 127, 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	if err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: 1}})
	t.Cleanup(func() {
		link.Close()
		s.Close()
	})

	go func() {
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			frame := pkt.ToView().AsSlice()
			err := nic.send(frame)
			pkt.DecRef()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			frame, err := nic.recv()
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
			link.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	return s
}

func transportManifest(t *testing.T, host, kind string, creds []CredentialSpec) *Manifest {
	t.Helper()
	doc := v2ManifestDoc(host, kind)
	doc["guest"] = map[string]any{"system": doc["guest"].(map[string]any)["system"], "ip": testGuestIP, "gatewayIp": "192.168.127.1", "subnet": "192.168.127.0/24", "mac": testGuestMAC}
	doc["hostLoopback"] = true
	m, err := parseManifest(encodeDoc(t, doc), host)
	if err != nil {
		t.Fatal(err)
	}
	m.Credentials = creds
	return m
}

// Short enough for sun_path wherever the test runs; t.TempDir nests under
// the test name.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sprout-net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveOnce(t *testing.T, ln net.Listener, reply string) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, reply) //nolint:errcheck
			c.Close()
		}
	}()
}

func readAll(t *testing.T, c net.Conn) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dialGuestBanner(t *testing.T, ctx context.Context, vn *virtualnetwork.VirtualNetwork) string {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := vn.DialContextTCP(dctx, net.JoinHostPort(testGuestIP, "22"))
	if err != nil {
		t.Fatalf("dial guest sshd: %v", err)
	}
	defer c.Close()
	return readAll(t, c)
}

func guestDial(t *testing.T, ctx context.Context, s *stack.Stack, ip string, port int) string {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(net.ParseIP(ip).To4()), Port: uint16(port)}
	c, err := gonet.DialContextTCP(dctx, s, addr, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest dial %s:%d: %v", ip, port, err)
	}
	defer c.Close()
	return readAll(t, c)
}

func hostLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	serveOnce(t, ln, "host")
	return ln.Addr().(*net.TCPAddr).Port
}

func listenGuestSSH(t *testing.T, s *stack.Stack, banner string) {
	t.Helper()
	ln, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: 1, Port: 22}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	serveOnce(t, ln, banner)
}

// The paths sprout relies on, end to end through each transport's socket:
// host-to-guest dialing (SSH, exec, forwards, the router), guest-to-host
// loopback through the opt-in alias, and a socket credential bridged from a
// host Unix socket.
func TestNetworkPathsWorkOverEachTransport(t *testing.T) {
	if inChildProcess(t) {
		return
	}
	for _, c := range []struct {
		name, host, kind string
		dial             func(*testing.T, string) guestNIC
	}{
		{"vfkit-unixgram", "aarch64-darwin", "vfkit", dialVfkitNIC},
		{"qemu-stream", "x86_64-linux", "qemu", dialQemuNIC},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := shortSocketDir(t)

			agent, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			serveOnce(t, agent, "agent")
			m := transportManifest(t, c.host, c.kind, []CredentialSpec{
				{Name: "ssh-agent", Strategy: "socket", Source: agent.Addr().String(), GuestPort: 2222},
			})

			vn, err := startNetwork(ctx, filepath.Join(dir, "net.sock"), m)
			if err != nil {
				t.Fatal(err)
			}
			if err := startSocketForwards(ctx, vn, m); err != nil {
				t.Fatal(err)
			}
			nic := c.dial(t, filepath.Join(dir, "net.sock"))
			defer nic.Close()
			guest := startFakeGuest(t, ctx, nic)
			listenGuestSSH(t, guest, "SSH-2.0-guest")

			// First, as a booting guest's DHCP would be: the switch learns the
			// guest's MAC only from frames the guest sends.
			if got := guestDial(t, ctx, guest, hostAlias, hostLoopbackPort(t)); got != "host" {
				t.Errorf("guest to host loopback: read %q", got)
			}

			if got := dialGuestBanner(t, ctx, vn); got != "SSH-2.0-guest" {
				t.Errorf("host to guest: read %q", got)
			}

			if got := guestDial(t, ctx, guest, "192.168.127.1", 2222); got != "agent" {
				t.Errorf("socket credential: read %q", got)
			}
		})
	}
}

// QEMU dials the socket once per process, so a runner restarted under the
// same daemon must find the listener still accepting.
func TestQemuStreamAcceptsARestartedRunner(t *testing.T) {
	if inChildProcess(t) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "net.sock")
	vn, err := startNetwork(ctx, sock, transportManifest(t, "x86_64-linux", "qemu", nil))
	if err != nil {
		t.Fatal(err)
	}
	loopback := hostLoopbackPort(t)

	for _, banner := range []string{"SSH-2.0-first-boot", "SSH-2.0-second-boot"} {
		gctx, gcancel := context.WithCancel(ctx)
		nic := dialQemuNIC(t, sock)
		guest := startFakeGuest(t, gctx, nic)
		listenGuestSSH(t, guest, banner)
		guestDial(t, ctx, guest, hostAlias, loopback)
		if got := dialGuestBanner(t, ctx, vn); got != banner {
			t.Errorf("read %q, want %q", got, banner)
		}
		gcancel()
		nic.Close()
		guest.Close()
	}
}

func TestVfkitHostDialRecoversAfterFailedSend(t *testing.T) {
	if inChildProcess(t) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sock := filepath.Join(shortSocketDir(t), "net.sock")
	m := transportManifest(t, "aarch64-darwin", "vfkit", nil)
	vn, err := startNetwork(ctx, sock, m)
	if err != nil {
		t.Fatal(err)
	}
	nic := dialVfkitNIC(t, sock).(*vfkitNIC)
	defer nic.Close()
	primeVfkitGuestMAC(t, nic, m.Guest.GatewayIP)
	switchAPI := vn.ServicesMux()
	port, known := vfkitGuestSwitchPort(t, switchAPI)
	if !known {
		t.Fatal("switch did not learn the guest MAC")
	}

	path := nic.conn.LocalAddr().String()
	failed := make(vfkitSendFailures, 1)
	// The patched build reports the failed send in sprout's drop log. The
	// unpatched build reports it in the switch's log.
	sproutLog, switchLog := log.Writer(), logrus.StandardLogger().Out
	log.SetOutput(io.MultiWriter(sproutLog, failed))
	logrus.SetOutput(io.MultiWriter(switchLog, failed))
	t.Cleanup(func() {
		log.SetOutput(sproutLog)
		logrus.SetOutput(switchLog)
	})
	if err := os.Rename(path, path+".away"); err != nil {
		t.Fatal(err)
	}
	dctx, stopDial := context.WithTimeout(ctx, 5*time.Second)
	defer stopDial()
	done := make(chan error, 1)
	go func() {
		c, err := vn.DialContextTCP(dctx, net.JoinHostPort(testGuestIP, "22"))
		if err == nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case msg := <-failed:
		t.Logf("send failed with the guest path renamed: %s", msg)
	case <-dctx.Done():
		t.Fatal("no failed unixgram send while the guest path was renamed")
	}
	stopDial()
	if err := <-done; err == nil {
		t.Fatal("host dial succeeded with the guest path renamed")
	}
	if err := os.Rename(path+".away", path); err != nil {
		t.Fatal(err)
	}
	if current, known := vfkitGuestSwitchPort(t, switchAPI); !known || current != port {
		t.Errorf("switch lost the guest MAC on port %d after the failed send", port)
	}

	// Start the guest stack after checking the MAC entry because a guest frame
	// would teach the switch its MAC again and hide a lost entry.
	guest := startFakeGuest(t, ctx, nic)
	listenGuestSSH(t, guest, "SSH-2.0-guest")
	if got := dialGuestBanner(t, ctx, vn); got != "SSH-2.0-guest" {
		t.Fatalf("fresh host dial read %q", got)
	}

	cancel()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, known := vfkitGuestSwitchPort(t, switchAPI); !known {
			return
		}
		select {
		case <-deadline:
			t.Fatal("closing the listener did not end the vfkit session")
		case <-ticker.C:
		}
	}
}

func vfkitGuestSwitchPort(t *testing.T, handler http.Handler) (int, bool) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/cam", nil))
	var ports map[string]int
	if err := json.Unmarshal(response.Body.Bytes(), &ports); err != nil {
		t.Fatalf("decode switch MAC table: %v", err)
	}
	port, known := ports[testGuestMAC]
	return port, known
}

// A single ARP exchange teaches the switch the guest MAC. Later guest frames
// could restore a lost MAC entry before the test checks it.
func primeVfkitGuestMAC(t *testing.T, nic *vfkitNIC, gatewayIP string) {
	t.Helper()
	mac, err := net.ParseMAC(testGuestMAC)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(mac),
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.ARPProtocolNumber,
	})
	arpRequest := header.ARP(frame[header.EthernetMinimumSize:])
	arpRequest.SetIPv4OverEthernet()
	arpRequest.SetOp(header.ARPRequest)
	copy(arpRequest.HardwareAddressSender(), mac)
	copy(arpRequest.ProtocolAddressSender(), net.ParseIP(testGuestIP).To4())
	copy(arpRequest.ProtocolAddressTarget(), net.ParseIP(gatewayIP).To4())
	if err := nic.send(frame); err != nil {
		t.Fatal(err)
	}
	if err := nic.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reply, err := nic.recv()
	if err != nil {
		t.Fatalf("read gateway ARP reply: %v", err)
	}
	if len(reply) < len(frame) || header.Ethernet(reply).Type() != header.ARPProtocolNumber ||
		header.ARP(reply[header.EthernetMinimumSize:]).Op() != header.ARPReply {
		t.Fatalf("want gateway ARP reply, got %x", reply)
	}
	if err := nic.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

type vfkitSendFailures chan string

func (c vfkitSendFailures) Write(p []byte) (int, error) {
	msg := string(p)
	if strings.Contains(msg, "write unixgram ") && strings.Contains(msg, syscall.ENOENT.Error()) {
		select {
		case c <- strings.TrimSpace(msg):
		default:
		}
	}
	return len(p), nil
}

// vfkit's accept returns the listening socket itself, so frames still queued
// behind a running session must not be taken for another peer.
func TestVfkitFramesQueuedDuringASessionOpenNoSecondSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sock := filepath.Join(shortSocketDir(t), "net.sock")
	ln, err := transport.ListenUnixgram("unixgram://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	nic := dialVfkitNIC(t, sock)
	defer nic.Close()
	// Few enough for macOS, which caps a unixgram receive queue at a few KiB;
	// a single queued frame already lets a peeking accept succeed repeatedly.
	for range 4 {
		if err := nic.send(make([]byte, 60)); err != nil {
			t.Fatal(err)
		}
	}

	var sessions atomic.Int32
	first := make(chan struct{})
	accept := func() (net.Conn, error) { return transport.AcceptVfkit(ln) }
	go serveNICSessions(ctx, "vfkit", accept, func(ctx context.Context, _ net.Conn) error {
		if sessions.Add(1) == 1 {
			close(first)
		}
		<-ctx.Done()
		return nil
	})

	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no session for the queued frames")
	}
	time.Sleep(100 * time.Millisecond)
	if n := sessions.Load(); n != 1 {
		t.Fatalf("%d sessions opened for one vfkit peer, want 1", n)
	}
}

// A listener shut down by its daemon's cancel must not unlink the path after
// the fact: by then a newer daemon may have bound its own socket there.
func TestQemuStreamShutdownLeavesARebindingDaemonsSocket(t *testing.T) {
	if inChildProcess(t) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sock := filepath.Join(shortSocketDir(t), "net.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startNetwork(ctx, sock, transportManifest(t, "x86_64-linux", "qemu", nil)); err != nil {
		t.Fatalf("stale file at the socket path blocked the listener: %v", err)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v; want a 0600 socket", fi, err)
	}
	removeSocketFiles([]string{sock})
	newer, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	cancel()
	time.Sleep(200 * time.Millisecond)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the shut-down listener removed the newer daemon's socket: %v", err)
	}
	conn.Close()
}

// Re-runs the calling test in a child process and reports whether the caller
// is the parent, which should return. Every test that builds a VirtualNetwork
// uses it: the type has no Close, and under -race the stacks left behind keep
// every core busy for the rest of the package run, timing out unrelated tests.
// The child takes them with it when it exits.
func inChildProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv("SPROUT_TEST_CHILD") == t.Name() {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd.Env = append(os.Environ(), "SPROUT_TEST_CHILD="+t.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return true
}
