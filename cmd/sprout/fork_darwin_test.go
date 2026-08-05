//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForkLiveSucceedsOnCoW(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "cccc2222dddd"
	srcDir := newTestInstance(t, root, srcID, "source", "running /var")

	lock, err := acquireInstanceLock(srcDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := cmdFork(srcID, true, "forked"); err != nil {
		t.Fatalf("fork --live: %v", err)
	}

	ids, err := instancesNamed("forked")
	if err != nil || len(ids) != 1 {
		t.Fatalf("instancesNamed(forked) = %v (err %v), want exactly one", ids, err)
	}
	_, dstDir, err := loadInstance(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "var.img"))
	if err != nil || string(got) != "running /var" {
		t.Fatalf("forked /var = %q (err %v), want the source's", got, err)
	}
}

// A source being written while it is read has no consistent state to copy.
func TestForkRefusesRunningSourceWithoutLive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	work := t.TempDir()
	t.Chdir(work)

	srcID := "cccc3333dddd"
	srcDir := newTestInstance(t, root, srcID, "source", "running /var")

	lock, err := acquireInstanceLock(srcDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	err = cmdFork(srcID, false, "forked")
	if err == nil {
		t.Fatal("fork of a running source succeeded without --live")
	}
	if ids, _ := instancesNamed("forked"); len(ids) != 0 {
		t.Fatalf("a refused fork left state behind: %v", ids)
	}
}
