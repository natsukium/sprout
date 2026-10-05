package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProbeParentStandIn is a helper process, not a test: a `sprout doctor`
// probing a virtiofsd that never binds, so it can be interrupted mid-probe.
func TestProbeParentStandIn(t *testing.T) {
	if os.Getenv("SPROUT_TEST_PROBE_PARENT") == "" {
		t.Skip("helper process only")
	}
	sidecarReadyTimeout = time.Hour
	_ = probeVirtiofsd([]string{os.Args[0], "-test.run=^TestVirtiofsdStandIn$", "--"})
}

// An interrupted or killed doctor must not leave the probed virtiofsd
// running; an interrupt also removes the probe's directories.
func TestInterruptedProbeLeavesNothingBehind(t *testing.T) {
	for _, c := range []struct {
		sig      syscall.Signal
		cleansUp bool
	}{{syscall.SIGTERM, true}, {syscall.SIGINT, true}, {syscall.SIGKILL, false}} {
		t.Run(c.sig.String(), func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "record")
			parent := exec.Command(os.Args[0], "-test.run=^TestProbeParentStandIn$")
			parent.Env = append(os.Environ(), "SPROUT_TEST_PROBE_PARENT=1", "SPROUT_TEST_VIRTIOFSD=silent", "SPROUT_TEST_VIRTIOFSD_RECORD="+record)
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })

			var fields []string
			if !pollUntil(10*time.Second, 20*time.Millisecond, func() bool {
				b, err := os.ReadFile(record)
				fields = strings.Split(strings.TrimSpace(string(b)), "\n")
				return err == nil && len(fields) == 3
			}) {
				t.Fatal("the probed stand-in never started")
			}
			pid, _ := strconv.Atoi(fields[0])
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(fields[1])); _ = os.RemoveAll(fields[2]) })

			if err := parent.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			_ = parent.Wait()
			waitProcessGone(t, pid, "the virtiofsd of an interrupted probe")
			if !c.cleansUp {
				return
			}
			for _, dir := range []string{filepath.Dir(fields[1]), fields[2]} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("%s survived the interrupted probe (stat err %v)", dir, err)
				}
			}
		})
	}
}
