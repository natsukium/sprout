package main

import (
	"context"
	"testing"
	"time"
)

// Idleness means an empty session count, measured from the moment the last
// session ended.
func TestSessionTrackerIdleClock(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	tr := newSessionTracker(t0)

	// Fresh tracker: idle since creation.
	if idle, dur := tr.idleFor(t0.Add(10 * time.Second)); !idle || dur != 10*time.Second {
		t.Fatalf("fresh tracker idleFor = (%v, %v), want (true, 10s)", idle, dur)
	}

	// An open session is never idle, however long it runs.
	tr.begin()
	if idle, _ := tr.idleFor(t0.Add(time.Hour)); idle {
		t.Fatal("tracker with an active session reported idle")
	}

	// Closing the session restarts the clock from the close time.
	tr.end(t0.Add(20 * time.Second))
	if idle, dur := tr.idleFor(t0.Add(25 * time.Second)); !idle || dur != 5*time.Second {
		t.Fatalf("after end idleFor = (%v, %v), want (true, 5s)", idle, dur)
	}
}

// The VM stays busy until the last overlapping session closes, so a second
// `sprout shell` is not cut off when the first disconnects.
func TestSessionTrackerConcurrentSessions(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	tr := newSessionTracker(t0)

	tr.begin()
	tr.begin()
	tr.end(t0.Add(time.Second))
	if idle, _ := tr.idleFor(t0.Add(2 * time.Second)); idle {
		t.Fatal("reported idle while a session was still active")
	}
	tr.end(t0.Add(3 * time.Second))
	if idle, dur := tr.idleFor(t0.Add(4 * time.Second)); !idle || dur != time.Second {
		t.Fatalf("after last end idleFor = (%v, %v), want (true, 1s)", idle, dur)
	}
}

// touch restarts the clock without opening a session, which is how idle timing
// starts at readiness rather than at boot.
func TestSessionTrackerTouch(t *testing.T) {
	t0 := time.Unix(3_000_000, 0)
	tr := newSessionTracker(t0)
	tr.touch(t0.Add(time.Minute))
	if idle, dur := tr.idleFor(t0.Add(90 * time.Second)); !idle || dur != 30*time.Second {
		t.Fatalf("after touch idleFor = (%v, %v), want (true, 30s)", idle, dur)
	}
}

// The watcher loop itself, not just the tracker it reads: idle past the
// threshold fires the daemon's stop exactly once and ends the loop.
func TestIdleWatcherStopsAnIdleInstance(t *testing.T) {
	stopped := make(chan struct{})
	srv := &controlServer{
		inst:     &Instance{Name: "idle-test"},
		sessions: newSessionTracker(time.Now().Add(-time.Hour)),
		stop:     func() { close(stopped) },
	}
	done := make(chan struct{})
	go func() {
		// after=1s clamps the tick interval to its 1s floor, so the first tick
		// already sees an hour of idleness.
		idleWatcher(context.Background(), time.Second, srv)
		close(done)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("idle watcher never fired for an instance idle past the threshold")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle watcher kept running after firing the stop")
	}
}

// Daemon shutdown ends the watcher without a spurious stop.
func TestIdleWatcherHonorsCancellation(t *testing.T) {
	srv := &controlServer{
		inst:     &Instance{Name: "busy-test"},
		sessions: newSessionTracker(time.Now()),
		stop:     func() { t.Error("stop fired for an instance that was never idle long enough") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		idleWatcher(ctx, time.Hour, srv)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle watcher ignored context cancellation")
	}
}
