//go:build integration

// The Nix to Go seam, checked against a real `nix build` bundle. Needs Nix and
// a working flake, so it is gated behind the `integration` tag:
//
//	go test -tags integration ./cmd/sprout
package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// How each kind's runner must carry the substituted control socket, which
// sprout dials to stop the VM.
var controlSocketReferences = map[string]func(sock string) string{
	// Absolute, or microvm.nix expands it against the runner's cwd (see
	// nix/bundle.nix).
	"vfkit": func(sock string) string { return "SOCKET_ABS=" + sock },
	"qemu":  func(sock string) string { return "-qmp unix:" + sock + "," },
}

// Every symbol the built manifest asks for is resolvable, every placeholder it
// names is present in the runner or a sidecar, and the rewritten runner is one
// sprout can still stop and recognise as an orphan, so drift in either
// direction fails here instead of at a user's `sprout up`.
func TestManifestRunnerContract(t *testing.T) {
	host, err := hostNixSystem()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := nixBuild(filepath.Join(t.TempDir(), "bundle"), ".", host, "dev")
	if err != nil {
		t.Fatalf("nix build: %v", err)
	}
	m, err := loadManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	kind := m.contract.kind.name
	controlRef, ok := controlSocketReferences[kind]
	if !ok {
		t.Fatalf("no control socket expectation for the %s backend", kind)
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

	const sockDir = "/dummy/sock"
	subs := map[string]string{
		"dataDir":    "/dummy/data",
		"workspace":  "/dummy/workspace",
		"gitCommon":  "/dummy/gitcommon",
		"consolePty": "virtio-serial,pty",
		"hostUid":    "1000",
		"hostGid":    "100",
	}
	for _, name := range m.contract.socketNames() {
		subs["socket:"+name] = sockDir + "/" + name
	}
	for _, c := range m.Credentials {
		subs["credential:"+c.Name] = "/dummy/credential/" + c.Name
	}
	for _, c := range m.Caches {
		subs["cache:"+c.Name] = "/dummy/cache/" + c.Name
	}

	out := filepath.Join(t.TempDir(), "run.sh")
	sidecars, err := rewriteRunner(filepath.Join(bundle, "runner"), m, subs, out)
	if err != nil {
		t.Fatalf("manifest/runner contract broken: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	runner := string(data)
	// The flake tags every substitution target under /sprout/placeholder/; none
	// may survive the rewrite.
	if strings.Contains(runner, "/sprout/placeholder/") {
		t.Fatalf("rewritten runner still contains an unresolved placeholder")
	}
	for _, s := range sidecars {
		for _, arg := range s.Exec {
			if strings.Contains(arg, "/sprout/placeholder/") {
				t.Fatalf("sidecar %s still has an unresolved placeholder: %s", s.Name, arg)
			}
		}
	}
	if want := controlRef(subs["socket:"+m.contract.controlSocket]); !strings.Contains(runner, want) {
		t.Fatalf("runner does not take the %s control socket as %q", kind, want)
	}

	// Whitespace splitting with quotes trimmed is enough to recover the
	// arguments the matchers look at, including vfkit's nested '\'' quoting.
	matchers := m.contract.orphanArgs(sockDir, "/dummy/instance")
	args := strings.Fields(runner)
	for i, a := range args {
		args[i] = strings.Trim(a, `'"\`)
	}
	if !slices.ContainsFunc(args, func(a string) bool {
		return slices.ContainsFunc(matchers, func(match argMatcher) bool { return match(a) })
	}) {
		t.Fatalf("no argument of the rewritten %s runner matches its orphan matchers, so a leftover VM would go unrecognised", kind)
	}
}
