package system

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

func TestCPUUsageUsesDeltas(t *testing.T) {
	previous := cpu.TimesStat{User: 40, Idle: 60}
	current := cpu.TimesStat{User: 100, Idle: 100}
	value := usage(previous, current)
	if value == nil || math.Abs(*value-60) > 0.01 {
		t.Fatalf("expected 60%% utilization, got %v", value)
	}
	if usage(current, current) != nil {
		t.Fatal("zero-length CPU sample reported a utilization")
	}
}

func TestPeriodicAndRequestedSampling(t *testing.T) {
	for _, interval := range []time.Duration{-time.Second, 100 * time.Millisecond} {
		t.Run(interval.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			collector := New([]string{t.TempDir()}, interval)
			first, err := collector.Refresh(ctx)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); collector.Run(ctx) }()
			defer func() { cancel(); <-done }()
			if interval < 0 {
				collector.Request()
			}
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				current := collector.Snapshot()
				if current.SampledAt.After(first.SampledAt) {
					if current.MemoryTotalBytes == 0 || len(current.Disks) != 1 || current.Disks[0].TotalBytes == 0 {
						t.Fatalf("missing resource sample: %+v", current)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
		})
	}
}
