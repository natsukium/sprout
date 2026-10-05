package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wakeTestID = "aaaa00000050"

// The test instance's bundle does not exist, so this fails right after its
// claim.
func runWakeDaemon() {
	if err := startForeground(&Identity{ID: wakeTestID, Name: "waking"}, takeClaimReport()); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func wakeBehindAnOldDaemon(t *testing.T) (r *router, oldLock *os.File) {
	t.Helper()
	root := shortStateRoot(t)
	dir := newTestInstance(t, root, wakeTestID, "waking", "var-data")
	oldLock, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { oldLock.Close() })

	t.Setenv("SPROUT_TEST_WAKE_CHILD", t.Name())
	restore := wakeInstance
	wakeInstance = func(id string, w *wakeWatch) error {
		return bootDetached(id, []string{"-test.run=^" + t.Name() + "$"}, 0, "boot", false, nil, w)
	}
	t.Cleanup(func() { wakeInstance = restore })

	r = &router{}
	if !r.startWake(wakeTestID) {
		t.Fatal("wake refused")
	}
	if !pollUntil(5*time.Second, 20*time.Millisecond, func() bool { return wakeOf(r).process() != nil }) {
		t.Fatal("the wake's daemon never started")
	}
	return r, oldLock
}

func wakeOf(r *router) *wakeWatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wakes[wakeTestID]
}

// An older daemon holding the lock without serving does not count as the
// wake's claim.
func TestRouterExitWaitsForTheWakesOwnDaemonToClaim(t *testing.T) {
	if os.Getenv("SPROUT_TEST_WAKE_CHILD") == t.Name() {
		runWakeDaemon()
	}
	r, oldLock := wakeBehindAnOldDaemon(t)
	go func() {
		time.Sleep(700 * time.Millisecond)
		oldLock.Close()
	}()
	start := time.Now()
	r.settleWakes(10 * time.Second)
	if took := time.Since(start); took < 600*time.Millisecond || took > 5*time.Second {
		t.Errorf("router exit waited %s, want until the wake's daemon claimed after the old one let go about 700ms in", took)
	}
}

// A wake's daemon still unclaimed at the deadline is terminated and reaped.
func TestRouterExitEndsAWakeThatNeverClaimed(t *testing.T) {
	if os.Getenv("SPROUT_TEST_WAKE_CHILD") == t.Name() {
		runWakeDaemon()
	}
	r, _ := wakeBehindAnOldDaemon(t)
	w := wakeOf(r)

	r.settleWakes(time.Second)
	select {
	case <-w.exited:
	default:
		t.Fatal("router exited with the unclaimed wake's daemon still running")
	}
}

// A request on an already-accepted connection starts no wake once shutdown
// has begun.
func TestRouterStartsNoWakeOnceShutdownBegins(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000052"
	dir := newTestInstance(t, root, id, "late", "var-data")
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "late", KeySource: "directory", GuestIP: "127.0.0.1", Bundle: dir,
	}); err != nil {
		t.Fatal(err)
	}
	woken := make(chan string, 1)
	restore := wakeInstance
	wakeInstance = func(id string, _ *wakeWatch) error { woken <- id; return nil }
	t.Cleanup(func() { wakeInstance = restore })

	r := &router{domain: "sprout.localhost", wake: true}
	addr := startRouter(t, r)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	r.settleWakes(time.Second)
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: late.sprout.localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(resp), "HTTP/1.1 503") || !strings.Contains(string(resp), "shutting down") {
		t.Errorf("a request during shutdown got:\n%s", resp)
	}
	select {
	case id := <-woken:
		t.Errorf("woke %s after shutdown began", id)
	case <-time.After(500 * time.Millisecond):
	}
}

// A wake that ended without forking a daemon does not hold the router's exit.
func TestRouterExitStopsWaitingForAnEndedWake(t *testing.T) {
	shortStateRoot(t)
	const id = "aaaa00000051"
	w := newWakeWatch()
	r := &router{wakes: map[string]*wakeWatch{id: w}}
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(w.ended)
	}()
	start := time.Now()
	r.settleWakes(10 * time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("router exit waited %s on a wake that had already ended", took)
	}
}
