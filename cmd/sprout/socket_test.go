package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Stands in for a real ssh-agent, so the bridge runs over ordinary sockets.
func fakeAgent(t *testing.T) string {
	t.Helper()
	// A short temp dir, not t.TempDir(): long subtest names blow past the
	// ~104-char unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "sprout-agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
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
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					fmt.Fprintf(conn, "ACK:%s", line)
				}
			}()
		}
	}()
	return sock
}

// A guest-side connection reaches the host unix socket and gets a reply back.
// A plain TCP listener stands in for gvisor.
func TestSocketBridge(t *testing.T) {
	sock := fakeAgent(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go socketBridge(ln, sock)

	guest, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()

	if _, err := fmt.Fprintf(guest, "hello\n"); err != nil {
		t.Fatal(err)
	}
	_ = guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(guest).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != "ACK:hello\n" {
		t.Fatalf("round trip = %q, want %q", got, "ACK:hello\n")
	}
}

// Adapts a real TCP listener to socketListener, so startSocketForwards runs
// end to end without a virtual network.
type fakeVN struct{ ln net.Listener }

func (f *fakeVN) Listen(network, addr string) (net.Listener, error) { return f.ln, nil }

func TestStartSocketForwardsValidation(t *testing.T) {
	base := &Manifest{}
	base.Guest.GatewayIP = "192.168.127.1"

	t.Run("empty source errors", func(t *testing.T) {
		t.Setenv("SPROUT_TEST_AGENT", "")
		m := *base
		m.Credentials = []CredentialSpec{{Name: "ssh-agent", Strategy: "socket", Source: "$SPROUT_TEST_AGENT", GuestPort: 62222}}
		if err := startSocketForwards(t.Context(), &fakeVN{}, &m); err == nil {
			t.Fatal("want error when source expands empty")
		}
	})

	t.Run("missing guest port errors", func(t *testing.T) {
		m := *base
		m.Credentials = []CredentialSpec{{Name: "ssh-agent", Strategy: "socket", Source: "/tmp/a.sock"}}
		if err := startSocketForwards(t.Context(), &fakeVN{}, &m); err == nil {
			t.Fatal("want error when guest port is unset")
		}
	})

	t.Run("valid socket cred forwards", func(t *testing.T) {
		sock := fakeAgent(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		m := *base
		m.Credentials = []CredentialSpec{{Name: "ssh-agent", Strategy: "socket", Source: sock, GuestPort: 62222}}
		if err := startSocketForwards(t.Context(), &fakeVN{ln: ln}, &m); err != nil {
			t.Fatal(err)
		}
		guest, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer guest.Close()
		fmt.Fprintf(guest, "ping\n")
		_ = guest.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, _ := bufio.NewReader(guest).ReadString('\n')
		if got != "ACK:ping\n" {
			t.Fatalf("got %q", got)
		}
	})
}
