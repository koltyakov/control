// Package system samples host resources independently of dashboard refreshes.
package system

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

type Collector struct {
	paths    []string
	interval time.Duration
	mu       sync.RWMutex
	latest   model.SystemInfo
	gate     chan struct{}
	trigger  chan struct{}
	previous *cpu.TimesStat
}

func New(paths []string, interval time.Duration) *Collector {
	unique := []string{}
	for _, path := range paths {
		found := false
		for _, old := range unique {
			if old == path {
				found = true
			}
		}
		if !found {
			unique = append(unique, path)
		}
	}
	return &Collector{paths: unique, interval: interval, gate: make(chan struct{}, 1), trigger: make(chan struct{}, 1), latest: model.SystemInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(), IntervalSeconds: int(interval / time.Second)}}
}

func (c *Collector) Snapshot() model.SystemInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.latest
	s.Disks = append([]model.DiskUsage(nil), s.Disks...)
	s.Errors = append([]string(nil), s.Errors...)
	if s.CPUUsagePercent != nil {
		value := *s.CPUUsagePercent
		s.CPUUsagePercent = &value
	}
	return s
}

// Request coalesces task-completion triggers. It never blocks task completion.
func (c *Collector) Request() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

func (c *Collector) Run(ctx context.Context) {
	var ticks <-chan time.Time
	if c.interval > 0 {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-c.trigger:
		}
		_, _ = c.Refresh(ctx)
	}
}

func (c *Collector) Refresh(ctx context.Context) (model.SystemInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return c.Snapshot(), ctx.Err()
	}
	s := c.Snapshot()
	s.Errors, s.Disks, s.CPUUsagePercent = nil, nil, nil
	s.MemoryTotalBytes, s.MemoryUsedBytes, s.MemoryAvailableBytes = 0, 0, 0
	report := func(label string, err error) {
		if err != nil {
			s.Errors = append(s.Errors, fmt.Sprintf("%s: %v", label, err))
		}
	}
	if s.CPUModel == "" {
		info, err := cpu.InfoWithContext(ctx)
		report("CPU model", err)
		if len(info) > 0 {
			s.CPUModel = info[0].ModelName
		}
	}
	if s.PhysicalCPUs == 0 {
		count, err := cpu.CountsWithContext(ctx, false)
		report("physical CPUs", err)
		s.PhysicalCPUs = count
	}
	if info, err := host.InfoWithContext(ctx); err == nil {
		s.Platform, s.PlatformVersion, s.KernelVersion, s.UptimeSeconds = info.Platform, info.PlatformVersion, info.KernelVersion, info.Uptime
	} else {
		report("OS", err)
	}
	if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
		if c.previous != nil {
			s.CPUUsagePercent = usage(*c.previous, times[0])
		}
		copy := times[0]
		c.previous = &copy
	} else {
		report("CPU utilization", err)
	}
	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemoryTotalBytes, s.MemoryUsedBytes, s.MemoryAvailableBytes = memory.Total, memory.Used, memory.Available
	} else {
		report("RAM", err)
	}
	for _, path := range c.paths {
		entry := model.DiskUsage{Path: path}
		if usage, err := disk.UsageWithContext(ctx, path); err == nil {
			entry.TotalBytes, entry.UsedBytes, entry.FreeBytes = usage.Total, usage.Used, usage.Free
		} else {
			entry.Error = err.Error()
			report("disk "+path, err)
		}
		s.Disks = append(s.Disks, entry)
	}
	if err := ctx.Err(); err != nil {
		return c.Snapshot(), err
	}
	s.SampledAt = time.Now().UTC()
	c.mu.Lock()
	c.latest = s
	c.mu.Unlock()
	return s, nil
}

// Utilization is averaged across logical CPUs between samples. The first sample
// is a baseline; unknown utilization is not reported as a false zero.
func usage(previous, current cpu.TimesStat) *float64 {
	total := func(t cpu.TimesStat) float64 {
		return t.User + t.System + t.Nice + t.Idle + t.Iowait + t.Irq + t.Softirq + t.Steal
	}
	delta := total(current) - total(previous)
	if delta <= 0 {
		return nil
	}
	idle := current.Idle + current.Iowait - previous.Idle - previous.Iowait
	value := math.Max(0, math.Min(100, (delta-idle)/delta*100))
	return &value
}
