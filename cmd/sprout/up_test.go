package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
