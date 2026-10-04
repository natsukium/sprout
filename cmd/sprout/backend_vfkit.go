package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
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
	return nil
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

func (ptyAnnounce) writer(consoleLog string) io.Writer {
	return &ptyWatcher{consoleLog: consoleLog}
}

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
