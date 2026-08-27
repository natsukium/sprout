package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only `up` and `fork` may bring an instance directory into existence, so a
// claim of an unknown id has to read as "no such instance" and leave the state
// root exactly as it found it.
func TestClaimInstanceNeverCreatesTheDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "eeee00000001"

	lock, err := claimInstance(id, 0, nil)
	if err == nil {
		lock.Close()
		t.Fatal("claimInstance of an id with no state succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claimInstance error = %v, want it to unwrap to os.ErrNotExist", err)
	}
	dir := filepath.Join(root, "sprout", "instances", id)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("claimInstance created %s (stat err = %v)", dir, err)
	}
}

// Two callers naming the same two instances in opposite orders must both keep
// making progress; taking the pair in the caller's order instead of a canonical
// one would leave each waiting on the lock the other already holds. The
// returned locks still follow the argument order, whichever order they were
// taken in.
func TestAcquireLifecyclePairDoesNotDeadlockOnOppositeOrders(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const (
		lower  = "aaaa00000001"
		higher = "aaaa00000002"
	)

	pairOnce := func(a, b string) {
		lockA, lockB, err := acquireLifecyclePair(a, b)
		if err != nil {
			t.Errorf("acquireLifecyclePair(%s, %s): %v", a, b, err)
			return
		}
		defer lockB.Close()
		defer lockA.Close()
		if got := filepath.Base(lockA.Name()); got != ".lifecycle-"+a {
			t.Errorf("first returned lock = %s, want the one for the first argument %s", got, a)
		}
		if got := filepath.Base(lockB.Name()); got != ".lifecycle-"+b {
			t.Errorf("second returned lock = %s, want the one for the second argument %s", got, b)
		}
	}

	const rounds = 50
	done := make(chan struct{}, 2)
	for _, order := range [][2]string{{lower, higher}, {higher, lower}} {
		go func() {
			defer func() { done <- struct{}{} }()
			for range rounds {
				pairOnce(order[0], order[1])
			}
		}()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("opposite-order callers of acquireLifecyclePair deadlocked")
		}
	}
}

// The confirmation prompt runs unlocked, so what the user agreed to must be
// pinned by the held fd: a directory renamed away and recreated at the same
// path is a different instance, however identical the pathname looks.
func TestIncarnationMarkerDetectsAReplacedDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "instance")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	marker, err := openIncarnationMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close()
	if err := marker.verify(dir); err != nil {
		t.Fatalf("verify of an untouched directory: %v", err)
	}

	if err := os.Rename(dir, filepath.Join(root, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	err = marker.verify(dir)
	if err == nil {
		t.Fatal("verify accepted a directory recreated at the same path")
	}
	if !strings.Contains(err.Error(), "changed since confirmation") {
		t.Fatalf("unexpected error: %v", err)
	}
}
