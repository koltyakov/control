package system

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestNVIDIAGPUParsing(t *testing.T) {
	devices := parseNVIDIAGPUs([]byte("NVIDIA RTX Test, 42, 8192, 2048\n\"NVIDIA, Second\", 0, 4096, 0\nNVIDIA Unknown, [N/A], [Not Supported], N/A\n"))
	if len(devices) != 3 || devices[0].Name != "NVIDIA RTX Test" || devices[0].UsagePercent == nil || *devices[0].UsagePercent != 42 || devices[0].MemoryTotalBytes != 8<<30 || devices[0].MemoryUsedBytes == nil || *devices[0].MemoryUsedBytes != 2<<30 {
		t.Fatalf("missing GPU readings: %+v", devices)
	}
	if devices[1].UsagePercent == nil || *devices[1].UsagePercent != 0 || devices[1].MemoryUsedBytes == nil || *devices[1].MemoryUsedBytes != 0 {
		t.Fatal("valid zero readings became unknown")
	}
	if devices[2].UsagePercent != nil || devices[2].MemoryTotalBytes != 0 || devices[2].MemoryUsedBytes != nil {
		t.Fatal("unsupported readings became zero")
	}
	for _, row := range []string{"GPU, NaN, 1, 2", "GPU, Inf, -1, -1", "GPU, 101, 18446744073709551615, 18446744073709551615"} {
		gpu := parseNVIDIAGPUs([]byte(row))
		if len(gpu) != 1 || gpu[0].UsagePercent != nil || gpu[0].MemoryUsedBytes != nil {
			t.Fatalf("invalid reading retained: %s: %+v", row, gpu)
		}
	}
	if gpu := parseNVIDIAGPUs([]byte("bad,csv")); len(gpu) != 0 {
		t.Fatal("malformed output accepted")
	}
	if gpu := parseNVIDIAGPUs([]byte(strings.Repeat("GPU,0,1,0\n", model.MaxGPUs+1))); len(gpu) != model.MaxGPUs {
		t.Fatal("GPU count was not bounded")
	}
}

func TestGPUInventoryParsing(t *testing.T) {
	darwin := parseDarwinGPUs([]byte(`{"SPDisplaysDataType":[{"sppci_model":"Apple M4","spdisplays_vram_shared":"32 GB"},{"_name":"AMD Radeon Test"}]}`))
	windows := parseWindowsGPUs([]byte(`[{"Name":"Intel(R) Graphics","AdapterCompatibility":"Intel Corporation","AdapterRAM":4293918720},{"Name":"NVIDIA Test"}]`))
	for _, devices := range [][]gpuDevice{darwin, windows} {
		if len(devices) != 2 {
			t.Fatalf("missing inventory: %+v", devices)
		}
		for _, device := range devices {
			if !device.info.Valid() || device.info.UsagePercent != nil || device.info.MemoryTotalBytes != 0 || device.info.MemoryUsedBytes != nil {
				t.Fatal("inventory invented utilization or dedicated memory", device)
			}
		}
	}
	if darwin[0].info.Vendor != "Apple" || darwin[1].info.Vendor != "AMD" || windows[0].info.Vendor != "Intel" || windows[1].info.Vendor != "NVIDIA" {
		t.Fatal("GPU vendors missing")
	}
	if gpuVendor("Microsoft Corporation") != "" || gpuVendor("Advanced Micro Devices, Inc.") != "AMD" {
		t.Fatal("GPU vendor incorrectly inferred")
	}
	for _, parse := range []func([]byte) []gpuDevice{parseDarwinGPUs, parseWindowsGPUs} {
		if len(parse([]byte(`invalid`))) != 0 {
			t.Fatal("malformed inventory accepted")
		}
	}
	if got := gpuText("\n\x1bGPU"+strings.Repeat("猫", 100), 128); len(got) > 128 || !((model.GPUInfo{Name: got}).Valid()) {
		t.Fatal("GPU name was not safely bounded", got)
	}
}

func TestGPUClassification(t *testing.T) {
	for _, test := range []struct{ name, vendor, kind string }{
		{"AMD Radeon RX 7600M XT", "AMD", model.GPUDiscrete},
		{"AMD Radeon RX 7900 XTX", "AMD", model.GPUDiscrete},
		{"Intel Arc A770", "Intel", model.GPUDiscrete},
		{"Intel Arc(TM) B580 Graphics", "Intel", model.GPUDiscrete},
		{"NVIDIA GeForce RTX 4090", "NVIDIA", model.GPUDiscrete},
		{"NVIDIA A100-SXM4-80GB", "NVIDIA", model.GPUDiscrete},
		{"AMD Radeon(TM) Graphics", "AMD", model.GPUIntegrated},
		{"AMD Radeon(TM) 610M", "AMD", model.GPUIntegrated},
		{"Intel UHD Graphics 770", "Intel", model.GPUIntegrated},
		{"Intel Arc 140V", "Intel", model.GPUIntegrated},
		{"Apple M4 Pro", "Apple", model.GPUIntegrated},
		{"NVIDIA Jetson Orin", "NVIDIA", model.GPUIntegrated},
		{"Parsec Virtual Display Adapter", "", model.GPUVirtual},
		{"USB Mobile Monitor Virtual Adapter", "", model.GPUVirtual},
		{"VMware SVGA 3D", "", model.GPUVirtual},
		{"Intel(R) AI Boost", "Intel", model.GPUNPU},
		{"AMD Ryzen AI NPU", "AMD", model.GPUNPU},
		{"Qualcomm Hexagon", "Qualcomm", model.GPUNPU},
		{"Unknown GPU with 24 GiB", "AMD", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := gpuKind(test.name, test.vendor); got != test.kind {
				t.Fatalf("kind = %q, want %q", got, test.kind)
			}
		})
	}
}

func TestWindowsGPUMemoryMapping(t *testing.T) {
	data := []byte(`[
		{"Name":"luid_0x00000001_0x00000002_phys_0","DedicatedUsage":8589934592},
		{"Name":"luid_0x00000001_0x00000002_phys_1","DedicatedUsage":1073741824},
		{"Name":"luid_0x00000003_0x00000004_phys_0","DedicatedUsage":0},
		{"Name":"luid_0x00000005_0x00000006_phys_0"},
		{"Name":"_Total","DedicatedUsage":999},
		{"Name":"luid_0x00000007_0x00000008_phys_0","DedicatedUsage":18446744073709551615},
		{"Name":"luid_0x00000007_0x00000008_phys_1","DedicatedUsage":1}
	]`)
	memory := parseWindowsGPUMemory(data)
	if len(memory) != 2 || memory[windowsGPUKey(1, 2)] != 9<<30 {
		t.Fatal("LUID or linked physical-adapter memory mapping failed", memory)
	}
	if zero, ok := memory[windowsGPUKey(3, 4)]; !ok || zero != 0 {
		t.Fatal("valid idle VRAM usage became unknown")
	}
	if _, ok := memory[windowsGPUKey(5, 6)]; ok {
		t.Fatal("absent counter became zero usage")
	}
	if parseWindowsGPUMemory([]byte(`[{"DedicatedUsage":-1}]`)) != nil || parseWindowsGPUMemory([]byte(`not json`)) != nil {
		t.Fatal("invalid memory counters accepted")
	}
}

func TestGPUHardwareClassification(t *testing.T) {
	for _, test := range []struct {
		name, inferred, kind string
		flags                gpuHardwareType
	}{
		{"discrete", "", model.GPUDiscrete, gpuHardwareType{Hardware: new(true), Integrated: new(false), GPU: true}},
		{"integrated", model.GPUDiscrete, model.GPUIntegrated, gpuHardwareType{Hardware: new(true), Integrated: new(true), GPU: true}},
		{"software", model.GPUDiscrete, model.GPUVirtual, gpuHardwareType{Hardware: new(false), Integrated: new(false), GPU: true}},
		{"virtual display", model.GPUVirtual, model.GPUVirtual, gpuHardwareType{Hardware: new(true), Integrated: new(false), GPU: true}},
		{"npu", "", model.GPUNPU, gpuHardwareType{Hardware: new(true), Integrated: new(true), NPU: true}},
		{"named compute npu", model.GPUNPU, model.GPUNPU, gpuHardwareType{Hardware: new(true)}},
		{"other accelerator", "", "", gpuHardwareType{Hardware: new(true), Integrated: new(false)}},
		{"unknown integration", model.GPUDiscrete, "", gpuHardwareType{Hardware: new(true), GPU: true}},
		{"unknown hardware", model.GPUDiscrete, "", gpuHardwareType{Integrated: new(false), GPU: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := gpuHardwareKind(test.inferred, test.flags); got != test.kind {
				t.Fatalf("kind = %q, want %q", got, test.kind)
			}
		})
	}
}

func TestGPUSnapshotIsolation(t *testing.T) {
	usage, used := 42.0, uint64(2<<30)
	c := New(nil, -time.Second)
	c.latest.GPUs = []model.GPUInfo{{Name: "GPU", UsagePercent: &usage, MemoryTotalBytes: 8 << 30, MemoryUsedBytes: &used}}
	s := c.Snapshot()
	s.GPUs[0].Name = "changed"
	*s.GPUs[0].UsagePercent = 99
	*s.GPUs[0].MemoryUsedBytes = 0
	got := c.Snapshot().GPUs[0]
	if got.Name != "GPU" || *got.UsagePercent != 42 || *got.MemoryUsedBytes != 2<<30 {
		t.Fatal("snapshot mutation changed cached GPU resources", got)
	}
}

func TestGPUCommandBounds(t *testing.T) {
	for _, mode := range []string{"oversized", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			if _, err := gpuCommand(ctx, os.Args[0], "-test.run=^TestGPUCommandHelper$", "--", mode); err == nil {
				t.Fatal("unbounded GPU command succeeded")
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("GPU command ignored cancellation")
			}
		})
	}
}

func TestGPUCommandHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "oversized":
		fmt.Print(strings.Repeat("x", 65<<10))
	case "blocked":
		time.Sleep(30 * time.Second)
	}
}
