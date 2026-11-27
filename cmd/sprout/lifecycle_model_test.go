package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Every step of a spec/lifecycle.qnt trace, run against the real lifecycle
// functions on a real state root, must produce the outcome the model chose,
// send STOP exactly when the model does, and leave the directory, record,
// worktree, control socket and locks where the model says.
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
	savedExit, savedInstance, savedBooting := stoppedExitWait, instanceLockWait, bootingServeWait
	stoppedExitWait, instanceLockWait, bootingServeWait = 0, 0, 0
	t.Cleanup(func() { stoppedExitWait, instanceLockWait, bootingServeWait = savedExit, savedInstance, savedBooting })
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			states := loadModelTrace(t, path)
			newModelReplay(t).run(states)
		})
	}
}

type modelState struct {
	dirIno    int
	dlock     map[int]string
	recBundle map[int]string
	worktree  bool
	claimed   map[string]int
	pc        map[string]string
	building  map[string]string
	proc      string
	act       string
	ino       int
}

func loadModelTrace(t *testing.T, path string) []modelState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		States []struct {
			DirIno    json.RawMessage `json:"dirIno"`
			Dlock     json.RawMessage `json:"dlock"`
			RecBundle json.RawMessage `json:"recBundle"`
			Worktree  bool            `json:"worktree"`
			Claimed   json.RawMessage `json:"claimed"`
			PC        json.RawMessage `json:"pc"`
			Building  json.RawMessage `json:"building"`
			LastStep  struct {
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
			dirIno:    itfInt(t, raw.DirIno),
			dlock:     map[int]string{},
			recBundle: map[int]string{},
			worktree:  raw.Worktree,
			claimed:   map[string]int{},
			pc:        map[string]string{},
			building:  map[string]string{},
			proc:      raw.LastStep.Proc,
			act:       raw.LastStep.Act,
			ino:       itfInt(t, raw.LastStep.Ino),
		}
		for _, kv := range itfMap(t, raw.Dlock) {
			s.dlock[itfInt(t, kv[0])] = itfString(t, kv[1])
		}
		for _, kv := range itfMap(t, raw.RecBundle) {
			s.recBundle[itfInt(t, kv[0])] = itfString(t, kv[1])
		}
		for _, kv := range itfMap(t, raw.Claimed) {
			s.claimed[itfString(t, kv[0])] = itfInt(t, kv[1])
		}
		for _, kv := range itfMap(t, raw.PC) {
			s.pc[itfString(t, kv[0])] = itfString(t, kv[1])
		}
		for _, kv := range itfMap(t, raw.Building) {
			s.building[itfString(t, kv[0])] = itfString(t, kv[1])
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

// The model's acts in which the code must send STOP, and those among them in
// which the daemon exits at once rather than lingering.
var (
	modelStopActs = map[string]bool{
		"stop.exited": true, "stop.lingering": true,
		"delete.stopped.removed": true, "delete.stopped.held": true,
		"up.reboot.ok": true, "up.reboot.held": true,
	}
	modelExitActs = map[string]bool{"stop.exited": true, "delete.stopped.removed": true, "up.reboot.ok": true}
)

type modelReplay struct {
	t        *testing.T
	id       string
	dir      string
	worktree string
	bundles  map[string]string
	manifest *Manifest
	// An open fd per incarnation pins its inode, so a freed inode reused by a
	// later incarnation cannot pass for the earlier one in SameFile.
	incarnations map[int]*os.File
	daemons      map[string]*modelDaemon
	tokens       map[string]*attemptToken
	building     map[string]string
	targets      map[string]deleteTarget
}

// Holds a claimed daemon.lock and, once serving, answers PING and STOP on the
// control socket the way a real daemon does, without a VM behind it.
type modelDaemon struct {
	mu         sync.Mutex
	lock       *os.File
	ln         net.Listener
	sock       string
	exitOnStop bool
	stopped    bool
}

func (d *modelDaemon) serve(sock string) error {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.ln, d.sock = ln, sock
	d.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if strings.TrimSpace(line) == "STOP" {
				// Silenced before the reply, so the caller's first PING after
				// it already finds nothing; what the lock does is the model's
				// choice.
				d.mu.Lock()
				d.stopped = true
				d.closeSocketLocked()
				if d.exitOnStop {
					d.lock.Close()
					d.lock = nil
				}
				d.mu.Unlock()
			}
			fmt.Fprintln(conn, "OK") //nolint:errcheck
			conn.Close()
		}
	}()
	return nil
}

func (d *modelDaemon) closeSocketLocked() {
	if d.ln != nil {
		d.ln.Close()
		os.Remove(d.sock)
		d.ln = nil
	}
}

func (d *modelDaemon) exit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeSocketLocked()
	if d.lock != nil {
		d.lock.Close()
		d.lock = nil
	}
}

func (d *modelDaemon) heldLock() *os.File {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lock
}

func newModelReplay(t *testing.T) *modelReplay {
	root := shortStateRoot(t)
	const id = "a0de10000001"
	cleanupSocketDir(t, id)
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	bundles := map[string]string{}
	for _, name := range []string{"A", "B"} {
		bundle := filepath.Join(root, "bundle-"+name)
		if err := os.MkdirAll(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostManifest(t, bundle)
		// `up` hands prepareUpBoot the canonical path intakeBundleArg resolved.
		if bundles[name], err = filepath.EvalSymlinks(bundle); err != nil {
			t.Fatal(err)
		}
	}
	r := &modelReplay{
		t:            t,
		id:           id,
		dir:          dir,
		worktree:     worktree,
		bundles:      bundles,
		manifest:     bootManifest(t),
		incarnations: map[int]*os.File{},
		daemons:      map[string]*modelDaemon{},
		tokens:       map[string]*attemptToken{},
		building:     map[string]string{},
		targets:      map[string]deleteTarget{},
	}
	t.Cleanup(func() {
		for _, f := range r.incarnations {
			f.Close()
		}
		for _, d := range r.daemons {
			d.exit()
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

func (r *modelReplay) identity() *Identity {
	return &Identity{ID: r.id, Name: "feature", KeySource: "directory", Worktree: r.worktree, RepoRoot: r.worktree}
}

func (r *modelReplay) run(states []modelState) {
	r.check(0, states[0])
	for i, s := range states[1:] {
		step := i + 1
		r.primeStop(s.act)
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
		if sent := r.stopSent(); sent != modelStopActs[s.act] {
			r.t.Fatalf("step %d: %s %q sent STOP = %v", step, s.proc, s.act, sent)
		}
		r.reapExited()
		r.check(step, s)
	}
}

func (r *modelReplay) primeStop(act string) {
	for _, d := range r.daemons {
		d.mu.Lock()
		d.exitOnStop, d.stopped = modelExitActs[act], false
		d.mu.Unlock()
	}
}

func (r *modelReplay) stopSent() bool {
	for _, d := range r.daemons {
		d.mu.Lock()
		stopped := d.stopped
		d.mu.Unlock()
		if stopped {
			return true
		}
	}
	return false
}

// Whether a daemon told to STOP during this step still holds daemon.lock.
func (r *modelReplay) stopLingers() bool {
	for _, d := range r.daemons {
		d.mu.Lock()
		lingers := d.stopped && d.lock != nil
		d.mu.Unlock()
		if lingers {
			return true
		}
	}
	return false
}

func (r *modelReplay) reapExited() {
	for p, d := range r.daemons {
		if d.heldLock() == nil {
			d.exit()
			delete(r.daemons, p)
		}
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
		r.building[p] = s.building[p]
		if statErr == nil {
			return "up.publish.existing"
		}
		f, err := os.Open(r.dir)
		if err != nil {
			return fmt.Sprintf("up.publish: created dir cannot be opened: %v", err)
		}
		r.incarnations[s.ino] = f
		return "up.publish.created"
	case "up.boot.stale", "up.boot.converged", "up.boot.held", "up.boot.ok", "up.reboot.held", "up.reboot.ok":
		tok, bundle := r.tokens[p], r.bundles[r.building[p]]
		delete(r.tokens, p)
		delete(r.building, p)
		keepToken := false
		lock, _, err := prepareUpBoot(r.identity(), r.dir, tok, "dev", bundle, r.manifest, 0, &keepToken)
		if keepToken {
			return fmt.Sprintf("up.boot kept its token: %v", err)
		}
		tok.remove()
		rebooted := r.stopSent()
		switch {
		case errors.Is(err, errInstanceDeletedWhileBuilding):
			return "up.boot.stale"
		case errors.Is(err, errUpConverged):
			return "up.boot.converged"
		case err != nil && rebooted:
			return "up.reboot.held"
		case err != nil:
			return "up.boot.held"
		}
		r.daemons[p] = &modelDaemon{lock: lock}
		if rebooted {
			return "up.reboot.ok"
		}
		return "up.boot.ok"
	case "start.notfound", "start.running", "start.held", "start.norecord", "start.ok":
		_, statErr := os.Stat(r.dir)
		lock, _, _, err := prepareStartBoot(r.identity(), r.dir, nil)
		var notFound *instanceNotFoundError
		switch {
		case errors.As(err, &notFound) && statErr != nil:
			return "start.notfound"
		case errors.As(err, &notFound):
			return "start.norecord"
		case errors.Is(err, errInstanceNowServing):
			return "start.running"
		case err != nil:
			return "start.held"
		}
		r.daemons[p] = &modelDaemon{lock: lock}
		return "start.ok"
	case "daemon.serve":
		sockDir, err := prepareSocketDir(socketDirBase(), r.dir)
		if err != nil {
			return fmt.Sprintf("daemon.serve: %v", err)
		}
		sock, err := socketPathIn(sockDir, controlSocketName)
		if err != nil {
			return fmt.Sprintf("daemon.serve: %v", err)
		}
		if err := r.daemons[p].serve(sock); err != nil {
			return fmt.Sprintf("daemon.serve: %v", err)
		}
		return "daemon.serve"
	case "daemon.exit":
		r.daemons[p].exit()
		delete(r.daemons, p)
		return "daemon.exit"
	case "stop.exited", "stop.lingering", "stop.busy", "stop.notrunning":
		err := stopOne(r.id, stopBehavior{})
		switch {
		case err == nil && r.stopLingers():
			return "stop.lingering"
		case err == nil:
			return "stop.exited"
		case strings.Contains(err.Error(), "is not running"):
			return "stop.notrunning"
		}
		return "stop.busy"
	case "delete.select", "delete.select.absent":
		tg, err := newDeleteTarget(r.id)
		if err != nil {
			return "delete.select.absent"
		}
		r.targets[p] = tg
		return "delete.select"
	case "delete.changed", "delete.held", "delete.removed", "delete.stopped.held", "delete.stopped.removed":
		tg := r.targets[p]
		delete(r.targets, p)
		defer tg.marker.Close()
		err := deleteOne(tg)
		stopped := r.stopSent()
		switch {
		case errors.Is(err, errIncarnationChanged):
			return "delete.changed"
		case err != nil && stopped:
			return "delete.stopped.held"
		case err != nil:
			return "delete.held"
		case stopped:
			return "delete.stopped.removed"
		}
		return "delete.removed"
	case "worktree.remove":
		if err := os.RemoveAll(r.worktree); err != nil {
			return fmt.Sprintf("worktree.remove: %v", err)
		}
		return "worktree.remove"
	case "worktree.restore":
		if err := os.MkdirAll(r.worktree, 0o700); err != nil {
			return fmt.Sprintf("worktree.restore: %v", err)
		}
		return "worktree.restore"
	case "prune.select", "prune.select.none":
		ids, err := instanceIDs()
		if err != nil {
			return fmt.Sprintf("prune.select: %v", err)
		}
		targets, err := pruneCandidates(ids)
		if err != nil {
			return fmt.Sprintf("prune.select: %v", err)
		}
		if len(targets) == 0 {
			return "prune.select.none"
		}
		r.targets[p] = targets[0]
		return "prune.select"
	case "prune.changed", "prune.held", "prune.adopted", "prune.removed":
		tg := r.targets[p]
		delete(r.targets, p)
		defer tg.marker.Close()
		err := pruneOne(tg)
		var skip *pruneSkip
		switch {
		case err == nil:
			return "prune.removed"
		case !errors.As(err, &skip):
			return fmt.Sprintf("prune failed instead of skipping: %v", err)
		case errors.Is(err, errIncarnationChanged):
			return "prune.changed"
		case errors.Is(err, errNoLongerOrphaned):
			return "prune.adopted"
		}
		return "prune.held"
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
		got := ""
		if inst, _, err := loadInstance(r.id); err == nil {
			got = inst.Bundle
		}
		if want := r.bundles[s.recBundle[s.dirIno]]; got != want {
			t.Fatalf("step %d (%s): instance.json names bundle %q, model says %q", step, s.act, got, want)
		}
	}
	if _, err := os.Stat(r.worktree); (err == nil) != s.worktree {
		t.Fatalf("step %d (%s): worktree exists = %v, model says %v", step, s.act, err == nil, s.worktree)
	}
	serving := false
	for p, pc := range s.pc {
		serving = serving || (pc == "serving" && s.claimed[p] == s.dirIno && s.dirIno != 0)
	}
	if running := instanceRunning(r.id); running != serving {
		t.Fatalf("step %d (%s): control answers = %v, model has a serving daemon = %v", step, s.act, running, serving)
	}
	if lifecycleLockHeld(t, r.id) {
		t.Fatalf("step %d (%s): lifecycle lock left held after the step", step, s.act)
	}
	for p, ino := range s.claimed {
		var f *os.File
		if d := r.daemons[p]; d != nil {
			f = d.heldLock()
		}
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
