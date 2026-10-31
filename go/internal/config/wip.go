package config

import "runtime"

// Each agent with its worktree (Claude Code, the project's build and tests) wants about this much of the Mac.
const (
	coresPerAgent = 2
	gbPerAgent    = 4
)

// AutoWip: how many agents this Mac runs at once without them slowing each other down, from its CPU cores and
// memory; at least 1.
func AutoWip() int { return autoWip(runtime.NumCPU(), memoryBytes()) }

// autoWip: mem 0 is unknown and counts only the cores.
func autoWip(cores int, mem uint64) int {
	n := cores / coresPerAgent
	if mem > 0 {
		if byMem := int(mem / (gbPerAgent << 30)); byMem < n {
			n = byMem
		}
	}
	return max(n, 1)
}
