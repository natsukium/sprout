package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Every dial succeeds, as if the guest's sshd were already answering.
type sshUpDialer struct{}

func (sshUpDialer) DialContextTCP(_ context.Context, _ string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	c2.Close()
	return c1, nil
}

func shortReadyWaits(t *testing.T) {
	t.Helper()
	oldTimeout, oldPoll := readyTimeout, readyPoll
	readyTimeout, readyPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { readyTimeout, readyPoll = oldTimeout, oldPoll })
}

// A gated bundle is not ready on SSH alone; the guest's sprout-ready marker
// flips it. Without the gate `up` returns while a bootstrap is still churning.
func TestWaitReadyGatesOnMarkerFile(t *testing.T) {
	shortReadyWaits(t)
	marker := filepath.Join(t.TempDir(), "ready")
	var ready atomic.Bool
	done := make(chan struct{})
	go func() {
		waitReady(context.Background(), sshUpDialer{}, &Instance{GuestIP: "192.0.2.1", Name: "x"}, marker, func() { ready.Store(true) })
		close(done)
	}()

	// SSH is answering the whole time; readiness must still hold back.
	time.Sleep(100 * time.Millisecond)
	if ready.Load() {
		t.Fatal("reported ready before the guest touched the marker")
	}

	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("marker did not unblock readiness")
	}
	if !ready.Load() {
		t.Fatal("onReady not called after the marker appeared")
	}
}
