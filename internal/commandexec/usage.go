package commandexec

import (
	"math"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// measured holds raw readings for one invocation. Nil fields are unknown.
// On Unix the helper reports the requested program's wait4 rusage over its
// private status channel; on Windows the parent reads the Job Object.
type measured struct {
	UserNs      *int64 `json:"userNs,omitempty"`
	SystemNs    *int64 `json:"systemNs,omitempty"`
	MaxRSSBytes *int64 `json:"maxRssBytes,omitempty"`
	// Never accepted from the helper: only the parent can read the job.
	PeakJobMemoryBytes *int64 `json:"-"`
}

func known(v int64) *int64 { return &v }

// rusageReading converts wait4 rusage. maxRSSUnit is 1 where ru_maxrss is in
// bytes (macOS) and 1024 where it is in kilobytes (Linux and other Unix). A
// zero or negative maximum resident set is not a reading.
func rusageReading(userNs, systemNs, maxRSS, maxRSSUnit int64) *measured {
	m := &measured{}
	if userNs >= 0 {
		m.UserNs = known(userNs)
	}
	if systemNs >= 0 {
		m.SystemNs = known(systemNs)
	}
	if maxRSS > 0 && maxRSSUnit > 0 && maxRSS <= math.MaxInt64/maxRSSUnit {
		m.MaxRSSBytes = known(maxRSS * maxRSSUnit)
	}
	return m
}

// filetimeTicks joins a FILETIME duration into 100-nanosecond ticks.
func filetimeTicks(high, low uint32) int64 {
	return int64(uint64(high)<<32 | uint64(low))
}

// jobAccounting carries Windows Job Object readings in 100 ns ticks. The job
// also contains the ownership helper, whose own process times are subtracted
// so CPU covers the requested program and its descendants. Its committed
// memory cannot be separated from the job's peak, which is reported as is.
type jobAccounting struct {
	cpuKnown     bool
	user, kernel int64
	helperKnown  bool
	helperUser   int64
	helperKernel int64
	peakKnown    bool
	peak         uint64
}

func (j jobAccounting) reading() *measured {
	m := &measured{}
	if j.cpuKnown && j.helperKnown && j.user >= 0 && j.kernel >= 0 && j.helperUser >= 0 && j.helperKernel >= 0 {
		m.UserNs = known(max(j.user-j.helperUser, 0) * 100)
		m.SystemNs = known(max(j.kernel-j.helperKernel, 0) * 100)
	}
	if j.peakKnown && j.peak > 0 && j.peak <= math.MaxInt64 {
		m.PeakJobMemoryBytes = known(int64(j.peak))
	}
	return m
}

// commandUsage publishes a reading. wall < 0 means the program never ran.
// It returns nil when nothing is known.
func commandUsage(wall time.Duration, m *measured) *model.CommandUsage {
	usage := &model.CommandUsage{}
	if wall >= 0 {
		usage.WallMs = known(wall.Milliseconds())
	}
	if m != nil {
		usage.UserCPUMs = milliseconds(m.UserNs)
		usage.SystemCPUMs = milliseconds(m.SystemNs)
		if m.MaxRSSBytes != nil && *m.MaxRSSBytes > 0 {
			usage.MaxRSSBytes = known(*m.MaxRSSBytes)
		}
		if m.PeakJobMemoryBytes != nil && *m.PeakJobMemoryBytes > 0 {
			usage.PeakJobMemoryBytes = known(*m.PeakJobMemoryBytes)
		}
	}
	if *usage == (model.CommandUsage{}) {
		return nil
	}
	return usage
}

func milliseconds(ns *int64) *int64 {
	if ns == nil || *ns < 0 {
		return nil
	}
	return known(*ns / int64(time.Millisecond))
}
