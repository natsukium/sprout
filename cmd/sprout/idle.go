package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// A malformed duration disables auto-stop with a warning rather than failing
// an otherwise-healthy VM.
func startIdleWatch(ctx context.Context, m *Manifest, srv *controlServer) {
	if m.Idle.Action != "stop" {
		return
	}
	after, err := time.ParseDuration(m.Idle.After)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle: invalid idle.after %q: %v; auto-stop disabled\n", m.Idle.After, err)
		return
	}
	srv.sessions.touch(time.Now())
	idleWatcher(ctx, after, srv)
}

// "Idle" is zero active sessions. The clock starts when the last session
// ends, or at readiness for a booted-but-untouched VM.
type sessionTracker struct {
	mu         sync.Mutex
	active     int
	lastActive time.Time
}

func newSessionTracker(now time.Time) *sessionTracker {
	return &sessionTracker{lastActive: now}
}

func (s *sessionTracker) begin() {
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
}

func (s *sessionTracker) end(now time.Time) {
	s.mu.Lock()
	if s.active > 0 {
		s.active--
	}
	if s.active == 0 {
		s.lastActive = now
	}
	s.mu.Unlock()
}

// Restarts the clock without opening a session, so idle.after runs from
// readiness: measured from daemon start, a short one could fire mid-boot.
func (s *sessionTracker) touch(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.mu.Unlock()
}

func (s *sessionTracker) idleFor(now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		return false, 0
	}
	return true, now.Sub(s.lastActive)
}

// Goes through the daemon's own stopOnce, so a manual stop and an idle stop
// cannot race.
func idleWatcher(ctx context.Context, after time.Duration, srv *controlServer) {
	interval := after / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if idle, dur := srv.sessions.idleFor(now); idle && dur >= after {
				fmt.Printf("instance %q idle for %s, auto-stopping\n", srv.inst.Name, after)
				srv.stopOnce.Do(srv.stop)
				return
			}
		}
	}
}
