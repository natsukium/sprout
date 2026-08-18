package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Dialed through the short socket directory (see socketdir.go); doing that on
// every dial is also what heals the symlink after a /tmp cleanup.
func controlDial(id string) (net.Conn, error) {
	dir, err := instanceDir(id)
	if err != nil {
		return nil, err
	}
	sock, err := socketPath(id, dir, controlSocketName)
	if err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", sock, 2*time.Second)
}

// Every verb answers one status line beginning with OK or ERR. The reader is
// returned because a DIAL's stream continues through it: the daemon may have
// written guest bytes into the same buffer as the status line.
func controlRoundTrip(conn net.Conn, command string) (*bufio.Reader, string, error) {
	if _, err := fmt.Fprintln(conn, command); err != nil {
		return nil, "", err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, "", err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK") {
		return nil, line, errControlRejected
	}
	return r, line, nil
}

var errControlRejected = errors.New("rejected")

func controlRequest(id, command string) (string, error) {
	conn, err := controlDial(id)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, line, err := controlRoundTrip(conn, command)
	if err != nil {
		if errors.Is(err, errControlRejected) {
			return "", fmt.Errorf("control: %s", controlReason(line))
		}
		return "", err
	}
	return line, nil
}

// Answering PING is what "running" means everywhere in this binary: the daemon
// holds the control socket, so a reply proves it is alive and serving. It does
// not prove the guest has finished booting — INFO's Ready reports that.
func instanceRunning(id string) bool {
	_, err := controlRequest(id, "PING")
	return err == nil
}

// The returned reader may already hold guest bytes past the status line, so
// callers must keep reading from it, never from conn.
func dialHandshake(conn net.Conn, addr string) (*bufio.Reader, error) {
	r, line, err := controlRoundTrip(conn, "DIAL "+addr)
	if err != nil {
		if errors.Is(err, errControlRejected) {
			return nil, &controlRejectedError{addr: addr, reply: line}
		}
		return nil, err
	}
	return r, nil
}

// A refusal came back from the address the daemon dialed, so unlike every
// other dial failure it proves the daemon itself was there.
type controlRejectedError struct {
	addr  string
	reply string
}

func (e *controlRejectedError) Error() string { return fmt.Sprintf("dial %s: %s", e.addr, e.reply) }

func (e *controlRejectedError) Unwrap() error { return errControlRejected }

func (e *controlRejectedError) reason() string {
	return controlReason(e.reply)
}

func controlReason(reply string) string {
	return strings.TrimSpace(strings.TrimPrefix(reply, "ERR"))
}

type controlInfo struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	GuestIP    string `json:"guestIp"`
	PID        int    `json:"pid"`
	UptimeSec  int    `json:"uptimeSec"`
	Ready      bool   `json:"ready"`
	// Zero means the sample failed.
	MemBytes int64   `json:"memBytes"`
	CPUPct   float64 `json:"cpuPct"`
}

func queryInfo(id string) (*controlInfo, error) {
	line, err := controlRequest(id, "INFO")
	if err != nil {
		return nil, err
	}
	_, payload, ok := strings.Cut(line, " ")
	if !ok {
		return nil, fmt.Errorf("malformed INFO response: %q", line)
	}
	var info controlInfo
	if err := json.Unmarshal([]byte(payload), &info); err != nil {
		return nil, err
	}
	return &info, nil
}
