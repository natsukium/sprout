package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseControlLine(t *testing.T) {
	cases := []struct {
		in      string
		wantCmd string
		wantArg string
	}{
		{"PING\n", "PING", ""},
		{"DIAL ssh\n", "DIAL", "ssh"},
		{"DIAL 192.168.127.2:22\n", "DIAL", "192.168.127.2:22"},
		{"  STOP  \n", "STOP", ""},
		{"DIAL a b c\n", "DIAL", "a b c"},
		{"\n", "", ""},
	}
	for _, c := range cases {
		cmd, arg := parseControlLine(c.in)
		if cmd != c.wantCmd || arg != c.wantArg {
			t.Fatalf("parseControlLine(%q) = (%q, %q), want (%q, %q)", c.in, cmd, arg, c.wantCmd, c.wantArg)
		}
	}
}

// Whatever a local client sends over control.sock, the parser never panics and
// never leaks a newline or surrounding whitespace into the verb.
func FuzzControlLine(f *testing.F) {
	for _, s := range []string{"PING\n", "DIAL ssh\n", "", "   ", "\x00\n", "A B\nC"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		cmd, _ := parseControlLine(line)
		if strings.ContainsAny(cmd, " \t\n\r") {
			t.Fatalf("verb %q contains whitespace", cmd)
		}
	})
}

// The router asks INFO on every request it routes, so "brief" must skip the
// sample — two forks on the host — while the bare form keeps it: a CLI
// predating the argument sends bare INFO and must not lose its CPU/MEM
// columns against a newer daemon.
func TestInfoSkipsTheSampleOnlyForBrief(t *testing.T) {
	for _, tc := range []struct {
		command    string
		wantSample bool
	}{
		{"INFO", true},
		{"INFO brief", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			sampled := false
			restore := sampleProcTree
			sampleProcTree = func(int) (procStats, error) {
				sampled = true
				return procStats{MemBytes: 4096, CPUPct: 12}, nil
			}
			t.Cleanup(func() { sampleProcTree = restore })

			line := askControl(t, &controlServer{inst: &Instance{Name: "webapp"}}, tc.command)
			if sampled != tc.wantSample {
				t.Errorf("%q sampled = %t, want %t", tc.command, sampled, tc.wantSample)
			}
			if got := strings.Contains(line, `"memBytes":4096`); got != tc.wantSample {
				t.Errorf("%q reported memBytes = %t, want %t: %s", tc.command, got, tc.wantSample, line)
			}
		})
	}
}

func askControl(t *testing.T, srv *controlServer, command string) string {
	t.Helper()
	client, daemon := net.Pipe()
	t.Cleanup(func() { client.Close() })
	go srv.handle(context.Background(), daemon)
	if _, err := fmt.Fprintln(client, command); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func stopRecorder() (srv *controlServer, graceful, hard chan struct{}) {
	graceful = make(chan struct{}, 4)
	hard = make(chan struct{}, 4)
	srv = &controlServer{
		inst:     &Instance{Name: "webapp"},
		stop:     func() { graceful <- struct{}{} },
		hardStop: func() { hard <- struct{}{} },
	}
	return srv, graceful, hard
}

// A graceful STOP has already spent stopOnce when a `STOP hard` arrives, so
// the power cut must not go through it.
func TestStopHardEscalatesAGracefulStop(t *testing.T) {
	srv, _, hard := stopRecorder()
	if line := strings.TrimSpace(askControl(t, srv, "STOP")); line != "OK" {
		t.Fatalf("STOP answered %q", line)
	}
	if line := strings.TrimSpace(askControl(t, srv, "STOP hard")); line != "OK hard" {
		t.Fatalf("STOP hard answered %q, want the acknowledgment an old daemon cannot give", line)
	}
	select {
	case <-hard:
	case <-time.After(5 * time.Second):
		t.Fatal("STOP hard after a graceful STOP never reached the power cut")
	}
}

func TestStopAfterStopHardStartsNoGracefulStop(t *testing.T) {
	srv, graceful, hard := stopRecorder()
	askControl(t, srv, "STOP hard")
	<-hard
	askControl(t, srv, "STOP")
	time.Sleep(100 * time.Millisecond)
	if len(graceful) != 0 {
		t.Fatal("a graceful stop started after the hard stop")
	}
}

func TestInfoAdvertisesHardStop(t *testing.T) {
	line := askControl(t, &controlServer{inst: &Instance{Name: "webapp"}}, "INFO brief")
	if !strings.Contains(line, `"hardStop":true`) {
		t.Fatalf("INFO does not advertise STOP hard: %s", line)
	}
}
