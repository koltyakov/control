package system

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestLinuxGPUResources(t *testing.T) {
	root := t.TempDir()
	write := func(card, name, value string) {
		t.Helper()
		dir := filepath.Join(root, card, "device")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("card0", "vendor", "0x1002")
	write("card0", "product_name", "AMD Radeon RX 7900 XTX")
	write("card0", "gpu_busy_percent", "17")
	write("card0", "mem_info_vram_total", "8589934592")
	write("card0", "mem_info_vram_used", "1073741824")
	write("card0-DP-1", "vendor", "0x1002")
	write("card1", "vendor", "0x8086")
	devices := linuxGPUInventory(root)
	if len(devices) != 2 {
		t.Fatal("connector counted as a GPU", devices)
	}
	gpu := sampleGPUDevice(devices[0])
	if gpu.Kind != model.GPUDiscrete || gpu.UsagePercent == nil || *gpu.UsagePercent != 17 || gpu.MemoryTotalBytes != 8<<30 || gpu.MemoryUsedBytes == nil || *gpu.MemoryUsedBytes != 1<<30 {
		t.Fatal("missing Linux GPU readings", gpu)
	}
	intel := sampleGPUDevice(devices[1])
	if intel.Vendor != "Intel" || intel.UsagePercent != nil || intel.MemoryUsedBytes != nil {
		t.Fatal("missing readings were invented", intel)
	}
	write("card0", "gpu_busy_percent", "101")
	write("card0", "mem_info_vram_used", "99999999999")
	gpu = sampleGPUDevice(devices[0])
	if gpu.UsagePercent != nil || gpu.MemoryUsedBytes != nil {
		t.Fatal("invalid GPU readings accepted", gpu)
	}
}

func TestLinuxNPUInventory(t *testing.T) {
	root := t.TempDir()
	for name, driver := range map[string]string{"accel0": "intel_vpu", "accel1": "amdxdna", "accel2": "qaic", "accel3": "unknown", "accel0-display": "ivpu"} {
		path := filepath.Join(root, name, "device")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("/drivers", driver), filepath.Join(path, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	devices := linuxNPUInventory(root)
	if len(devices) != 3 {
		t.Fatal("NPU inventory included unknown or non-device entries", devices)
	}
	for _, device := range devices {
		if device.info.Kind != model.GPUNPU || !device.info.Displayable() || device.info.MemoryUsedBytes != nil || device.info.UsagePercent != nil {
			t.Fatal("NPU classification invented resource readings", device)
		}
	}
}
