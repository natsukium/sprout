//go:build integration

// The Nix to Go seam, checked against a real `nix build` bundle. Needs Nix and
// a working flake, so it is gated behind the `integration` tag:
//
//	go test -tags integration ./cmd/sprout
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every symbol the built manifest asks for is resolvable and every placeholder
// it names is present in the runner, so drift in either direction fails here
// instead of at a user's `sprout up`.
func TestManifestRunnerContract(t *testing.T) {
	bundle, err := nixBuild(".", "dev")
	if err != nil {
		t.Fatalf("nix build: %v", err)
	}
	m, err := loadManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Scalar fields have no placeholder to catch drift. hostLoopback especially:
	// false and absent deserialize identically, so only the raw JSON proves the
	// Nix side is making the choice.
	raw, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hostLoopback"`) {
		t.Fatal("manifest omits hostLoopback; the flake no longer decides guest loopback access")
	}

	subs := map[string]string{
		"netSocket":  "/dummy/net.sock",
		"restSocket": "/dummy/vfkit-rest.sock",
		"dataDir":    "/dummy/data",
		"workspace":  "/dummy/workspace",
		"gitCommon":  "/dummy/gitcommon",
		"consolePty": "virtio-serial,pty",
	}
	for _, c := range m.Credentials {
		subs["credential:"+c.Name] = "/dummy/credential/" + c.Name
	}
	for _, c := range m.Caches {
		subs["cache:"+c.Name] = "/dummy/cache/" + c.Name
	}

	out := filepath.Join(t.TempDir(), "run.sh")
	if err := rewriteRunner(filepath.Join(bundle, "runner"), m, subs, out); err != nil {
		t.Fatalf("manifest/runner contract broken: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The flake tags every substitution target under /sprout/placeholder/; none
	// may survive the rewrite.
	if strings.Contains(string(data), "/sprout/placeholder/") {
		t.Fatalf("rewritten runner still contains an unresolved placeholder")
	}
	// The REST socket must reach the runner absolute, or microvm.nix expands it
	// against the runner's cwd (see nix/bundle.nix).
	if !strings.Contains(string(data), "SOCKET_ABS="+subs["restSocket"]) {
		t.Fatal("runner does not take vfkit's REST socket from the substituted absolute path")
	}
}
