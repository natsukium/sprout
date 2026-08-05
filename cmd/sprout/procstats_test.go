package main

import "testing"

func TestParseProcTreeCollectsSubtreeAndSumsCPU(t *testing.T) {
	// 100 is root; 200 and 201 are its children; 300 is a grandchild via 200;
	// 999 is an unrelated process that must not be counted.
	ps := "" +
		"  1     0   0.0\n" +
		"100     1   1.5\n" +
		"200   100   2.0\n" +
		"201   100   0.5\n" +
		"300   200   4.0\n" +
		"999     1  10.0\n"

	pids, cpu := parseProcTree(ps, 100)

	got := map[int]bool{}
	for _, p := range pids {
		got[p] = true
	}
	for _, want := range []int{100, 200, 201, 300} {
		if !got[want] {
			t.Errorf("expected pid %d in subtree, got %v", want, pids)
		}
	}
	if got[999] {
		t.Errorf("unrelated pid 999 must not be in subtree, got %v", pids)
	}
	if cpu != 8.0 {
		t.Errorf("expected summed CPU 8.0 (1.5+2.0+0.5+4.0), got %v", cpu)
	}
}

// A root with no recorded row, having exited between sampling ps and walking
// the tree, yields no pids and zero CPU rather than panicking.
func TestParseProcTreeMissingRoot(t *testing.T) {
	pids, cpu := parseProcTree("1 0 0.0\n", 4242)
	if len(pids) != 0 || cpu != 0 {
		t.Errorf("expected empty result for absent root, got pids=%v cpu=%v", pids, cpu)
	}
}

// Per-process footprints are summed; the "Summary Footprint" total would
// double-count, and the phys_footprint lines are auxiliary.
func TestParseFootprintBytesSumsPerProcessOnly(t *testing.T) {
	out := "" +
		"======================================================================\n" +
		"vfkit [61992]: 64-bit    Footprint: 2000000000 B (16384 bytes per page)\n" +
		"======================================================================\n" +
		"\n" +
		"Auxiliary data:\n" +
		"    phys_footprint: 2000000000 B\n" +
		"    phys_footprint_peak: 2100000000 B\n" +
		"\n" +
		"======================================================================\n" +
		"microvm-run [61990]: 64-bit    Footprint: 3000000 B (16384 bytes per page)\n" +
		"======================================================================\n" +
		"\n" +
		"======================================================================\n" +
		"Summary Footprint: 2003000000 B\n" +
		"======================================================================\n"

	if got := parseFootprintBytes(out); got != 2003000000 {
		t.Errorf("expected 2003000000 (2000000000+3000000), got %d", got)
	}
}
