package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"pgregory.net/rapid"
)

func manifestFrom(subs [][2]string) *Manifest {
	m := &Manifest{}
	for _, s := range subs {
		m.Substitutions = append(m.Substitutions, struct {
			Placeholder string `json:"placeholder"`
			Value       string `json:"value"`
		}{Placeholder: s[0], Value: s[1]})
	}
	return m
}

func runRewrite(t *testing.T, runner string, m *Manifest, values map[string]string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "runner")
	out := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(in, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteRunner(in, m, values, out); err != nil {
		return "", err
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(got), nil
}

func TestRewriteRunner(t *testing.T) {
	t.Run("replaces every occurrence", func(t *testing.T) {
		m := manifestFrom([][2]string{{"@NET@", "netSocket"}})
		got, err := runRewrite(t, "a @NET@ b @NET@ c", m, map[string]string{"netSocket": "/x.sock"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "a /x.sock b /x.sock c" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unknown symbol errors", func(t *testing.T) {
		m := manifestFrom([][2]string{{"@X@", "mystery"}})
		if _, err := runRewrite(t, "@X@", m, map[string]string{}); err == nil {
			t.Fatal("want error for unknown symbol")
		}
	})

	t.Run("missing placeholder errors", func(t *testing.T) {
		m := manifestFrom([][2]string{{"@X@", "sym"}})
		if _, err := runRewrite(t, "no placeholder here", m, map[string]string{"sym": "v"}); err == nil {
			t.Fatal("want error for missing placeholder")
		}
	})

	t.Run("shell-unsafe value errors", func(t *testing.T) {
		for _, unsafe := range []string{"/home/u/My Projects", "/home/u/it's", "$HOME/ws", ""} {
			m := manifestFrom([][2]string{{"@WS@", "workspace"}})
			if _, err := runRewrite(t, "@WS@", m, map[string]string{"workspace": unsafe}); err == nil {
				t.Errorf("value %q accepted; want error", unsafe)
			}
		}
	})

	// The full punctuation escapeShellArg leaves unquoted must pass, or paths
	// like ~/repo-name and values like "virtio-serial,pty" start erroring.
	t.Run("safe punctuation accepted", func(t *testing.T) {
		m := manifestFrom([][2]string{{"@V@", "consolePty"}})
		if _, err := runRewrite(t, "@V@", m, map[string]string{"consolePty": "virtio-serial,pty_+:@%/=-."}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// One placeholder a prefix of another must not corrupt the rewrite:
	// sequential ReplaceAll would rewrite the substring inside the longer
	// placeholder, then report the longer one missing.
	t.Run("prefix-colliding placeholders", func(t *testing.T) {
		m := manifestFrom([][2]string{
			{"/sprout/placeholder/credential/aws", "credential:aws"},
			{"/sprout/placeholder/credential/aws-extra", "credential:aws-extra"},
		})
		runner := "A=/sprout/placeholder/credential/aws B=/sprout/placeholder/credential/aws-extra"
		got, err := runRewrite(t, runner, m, map[string]string{
			"credential:aws":       "/home/u/.aws",
			"credential:aws-extra": "/home/u/.aws-extra",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "A=/home/u/.aws B=/home/u/.aws-extra" {
			t.Fatalf("prefix collision corrupted output: %q", got)
		}
	})
}

// Two invariants over many placeholders, some sharing prefixes: no placeholder
// text survives, and each is replaced by exactly its mapped value.
func TestRewriteRunnerProperties(t *testing.T) {
	// Prefix-colliding tokens, so drawing several reproduces the case that
	// motivated the single-pass replacer.
	pool := []string{"p", "pp", "ppp", "q", "qq", "r", "rs", "rst"}
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, len(pool)).Draw(t, "n")
		type sub struct {
			placeholder string
			symbol      string
			value       string
		}
		var subs []sub
		seen := map[string]bool{}
		for i := 0; i < n; i++ {
			token := rapid.SampledFrom(pool).Draw(t, "token")
			// No trailing delimiter, as real placeholders have none, so "p" and
			// "pp" genuinely collide.
			base := "@P" + token
			if seen[base] {
				continue
			}
			seen[base] = true
			subs = append(subs, sub{
				placeholder: base,
				symbol:      "sym-" + token,
				// Percent-delimited: never mistakable for an @P placeholder,
				// and inside the charset the rewrite accepts.
				value: "%v-" + token + "%",
			})
		}

		var pairs [][2]string
		values := map[string]string{}
		var runner strings.Builder
		for _, s := range subs {
			pairs = append(pairs, [2]string{s.placeholder, s.symbol})
			values[s.symbol] = s.value
			runner.WriteString("x=" + s.placeholder + " ")
		}
		m := manifestFrom(pairs)

		// rapid.T has no TempDir.
		dir, err := os.MkdirTemp("", "rewrite")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		in := filepath.Join(dir, "runner")
		out := filepath.Join(dir, "run.sh")
		if err := os.WriteFile(in, []byte(runner.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rewriteRunner(in, m, values, out); err != nil {
			t.Fatalf("rewriteRunner: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		result := string(data)
		for _, s := range subs {
			if strings.Contains(result, s.placeholder+" ") {
				t.Fatalf("placeholder %q survived in %q", s.placeholder, result)
			}
			if !strings.Contains(result, s.value) {
				t.Fatalf("value %q for %q missing in %q", s.value, s.placeholder, result)
			}
		}
	})
}

// A detached daemon outlives the command that started it, so a caller that
// wrapped `sprout up` in `flock 9> lock` handed it the boot lock for the VM's
// whole life. The inherited case runs too, so a regression cannot pass by
// making the test blind to the leak.
func TestDropInheritedDescriptorsReleasesCallerLocks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drop     bool
		wantHeld bool
	}{
		{name: "inherited", drop: false, wantHeld: true},
		{name: "dropped", drop: true, wantHeld: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := lockHeldAfterChild(t, tc.drop)
			if held != tc.wantHeld {
				t.Errorf("lock held after the caller closed it = %v, want %v", held, tc.wantHeld)
			}
		})
	}
}

// Mimics a shell's `flock 9> lock` around a command that leaves a child running,
// and reports whether the lock survives the caller's own descriptor.
func lockHeldAfterChild(t *testing.T, drop bool) bool {
	t.Helper()
	lockPath := filepath.Join(t.TempDir(), "boot.lock")
	f, err := os.Create(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// dup, because Go opens its own files close-on-exec: a descriptor from a
	// shell redirection arrives without the flag, which is what makes the leak
	// possible.
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("locking %s: %v", lockPath, err)
	}

	if drop {
		dropInheritedDescriptors()
	}
	child := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	// Past this close only an inheriting child can still be holding the lock.
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil
}

// stdin/stdout/stderr belong to the command the user ran: marking them would
// only change what unrelated children see.
func TestDropInheritedDescriptorsLeavesStdioAlone(t *testing.T) {
	before := stdioFlags(t)
	dropInheritedDescriptors()
	if after := stdioFlags(t); after != before {
		t.Errorf("stdio descriptor flags = %v, want %v", after, before)
	}
}

func stdioFlags(t *testing.T) [3]int {
	t.Helper()
	var flags [3]int
	for fd := range flags {
		got, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("F_GETFD on fd %d: %v", fd, err)
		}
		flags[fd] = got
	}
	return flags
}

// Simulates the losing side of a boot race: the instance flock is already
// held, and the winner's control socket starts answering only after delay. The
// socket is bound up front and renamed into place on schedule, because binding
// it from the timing goroutine would put t.Fatal off the test goroutine.
func holdLockAndServeLater(t *testing.T, dir string, delay time.Duration) {
	t.Helper()
	held, err := acquireInstanceLock(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })

	pending := filepath.Join(dir, "control.sock.pending")
	ln, err := net.Listen("unix", pending)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &fakeDaemon{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handle(conn)
		}
	}()
	go func() {
		time.Sleep(delay)
		_ = os.Rename(pending, filepath.Join(dir, "control.sock"))
	}()
}

// A second `up` that reads `stopped` while the first is mid-boot must
// converge to the first's daemon and exit clean, not wait out the instance
// lock and fail.
func TestUpForegroundHandsOffToConcurrentBoot(t *testing.T) {
	root := shortStateRoot(t)
	const id = "upbootrace"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostManifest(t, bundle)
	withBootableHostBackend(t)
	// The record's bundle matches the one being booted, so the post-handoff
	// re-check must settle on "already running" rather than a reboot.
	if err := writeJSON(filepath.Join(dir, "instance.json"), &Instance{
		ID: id, Name: "webapp", KeySource: "directory", Bundle: bundle, GuestIP: "127.0.0.1", Platform: hostPlatform(t),
	}); err != nil {
		t.Fatal(err)
	}

	holdLockAndServeLater(t, dir, 400*time.Millisecond)

	out := captureStdout(t, func() error {
		return upForeground(&Identity{ID: id, Name: "webapp", KeySource: "directory"}, "", ".", bundle)
	})
	if !strings.Contains(out, "already running") {
		t.Errorf("up should have handed off to the concurrent boot, output:\n%s", out)
	}
}

func bootManifest(t *testing.T) *Manifest {
	return hostManifest(t)
}

func upIdentity(root, id string) *Identity {
	return &Identity{ID: id, Name: "feature", KeySource: "directory", Worktree: root, RepoRoot: root}
}

// The boot phase minus the daemon: the record lands canonicalized, the
// attempt's token is retired now that the canonical root took over, and the
// caller gets the boot lock.
func TestPrepareUpBootCommitsTheRecordAndRetiresTheToken(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00001111"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)

	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()

	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(bundle)
	if err != nil {
		t.Fatal(err)
	}

	keepToken := false
	lock, inst, err := prepareUpBoot(upIdentity(root, id), dir, tok, "dev", bundle, bootManifest(t), 0, &keepToken)
	if err != nil {
		t.Fatalf("prepareUpBoot: %v", err)
	}
	if lock == nil {
		t.Fatal("prepareUpBoot returned no boot lock")
	}
	defer lock.Close()

	if inst.Bundle != canonical {
		t.Errorf("instance bundle = %q, want the canonical %q", inst.Bundle, canonical)
	}
	if got := readRecord(t, dir).Bundle; got != canonical {
		t.Errorf("record on disk = %q, want the canonical %q", got, canonical)
	}
	if keepToken {
		t.Error("a successful boot asked for its token to be kept")
	}
	if names := stagingEntries(t, dir); len(names) != 0 {
		t.Errorf("staging holds %v after a successful boot, want it empty", names)
	}
}

// The central regression: an `up` whose instance was deleted (and recreated at
// the same path) mid-build must abort before it writes anything, or it would
// claim and overwrite a stranger's incarnation.
func TestPrepareUpBootAbortsWhenTheInstanceWasRecreated(t *testing.T) {
	root := shortStateRoot(t)
	const id = "aaaa00001112"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)

	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()

	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	// Delete and recreate: the pathname is the one the attempt published under,
	// the directory behind it is a different incarnation.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	keepToken := false
	_, _, err = prepareUpBoot(upIdentity(root, id), dir, tok, "dev", bundle, bootManifest(t), 0, &keepToken)
	if err == nil {
		t.Fatal("a stale attempt booted into a recreated instance")
	}
	if !strings.Contains(err.Error(), "deleted while building") {
		t.Fatalf("error = %v, want it to name the deletion", err)
	}
	for _, name := range []string{"instance.json", "daemon.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("the stale attempt left %s in the recreated instance: %v", name, err)
		}
	}
}

// The record is committed before the roots are reconciled, so a failed pin
// leaves a record whose only protection is the attempt's staging root: the
// token must outlive the failure.
func TestPrepareUpBootKeepsTheTokenWhenReconcileFails(t *testing.T) {
	storePath := anyStoreDir(t)
	root := shortStateRoot(t)
	const id = "aaaa00001113"
	t.Cleanup(func() { removeSocketDir(id) })
	dir := filepath.Join(root, "sprout", "instances", id)

	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	// The staging root a store-backed intake parks in the token dir — after
	// the pin failure it is the committed record's only GC protection.
	if err := os.Symlink(storePath, stagingBundleLink(tok.dir)); err != nil {
		t.Fatal(err)
	}

	pinFailed := errors.New("nix build refused")
	recordPins(t, func(int) error { return pinFailed })

	keepToken := false
	_, _, err = prepareUpBoot(upIdentity(root, id), dir, tok, "dev", storePath, bootManifest(t), 0, &keepToken)
	if !errors.Is(err, pinFailed) {
		t.Fatalf("prepareUpBoot error = %v, want the pin failure", err)
	}
	if !keepToken {
		t.Error("the attempt's token was retired even though the canonical pin failed")
	}
	if resolved, err := filepath.EvalSymlinks(stagingBundleLink(tok.dir)); err != nil || resolved != storePath {
		t.Errorf("staging root = %q (err %v), want it still resolving to %q", resolved, err, storePath)
	}
	if got := readRecord(t, dir).Bundle; got != storePath {
		t.Errorf("record on disk = %q, want the committed %q", got, storePath)
	}
}

// Convergence compares builds, not spellings: a legacy record that resolves to
// the bundle being booted is the same build, and anything unresolvable is not.
func TestSameBundle(t *testing.T) {
	dir := t.TempDir()
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bundle")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		what      string
		recorded  string
		canonical string
		want      bool
	}{
		{what: "identical paths", recorded: target, canonical: target, want: true},
		{what: "symlink to the canonical path", recorded: link, canonical: target, want: true},
		{what: "relative record", recorded: "relative/bundle", canonical: target},
		{what: "missing record", recorded: filepath.Join(dir, "gone"), canonical: target},
		{what: "different bundle", recorded: dir, canonical: target},
	}
	for _, c := range cases {
		if got := sameBundle(c.recorded, c.canonical); got != c.want {
			t.Errorf("%s: sameBundle(%q, %q) = %v, want %v", c.what, c.recorded, c.canonical, got, c.want)
		}
	}
}

// The host key lives only in /var, so trust in it cannot outlive var.img.
func TestResetStaleHostTrustDropsKnownHostsWhenVarImageIsGone(t *testing.T) {
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("sprout-main ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	warning := captureStderr(t, func() { err = resetStaleHostTrust(dir) })
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(knownHosts); !os.IsNotExist(statErr) {
		t.Fatalf("known_hosts survived a missing var.img: %v", statErr)
	}
	if !strings.Contains(warning, "/var volume is gone") {
		t.Errorf("lost /var was not reported to the user, got %q", warning)
	}
}

func TestResetStaleHostTrustKeepsKnownHostsWhenVarImageExists(t *testing.T) {
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("sprout-main ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "var.img"), []byte("var-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	warning := captureStderr(t, func() { err = resetStaleHostTrust(dir) })
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(knownHosts); statErr != nil {
		t.Fatalf("known_hosts was dropped even though /var is intact: %v", statErr)
	}
	if warning != "" {
		t.Errorf("intact instance warned anyway: %q", warning)
	}
}

// A first boot has neither file; it has lost nothing and must stay quiet.
func TestResetStaleHostTrustIsSilentOnFirstBoot(t *testing.T) {
	dir := t.TempDir()

	var err error
	warning := captureStderr(t, func() { err = resetStaleHostTrust(dir) })
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("first boot warned about state it never had: %q", warning)
	}
}
