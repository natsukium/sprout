//go:build !linux

package main

import "os/exec"

// No PDEATHSIG here: a runner outliving a crashed daemon is left to the next
// boot's orphan reaper.
func startManaged(cmd *exec.Cmd) (*runnerExit, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return watchRunner(cmd), nil
}

// Not scanned without /proc; under vfkit a second holder surfaces instead as
// the framework's storage-attachment error, which vfkitRunnerFailureHint
// explains.
func varImageHolders(string) []string { return nil }
