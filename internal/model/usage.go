package model

// CommandUsage is resource accounting for one command invocation's process
// tree: exec.run, agent.run or a configured subprocess provider. An absent
// field is unknown, never zero. Readings are accounting only; they impose no
// limit and never influence admission or placement. A published value is
// never mutated, so task-record copies may share it.
type CommandUsage struct {
	// WallMs runs from releasing the requested program to its exit report.
	WallMs *int64 `json:"wallMs,omitempty"`
	// UserCPUMs and SystemCPUMs total the CPU time the platform attributes to
	// the invocation; see protocol.md for which descendants each OS includes.
	UserCPUMs   *int64 `json:"userCpuMs,omitempty"`
	SystemCPUMs *int64 `json:"systemCpuMs,omitempty"`
	// MaxRSSBytes is Unix ru_maxrss: the largest resident set of a single
	// process among the program and the descendants it waited for, not a sum.
	MaxRSSBytes *int64 `json:"maxRssBytes,omitempty"`
	// PeakJobMemoryBytes is the Windows Job Object's peak committed memory of
	// all its processes together, including the small ownership helper.
	PeakJobMemoryBytes *int64 `json:"peakJobMemoryBytes,omitempty"`
}
