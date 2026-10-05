package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	sidecarReadyTimeout = 10 * time.Second
	sidecarReadyPoll    = 20 * time.Millisecond
	// virtiofsd exits by itself once its vhost-user peer disconnects; this is
	// how long that is allowed to take after the runner is gone.
	sidecarExitWait  = 2 * time.Second
	sidecarTermWait  = 2 * time.Second
	sidecarLostGrace = time.Second
)

const sidecarLogTailLines = 20

type sidecar struct {
	name string
	cmd  *exec.Cmd
	exit *runnerExit
	out  *prefixWriter
}

type sidecarSet struct {
	procs   []*sidecar
	log     *os.File
	logPath string
	died    chan *sidecar
}

// Each sidecar is ready once its socket file exists. Nothing connects to
// check: virtiofsd serves a single client and exits when that client
// disconnects, so a probe would consume it. The caller has already removed
// the sockets, so a stale one cannot pass for ready.
func startSidecars(specs []SidecarSpec, socks instanceSockets, dir string) (*sidecarSet, error) {
	logPath := sidecarLogPath(dir)
	log, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	set := &sidecarSet{log: log, logPath: logPath, died: make(chan *sidecar, len(specs))}
	var mu sync.Mutex
	for _, spec := range specs {
		out := &prefixWriter{w: log, mu: &mu, prefix: "[" + spec.Name + "] "}
		cmd := exec.Command(spec.Exec[0], spec.Exec[1:]...)
		cmd.Dir = dir
		cmd.Stdout = out
		cmd.Stderr = out
		exit, err := startManaged(cmd)
		if err != nil {
			set.stop()
			return nil, fmt.Errorf("sidecar %s start: %w", spec.Name, err)
		}
		p := &sidecar{name: spec.Name, cmd: cmd, exit: exit, out: out}
		set.procs = append(set.procs, p)
		go func() {
			<-exit.done
			set.died <- p
		}()
		if err := awaitSidecarSocket(socks.named[spec.Ready.Socket], exit); err != nil {
			set.stop()
			return nil, set.failure(p, err.Error())
		}
	}
	return set, nil
}

func awaitSidecarSocket(path string, exit *runnerExit) error {
	deadline := time.Now().Add(sidecarReadyTimeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-exit.done:
			return fmt.Errorf("exited before its socket appeared: %v", exit.err)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("did not create its socket within %s", sidecarReadyTimeout)
		}
		time.Sleep(sidecarReadyPoll)
	}
}

const userNamespaceHint = "hint: the sidecar could not enter its user namespace sandbox; unprivileged user namespaces are disabled on this host (check sysctl kernel.unprivileged_userns_clone, or on Ubuntu kernel.apparmor_restrict_unprivileged_userns)"

func (s *sidecarSet) failure(p *sidecar, what string) error {
	msg := fmt.Sprintf("%s %s (see %s)", p.name, what, s.logPath)
	if tail := p.out.tail(); tail != "" {
		msg += "\n" + tail
		if strings.Contains(tail, "entering sandbox") {
			msg += "\n" + userNamespaceHint
		}
	}
	return errors.New(msg)
}

func (s *sidecarSet) identities() []procIdentity {
	if s == nil {
		return nil
	}
	ids := make([]procIdentity, len(s.procs))
	for i, p := range s.procs {
		ids[i] = p.exit.proc
	}
	return ids
}

// Delivers the first sidecar to exit; nil when there are none.
func (s *sidecarSet) exited() <-chan *sidecar {
	if s == nil {
		return nil
	}
	return s.died
}

func (s *sidecarSet) stop() {
	if s == nil {
		return
	}
	var wg sync.WaitGroup
	for _, p := range s.procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.exit.within(sidecarExitWait) {
				return
			}
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			if p.exit.within(sidecarTermWait) {
				return
			}
			_ = p.cmd.Process.Kill()
			<-p.exit.done
		}()
	}
	wg.Wait()
	for _, p := range s.procs {
		p.out.flush()
	}
	s.log.Close()
}

// Prefixes each line with its sidecar's name, since several sidecars share
// one log, and keeps the last lines for failure reports.
type prefixWriter struct {
	w       io.Writer
	mu      *sync.Mutex
	prefix  string
	partial []byte
	lines   []string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			break
		}
		line := string(p.partial[:i])
		p.partial = p.partial[i+1:]
		if _, err := io.WriteString(p.w, p.prefix+line+"\n"); err != nil {
			return len(b), err
		}
		p.lines = append(p.lines, line)
		if len(p.lines) > sidecarLogTailLines {
			p.lines = p.lines[1:]
		}
	}
	return len(b), nil
}

func (p *prefixWriter) tail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := p.lines
	if len(p.partial) > 0 {
		lines = append(lines[:len(lines):len(lines)], string(p.partial))
	}
	return strings.Join(lines, "\n")
}

func (p *prefixWriter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.partial) > 0 {
		_, _ = io.WriteString(p.w, p.prefix+string(p.partial)+"\n")
		p.partial = nil
	}
}
