package model

import (
	"math"
	"strings"
	"testing"
)

func TestGPUInfoValidation(t *testing.T) {
	zero, negative, high, nan, infinite := 0.0, -1.0, 101.0, math.NaN(), math.Inf(1)
	used := uint64(2)
	for _, gpu := range []GPUInfo{{Name: "GPU"}, {Name: "GPU", Kind: GPUDiscrete, UsagePercent: &zero}, {Name: "GPU", Kind: GPUIntegrated}, {Name: "Virtual", Kind: GPUVirtual}, {Name: "NPU", Kind: GPUNPU}, {Name: "GPU", MemoryTotalBytes: 2, MemoryUsedBytes: &used}} {
		if !gpu.Valid() {
			t.Fatal("valid GPU rejected", gpu)
		}
	}
	for _, gpu := range []GPUInfo{{}, {Name: "GPU", Kind: "unknown-kind"}, {Name: strings.Repeat("x", 129)}, {Name: "GPU", Vendor: strings.Repeat("x", 33)}, {Name: "GPU\n"}, {Name: "GPU", UsagePercent: &negative}, {Name: "GPU", UsagePercent: &high}, {Name: "GPU", UsagePercent: &nan}, {Name: "GPU", UsagePercent: &infinite}, {Name: "GPU", MemoryTotalBytes: 1, MemoryUsedBytes: &used}} {
		if gpu.Valid() {
			t.Fatal("invalid GPU accepted", gpu)
		}
	}
}

func TestGPUDisplayKinds(t *testing.T) {
	for _, kind := range []string{"", GPUDiscrete, GPUIntegrated, GPUVirtual, GPUNPU} {
		if got := (GPUInfo{Kind: kind}).Displayable(); got != (kind == GPUDiscrete || kind == GPUNPU) {
			t.Fatal("wrong accelerator visibility", kind, got)
		}
	}
}
