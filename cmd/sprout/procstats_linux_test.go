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
func statLine(pid int, comm string, ppid int, utime, stime, start uint64) string {
	f := make([]string, 50)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[1] = fmt.Sprint(ppid)
	f[11] = fmt.Sprint(utime)
	f[12] = fmt.Sprint(stime)
	f[19] = fmt.Sprint(start)
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(f, " "))
}

// A comm may hold spaces and parentheses; the fields are counted from the
// last ')'.
func TestParseProcStatReadsFieldsPastAnAwkwardComm(t *testing.T) {
	s, err := parseProcStat(statLine(4242, "qemu (x) ) y", 1, 30, 12, 98765))
	if err != nil {
		t.Fatal(err)
	}
	want := procStat{pid: 4242, ppid: 1, start: 98765, ticks: 42}
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
		strings.Replace(statLine(4242, "qemu", 1, 0, 0, 98765), " 98765 ", " later ", 1),
	} {
		if s, err := parseProcStat(line); err == nil {
			t.Errorf("parseProcStat(%q) = %+v, want an error", line, s)
		}
	}
}

const smapsRollup = "" +
	"6231e22ea000-7ffe8efa6000 ---p 00000000 00:00 0                          [rollup]\n" +
	"Rss:             3164 kB\n" +
	"Pss:              288 kB\n" +
	"Pss_Dirty:        204 kB\n" +
	"Pss_Anon:         204 kB\n" +
	"Shared_Clean:    2896 kB\n"

// The Pss total is taken, not Rss or the Pss_* breakdown that follows it.
func TestParseSmapsRollupPSSReadsThePssTotal(t *testing.T) {
	got, err := parseSmapsRollupPSS(strings.NewReader(smapsRollup))
	if err != nil {
		t.Fatal(err)
	}
	if got != 288*1024 {
		t.Errorf("PSS = %d, want %d", got, 288*1024)
	}
}

func TestParseSmapsRollupPSSRejectsMissingOrMalformedPss(t *testing.T) {
	for _, in := range []string{
		"",
		"Rss:  3164 kB\n",
		"Pss:  288 pages\n",
		"Pss:  many kB\n",
	} {
		if got, err := parseSmapsRollupPSS(strings.NewReader(in)); err == nil {
			t.Errorf("parseSmapsRollupPSS(%q) = %d, want an error", in, got)
		}
	}
}

type fakeProcEntry struct {
	stat  string
	smaps string
}

func fakeProc(t *testing.T, entries map[string]fakeProcEntry) string {
	t.Helper()
	dir := t.TempDir()
	for name, e := range entries {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for file, content := range map[string]string{"stat": e.stat, "smaps_rollup": e.smaps} {
			if content == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, name, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// Exited or unreadable pids, non-pid entries, and a stat naming a pid other
// than its directory's are left out rather than failing the scan.
func TestReadProcStatsSkipsWhatItCannotTrust(t *testing.T) {
	dir := fakeProc(t, map[string]fakeProcEntry{
		"100":  {stat: statLine(100, "qemu", 1, 0, 0, 5)},
		"101":  {},
		"102":  {stat: "garbage"},
		"103":  {stat: statLine(999, "other", 1, 0, 0, 5)},
		"self": {stat: statLine(100, "qemu", 1, 0, 0, 5)},
	})

	got := readProcStats(dir)

	if len(got) != 1 || got[100].start != 5 {
		t.Errorf("expected only pid 100, got %+v", got)
	}
}

func TestReadPSSKeepsTheFigureWhileTheSameProcessHoldsThePID(t *testing.T) {
	dir := fakeProc(t, map[string]fakeProcEntry{
		"100": {stat: statLine(100, "qemu", 1, 0, 0, 500), smaps: smapsRollup},
	})

	got, ok := readPSS(dir, procStat{pid: 100, ppid: 1, start: 500})

	if !ok || got != 288*1024 {
		t.Errorf("readPSS = %d, %t, want %d, true", got, ok, 288*1024)
	}
}

// stat is read again after smaps_rollup; a start time differing from the
// snapshot's means the pid changed hands, so the figure may be someone
// else's and is dropped.
func TestReadPSSDropsAFigureWhenThePIDChangedHands(t *testing.T) {
	dir := fakeProc(t, map[string]fakeProcEntry{
		"100": {stat: statLine(100, "other", 1, 0, 0, 900), smaps: smapsRollup},
	})

	if got, ok := readPSS(dir, procStat{pid: 100, ppid: 1, start: 500}); ok {
		t.Errorf("readPSS = %d, true; want the figure dropped", got)
	}
}

// An unreadable smaps_rollup (the process exited, or ptrace access is
// denied) yields no figure rather than an RSS fallback.
func TestReadPSSReportsNothingWithoutSmapsRollup(t *testing.T) {
	dir := fakeProc(t, map[string]fakeProcEntry{
		"100": {stat: statLine(100, "qemu", 1, 0, 0, 500)},
	})

	if got, ok := readPSS(dir, procStat{pid: 100, ppid: 1, start: 500}); ok {
		t.Errorf("readPSS = %d, true; want no figure", got)
	}
}

func procTable(stats ...procStat) map[int]procStat {
	m := map[int]procStat{}
	for _, s := range stats {
		m[s.pid] = s
	}
	return m
}

func pssTable(m map[int]int64) func(procStat) (int64, bool) {
	return func(s procStat) (int64, bool) {
		v, ok := m[s.pid]
		return v, ok
	}
}

// QEMU (with a child of its own) and a virtiofsd sidecar (with its sandbox
// child) are summed; an unrelated process is not.
func TestAggregateProcTreeSumsRunnerAndSidecarTrees(t *testing.T) {
	stats := procTable(
		procStat{pid: 1, ppid: 0, start: 1},
		procStat{pid: 50, ppid: 1, start: 400},
		procStat{pid: 100, ppid: 50, start: 500, ticks: 1000},
		procStat{pid: 101, ppid: 100, start: 510},
		procStat{pid: 200, ppid: 50, start: 450, ticks: 500},
		procStat{pid: 201, ppid: 200, start: 460},
		procStat{pid: 999, ppid: 1, start: 600},
	)
	pss := pssTable(map[int]int64{1: 1000, 50: 1000, 100: 300, 101: 20, 200: 4, 201: 1, 999: 5000})
	roots := []procIdentity{{pid: 100, start: 500}, {pid: 200, start: 450}}

	got := aggregateProcTree(stats, roots, 15, pss)

	if want := int64(300 + 20 + 4 + 1); got.MemBytes != want {
		t.Errorf("MemBytes = %d, want %d", got.MemBytes, want)
	}
	// 10s of CPU over 10s alive plus 5s over 10.5s alive.
	if want := 100.0 + 500.0/10.5; got.CPUPct < want-0.001 || got.CPUPct > want+0.001 {
		t.Errorf("CPUPct = %v, want %v", got.CPUPct, want)
	}
}

// Guest RAM mapped by both QEMU and virtiofsd is split between them in PSS,
// so the total counts it once: 1000 shared plus 10 and 2 private, not the
// 2012 their RSS would sum to.
func TestAggregateProcTreeCountsSharedGuestRAMOnce(t *testing.T) {
	const shared, qemuPrivate, sidecarPrivate = 1000, 10, 2
	stats := procTable(
		procStat{pid: 100, ppid: 50, start: 500},
		procStat{pid: 200, ppid: 50, start: 450},
	)
	pss := pssTable(map[int]int64{100: qemuPrivate + shared/2, 200: sidecarPrivate + shared/2})
	roots := []procIdentity{{pid: 100, start: 500}, {pid: 200, start: 450}}

	if got := aggregateProcTree(stats, roots, 15, pss); got.MemBytes != shared+qemuPrivate+sidecarPrivate {
		t.Errorf("MemBytes = %d, want %d", got.MemBytes, shared+qemuPrivate+sidecarPrivate)
	}
}

// A process whose PSS is unavailable contributes neither memory nor CPU,
// while the rest of the tree is still reported.
func TestAggregateProcTreeLeavesOutProcessesWithoutPSS(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 500, ticks: 1000},
		procStat{pid: 101, ppid: 100, start: 510, ticks: 1000},
	)
	pss := pssTable(map[int]int64{101: 20})

	got := aggregateProcTree(stats, []procIdentity{{pid: 100, start: 500}}, 15, pss)

	if got.MemBytes != 20 {
		t.Errorf("MemBytes = %d, want 20", got.MemBytes)
	}
	if want := 1000.0 / 9.9; got.CPUPct < want-0.001 || got.CPUPct > want+0.001 {
		t.Errorf("CPUPct = %v, want only pid 101's %v", got.CPUPct, want)
	}
}

// A root whose pid now belongs to a process with another start time has
// exited; neither that process nor its children may be reported.
func TestAggregateProcTreeIgnoresReusedRootPID(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 900},
		procStat{pid: 101, ppid: 100, start: 910},
	)
	pss := pssTable(map[int]int64{100: 300, 101: 20})

	got := aggregateProcTree(stats, []procIdentity{{pid: 100, start: 500}}, 15, pss)

	if got != (procStats{}) {
		t.Errorf("expected nothing for a reused pid, got %+v", got)
	}
}

// A root that has exited, or whose start time was never recorded, reports
// nothing rather than whatever holds the pid now.
func TestAggregateProcTreeTreatsUnverifiableRootsAsAbsent(t *testing.T) {
	stats := procTable(procStat{pid: 100, ppid: 1, start: 500})
	pss := pssTable(map[int]int64{100: 300})

	for _, root := range []procIdentity{{pid: 4242, start: 500}, {pid: 100}} {
		if got := aggregateProcTree(stats, []procIdentity{root}, 15, pss); got != (procStats{}) {
			t.Errorf("root %+v: expected nothing, got %+v", root, got)
		}
	}
}

// A child older than its parent was read against an earlier holder of the
// parent's pid, so it is not part of the tree.
func TestAggregateProcTreeSkipsChildOlderThanParent(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 500},
		procStat{pid: 101, ppid: 100, start: 400},
	)
	pss := pssTable(map[int]int64{100: 300, 101: 20})

	got := aggregateProcTree(stats, []procIdentity{{pid: 100, start: 500}}, 15, pss)

	if got.MemBytes != 300 {
		t.Errorf("MemBytes = %d, want 300", got.MemBytes)
	}
}

func TestAggregateProcTreeCountsSharedDescendantOnce(t *testing.T) {
	stats := procTable(
		procStat{pid: 100, ppid: 1, start: 500},
		procStat{pid: 101, ppid: 100, start: 510},
	)
	pss := pssTable(map[int]int64{100: 300, 101: 20})
	roots := []procIdentity{{pid: 100, start: 500}, {pid: 101, start: 510}}

	if got := aggregateProcTree(stats, roots, 15, pss); got.MemBytes != 320 {
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
		t.Errorf("expected proportional memory for a live process, got %+v", live)
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
