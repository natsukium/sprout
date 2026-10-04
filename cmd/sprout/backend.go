package main

// Behaviour comes from vocabulary values, never from a switch on the kind.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

type BackendSpec struct {
	Kind    string `json:"kind"`
	Network struct {
		Transport string `json:"transport"`
		Socket    string `json:"socket"`
	} `json:"network"`
	Control struct {
		Protocol string `json:"protocol"`
		Socket   string `json:"socket"`
	} `json:"control"`
	Console struct {
		Mode string `json:"mode"`
	} `json:"console"`
	Sidecars []SidecarSpec `json:"sidecars"`
}

// SidecarSpec is a host process the runner needs started first; it counts as
// ready once Ready.Socket exists.
type SidecarSpec struct {
	Name  string   `json:"name"`
	Exec  []string `json:"exec"`
	Ready struct {
		Socket string `json:"socket"`
	} `json:"ready"`
}

type networkTransport interface {
	protocol() types.Protocol
	serve(ctx context.Context, vn *virtualnetwork.VirtualNetwork, sock string) error
}

type controlProtocol interface {
	requestStop(sock string, hard bool) error
}

type consoleMode interface {
	writer(consoleLog string) (io.WriteCloser, error)
	// Whether console.log carries the runner's whole output, which makes
	// runner.log a duplicate not worth showing.
	mirrorsRunnerLog() bool
}

// A nil entry is a value the schema defines but this sprout cannot run yet:
// manifests carrying it validate, and booting them is refused.
var (
	networkTransports = map[string]networkTransport{
		"vfkit-unixgram": vfkitUnixgram{},
		"qemu-stream":    qemuStream{},
	}
	controlProtocols = map[string]controlProtocol{
		"vfkit-rest": vfkitREST{},
		"qmp":        qmpControl{},
	}
	consoleModes = map[string]consoleMode{
		"pty-announce": ptyAnnounce{},
		"stdio":        stdioConsole{},
	}
)

// argMatcher recognises one whole command-line argument.
type argMatcher func(arg string) bool

// A path to any socket directly inside dir, not just the one the current
// manifest names: a runner or sidecar started from a bundle that named its
// socket differently is still this instance's, and still holds its disk.
func socketIn(dir string) func(path string) bool {
	return func(path string) bool {
		return filepath.Dir(path) == dir && socketNamePattern.MatchString(filepath.Base(path))
	}
}

// key=<socket> as a whole argument.
func socketArg(key string, inDir func(string) bool) argMatcher {
	return func(arg string) bool {
		path, ok := strings.CutPrefix(arg, key+"=")
		return ok && inDir(path)
	}
}

// A comma-separated device argument by its head and one key=<socket> option,
// so a path that merely extends a socket path never matches.
func deviceSocketArg(head, key string, inDir func(string) bool) argMatcher {
	return func(arg string) bool {
		fields := strings.Split(arg, ",")
		return fields[0] == head && slices.ContainsFunc(fields[1:], socketArg(key, inDir))
	}
}

type backendVocabulary struct {
	network, control, console string
}

// The host set of each kind must agree with nix/systems.nix, or a bundle the
// flake builds is refused here (or the reverse).
type backendKind struct {
	name  string
	hosts []string
	// What this kind's Nix backend module emits; the kind is bootable once
	// every value in it is implemented.
	vocabulary backendVocabulary
	// The runner argument, naming a socket the daemon substituted, that
	// identifies this kind's VM process. instDir covers runners from before
	// the socket directory, which carry the instance directory instead.
	runnerArgs func(sockDir, instDir string) []argMatcher
	// Optional: a hint for a runner failure only this kind can explain.
	failureHint func(runnerOutput, instDir string) string
}

var backendKinds = []*backendKind{
	{
		name:       "vfkit",
		hosts:      []string{"aarch64-darwin"},
		vocabulary: backendVocabulary{network: "vfkit-unixgram", control: "vfkit-rest", console: "pty-announce"},
		runnerArgs: func(sockDir, instDir string) []argMatcher {
			return []argMatcher{
				deviceSocketArg("virtio-net", "unixSocketPath", socketIn(sockDir)),
				deviceSocketArg("virtio-net", "unixSocketPath", socketIn(instDir)),
			}
		},
		failureHint: vfkitRunnerFailureHint,
	},
	{
		name:       "qemu",
		hosts:      []string{"aarch64-linux", "x86_64-linux"},
		vocabulary: backendVocabulary{network: "qemu-stream", control: "qmp", console: "stdio"},
		runnerArgs: func(sockDir, _ string) []argMatcher {
			return []argMatcher{deviceSocketArg("stream", "addr.path", socketIn(sockDir))}
		},
		failureHint: qemuRunnerFailureHint,
	},
}

func lookupBackendKind(name string) *backendKind {
	for _, k := range backendKinds {
		if k.name == name {
			return k
		}
	}
	return nil
}

func backendKindsFor(host string) []*backendKind {
	var kinds []*backendKind
	for _, k := range backendKinds {
		if slices.Contains(k.hosts, host) {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func kindNames(kinds []*backendKind) string {
	if len(kinds) == 0 {
		return "no backend"
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.name
	}
	return strings.Join(names, ", ")
}

func (k *backendKind) implemented() bool {
	return networkTransports[k.vocabulary.network] != nil &&
		controlProtocols[k.vocabulary.control] != nil &&
		consoleModes[k.vocabulary.console] != nil
}

func notImplementedError(kind string) error {
	return fmt.Errorf("the %s backend is not implemented in this sprout yet", kind)
}

// Answered from the registry rather than the OS, so a host gains boot support
// exactly when one of its kinds has every operation implemented.
func bootableHostFor(host string) error {
	kinds := backendKindsFor(host)
	for _, k := range kinds {
		if k.implemented() {
			return nil
		}
	}
	if len(kinds) == 0 {
		return fmt.Errorf("sprout cannot boot VMs on %s: no backend supports this host", host)
	}
	return fmt.Errorf("sprout cannot boot VMs on %s yet: %w", host, notImplementedError(kindNames(kinds)))
}

// A single path component that fits the closed placeholder form
// /sprout/placeholder/sock/<name>; "control.sock" is the daemon's own.
var socketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\.sock$`)

type backendContract struct {
	kind          *backendKind
	transport     string
	network       networkTransport
	networkSocket string
	protocol      string
	control       controlProtocol
	controlSocket string
	consoleMode   string
	console       consoleMode
	sidecars      []SidecarSpec
}

// Validation only: a contract can be valid for a backend whose operations
// this sprout does not implement, which bootable reports separately.
func parseBackendContract(m *Manifest, host string) (*backendContract, error) {
	spec := m.Backend
	kind := lookupBackendKind(spec.Kind)
	if kind == nil || m.Host.System != host || !slices.Contains(kind.hosts, host) {
		target := spec.Kind
		if target == "" {
			target = "no backend"
		}
		on := m.Host.System
		if on == "" {
			on = "no host system"
		}
		return nil, fmt.Errorf("this bundle targets %s on %s; this sprout on %s supports %s", target, on, host, kindNames(backendKindsFor(host)))
	}
	if m.Guest.System == "" {
		return nil, fmt.Errorf("manifest names no guest system")
	}
	c := &backendContract{
		kind:          kind,
		transport:     spec.Network.Transport,
		networkSocket: spec.Network.Socket,
		protocol:      spec.Control.Protocol,
		controlSocket: spec.Control.Socket,
		consoleMode:   spec.Console.Mode,
		sidecars:      spec.Sidecars,
	}
	var ok bool
	if c.network, ok = networkTransports[c.transport]; !ok {
		return nil, unknownVocabulary("network transport", c.transport, networkTransports)
	}
	if c.control, ok = controlProtocols[c.protocol]; !ok {
		return nil, unknownVocabulary("control protocol", c.protocol, controlProtocols)
	}
	if c.console, ok = consoleModes[c.consoleMode]; !ok {
		return nil, unknownVocabulary("console mode", c.consoleMode, consoleModes)
	}

	owner := map[string]string{controlSocketName: "sprout's own control socket"}
	claim := func(what, name string) error {
		if !socketNamePattern.MatchString(name) {
			return fmt.Errorf("manifest %s socket %q is not a plain socket name (want lowercase letters, digits, '.', '_', '-', ending in .sock)", what, name)
		}
		if prev, taken := owner[name]; taken {
			return fmt.Errorf("manifest %s socket %q collides with %s", what, name, prev)
		}
		owner[name] = "the " + what + " socket"
		return nil
	}
	if err := claim("network", c.networkSocket); err != nil {
		return nil, err
	}
	if err := claim("control", c.controlSocket); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, s := range c.sidecars {
		if s.Name == "" {
			return nil, fmt.Errorf("manifest has a sidecar with no name")
		}
		if names[s.Name] {
			return nil, fmt.Errorf("manifest names sidecar %q twice", s.Name)
		}
		names[s.Name] = true
		if len(s.Exec) == 0 || !filepath.IsAbs(s.Exec[0]) {
			return nil, fmt.Errorf("manifest sidecar %q must exec an absolute path", s.Name)
		}
		if err := claim("sidecar "+s.Name+" readiness", s.Ready.Socket); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func unknownVocabulary[T any](what, value string, known map[string]T) error {
	names := make([]string, 0, len(known))
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("manifest backend %s %q is not one this sprout knows (it knows %s); upgrade sprout", what, value, strings.Join(names, ", "))
}

func (c *backendContract) bootable() error {
	if c.network == nil || c.control == nil || c.console == nil || len(c.sidecars) > 0 {
		return notImplementedError(c.kind.name)
	}
	return nil
}

// Every socket the backend binds or dials, by manifest name.
func (c *backendContract) socketNames() []string {
	names := []string{c.networkSocket, c.controlSocket}
	for _, s := range c.sidecars {
		names = append(names, s.Ready.Socket)
	}
	return names
}

// Sidecars are matched whatever this manifest declares: one left by a bundle
// that had them still holds this instance's shares.
func (c *backendContract) orphanArgs(sockDir, instDir string) []argMatcher {
	return append(c.kind.runnerArgs(sockDir, instDir), socketArg("--socket-path", socketIn(sockDir)))
}

func (c *backendContract) runnerFailureHint(runnerOutput, instDir string) string {
	if c.kind.failureHint == nil {
		return ""
	}
	return c.kind.failureHint(runnerOutput, instDir)
}
