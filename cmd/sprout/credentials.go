package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func expandTilde(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}

// virtiofs cannot follow a symlink used as the shared directory, so resolve
// sources before mounting. Reject empty expansion because filepath.Abs would
// otherwise turn it into the current directory.
func resolveCredentialSource(source string) (string, error) {
	source = os.ExpandEnv(source)
	if source == "" {
		return "", fmt.Errorf("source expanded to an empty string (unset environment variable?)")
	}
	expanded, err := expandTilde(source)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("credential source %q: %w", source, err)
	}
	return resolved, nil
}

// Unsupported strategies fail before boot so the manifest cannot silently
// omit a credential.
func setupCredentials(m *Manifest, subs map[string]string, dataDir string) error {
	for _, c := range m.Credentials {
		// A separator would let materialize escape data/credentials.
		if err := validateNameComponent("credential", c.Name); err != nil {
			return err
		}
		switch c.Strategy {
		case "mount":
			resolved, err := resolveCredentialSource(c.Source)
			if err != nil {
				return fmt.Errorf("credential %q: %w", c.Name, err)
			}
			subs["credential:"+c.Name] = resolved
		case "materialize":
			if err := materializeCredential(c, dataDir); err != nil {
				return fmt.Errorf("credential %q: %w", c.Name, err)
			}
		case "socket":
			// Socket projection waits for runDaemon because it needs the network stack.
		default:
			return fmt.Errorf("credential %q: unsupported strategy %q", c.Name, c.Strategy)
		}
	}
	return nil
}

// Empty output is valid because credentials such as aws-config may have
// nothing to project.
func materializeCredential(c CredentialSpec, dataDir string) error {
	if len(c.Exec) == 0 {
		return fmt.Errorf("materialize strategy requires a non-empty exec command")
	}
	cmd := exec.Command(c.Exec[0], c.Exec[1:]...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("running %v: %w", c.Exec, err)
	}
	dir := filepath.Join(dataDir, "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, c.Name), out, 0o600)
}

// SIGKILL and host crashes bypass daemon cleanup. Sweep only while holding the
// instance lock; a lock failure means a live daemon owns the credentials.
func sweepStaleCredentials(id string) {
	dir, err := instanceDir(id)
	if err != nil {
		return
	}
	lock, err := acquireInstanceLock(dir, 0)
	if err != nil {
		return
	}
	defer lock.Close()
	_ = os.RemoveAll(credentialsDir(dir))
}
