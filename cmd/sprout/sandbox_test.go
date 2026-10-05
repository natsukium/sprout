package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func fakeSysctls(t *testing.T, values map[string]string) {
	prev := readSysctl
	readSysctl = func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
	t.Cleanup(func() { readSysctl = prev })
}

func TestSandboxFailureNamesACauseOnlyWhenTheHostProvesIt(t *testing.T) {
	for _, c := range []struct {
		name    string
		sysctls map[string]string
		want    []string
		notWant []string
	}{
		{"userns clone disabled", map[string]string{"kernel.unprivileged_userns_clone": "0"},
			[]string{"kernel.unprivileged_userns_clone = 0", "set it to 1"}, []string{"candidates"}},
		{"no user namespaces allowed", map[string]string{"user.max_user_namespaces": "0"},
			[]string{"user.max_user_namespaces = 0", "raise it"}, []string{"candidates"}},
		// The AppArmor sysctl shows a restriction exists, not that it caused
		// this failure.
		{"apparmor restriction present", map[string]string{"kernel.apparmor_restrict_unprivileged_userns": "1", "kernel.unprivileged_userns_clone": "1"},
			[]string{"does not reveal", "AppArmor", "seccomp"}, nil},
		{"nothing readable", map[string]string{},
			[]string{"does not reveal", "seccomp"}, []string{"AppArmor"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeSysctls(t, c.sysctls)
			got := explainSandboxFailure()
			for _, w := range append(c.want, "does not fall back to 9p") {
				if !strings.Contains(got, w) {
					t.Errorf("explanation lacks %q:\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("explanation has %q:\n%s", w, got)
				}
			}
		})
	}
}

// TestVirtiofsdStandIn is a helper process, not a test: a virtiofsd behaving
// as SPROUT_TEST_VIRTIOFSD says.
func TestVirtiofsdStandIn(t *testing.T) {
	mode := os.Getenv("SPROUT_TEST_VIRTIOFSD")
	if mode == "" {
		t.Skip("helper process only")
	}
	var sock string
	for _, a := range os.Args {
		if p, ok := strings.CutPrefix(a, "--socket-path="); ok {
			sock = p
		}
	}
	switch mode {
	case "serve":
		if _, err := net.Listen("unix", sock); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	case "fail":
		fmt.Fprintln(os.Stderr, "[ERROR virtiofsd] Error entering sandbox: Unshare(Os { code: 1, kind: PermissionDenied })")
		os.Exit(1)
	case "silent":
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

func virtiofsdStandIn(t *testing.T, mode string) []string {
	t.Setenv("SPROUT_TEST_VIRTIOFSD", mode)
	return []string{os.Args[0], "-test.run=^TestVirtiofsdStandIn$", "--"}
}

func TestProbePassesOnceVirtiofsdBindsItsSocket(t *testing.T) {
	shortenSidecarWaits(t)
	if err := probeVirtiofsd(virtiofsdStandIn(t, "serve")); err != nil {
		t.Fatal(err)
	}
}

func TestProbeReportsASandboxFailureWithItsExplanation(t *testing.T) {
	shortenSidecarWaits(t)
	fakeSysctls(t, map[string]string{"kernel.unprivileged_userns_clone": "0"})
	err := probeVirtiofsd(virtiofsdStandIn(t, "fail"))
	if err == nil {
		t.Fatal("probe passed for a virtiofsd that could not enter its sandbox")
	}
	for _, want := range []string{"exited before its socket appeared", "Error entering sandbox", "kernel.unprivileged_userns_clone = 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestProbeTimesOutOnAVirtiofsdThatNeverBinds(t *testing.T) {
	shortenSidecarWaits(t)
	sidecarReadyTimeout = 300 * time.Millisecond
	err := probeVirtiofsd(virtiofsdStandIn(t, "silent"))
	if err == nil || !strings.Contains(err.Error(), "did not create its socket") {
		t.Fatalf("err = %v, want a readiness timeout", err)
	}
}

// Whichever way the host answers, the real virtiofsd's verdict must be a
// clean pass or a failure carrying its explanation, never a probe malfunction.
func TestProbeOfTheRealVirtiofsd(t *testing.T) {
	path, err := exec.LookPath("virtiofsd")
	if err != nil {
		t.Skip("no virtiofsd on PATH")
	}
	err = probeVirtiofsd([]string{path})
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), sandboxFailureMarker) || !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("the probe failed without a sandbox diagnosis:\n%v", err)
	}
	t.Logf("this host refuses virtiofsd's sandbox:\n%v", err)
}
