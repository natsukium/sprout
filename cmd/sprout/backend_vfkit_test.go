package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

type vfkitWriteStub struct {
	net.Conn
	n   int
	err error
}

func (s vfkitWriteStub) Write([]byte) (int, error) { return s.n, s.err }

func TestVfkitDatagramConnWrite(t *testing.T) {
	frame := make([]byte, 60)
	for _, tc := range []struct {
		name string
		err  error
		drop bool
	}{
		{name: "success"},
		{name: "file table full", err: os.NewSyscallError("sendto", syscall.ENFILE), drop: true},
		{name: "peer missing", err: os.NewSyscallError("sendto", syscall.ENOENT), drop: true},
		{name: "vfkit receive buffer full", err: os.NewSyscallError("sendto", syscall.ENOBUFS)},
		{name: "closed", err: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := vfkitWriteStub{n: len(frame)}
			if tc.err != nil {
				stub.n = 0
				stub.err = &net.OpError{
					Op:  "write",
					Net: "unixgram",
					Err: tc.err,
				}
			}
			wantN, wantErr := stub.n, stub.err
			if tc.drop {
				wantN, wantErr = len(frame), nil
			}
			conn := vfkitDatagramConn{Conn: stub}
			if n, err := conn.Write(frame); n != wantN || err != wantErr {
				t.Fatalf("Write = (%d, %v), want (%d, %v)", n, err, wantN, wantErr)
			}
		})
	}
}

func TestVfkitDatagramConnBoundsDropLogs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		out := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(out)

		frame := make([]byte, 60)
		conn := vfkitDatagramConn{Conn: vfkitWriteStub{err: syscall.ENFILE}}
		for range 100 {
			if n, err := conn.Write(frame); n != len(frame) || err != nil {
				t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(frame))
			}
		}
		if got := logs.String(); strings.Count(got, "\n") != 1 ||
			!strings.Contains(got, "count=1, latest error="+syscall.ENFILE.Error()) {
			t.Fatalf("want one log for the first drop, got %q", got)
		}

		time.Sleep(vfkitDropLogInterval)
		conn.Conn = vfkitWriteStub{err: syscall.ENOENT}
		if n, err := conn.Write(frame); n != len(frame) || err != nil {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(frame))
		}
		if got := logs.String(); strings.Count(got, "\n") != 2 ||
			!strings.Contains(got, "count=100, latest error="+syscall.ENOENT.Error()) {
			t.Fatalf("want a log counting drops since the previous report, got %q", got)
		}

		for range 2 {
			if n, err := conn.Write(frame); n != len(frame) || err != nil {
				t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(frame))
			}
		}
		if got := logs.String(); strings.Count(got, "\n") != 2 {
			t.Fatalf("want no extra logs before recovery, got %q", got)
		}

		conn.Conn = vfkitWriteStub{n: len(frame)}
		for range 2 {
			if n, err := conn.Write(frame); n != len(frame) || err != nil {
				t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(frame))
			}
		}
		if got := logs.String(); strings.Count(got, "\n") != 3 ||
			!strings.Contains(got, "send recovered: unreported drops=2") {
			t.Fatalf("want one recovery log counting both unreported drops, got %q", got)
		}
	})
}
