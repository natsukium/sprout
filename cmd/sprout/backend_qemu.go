package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// Graceful is the ACPI power button rather than microvm.nix's shutdown
// helper, which sends ctrl-alt-del through socat and so depends on both a host
// tool and the guest mapping that key to poweroff.
type qmpControl struct{}

func (qmpControl) requestStop(sock string, hard bool) error {
	if hard {
		return qmpCommand(sock, "quit", true)
	}
	return qmpCommand(sock, "system_powerdown", false)
}

// QEMU's monitor serves one client at a time and leaves the next connected
// but without a greeting, so the deadline has to cover the greeting too.
var qmpTimeout = 5 * time.Second

// exits marks a command QEMU may die on before replying, for which the
// connection closing is the success it asked for.
func qmpCommand(sock, command string, exits bool) error {
	conn, err := net.DialTimeout("unix", sock, qmpTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(qmpTimeout)); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	var greeting struct {
		QMP json.RawMessage `json:"QMP"`
	}
	if err := dec.Decode(&greeting); err != nil {
		return fmt.Errorf("QMP greeting: %w", err)
	}
	if greeting.QMP == nil {
		return errors.New("QMP greeting: not a QMP monitor")
	}
	if err := qmpExecute(conn, dec, "qmp_capabilities"); err != nil {
		return err
	}
	err = qmpExecute(conn, dec, command)
	if exits && connectionClosed(err) {
		return nil
	}
	return err
}

type qmpReply struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

// Events (POWERDOWN, SHUTDOWN, …) can arrive ahead of the reply and are
// skipped.
func qmpExecute(conn net.Conn, dec *json.Decoder, command string) error {
	if _, err := fmt.Fprintf(conn, "{\"execute\":%q}\n", command); err != nil {
		return fmt.Errorf("QMP %s: %w", command, err)
	}
	for {
		var r qmpReply
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("QMP %s: %w", command, err)
		}
		if r.Error != nil {
			return fmt.Errorf("QMP %s: %s: %s", command, r.Error.Class, r.Error.Desc)
		}
		if r.Return != nil {
			return nil
		}
	}
}

func connectionClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// The runner's output is the serial console itself, so console.log mirrors
// runner.log and `sprout logs -f` follows the same file for every backend.
type stdioConsole struct{}

func (stdioConsole) writer(consoleLog string) (io.WriteCloser, error) {
	return os.Create(consoleLog)
}

func (stdioConsole) mirrorsRunnerLog() bool { return true }

// QEMU locks its disk images, so a second holder of var.img is named outright
// rather than inferred as it is for vfkit.
func qemuRunnerFailureHint(runnerOutput, dir string) string {
	switch {
	case strings.Contains(runnerOutput, `Failed to get "write" lock`):
		img := varImagePath(dir)
		return fmt.Sprintf("hint: another process still has %s open; find it with `fuser -v %s` or `lsof %s`, stop it, and retry", img, img, img)
	case strings.Contains(runnerOutput, "/dev/kvm") || strings.Contains(runnerOutput, "Could not access KVM kernel module"):
		return "hint: QEMU could not use KVM; `sprout doctor` checks the KVM prerequisites"
	}
	return ""
}
