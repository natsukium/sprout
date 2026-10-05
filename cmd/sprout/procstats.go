package main

type procStats struct {
	MemBytes int64
	CPUPct   float64
}

// A process the daemon spawned. start is its kernel start time, which tells
// it apart from a later process reusing pid; zero where the host offers none
// to compare.
type procIdentity struct {
	pid   int
	start uint64
}
