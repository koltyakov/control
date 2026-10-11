package system

import (
	"context"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

func TestCPUUsageUsesDeltas(t *testing.T) {
	previous := cpu.TimesStat{User: 40, Idle: 60}
	current := cpu.TimesStat{User: 100, Idle: 100}
	value := usage(previous, current, runtime.GOOS)
	if value == nil || math.Abs(*value-60) > 0.01 {
		t.Fatalf("expected 60%% utilization, got %v", value)
	}
	if usage(current, current, runtime.GOOS) != nil {
		t.Fatal("zero-length CPU sample reported a utilization")
	}
}

func TestCPUUsageInterruptAccounting(t *testing.T) {
	previous := cpu.TimesStat{User: 40, System: 20, Idle: 140, Irq: 10, Softirq: 5}
	current := cpu.TimesStat{User: 45, System: 40, Idle: 215, Irq: 20, Softirq: 10}
	for _, test := range []struct {
		goos string
		want float64
	}{
		{"windows", 25},
		{"linux", 100 * 40.0 / 115},
		{"darwin", 100 * 40.0 / 115},
	} {
		t.Run(test.goos, func(t *testing.T) {
			value := usage(previous, current, test.goos)
			if value == nil || math.Abs(*value-test.want) > 0.01 {
				t.Fatalf("expected %v%% utilization, got %v", test.want, value)
			}
			// Interrupt counters alone cannot advance a Windows sample because
			// they are only a breakdown of the already counted system time.
			interruptOnly := previous
			interruptOnly.Irq++
			value = usage(previous, interruptOnly, test.goos)
			if test.goos == "windows" {
				if value != nil {
					t.Fatal("interrupt breakdown advanced Windows CPU sample")
				}
			} else if value == nil || *value != 100 {
				t.Fatal("separate interrupt time was not counted")
			}
		})
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
