package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/.aws", filepath.Join(home, ".aws")},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~notme/x", "~notme/x"}, // ~user is intentionally left untouched
	}
	for _, c := range cases {
		got, err := expandTilde(c.in)
		if err != nil {
			t.Fatalf("expandTilde(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("expandTilde(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// virtiofs won't follow a symlinked source, so the real target directory comes
// back, not the link.
func TestResolveCredentialSourceSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-aws")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-aws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCredentialSource(link)
	if err != nil {
		t.Fatalf("resolveCredentialSource(%q): %v", link, err)
	}
	// EvalSymlinks also canonicalizes the temp dir prefix (/var to /private on
	// macOS), so the comparison uses the resolved path, not the raw one.
	wantReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantReal {
		t.Fatalf("resolved to %q, want the symlink target %q", got, wantReal)
	}
}

func TestResolveCredentialSourceMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := resolveCredentialSource(missing); err == nil {
		t.Fatal("missing source resolved without error; want a hard failure")
	}
}

// The "$VAR" contract for mount sources, matching the socket strategy.
func TestResolveCredentialSourceEnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPROUT_TEST_CRED_DIR", dir)

	got, err := resolveCredentialSource("$SPROUT_TEST_CRED_DIR")
	if err != nil {
		t.Fatalf("resolveCredentialSource($VAR): %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

// An unset variable must be a hard error: an empty expansion would otherwise
// pass through filepath.Abs as the cwd and mount that into the guest.
func TestResolveCredentialSourceUnsetEnvVar(t *testing.T) {
	t.Setenv("SPROUT_TEST_CRED_UNSET", "")
	if _, err := resolveCredentialSource("$SPROUT_TEST_CRED_UNSET"); err == nil {
		t.Fatal("empty expansion resolved without error; want a hard failure")
	}
}

func TestSetupCredentials(t *testing.T) {
	dir := t.TempDir()
	awsDir := filepath.Join(dir, ".aws")
	if err := os.Mkdir(awsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("mount populates the symbol", func(t *testing.T) {
		m := &Manifest{Credentials: []CredentialSpec{
			{Name: "aws", Strategy: "mount", Source: awsDir},
		}}
		subs := map[string]string{}
		if err := setupCredentials(m, subs, dir); err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(awsDir)
		if subs["credential:aws"] != want {
			t.Fatalf("credential:aws = %q, want %q", subs["credential:aws"], want)
		}
	})

	t.Run("unknown strategy errors", func(t *testing.T) {
		m := &Manifest{Credentials: []CredentialSpec{
			{Name: "weird", Strategy: "telepathy"},
		}}
		if err := setupCredentials(m, map[string]string{}, dir); err == nil {
			t.Fatal("want error for unsupported strategy")
		}
	})

	t.Run("missing mount source aborts", func(t *testing.T) {
		m := &Manifest{Credentials: []CredentialSpec{
			{Name: "aws", Strategy: "mount", Source: filepath.Join(dir, "nope")},
		}}
		if err := setupCredentials(m, map[string]string{}, dir); err == nil {
			t.Fatal("want error for missing source")
		}
	})

	t.Run("name escaping the credentials dir is rejected", func(t *testing.T) {
		// A name with a separator would write outside data/credentials, so it
		// fails before any strategy runs.
		m := &Manifest{Credentials: []CredentialSpec{
			{Name: "../ssh/authorized_keys", Strategy: "materialize", Exec: []string{"/bin/echo", "x"}},
		}}
		if err := setupCredentials(m, map[string]string{}, dir); err == nil {
			t.Fatal("want error for a credential name containing a path separator")
		}
	})
}

// A SIGKILLed daemon skips its deferred cleanup, so the client-side sweep
// deletes the leftover materialized credentials and nothing else in data/.
func TestSweepStaleCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "abc123def456"
	dir, err := instanceDir(id)
	if err != nil {
		t.Fatal(err)
	}
	credDir := filepath.Join(dir, "data", "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "gh"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "data", "instance.env")
	if err := os.WriteFile(keep, []byte("SPROUT_INSTANCE_ID='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sweepStaleCredentials(id)

	if _, err := os.Stat(credDir); !os.IsNotExist(err) {
		t.Fatal("stale credentials dir survived the sweep")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("sweep must not touch the rest of the data dir: %v", err)
	}
	// An instance with no state at all must stay a no-op, not create one.
	sweepStaleCredentials("000000000000")
	if noDir, err := instanceDir("000000000000"); err == nil {
		if _, err := os.Stat(noDir); !os.IsNotExist(err) {
			t.Fatal("sweep created state for a nonexistent instance")
		}
	}
}

func TestMaterializeCredential(t *testing.T) {
	t.Run("writes exec stdout with 0600", func(t *testing.T) {
		data := t.TempDir()
		c := CredentialSpec{
			Name:     "gh",
			Strategy: "materialize",
			Exec:     []string{"/bin/sh", "-c", "printf 'github.com:\\n    oauth_token: t\\n'"},
		}
		if err := materializeCredential(c, data); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(data, "credentials", "gh")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "github.com:\n    oauth_token: t\n" {
			t.Fatalf("unexpected content %q", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 600", info.Mode().Perm())
		}
	})

	t.Run("failing exec aborts", func(t *testing.T) {
		data := t.TempDir()
		c := CredentialSpec{Name: "gh", Strategy: "materialize", Exec: []string{"/bin/sh", "-c", "exit 3"}}
		if err := materializeCredential(c, data); err == nil {
			t.Fatal("want error when exec exits non-zero")
		}
		if _, err := os.Stat(filepath.Join(data, "credentials", "gh")); !os.IsNotExist(err) {
			t.Fatal("no credential file should be written on failure")
		}
	})

	t.Run("empty exec errors", func(t *testing.T) {
		if err := materializeCredential(CredentialSpec{Name: "x", Strategy: "materialize"}, t.TempDir()); err == nil {
			t.Fatal("want error for empty exec")
		}
	})
}
