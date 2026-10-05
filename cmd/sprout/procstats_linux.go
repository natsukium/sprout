package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// /proc counts CPU and start times in USER_HZ, which the kernel fixes at 100
// on every architecture Go supports; sysconf(_SC_CLK_TCK) would need cgo.
const userHZ = 100

type procStat struct {
	pid   int
	ppid  int
	start uint64
	ticks uint64
}

// sampleProcTree reports the proportional memory and lifetime CPU of the
// process subtrees rooted at roots: the runner, which execs QEMU in place, and
// each sidecar, which the daemon spawns beside QEMU rather than under it. A
// root whose start time no longer matches has exited and its pid may belong to
// someone else, so it and everything under it count as absent.
//
// PSS rather than RSS: guest RAM is a shared memfd that QEMU and every
// vhost-user virtiofsd map, so summed RSS counts each guest page once per
// process that touched it and overstates the VM several times over.
//
// A variable so a test can stub it.
var sampleProcTree = func(roots []procIdentity) (procStats, error) {
	uptime, err := readUptime("/proc/uptime")
	if err != nil {
		return procStats{}, err
	}
	pss := func(s procStat) (int64, bool) { return readPSS("/proc", s) }
	return aggregateProcTree(readProcStats("/proc"), roots, uptime, pss), nil
}

// A process that exits mid-scan, or that this user may not read, is left out.
func readProcStats(procDir string) map[int]procStat {
	entries, _ := os.ReadDir(procDir)
	stats := map[int]procStat{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		s, err := readProcStat(procDir, pid)
		if err != nil {
			continue
		}
		stats[pid] = s
	}
	return stats
}

func readProcStat(procDir string, pid int) (procStat, error) {
	b, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	s, err := parseProcStat(string(b))
	if err != nil {
		return procStat{}, err
	}
	if s.pid != pid {
		return procStat{}, fmt.Errorf("stat of %d names pid %d", pid, s.pid)
	}
	return s, nil
}

func procStartTime(pid int) (uint64, error) {
	s, err := readProcStat("/proc", pid)
	if err != nil {
		return 0, err
	}
	return s.start, nil
}

// smaps_rollup is a second read after stat, so the pid may have changed hands
// between them; stat is read again afterwards and the figure kept only if the
// same process still holds the pid.
func readPSS(procDir string, s procStat) (int64, bool) {
	f, err := os.Open(filepath.Join(procDir, strconv.Itoa(s.pid), "smaps_rollup"))
	if err != nil {
		return 0, false
	}
	pss, err := parseSmapsRollupPSS(f)
	f.Close()
	if err != nil {
		return 0, false
	}
	again, err := readProcStat(procDir, s.pid)
	if err != nil || again.start != s.start {
		return 0, false
	}
	return pss, true
}

func parseSmapsRollupPSS(r io.Reader) (int64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Pss:")
		if !ok {
			continue
		}
		v, ok := strings.CutSuffix(strings.TrimSpace(rest), " kB")
		if !ok {
			return 0, fmt.Errorf("malformed Pss line %q", sc.Text())
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed Pss line %q: %w", sc.Text(), err)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("smaps_rollup has no Pss line")
}

func parseProcStat(line string) (procStat, error) {
	open := strings.IndexByte(line, '(')
	end := strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return procStat{}, errors.New("malformed stat: no comm")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return procStat{}, fmt.Errorf("malformed stat pid: %w", err)
	}
	// f[0] is field 3 of proc_pid_stat(5), so field n is f[n-3].
	f := strings.Fields(line[end+1:])
	if len(f) < 20 {
		return procStat{}, fmt.Errorf("malformed stat: %d fields after comm", len(f))
	}
	ppid, err1 := strconv.Atoi(f[1])
	utime, err2 := strconv.ParseUint(f[11], 10, 64)
	stime, err3 := strconv.ParseUint(f[12], 10, 64)
	start, err4 := strconv.ParseUint(f[19], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return procStat{}, fmt.Errorf("malformed stat: %w", err)
	}
	return procStat{pid: pid, ppid: ppid, start: start, ticks: utime + stime}, nil
}

func readUptime(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("%s: empty", path)
	}
	return strconv.ParseFloat(f[0], 64)
}

// A process whose PSS cannot be read contributes nothing rather than its RSS
// from stat, which would bring back the shared-RAM overcount.
func aggregateProcTree(stats map[int]procStat, roots []procIdentity, uptime float64, pss func(procStat) (int64, bool)) procStats {
	children := map[int][]int{}
	for pid, s := range stats {
		children[s.ppid] = append(children[s.ppid], pid)
	}

	var stack []int
	for _, r := range roots {
		if s, ok := stats[r.pid]; ok && r.start != 0 && s.start == r.start {
			stack = append(stack, r.pid)
		}
	}

	var out procStats
	seen := map[int]bool{}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		s := stats[pid]
		if mem, ok := pss(s); ok {
			out.MemBytes += mem
			out.CPUPct += lifetimeCPUPct(s, uptime)
		}
		for _, c := range children[pid] {
			// The scan is not atomic, so a parent's pid can change hands
			// between reading a child and reading the parent; a child
			// never predates its real parent.
			if stats[c].start >= s.start {
				stack = append(stack, c)
			}
		}
	}
	return out
}

// Lifetime average, as procps `ps -o pcpu` reports it.
func lifetimeCPUPct(s procStat, uptime float64) float64 {
	elapsed := uptime - float64(s.start)/userHZ
	if elapsed <= 0 {
		return 0
	}
	return float64(s.ticks) / userHZ / elapsed * 100
}
