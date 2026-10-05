package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// The manifest the Nix backend module for kind emits on host, as decoded
// JSON so a case can break any field of it.
func v2ManifestDoc(host, kind string) map[string]any {
	guest := host
	if host == "aarch64-darwin" {
		guest = "aarch64-linux"
	}
	backend := map[string]any{
		"kind":     kind,
		"network":  map[string]any{"transport": "vfkit-unixgram", "socket": "net.sock"},
		"control":  map[string]any{"protocol": "vfkit-rest", "socket": "vfkit-rest.sock"},
		"console":  map[string]any{"mode": "pty-announce"},
		"sidecars": []any{},
	}
	if kind == "qemu" {
		backend["network"] = map[string]any{"transport": "qemu-stream", "socket": "net.sock"}
		backend["control"] = map[string]any{"protocol": "qmp", "socket": "vm-control.sock"}
		backend["console"] = map[string]any{"mode": "stdio"}
	}
	return map[string]any{
		"version":    2,
		"definition": "dev",
		"host":       map[string]any{"system": host},
		"guest":      map[string]any{"system": guest, "ip": "127.0.0.1", "sshUser": "sprout"},
		"backend":    backend,
	}
}

func encodeDoc(t testing.TB, doc map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func defaultKindFor(host string) string {
	if host == "aarch64-darwin" {
		return "vfkit"
	}
	return "qemu"
}

func testHost(t testing.TB) string {
	t.Helper()
	host, err := hostNixSystem()
	if err != nil {
		t.Skip(err)
	}
	return host
}

// A parsed manifest for the host running the test, as its own flake would
// build it.
func hostManifest(t testing.TB) *Manifest {
	t.Helper()
	host := testHost(t)
	m, err := parseManifest(encodeDoc(t, v2ManifestDoc(host, defaultKindFor(host))), host)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func writeHostManifest(t testing.TB, bundle string) {
	t.Helper()
	host := testHost(t)
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), encodeDoc(t, v2ManifestDoc(host, defaultKindFor(host))), 0o600); err != nil {
		t.Fatal(err)
	}
}

type stubTransport struct{}

func (stubTransport) protocol() types.Protocol { return types.QemuProtocol }
func (stubTransport) serve(context.Context, *virtualnetwork.VirtualNetwork, string) error {
	return nil
}

type stubControl struct{}

func (stubControl) requestStop(string, bool) error { return nil }

type stubConsole struct{}

func (stubConsole) writer(string) (io.WriteCloser, error) { return nopWriteCloser{io.Discard}, nil }

func (stubConsole) mirrorsRunnerLog() bool { return false }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Fills the host kinds' unimplemented operations with stand-ins, so a test of
// lifecycle logic past the boot-capability gate runs on any host.
func withBootableHostBackend(t *testing.T) {
	t.Helper()
	for _, k := range backendKindsFor(testHost(t)) {
		if networkTransports[k.vocabulary.network] == nil {
			restoreEntry(t, networkTransports, k.vocabulary.network, networkTransport(stubTransport{}))
		}
		if controlProtocols[k.vocabulary.control] == nil {
			restoreEntry(t, controlProtocols, k.vocabulary.control, controlProtocol(stubControl{}))
		}
		if consoleModes[k.vocabulary.console] == nil {
			restoreEntry(t, consoleModes, k.vocabulary.console, consoleMode(stubConsole{}))
		}
	}
}

func restoreEntry[T any](t *testing.T, m map[string]T, key string, v T) {
	prev := m[key]
	m[key] = v
	t.Cleanup(func() { m[key] = prev })
}

func TestParseManifestAcceptsEachBackendOnItsHost(t *testing.T) {
	for _, c := range []struct{ host, kind, network, control string }{
		{"aarch64-darwin", "vfkit", "net.sock", "vfkit-rest.sock"},
		{"x86_64-linux", "qemu", "net.sock", "vm-control.sock"},
		{"aarch64-linux", "qemu", "net.sock", "vm-control.sock"},
	} {
		m, err := parseManifest(encodeDoc(t, v2ManifestDoc(c.host, c.kind)), c.host)
		if err != nil {
			t.Errorf("%s manifest on %s rejected: %v", c.kind, c.host, err)
			continue
		}
		if m.contract.kind.name != c.kind || m.contract.networkSocket != c.network || m.contract.controlSocket != c.control {
			t.Errorf("%s on %s parsed as kind %q, sockets %q/%q", c.kind, c.host, m.contract.kind.name, m.contract.networkSocket, m.contract.controlSocket)
		}
	}
}

func TestQemuManifestIsBootable(t *testing.T) {
	m, err := parseManifest(encodeDoc(t, v2ManifestDoc("x86_64-linux", "qemu")), "x86_64-linux")
	if err != nil {
		t.Fatalf("valid qemu manifest rejected: %v", err)
	}
	if err := m.contract.bootable(); err != nil {
		t.Fatalf("qemu manifest not bootable: %v", err)
	}
}

func TestVfkitManifestIsBootable(t *testing.T) {
	m, err := parseManifest(encodeDoc(t, v2ManifestDoc("aarch64-darwin", "vfkit")), "aarch64-darwin")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.contract.bootable(); err != nil {
		t.Fatalf("vfkit manifest not bootable: %v", err)
	}
}

func TestParseManifestRejectsBackendsForeignToThisHost(t *testing.T) {
	cases := []struct {
		what string
		host string
		doc  map[string]any
	}{
		{"qemu bundle on a mac", "aarch64-darwin", v2ManifestDoc("x86_64-linux", "qemu")},
		{"vfkit bundle on linux", "x86_64-linux", v2ManifestDoc("aarch64-darwin", "vfkit")},
		{"vfkit kind claiming a linux host", "x86_64-linux", v2ManifestDoc("x86_64-linux", "vfkit")},
		{"bundle for the other linux architecture", "aarch64-linux", v2ManifestDoc("x86_64-linux", "qemu")},
		{"unknown kind", "x86_64-linux", v2ManifestDoc("x86_64-linux", "crosvm")},
	}
	for _, c := range cases {
		_, err := parseManifest(encodeDoc(t, c.doc), c.host)
		if err == nil {
			t.Errorf("%s: accepted", c.what)
			continue
		}
		if !strings.Contains(err.Error(), "this bundle targets") || !strings.Contains(err.Error(), "this sprout on "+c.host+" supports") {
			t.Errorf("%s: error %q does not name the bundle's target and what this host supports", c.what, err)
		}
	}
}

func TestParseManifestRejectsUnknownVocabulary(t *testing.T) {
	for _, field := range []string{"network", "control", "console"} {
		doc := v2ManifestDoc("x86_64-linux", "qemu")
		backend := doc["backend"].(map[string]any)
		entry := backend[field].(map[string]any)
		for k := range entry {
			if k != "socket" {
				entry[k] = "carrier-pigeon"
			}
		}
		_, err := parseManifest(encodeDoc(t, doc), "x86_64-linux")
		if err == nil || !strings.Contains(err.Error(), `"carrier-pigeon"`) || !strings.Contains(err.Error(), "upgrade sprout") {
			t.Errorf("unknown %s value: error = %v, want it quoted with an upgrade hint", field, err)
		}
	}
}

func TestParseManifestRejectsUnsafeSocketNames(t *testing.T) {
	for _, name := range []string{"", "../net.sock", "sub/net.sock", "net", "Net.sock", ".sock", "-net.sock", "net sock.sock", "/tmp/net.sock"} {
		doc := v2ManifestDoc("x86_64-linux", "qemu")
		doc["backend"].(map[string]any)["network"].(map[string]any)["socket"] = name
		if _, err := parseManifest(encodeDoc(t, doc), "x86_64-linux"); err == nil {
			t.Errorf("network socket %q accepted", name)
		}
	}
}

func TestParseManifestRejectsSocketCollisions(t *testing.T) {
	sidecar := func(name, sock string) map[string]any {
		return map[string]any{"name": name, "exec": []any{"/nix/store/x/bin/virtiofsd"}, "ready": map[string]any{"socket": sock}}
	}
	cases := []struct {
		what   string
		mutate func(backend map[string]any)
	}{
		{"network and control share a socket", func(b map[string]any) {
			b["control"].(map[string]any)["socket"] = "net.sock"
		}},
		{"network takes the daemon's control socket", func(b map[string]any) {
			b["network"].(map[string]any)["socket"] = "control.sock"
		}},
		{"sidecar readiness reuses the control socket", func(b map[string]any) {
			b["sidecars"] = []any{sidecar("virtiofsd-a", "vm-control.sock")}
		}},
		{"two sidecars share a readiness socket", func(b map[string]any) {
			b["sidecars"] = []any{sidecar("virtiofsd-a", "fs.sock"), sidecar("virtiofsd-b", "fs.sock")}
		}},
	}
	for _, c := range cases {
		doc := v2ManifestDoc("x86_64-linux", "qemu")
		c.mutate(doc["backend"].(map[string]any))
		if _, err := parseManifest(encodeDoc(t, doc), "x86_64-linux"); err == nil || !strings.Contains(err.Error(), "collides") {
			t.Errorf("%s: error = %v, want a collision", c.what, err)
		}
	}
}

func TestParseManifestValidatesSidecars(t *testing.T) {
	cases := []struct {
		what     string
		sidecars []any
		ok       bool
	}{
		{"well formed", []any{
			map[string]any{"name": "virtiofsd-workspace", "exec": []any{"/nix/store/x/bin/virtiofsd", "--socket-path=/sprout/placeholder/sock/fs-workspace.sock"}, "ready": map[string]any{"socket": "fs-workspace.sock"}},
		}, true},
		{"relative executable", []any{
			map[string]any{"name": "a", "exec": []any{"virtiofsd"}, "ready": map[string]any{"socket": "fs-a.sock"}},
		}, false},
		{"empty argv", []any{
			map[string]any{"name": "a", "exec": []any{}, "ready": map[string]any{"socket": "fs-a.sock"}},
		}, false},
		{"duplicate name", []any{
			map[string]any{"name": "a", "exec": []any{"/bin/a"}, "ready": map[string]any{"socket": "fs-a.sock"}},
			map[string]any{"name": "a", "exec": []any{"/bin/a"}, "ready": map[string]any{"socket": "fs-b.sock"}},
		}, false},
		{"no readiness socket", []any{
			map[string]any{"name": "a", "exec": []any{"/bin/a"}},
		}, false},
	}
	for _, c := range cases {
		doc := v2ManifestDoc("x86_64-linux", "qemu")
		doc["backend"].(map[string]any)["sidecars"] = c.sidecars
		_, err := parseManifest(encodeDoc(t, doc), "x86_64-linux")
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.what, err, c.ok)
		}
	}
}

// Existing Darwin bundles predate v2; they must keep booting as the vfkit
// contract they always were.
func TestV1ManifestMigratesToVfkitOnDarwin(t *testing.T) {
	v1 := `{"version":1,"definition":"dev","guestArch":"aarch64-linux","restSocket":"vfkit-rest.sock","guest":{"ip":"192.168.127.2"}}`
	m, err := parseManifest([]byte(v1), "aarch64-darwin")
	if err != nil {
		t.Fatalf("v1 manifest rejected on aarch64-darwin: %v", err)
	}
	c := m.contract
	if c.kind.name != "vfkit" || c.transport != "vfkit-unixgram" || c.protocol != "vfkit-rest" || c.consoleMode != "pty-announce" {
		t.Errorf("v1 contract = %s/%s/%s/%s, want vfkit/vfkit-unixgram/vfkit-rest/pty-announce", c.kind.name, c.transport, c.protocol, c.consoleMode)
	}
	if c.networkSocket != "net.sock" || c.controlSocket != "vfkit-rest.sock" {
		t.Errorf("v1 sockets = %q/%q, want net.sock/vfkit-rest.sock", c.networkSocket, c.controlSocket)
	}
	if m.Host.System != "aarch64-darwin" || m.Guest.System != "aarch64-linux" {
		t.Errorf("v1 systems = host %q guest %q, want aarch64-darwin/aarch64-linux", m.Host.System, m.Guest.System)
	}
	if err := c.bootable(); err != nil {
		t.Errorf("migrated v1 manifest not bootable: %v", err)
	}
}

func TestV1ManifestIsRefusedOnLinux(t *testing.T) {
	v1 := `{"version":1,"definition":"dev","guestArch":"x86_64-linux","restSocket":"vfkit-rest.sock"}`
	_, err := parseManifest([]byte(v1), "x86_64-linux")
	if err == nil || !strings.Contains(err.Error(), "rebuild with a sprout flake that emits manifest v2") {
		t.Fatalf("v1 on linux: error = %v, want the rebuild remedy", err)
	}
}

func TestManifestFromANewerFlakeAsksToUpgrade(t *testing.T) {
	for _, v := range []string{`{"version":3}`, `{"version":0}`, `{}`} {
		_, err := parseManifest([]byte(v), "aarch64-darwin")
		if err == nil || !strings.Contains(err.Error(), "upgrade sprout") {
			t.Errorf("%s: error = %v, want an upgrade hint", v, err)
		}
	}
}

func TestBootableHostFor(t *testing.T) {
	for _, host := range []string{"aarch64-darwin", "x86_64-linux", "aarch64-linux"} {
		if err := bootableHostFor(host); err != nil {
			t.Errorf("%s rejected: %v", host, err)
		}
	}
}

// Bootability is a registry fact: implementing every operation of a host's
// kind is all it takes to boot there.
func TestBootableHostFollowsTheRegistry(t *testing.T) {
	restoreEntry(t, consoleModes, "stdio", consoleMode(nil))
	if err := bootableHostFor("x86_64-linux"); err == nil {
		t.Fatal("linux bootable with the qemu console still missing")
	}
	restoreEntry(t, consoleModes, "stdio", consoleMode(stubConsole{}))
	if err := bootableHostFor("x86_64-linux"); err != nil {
		t.Fatalf("linux still refused with every qemu operation registered: %v", err)
	}
}

func TestResolveInstanceSocketsPlacesEveryBackendSocketInTheSocketDir(t *testing.T) {
	doc := v2ManifestDoc("x86_64-linux", "qemu")
	doc["backend"].(map[string]any)["sidecars"] = []any{
		map[string]any{"name": "virtiofsd-workspace", "exec": []any{"/bin/virtiofsd"}, "ready": map[string]any{"socket": "fs-workspace.sock"}},
	}
	m, err := parseManifest(encodeDoc(t, doc), "x86_64-linux")
	if err != nil {
		t.Fatal(err)
	}
	socks, err := resolveInstanceSockets("/tmp/sprout-1000/abc", m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"net.sock":          "/tmp/sprout-1000/abc/net.sock",
		"vm-control.sock":   "/tmp/sprout-1000/abc/vm-control.sock",
		"fs-workspace.sock": "/tmp/sprout-1000/abc/fs-workspace.sock",
	}
	for name, path := range want {
		if socks.named[name] != path {
			t.Errorf("socket %s resolved to %q, want %q", name, socks.named[name], path)
		}
	}
	if socks.net != want["net.sock"] || socks.vmControl != want["vm-control.sock"] || socks.control != "/tmp/sprout-1000/abc/control.sock" {
		t.Errorf("sockets = %+v", socks)
	}

	if _, err := resolveInstanceSockets("/tmp/"+strings.Repeat("d", 100), m); err == nil || !strings.Contains(err.Error(), "AF_UNIX") {
		t.Errorf("over-long socket dir: error = %v, want the path-limit refusal", err)
	}
}

func hostPlatform(t testing.TB) Platform {
	t.Helper()
	return hostManifest(t).platform()
}
