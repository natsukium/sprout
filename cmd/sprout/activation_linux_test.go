package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSystemdListenFDsPicksTheNamedSockets(t *testing.T) {
	all, named, err := systemdListenFDs("42", "4", "route:other:route:other", 42, "route")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{3, 4, 5, 6}; !reflect.DeepEqual(all, want) {
		t.Errorf("all = %v, want %v", all, want)
	}
	if want := []int{3, 5}; !reflect.DeepEqual(named, want) {
		t.Errorf("named = %v, want %v (one socket per ListenStream under the same name)", named, want)
	}
}

func TestSystemdListenFDsRefusesWhatSystemdDidNotHandToThisProcess(t *testing.T) {
	for _, c := range []struct {
		desc, pid, count, names, want string
	}{
		{"not under systemd", "", "", "", "only works under a socket unit"},
		{"addressed to the parent that spawned this process", "41", "1", "route", "another process"},
		{"no descriptors", "42", "0", "", "passes no sockets"},
		{"unnamed descriptors", "42", "2", "route", "FileDescriptorName=route"},
		{"another socket unit's descriptors", "42", "1", "sprout-route.socket", "only sprout-route.socket"},
	} {
		t.Run(c.desc, func(t *testing.T) {
			_, named, err := systemdListenFDs(c.pid, c.count, c.names, 42, "route")
			if err == nil {
				t.Fatalf("accepted, adopting %v", named)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// Runs in a child that received the listener as fd 3, the way systemd hands
// one over: LISTEN_PID can only be the child's own pid once it exists, so the
// child sets it before adopting.
func TestActivatedListenersAdoptsTheSystemdSocket(t *testing.T) {
	if os.Getenv("SPROUT_TEST_ACTIVATION") == "child" {
		os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
		lns, err := activatedListeners("route")
		if err != nil {
			fmt.Println("adopt:", err)
			os.Exit(1)
		}
		for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
			if val, ok := os.LookupEnv(v); ok {
				fmt.Printf("%s=%s left for children to inherit\n", v, val)
				os.Exit(1)
			}
		}
		conn, err := lns[0].Accept()
		if err != nil {
			fmt.Println("accept:", err)
			os.Exit(1)
		}
		fmt.Fprintln(conn, "adopted")
		conn.Close()
		os.Exit(0)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), "SPROUT_TEST_ACTIVATION=child", "LISTEN_FDS=1", "LISTEN_FDNAMES=route")
	child.ExtraFiles = []*os.File{f}
	out := &strings.Builder{}
	child.Stdout, child.Stderr = out, out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		child.Process.Kill()
		child.Wait()
		t.Fatalf("dial: %v\n%s", err, out)
	}
	defer conn.Close()
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if err := child.Wait(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if line != "adopted\n" {
		t.Errorf("read %q from the handed-over socket, want the child's reply", line)
	}
}
