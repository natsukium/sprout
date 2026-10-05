package main

import (
	"os"
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
