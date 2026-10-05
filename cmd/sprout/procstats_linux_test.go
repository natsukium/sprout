package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// statLine renders a proc_pid_stat(5) line with the fields sprout reads set
// and every other field zero.
func statLine(pid int, comm string, ppid int, utime, stime, start uint64, rss int64) string {
	f := make([]string, 50)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[1] = fmt.Sprint(ppid)
	f[11] = fmt.Sprint(utime)
	f[12] = fmt.Sprint(stime)
	f[19] = fmt.Sprint(start)
	f[21] = fmt.Sprint(rss)
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(f, " "))
}

// A comm may hold spaces and parentheses; the fields are counted from the
// last ')'.
func TestParseProcStatReadsFieldsPastAnAwkwardComm(t *testing.T) {
	s, err := parseProcStat(statLine(4242, "qemu (x) ) y", 1, 30, 12, 98765, 2048))
	if err != nil {
		t.Fatal(err)
	}
	want := procStat{pid: 4242, ppid: 1, start: 98765, ticks: 42, rssPages: 2048}
	if s != want {
		t.Errorf("parsed %+v, want %+v", s, want)
	}
}

func TestParseProcStatRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{
		"",
		"4242 qemu S 1",
		"4242 (qemu) S 1 2 3",
		"x (qemu) " + strings.Repeat("0 ", 30),
		strings.Replace(statLine(4242, "qemu", 1, 0, 0, 5, 7), " 7 ", " seven ", 1),
	} {
		if s, err := parseProcStat(line); err == nil {
			t.Errorf("parseProcStat(%q) = %+v, want an error", line, s)
		}
	}
}

func fakeProc(t *testing.T, stats map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range stats {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name, "stat"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Exited or unreadable pids, non-pid entries, and a stat naming a pid other
// than its directory's are left out rather than failing the scan.
func TestReadProcStatsSkipsWhatItCannotTrust(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"100":  statLine(100, "qemu", 1, 0, 0, 5, 7),
		"101":  "",
		"102":  "garbage",
		"103":  statLine(999, "other", 1, 0, 0, 5, 7),
		"self": statLine(100, "qemu", 1, 0, 0, 5, 7),
	})

	got := readProcStats(dir)

	if len(got) != 1 || got[100].rssPages != 7 {
		t.Errorf("expected only pid 100, got %+v", got)
	}
}

func procTable(stats ...procStat) map[int]procStat {
	m := map[int]procStat{}
	for _, s := range stats {
		m[s.pid] = s
	}
	return m
}

// QEMU (with a child of its own) and a virtiofsd sidecar (with its
// sandbox child) are summed; an unrelated process is not.
func TestAggregateProcTreeSumsRunnerAndSidecarTrees(t *testing.T) {
	stats := procTable(
		procStat{pid: 1, ppid: 0, start: 1, rssPages: 1000},
		procStat{pid: 50, ppid: 1, start: 400, rssPages: 1000},
		procStat{pid: 100, ppid: 50, start: 500, ticks: 1000, rssPages: 300},
		procStat{pid: 101, ppid: 100, start: 510, rssPages: 20},
		procStat{pid: 200, ppid: 50, start: 450, ticks: 500, rssPages: 4},
		procStat{pid: 201, ppid: 200, start: 460, rssPages: 1},
		procStat{pid: 999, ppid: 1, start: 600, rssPages: 5000},
	)
	roots := []procIdentity{{pid: 100, start: 500}, {pid: 200, start: 450}}

	got := aggregateProcTree(stats, roots, 15, 4096)

	if want := int64(300+20+4+1) * 4096; got.MemBytes != want {
		t.Errorf("MemBytes = %d, want %d", got.MemBytes, want)
	}
	// 10s of CPU over 10s alive plus 5s over 10.5s alive.
	if want := 100.0 + 500.0/10.5; got.CPUPct < want-0.001 || got.CPUPct > want+0.001 {
		t.Errorf("CPUPct = %v, want %v", got.CPUPct, want)
	}
}

// A root whose pid now belongs to a process with another start time has
// exited; neither that process nor its children may be reported.
func TestAggregateProcTreeIgnoresReusedRootPID(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 900, rssPages: 300},
		procStat{pid: 101, ppid: 100, start: 910, rssPages: 20},
	)

	got := aggregateProcTree(stats, []procIdentity{{pid: 100, start: 500}}, 15, 4096)

	if got != (procStats{}) {
		t.Errorf("expected nothing for a reused pid, got %+v", got)
	}
}

// A root that has exited, or whose start time was never recorded, reports
// nothing rather than whatever holds the pid now.
func TestAggregateProcTreeTreatsUnverifiableRootsAsAbsent(t *testing.T) {
	stats := procTable(procStat{pid: 100, ppid: 1, start: 500, rssPages: 300})

	for _, root := range []procIdentity{{pid: 4242, start: 500}, {pid: 100}} {
		if got := aggregateProcTree(stats, []procIdentity{root}, 15, 4096); got != (procStats{}) {
			t.Errorf("root %+v: expected nothing, got %+v", root, got)
		}
	}
}

// A child older than its parent was read against an earlier holder of the
// parent's pid, so it is not part of the tree.
func TestAggregateProcTreeSkipsChildOlderThanParent(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 500, rssPages: 300},
		procStat{pid: 101, ppid: 100, start: 400, rssPages: 20},
	)

	got := aggregateProcTree(stats, []procIdentity{{pid: 100, start: 500}}, 15, 1)

	if got.MemBytes != 300 {
		t.Errorf("MemBytes = %d, want 300", got.MemBytes)
	}
}

func TestAggregateProcTreeCountsSharedDescendantOnce(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 500, rssPages: 300},
		procStat{pid: 101, ppid: 100, start: 510, rssPages: 20},
	)
	roots := []procIdentity{{pid: 100, start: 500}, {pid: 101, start: 510}}

	if got := aggregateProcTree(stats, roots, 15, 1); got.MemBytes != 320 {
		t.Errorf("MemBytes = %d, want 320", got.MemBytes)
	}
}

// startManaged records the identity the sampler checks, so a live managed
// process is measured and the same pid stops being measured once reaped.
func TestSampleProcTreeMeasuresManagedProcessUntilItExits(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	exit, err := startManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if exit.proc.pid != cmd.Process.Pid || exit.proc.start == 0 {
		t.Fatalf("startManaged recorded %+v for pid %d", exit.proc, cmd.Process.Pid)
	}
	live, err := sampleProcTree([]procIdentity{exit.proc})
	if err != nil {
		t.Fatal(err)
	}
	if live.MemBytes <= 0 {
		t.Errorf("expected resident memory for a live process, got %+v", live)
	}
	stale := exit.proc
	stale.start++
	if got, _ := sampleProcTree([]procIdentity{stale}); got != (procStats{}) {
		t.Errorf("expected nothing for a mismatched start time, got %+v", got)
	}

	_ = cmd.Process.Kill()
	<-exit.done
	if got, _ := sampleProcTree([]procIdentity{exit.proc}); got != (procStats{}) {
		t.Errorf("expected nothing after the process exited, got %+v", got)
	}
}
