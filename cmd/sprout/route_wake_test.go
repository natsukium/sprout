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

// A wake's daemon claims its instance only after the router forked it, so a
// router that exited in between would leave a boot no following stop sees.
func TestRouterExitWaitsForEachWakeToClaimItsInstance(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00000050"
	dir := newTestInstance(t, root, id, "waking", "var-data")
	r := &router{waking: map[string]bool{id: true}}

	claimed := make(chan *os.File, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		lock, err := acquireInstanceLock(dir, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		claimed <- lock
	}()
	start := time.Now()
	r.settleWakes(10 * time.Second)
	took := time.Since(start)
	if lock := <-claimed; lock != nil {
		defer lock.Close()
	}
	if took < 400*time.Millisecond || took > 5*time.Second {
		t.Errorf("router exit waited %s, want until the claim about 500ms in", took)
	}
}

// Closing the listeners leaves an accepted connection's handler running, so a
// request that arrives on it once shutdown has begun must not start a boot
// that nothing then waits for.
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
	wakeInstance = func(id string) error { woken <- id; return nil }
	t.Cleanup(func() { wakeInstance = restore })

	r := &router{domain: "sprout.localhost", wake: true, waking: map[string]bool{}}
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

// A wake that fails before its daemon claims anything must not hold the
// router's exit to the full wait.
func TestRouterExitStopsWaitingForAnEndedWake(t *testing.T) {
	shortStateRoot(t)
	const id = "aaaa00000051"
	r := &router{waking: map[string]bool{id: true}}
	go func() {
		time.Sleep(300 * time.Millisecond)
		r.mu.Lock()
		delete(r.waking, id)
		r.mu.Unlock()
	}()
	start := time.Now()
	r.settleWakes(10 * time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("router exit waited %s on a wake that had already ended", took)
	}
}
