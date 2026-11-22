package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

type vfkitUnixgram struct{}

func (vfkitUnixgram) protocol() types.Protocol { return types.VfkitProtocol }

func (vfkitUnixgram) serve(ctx context.Context, vn *virtualnetwork.VirtualNetwork, netSock string) error {
	_ = os.Remove(netSock)
	ln, err := transport.ListenUnixgram("unixgram://" + netSock)
	if err != nil {
		return fmt.Errorf("vfkit socket listen: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	accept := func() (net.Conn, error) {
		conn, err := transport.AcceptVfkit(ln)
		if err != nil {
			return nil, err
		}
		return &vfkitDatagramConn{Conn: conn}, nil
	}
	go serveNICSessions(ctx, "vfkit", accept, vn.AcceptVfkit)
	return nil
}

const vfkitDropLogInterval = time.Second

// A failed datagram send loses one Ethernet frame. Returning the error would
// make the switch close the listener and forget the guest's MAC address.
type vfkitDatagramConn struct {
	net.Conn
	dropMu          sync.Mutex
	nextDropLog     time.Time
	unreportedDrops atomic.Uint64
}

func (c *vfkitDatagramConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil {
		if c.unreportedDrops.Load() != 0 {
			if dropped := c.unreportedDrops.Swap(0); dropped != 0 {
				log.Printf("vfkit network: send recovered: unreported drops=%d", dropped)
			}
		}
		return n, nil
	}

	// The switch retries ENOBUFS when vfkit's receive buffer is full.
	// Pass net.ErrClosed through for the switch's usual disconnect when the
	// daemon closes the listener and keep shutdown out of the drop log.
	if errors.Is(err, syscall.ENOBUFS) || errors.Is(err, net.ErrClosed) {
		return n, err
	}

	c.dropMu.Lock()
	defer c.dropMu.Unlock()
	if now := time.Now(); !now.Before(c.nextDropLog) {
		dropped := 1 + c.unreportedDrops.Swap(0)
		log.Printf("vfkit network: dropped frames: count=%d, latest error=%v", dropped, err)
		c.nextDropLog = now.Add(vfkitDropLogInterval)
	} else {
		c.unreportedDrops.Add(1)
	}
	return len(b), nil
}

type vfkitREST struct{}

func (vfkitREST) requestStop(sock string, hard bool) error {
	if hard {
		return vfkitRestState(sock, "HardStop")
	}
	return vfkitRestState(sock, "Stop")
}

func vfkitRestState(sock, state string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Post("http://vfkit/vm/state", "application/json",
		strings.NewReader(fmt.Sprintf(`{"state":%q}`, state)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("vfkit REST %s: %s", state, resp.Status)
	}
	return nil
}

type ptyAnnounce struct{}

func (ptyAnnounce) writer(consoleLog string) (io.WriteCloser, error) {
	return &ptyWatcher{consoleLog: consoleLog}, nil
}

func (ptyAnnounce) mirrorsRunnerLog() bool { return false }

var ptyPattern = regexp.MustCompile(`/dev/ttys[0-9]+`)

// Long enough to reassemble a "/dev/ttysNNN" split across two writes, short
// enough that watching an unbounded log never grows the buffer.
const ptyWatcherCarry = 64

// Scans runner output for the console PTY vfkit announces. The PTY has to be
// opened read-write and drained continuously: the kernel blocks boot until the
// device is opened, and a full buffer stalls the console.
type ptyWatcher struct {
	consoleLog string
	once       bool
	buf        []byte
}

func (w *ptyWatcher) Write(p []byte) (int, error) {
	if !w.once {
		w.buf = append(w.buf, p...)
		if m := ptyPattern.Find(w.buf); m != nil {
			w.once = true
			go w.attach(string(m))
		} else if len(w.buf) > ptyWatcherCarry {
			w.buf = w.buf[len(w.buf)-ptyWatcherCarry:]
		}
	}
	return len(p), nil
}

// The attached PTY is not the runner's output stream and closes itself.
func (w *ptyWatcher) Close() error { return nil }

func (w *ptyWatcher) attach(path string) {
	pty, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console attach %s: %v\n", path, err)
		return
	}
	logf, err := os.Create(w.consoleLog)
	if err != nil {
		pty.Close()
		return
	}
	fmt.Printf("console: %s (log: %s)\n", path, w.consoleLog)
	go func() {
		defer pty.Close()
		defer logf.Close()
		io.Copy(logf, pty) //nolint:errcheck
	}()
}

// The framework's "storage device attachment is invalid" gives no hint that
// another process holds var.img, and reaching this message means reapOrphans
// found nothing, so the holder is unrecognized.
func vfkitRunnerFailureHint(runnerOutput, dir string) string {
	if strings.Contains(runnerOutput, "VZErrorDomain") && strings.Contains(runnerOutput, "storage device attachment is invalid") {
		img := varImagePath(dir)
		return fmt.Sprintf("hint: another process still has %s open, which is what the framework is refusing; find it with `lsof %s`, stop it, and retry", img, img)
	}
	return ""
}
