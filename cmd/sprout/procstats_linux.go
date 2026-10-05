package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// /proc counts CPU and start times in USER_HZ, which the kernel fixes at 100
// on every architecture Go supports; sysconf(_SC_CLK_TCK) would need cgo.
const userHZ = 100

type procStat struct {
	pid      int
	ppid     int
	start    uint64
	ticks    uint64
	rssPages int64
}

// sampleProcTree reports the resident memory and lifetime CPU of the process
// subtrees rooted at roots: the runner, which execs QEMU in place, and each
// sidecar, which the daemon spawns beside QEMU rather than under it. A root
// whose start time no longer matches has exited and its pid may belong to
// someone else, so it and everything under it count as absent.
//
// A variable so a test can stub it.
var sampleProcTree = func(roots []procIdentity) (procStats, error) {
	uptime, err := readUptime("/proc/uptime")
	if err != nil {
		return procStats{}, err
	}
	return aggregateProcTree(readProcStats("/proc"), roots, uptime, int64(os.Getpagesize())), nil
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
		b, err := os.ReadFile(filepath.Join(procDir, e.Name(), "stat"))
		if err != nil {
			continue
		}
		s, err := parseProcStat(string(b))
		if err != nil || s.pid != pid {
			continue
		}
		stats[pid] = s
	}
	return stats
}

func procStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s, err := parseProcStat(string(b))
	if err != nil {
		return 0, err
	}
	return s.start, nil
}

// Identity, parentage and RSS all come from the one stat read rather than
// adding statm or status: a second file read later could describe a process
// that has since replaced this one under the same pid.
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
	if len(f) < 22 {
		return procStat{}, fmt.Errorf("malformed stat: %d fields after comm", len(f))
	}
	ppid, err1 := strconv.Atoi(f[1])
	utime, err2 := strconv.ParseUint(f[11], 10, 64)
	stime, err3 := strconv.ParseUint(f[12], 10, 64)
	start, err4 := strconv.ParseUint(f[19], 10, 64)
	rss, err5 := strconv.ParseInt(f[21], 10, 64)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		return procStat{}, fmt.Errorf("malformed stat: %w", err)
	}
	return procStat{pid: pid, ppid: ppid, start: start, ticks: utime + stime, rssPages: rss}, nil
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

func aggregateProcTree(stats map[int]procStat, roots []procIdentity, uptime float64, pageSize int64) procStats {
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
		out.MemBytes += s.rssPages * pageSize
		out.CPUPct += lifetimeCPUPct(s, uptime)
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
