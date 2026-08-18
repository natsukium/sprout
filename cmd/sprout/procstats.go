package main

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type procStats struct {
	MemBytes int64
	CPUPct   float64
}

// sampleProcTree reports the host resource use of the process subtree rooted
// at root — how much host CPU/memory the VM actually occupies, not what it
// was allocated (vfkit lazily faults guest RAM into its own address space, so
// memory is a high-water mark that never balloons back down). The subtree is
// summed, not just root, because microvm's runner may exec vfkit in place or
// spawn it as a child. `ps` refuses the rss/vsz keywords without an
// entitlement sprout cannot claim, so memory comes from `footprint` instead
// while CPU still comes from `ps`; both are cgo-free.
//
// A variable so a test can stub it: `footprint` is macOS-only.
var sampleProcTree = func(root int) (procStats, error) {
	psOut, err := exec.Command("ps", "-axo", "pid=,ppid=,pcpu=").Output()
	if err != nil {
		return procStats{}, err
	}
	pids, cpu := parseProcTree(string(psOut), root)

	mem, err := footprintBytes(pids)
	if err != nil {
		return procStats{}, err
	}
	return procStats{MemBytes: mem, CPUPct: cpu}, nil
}

func parseProcTree(psOut string, root int) (pids []int, cpu float64) {
	type node struct{ cpu float64 }
	stats := map[int]node{}
	children := map[int][]int{}
	for _, line := range strings.Split(psOut, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pc, err3 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		stats[pid] = node{cpu: pc}
		children[ppid] = append(children[ppid], pid)
	}

	seen := map[int]bool{}
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if n, ok := stats[pid]; ok {
			pids = append(pids, pid)
			cpu += n.cpu
		}
		stack = append(stack, children[pid]...)
	}
	return pids, cpu
}

// footprintOnly matches footprint's per-process lines ("name [pid]: … Footprint:
// N B") while skipping the trailing "Summary Footprint:" total, which carries no
// [pid] and would otherwise double-count.
var footprintOnly = regexp.MustCompile(`\[[0-9]+\][^\n]*Footprint:\s+([0-9]+)\s+B`)

func footprintBytes(pids []int) (int64, error) {
	if len(pids) == 0 {
		return 0, nil
	}
	args := []string{"--noCategories", "-f", "bytes"}
	for _, pid := range pids {
		args = append(args, "--pid", strconv.Itoa(pid))
	}
	// footprint exits 0 and reports unresolvable pids on stderr, so a partial
	// failure (a process that exited between the two samples) still yields the
	// footprints it could gather rather than erroring the whole call.
	out, err := exec.Command("footprint", args...).Output()
	if err != nil {
		return 0, err
	}
	return parseFootprintBytes(string(out)), nil
}

func parseFootprintBytes(out string) int64 {
	var total int64
	for _, m := range footprintOnly.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			total += v
		}
	}
	return total
}
