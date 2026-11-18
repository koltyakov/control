package system

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestWindowsDXCoreInventoryAndMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices := dxCoreInventory(ctx)
	if len(devices) == 0 {
		t.Skip("DXCore hardware inventory unavailable")
	}
	if len(devices) > model.MaxGPUs {
		t.Fatal("unbounded DXCore inventory")
	}
	for _, info := range sampleGPUDevices(ctx, devices) {
		if !info.Valid() {
			t.Fatal("invalid native GPU/NPU sample", info)
		}
		if info.Displayable() {
			used := "unknown"
			if info.MemoryUsedBytes != nil {
				used = strconv.FormatUint(*info.MemoryUsedBytes, 10)
			}
			t.Logf("%s %s: dedicated capacity=%d used=%s", info.Kind, info.Name, info.MemoryTotalBytes, used)
		}
	}
}

// The persistent PDH query must remain usable across samples.
func TestWindowsGPUMemoryCounters(t *testing.T) {
	first, err := gpuMemoryCounters.sample()
	if err != nil {
		t.Skip("GPU memory counters unavailable:", err)
	}
	second, err := gpuMemoryCounters.sample()
	if err != nil {
		t.Fatal("reused GPU memory query failed:", err)
	}
	for key := range second {
		if _, ok := first[key]; !ok {
			t.Logf("adapter %s appeared between samples", key)
		}
	}
	t.Logf("GPU memory counters: %v", second)
}

func BenchmarkWindowsGPUMemoryCounters(b *testing.B) {
	if _, err := gpuMemoryCounters.sample(); err != nil {
		b.Skip("GPU memory counters unavailable:", err)
	}
	for b.Loop() {
		if _, err := gpuMemoryCounters.sample(); err != nil {
			b.Fatal(err)
		}
	}
}
