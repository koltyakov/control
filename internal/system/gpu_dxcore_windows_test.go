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
