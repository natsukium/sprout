package main

import (
	"strings"
	"testing"
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
