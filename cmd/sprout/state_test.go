package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// A name sanitizes to a valid filesystem/hostname token, or errors when
// nothing usable remains.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain branch", "feature", "feature", false},
		{"slash becomes dash", "feature/foo", "feature-foo", false},
		{"leading dashes trimmed", "---x", "x", false},
		{"dots and dashes trimmed at edges", ".-x-.", "x", false},
		{"unicode collapses to single dash", "a…—b", "a-b", false},
		{"run of specials collapses to one dash", "a///b", "a-b", false},
		{"underscores are allowed, not specials", "a___b", "a___b", false},
		{"only specials is an error", "///", "", true},
		{"empty is an error", "", "", true},
		// A dot collapses to a dash: the router reads a dotted label as a
		// <port>.<name> boundary.
		{"dot collapses to dash", "v1.2_beta-3", "v1-2_beta-3", false},
		{"digit-leading dotted name stays one label", "2024.q3", "2024-q3", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sanitizeName(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("sanitizeName(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeName(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("sanitizeName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Invariants for any input: safe charset, no edge dashes or dots,
// idempotence, and an error only when no usable output existed.
func TestSanitizeNameProperties(t *testing.T) {
	// No dot, so the router never mistakes part of a name for a
	// <port>.<name> boundary.
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	safe := func(s string) bool {
		for _, r := range s {
			if !strings.ContainsRune(allowed, r) {
				return false
			}
		}
		return true
	}
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.String().Draw(t, "in")
		got, err := sanitizeName(in)
		if err != nil {
			if got != "" {
				t.Fatalf("error but non-empty result %q", got)
			}
			return
		}
		if got == "" {
			t.Fatalf("empty result without error for %q", in)
		}
		if !safe(got) {
			t.Fatalf("sanitizeName(%q) = %q contains unsafe chars", in, got)
		}
		if strings.HasPrefix(got, "-") || strings.HasPrefix(got, ".") ||
			strings.HasSuffix(got, "-") || strings.HasSuffix(got, ".") {
			t.Fatalf("sanitizeName(%q) = %q has a trimmable edge", in, got)
		}
		again, err := sanitizeName(got)
		if err != nil || again != got {
			t.Fatalf("not idempotent: sanitize(%q)=%q then %q (err %v)", in, got, again, err)
		}
	})
}

// An unimplemented schema version fails loudly instead of booting with
// silently ignored instructions.
func TestManifestVersionRejected(t *testing.T) {
	var m Manifest
	if err := jsonUnmarshalStrictVersion([]byte(`{"version":2}`), &m); err == nil {
		t.Fatal("version 2 manifest accepted, want rejection")
	}
	if err := jsonUnmarshalStrictVersion([]byte(`{"version":1,"definition":"dev"}`), &m); err != nil {
		t.Fatalf("version 1 manifest rejected: %v", err)
	}
	if m.Definition != "dev" {
		t.Fatalf("definition not parsed: %q", m.Definition)
	}
}

// An uppercased ID prefix still resolves as an ID. Without folding it would
// fall through to name-key resolution and mint a fresh instance.
func TestMatchIDPrefixFoldsCase(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if err := os.MkdirAll(filepath.Join(root, "sprout", "instances", "abcd12345678"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := matchIDPrefix("ABCD1234")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcd12345678" {
		t.Fatalf("matchIDPrefix(ABCD1234) = %q, want abcd12345678", got)
	}
}

// A rewrite goes through a rename, never a truncate-in-place: a reader racing
// the write must see valid JSON.
func TestWriteJSONReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.json")
	if err := writeJSON(path, &Instance{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, &Instance{ID: "two"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil || inst.ID != "two" {
		t.Fatalf("rewritten record unreadable: %v (%q)", err, data)
	}
	// The staging file must not linger next to the record.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// New state records carry an explicit schema version, and a record from a
// different one is rejected before callers receive an incomplete instance.
func TestInstanceSchemaVersion(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := filepath.Join(root, "sprout", "instances", "abcd12345678")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "instance.json")
	if err := writeJSON(path, &Instance{ID: "abcd12345678", Name: "main"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("instance record omitted schema version: %s", data)
	}
	if _, _, err := loadInstance("abcd12345678"); err != nil {
		t.Fatalf("version-1 instance rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"id":"abcd12345678"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadInstance("abcd12345678"); err == nil {
		t.Fatal("unsupported instance version accepted")
	}
}

// An omitted hostLoopback field stays disabled: the alias exposes all of the
// host's 127.0.0.1 listeners to the guest, so absent must mean off.
func TestManifestHostLoopbackDefaultsOff(t *testing.T) {
	var m Manifest
	if err := jsonUnmarshalStrictVersion([]byte(`{"version":1,"definition":"dev"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.HostLoopback {
		t.Fatal("hostLoopback true for a manifest that never mentioned it")
	}
	if err := jsonUnmarshalStrictVersion([]byte(`{"version":1,"hostLoopback":true}`), &m); err != nil {
		t.Fatal(err)
	}
	if !m.HostLoopback {
		t.Fatal("explicit hostLoopback:true not parsed")
	}
}

// The manifest is the Nix to Go trust boundary, so arbitrary bytes must never
// panic and must still meet the version gate.
func FuzzManifestParse(f *testing.F) {
	f.Add([]byte(`{"version":1,"definition":"dev","guest":{"ip":"192.168.127.2"}}`))
	f.Add([]byte(`{"version":2}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		var m Manifest
		err := jsonUnmarshalStrictVersion(data, &m)
		if err == nil && m.Version != manifestSchemaVersion {
			t.Fatalf("accepted unsupported version %d", m.Version)
		}
	})
}
