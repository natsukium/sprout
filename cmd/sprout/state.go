package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Manifest is the host-side instruction set the Nix flake module produces.
// The binary implements only the generic substitution symbols; everything
// tool-specific stays in Nix.
type Manifest struct {
	Version    int    `json:"version"`
	Definition string `json:"definition"`
	Guest      struct {
		IP        string `json:"ip"`
		GatewayIP string `json:"gatewayIp"`
		Subnet    string `json:"subnet"`
		MAC       string `json:"mac"`
		SSHUser   string `json:"sshUser"`
	} `json:"guest"`
	Workspace bool `json:"workspace"`
	// HostLoopback lets the guest reach the host's 127.0.0.1 through the
	// gateway alias.
	HostLoopback bool   `json:"hostLoopback"`
	GuestArch    string `json:"guestArch"`
	RestSocket   string `json:"restSocket"`
	// Idle drives auto-stop: "stop" powers the instance off after After of no
	// activity, "none" disables it. Activity is SSH sessions plus router
	// connections that opt in with the DIAL "track" suffix.
	Idle struct {
		Action string `json:"action"`
		After  string `json:"after"`
	} `json:"idle"`
	// DNS names the domains whose subdomains the gateway resolver answers
	// with the guest's own loopback.
	DNS struct {
		WildcardDomains []string `json:"wildcardDomains,omitempty"`
	} `json:"dns,omitempty"`
	Credentials   []CredentialSpec `json:"credentials"`
	Caches        []CacheSpec      `json:"caches"`
	Substitutions []struct {
		Placeholder string `json:"placeholder"`
		Value       string `json:"value"`
	} `json:"substitutions"`
}

// CredentialSpec's used fields depend on Strategy: `mount` reads Source, a
// host path resolved into the runner substitution.
type CredentialSpec struct {
	Name      string   `json:"name"`
	Strategy  string   `json:"strategy"`
	Source    string   `json:"source,omitempty"`
	Exec      []string `json:"exec,omitempty"`
	GuestPort int      `json:"guestPort,omitempty"`
}

// CacheSpec's Scope decides where the host directory lives: "shared" under the
// per-arch cache root, reused by every project; "project" one level deeper,
// keyed by the clone. Instance-scoped caches live on the guest's /var and
// never appear here.
type CacheSpec struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

type Instance struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	// KeySource is "branch", "name", or "directory": whether Name is a branch
	// worth comparing against the live checkout.
	KeySource string `json:"keySource"`
	// RepoRoot is the repository's git-common-dir, shared by every worktree of
	// the same clone, or Workspace outside a git repository.
	RepoRoot   string `json:"repoRoot"`
	Definition string `json:"definition"`
	Bundle     string `json:"bundle"`
	Workspace  string `json:"workspace"`
	// WorkspaceMounted mirrors the manifest's workspace flag so client commands
	// need not read the bundle, which nix GC may have reclaimed by then.
	WorkspaceMounted bool   `json:"workspaceMounted,omitempty"`
	GuestIP          string `json:"guestIp"`
	SSHUser          string `json:"sshUser"`
	PID              int    `json:"pid"`
}

const instanceSchemaVersion = 1

func xdgDir(envVar string, fallback ...string) (string, error) {
	if dir := os.Getenv(envVar); dir != "" {
		return filepath.Join(dir, "sprout"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	parts := append(append([]string{home}, fallback...), "sprout")
	return filepath.Join(parts...), nil
}

func stateRoot() (string, error) {
	return xdgDir("XDG_STATE_HOME", ".local", "state")
}

func cacheRoot() (string, error) {
	return xdgDir("XDG_CACHE_HOME", ".cache")
}

func instanceDir(id string) (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "instances", id), nil
}

func varImagePath(instDir string) string       { return filepath.Join(instDir, "var.img") }
func instanceRecordPath(instDir string) string { return filepath.Join(instDir, "instance.json") }
func consoleLogPath(instDir string) string     { return filepath.Join(instDir, "console.log") }
func runnerLogPath(instDir string) string      { return filepath.Join(instDir, "runner.log") }
func upLogPath(instDir string) string          { return filepath.Join(instDir, "up.log") }

func knownHostsPath(instDir string) string { return filepath.Join(instDir, "known_hosts") }

// The data share, mounted in the guest at /run/sprout. These names are a
// contract with the guest modules that read them, so they cannot change here
// alone — nix/guest/base.nix names the same files.
func dataDir(instDir string) string    { return filepath.Join(instDir, "data") }
func sshDataDir(instDir string) string { return filepath.Join(dataDir(instDir), "ssh") }
func authorizedKeysPath(instDir string) string {
	return filepath.Join(sshDataDir(instDir), "authorized_keys")
}
func instanceEnvPath(instDir string) string { return filepath.Join(dataDir(instDir), "instance.env") }
func readyFilePath(instDir string) string   { return filepath.Join(dataDir(instDir), "ready") }
func credentialsDir(instDir string) string  { return filepath.Join(dataDir(instDir), "credentials") }

func loadInstance(id string) (*Instance, string, error) {
	dir, err := instanceDir(id)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(instanceRecordPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, dir, &instanceNotFoundError{selector: id}
		}
		return nil, dir, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return nil, dir, err
	}
	if inst.Version != instanceSchemaVersion {
		return nil, dir, fmt.Errorf("instance %q uses state version %d, but this sprout supports version %d; delete it and run `sprout up` again", id, inst.Version, instanceSchemaVersion)
	}
	return &inst, dir, nil
}

// Unwraps to os.ErrNotExist so callers keep telling "no record here" (status
// reports it as absent) from a record that failed to read.
type instanceNotFoundError struct{ selector string }

func (e *instanceNotFoundError) Error() string {
	return fmt.Sprintf("instance %q not found (run `sprout up` first)", e.selector)
}
func (e *instanceNotFoundError) Unwrap() error { return os.ErrNotExist }

const manifestSchemaVersion = 1

// An unsupported schema version fails loudly, rather than booting with
// silently ignored instructions.
func jsonUnmarshalStrictVersion(data []byte, m *Manifest) error {
	if err := json.Unmarshal(data, m); err != nil {
		return err
	}
	if m.Version != manifestSchemaVersion {
		return fmt.Errorf("manifest version %d is not supported by this sprout binary", m.Version)
	}
	return nil
}

// Temp file and rename, so a concurrent reader never sees a truncated write
// and misreads it as corrupt state.
func writeJSON(path string, v any) error {
	if inst, ok := v.(*Instance); ok && inst.Version == 0 {
		inst.Version = instanceSchemaVersion
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// One key for all instances: the isolation boundary is the VM, not the key.
func ensureSSHKey() (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	keyPath := filepath.Join(root, "id_ed25519")
	// Two parallel first boots would both pass the Stat below and race
	// ssh-keygen on the same path; the lock makes check-and-create atomic.
	lockPath := filepath.Join(root, ".id_ed25519.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("locking ssh key: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	if _, err := os.Stat(keyPath); err == nil {
		return keyPath, nil
	}
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sprout", "-f", keyPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %w", err)
	}
	return keyPath, nil
}
