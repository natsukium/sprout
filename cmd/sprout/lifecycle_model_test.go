package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Every step of a spec/lifecycle.qnt trace, run against the real lifecycle
// functions on a real state root, must produce the outcome the model chose,
// and leave the directory, daemon.lock and lifecycle lock where the model says.
// dev/check-model.sh generates the traces and points this test at them.
func TestLifecycleConformsToModel(t *testing.T) {
	dir := os.Getenv("SPROUT_MODEL_TRACES")
	if dir == "" {
		t.Skip("SPROUT_MODEL_TRACES is unset; run dev/check-model.sh")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.itf.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no *.itf.json traces in %s", dir)
	}
	savedDelete, savedInstance := deleteClaimWait, instanceLockWait
	deleteClaimWait, instanceLockWait = 0, 0
	t.Cleanup(func() { deleteClaimWait, instanceLockWait = savedDelete, savedInstance })
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			states := loadModelTrace(t, path)
			newModelReplay(t).run(states)
		})
	}
}

type modelState struct {
	dirIno   int
	dlock    map[int]string
	recorded map[int]bool
	claimed  map[string]int
	proc     string
	act      string
	ino      int
}

func loadModelTrace(t *testing.T, path string) []modelState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		States []struct {
			DirIno   json.RawMessage `json:"dirIno"`
			Dlock    json.RawMessage `json:"dlock"`
			Recorded json.RawMessage `json:"recorded"`
			Claimed  json.RawMessage `json:"claimed"`
			LastStep struct {
				Proc string          `json:"proc"`
				Act  string          `json:"act"`
				Ino  json.RawMessage `json:"ino"`
			} `json:"lastStep"`
		} `json:"states"`
	}
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	states := make([]modelState, 0, len(trace.States))
	for i, raw := range trace.States {
		s := modelState{
			dirIno:   itfInt(t, raw.DirIno),
			dlock:    map[int]string{},
			recorded: map[int]bool{},
			claimed:  map[string]int{},
			proc:     raw.LastStep.Proc,
			act:      raw.LastStep.Act,
			ino:      itfInt(t, raw.LastStep.Ino),
		}
		for _, kv := range itfMap(t, raw.Dlock) {
			s.dlock[itfInt(t, kv[0])] = itfString(t, kv[1])
		}
		for _, kv := range itfMap(t, raw.Recorded) {
			var b bool
			if err := json.Unmarshal(kv[1], &b); err != nil {
				t.Fatalf("ITF bool %s: %v", kv[1], err)
			}
			s.recorded[itfInt(t, kv[0])] = b
		}
		for _, kv := range itfMap(t, raw.Claimed) {
			s.claimed[itfString(t, kv[0])] = itfInt(t, kv[1])
		}
		if s.act == "" {
			t.Fatalf("state %d has no lastStep.act", i)
		}
		states = append(states, s)
	}
	return states
}

func itfInt(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var v struct {
		N string `json:"#bigint"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("ITF int %s: %v", raw, err)
	}
	n, err := strconv.Atoi(v.N)
	if err != nil {
		t.Fatalf("ITF int %s: %v", raw, err)
	}
	return n
}

func itfString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("ITF string %s: %v", raw, err)
	}
	return s
}

func itfMap(t *testing.T, raw json.RawMessage) [][2]json.RawMessage {
	t.Helper()
	var v struct {
		M [][2]json.RawMessage `json:"#map"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("ITF map %s: %v", raw, err)
	}
	return v.M
}

type modelReplay struct {
	t        *testing.T
	id       string
	dir      string
	root     string
	bundle   string
	manifest *Manifest
	// An open fd per incarnation pins its inode, so a freed inode reused by a
	// later incarnation cannot pass for the earlier one in SameFile.
	incarnations map[int]*os.File
	claims       map[string]*os.File
	tokens       map[string]*attemptToken
	targets      map[string]deleteTarget
}

func newModelReplay(t *testing.T) *modelReplay {
	root := shortStateRoot(t)
	const id = "a0de10000001"
	cleanupSocketDir(t, id)
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostManifest(t, bundle)
	r := &modelReplay{
		t:            t,
		id:           id,
		dir:          dir,
		root:         root,
		bundle:       bundle,
		manifest:     bootManifest(t),
		incarnations: map[int]*os.File{},
		claims:       map[string]*os.File{},
		tokens:       map[string]*attemptToken{},
		targets:      map[string]deleteTarget{},
	}
	t.Cleanup(func() {
		for _, f := range r.incarnations {
			f.Close()
		}
		for _, f := range r.claims {
			f.Close()
		}
		for _, tok := range r.tokens {
			tok.Close()
		}
		for _, tg := range r.targets {
			tg.marker.Close()
		}
	})
	return r
}

func (r *modelReplay) run(states []modelState) {
	r.check(0, states[0])
	for i, s := range states[1:] {
		step := i + 1
		done := make(chan string, 1)
		go func() { done <- r.apply(s) }()
		select {
		case got := <-done:
			if got != s.act {
				r.t.Fatalf("step %d: %s took %q, model took %q", step, s.proc, got, s.act)
			}
		case <-time.After(5 * time.Second):
			r.t.Fatalf("step %d: %s %q blocked, but the model never waits", step, s.proc, s.act)
		}
		r.check(step, s)
	}
}

// Returns the outcome the Go code produced, named as the model names it.
func (r *modelReplay) apply(s modelState) string {
	p := s.proc
	switch s.act {
	case "up.publish.created", "up.publish.existing":
		_, statErr := os.Stat(r.dir)
		tok, err := publishAttempt(r.id, r.dir)
		if err != nil {
			return fmt.Sprintf("up.publish error: %v", err)
		}
		r.tokens[p] = tok
		if statErr == nil {
			return "up.publish.existing"
		}
		f, err := os.Open(r.dir)
		if err != nil {
			return fmt.Sprintf("up.publish: created dir cannot be opened: %v", err)
		}
		r.incarnations[s.ino] = f
		return "up.publish.created"
	case "up.boot.stale", "up.boot.held", "up.boot.ok":
		tok := r.tokens[p]
		delete(r.tokens, p)
		keepToken := false
		lock, _, err := prepareUpBoot(upIdentity(r.root, r.id), r.dir, tok, "dev", r.bundle, r.manifest, 0, &keepToken)
		if keepToken {
			return fmt.Sprintf("up.boot kept its token: %v", err)
		}
		tok.remove()
		switch {
		case errors.Is(err, errInstanceDeletedWhileBuilding):
			return "up.boot.stale"
		case err != nil:
			return "up.boot.held"
		}
		r.claims[p] = lock
		return "up.boot.ok"
	case "start.notfound", "start.held", "start.norecord", "start.ok":
		_, statErr := os.Stat(r.dir)
		lock, _, _, err := prepareStartBoot(upIdentity(r.root, r.id), r.dir, nil)
		var notFound *instanceNotFoundError
		switch {
		case errors.As(err, &notFound) && statErr != nil:
			return "start.notfound"
		case errors.As(err, &notFound):
			return "start.norecord"
		case err != nil:
			return "start.held"
		}
		r.claims[p] = lock
		return "start.ok"
	case "daemon.exit":
		r.claims[p].Close()
		delete(r.claims, p)
		return "daemon.exit"
	case "delete.select", "delete.select.absent":
		tg, err := newDeleteTarget(r.id)
		if err != nil {
			return "delete.select.absent"
		}
		r.targets[p] = tg
		return "delete.select"
	case "delete.changed", "delete.held", "delete.removed":
		tg := r.targets[p]
		delete(r.targets, p)
		defer tg.marker.Close()
		err := deleteOne(tg)
		switch {
		case errors.Is(err, errIncarnationChanged):
			return "delete.changed"
		case err != nil:
			return "delete.held"
		}
		return "delete.removed"
	}
	r.t.Fatalf("model action %q has no Go counterpart", s.act)
	return ""
}

func (r *modelReplay) check(step int, s modelState) {
	t := r.t
	t.Helper()
	fi, err := os.Stat(r.dir)
	if s.dirIno == 0 {
		if !os.IsNotExist(err) {
			t.Fatalf("step %d (%s): model has no instance directory, stat = %v", step, s.act, err)
		}
	} else {
		if err != nil {
			t.Fatalf("step %d (%s): model has incarnation %d, stat = %v", step, s.act, s.dirIno, err)
		}
		want, err := r.incarnations[s.dirIno].Stat()
		if err != nil || !os.SameFile(fi, want) {
			t.Fatalf("step %d (%s): directory at the pathname is not incarnation %d", step, s.act, s.dirIno)
		}
		if held, want := daemonLockHeld(r.dir), s.dlock[s.dirIno] != ""; held != want {
			t.Fatalf("step %d (%s): daemon.lock held = %v, model says %v", step, s.act, held, want)
		}
		_, err = os.Stat(instanceRecordPath(r.dir))
		if has, want := err == nil, s.recorded[s.dirIno]; has != want {
			t.Fatalf("step %d (%s): instance.json present = %v, model says %v", step, s.act, has, want)
		}
	}
	if lifecycleLockHeld(t, r.id) {
		t.Fatalf("step %d (%s): lifecycle lock left held after the step", step, s.act)
	}
	for p, ino := range s.claimed {
		f := r.claims[p]
		if (ino != 0) != (f != nil) {
			t.Fatalf("step %d (%s): %s holds a claim = %v, model says incarnation %d", step, s.act, p, f != nil, ino)
		}
		if f == nil {
			continue
		}
		held, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		live, err := os.Stat(filepath.Join(r.dir, "daemon.lock"))
		onLive := err == nil && os.SameFile(held, live)
		if onLive != (ino == s.dirIno) {
			t.Fatalf("step %d (%s): %s's claim is on the live directory = %v, model says incarnation %d of live %d", step, s.act, p, onLive, ino, s.dirIno)
		}
	}
}

func lifecycleLockHeld(t *testing.T, id string) bool {
	t.Helper()
	root, err := instancesRoot()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, ".lifecycle-"+id))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return false
}
