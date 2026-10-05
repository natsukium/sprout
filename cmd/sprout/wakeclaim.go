package main

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const claimFDEnv = "SPROUT_CLAIM_FD"

// Cancelling refuses a launch still to come, so no fork follows a finished
// settle.
type wakeWatch struct {
	claimed chan struct{}
	exited  chan struct{}
	ended   chan struct{}

	mu        sync.Mutex
	proc      *os.Process
	cancelled bool
}

func newWakeWatch() *wakeWatch {
	return &wakeWatch{claimed: make(chan struct{}), exited: make(chan struct{}), ended: make(chan struct{})}
}

var errWakeCancelled = errors.New("the router is shutting down")

func (w *wakeWatch) launch(start func(report *os.File) (*os.Process, error)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancelled {
		return errWakeCancelled
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		return err
	}
	proc, err := start(wr)
	wr.Close()
	if err != nil {
		rd.Close()
		return err
	}
	w.proc = proc
	go func() {
		defer rd.Close()
		var b [1]byte
		if n, _ := rd.Read(b[:]); n > 0 {
			close(w.claimed)
		}
	}()
	return nil
}

// The wake ending is not enough: it also ends when the readiness wait gives
// up, with the daemon still queued for the lock.
func (w *wakeWatch) settle(deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ended := w.ended
	for {
		select {
		case <-w.claimed:
			return
		case <-w.exited:
			return
		case <-ended:
			if w.process() == nil {
				return
			}
			ended = nil
			continue
		case <-timer.C:
		}
		break
	}
	w.mu.Lock()
	w.cancelled = true
	proc := w.proc
	w.mu.Unlock()
	if proc == nil {
		return
	}
	// Unclaimed, it has not reached runDaemon's handler, so SIGTERM ends it;
	// one that claims meanwhile turns the signal into stopBeforeBoot's abort.
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-w.exited:
		return
	case <-time.After(wakeTerminateWait):
	}
	_ = proc.Kill()
	select {
	case <-w.exited:
	case <-time.After(wakeTerminateWait):
	}
}

func (w *wakeWatch) process() *os.Process {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.proc
}

var wakeTerminateWait = 5 * time.Second

// Close-on-exec so no runner or sidecar holds the pipe open past the daemon.
func takeClaimReport() *os.File {
	v, ok := os.LookupEnv(claimFDEnv)
	if !ok {
		return nil
	}
	os.Unsetenv(claimFDEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "claim-report")
}

func reportClaimed(f *os.File) {
	if f == nil {
		return
	}
	_, _ = f.Write([]byte{1})
	f.Close()
}
