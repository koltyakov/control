package system

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/koltyakov/control/internal/model"
)

type gpuDevice struct {
	info model.GPUInfo
	path string
}

func gpuInventory(_ context.Context) []gpuDevice {
	devices := linuxGPUInventory("/sys/class/drm")
	return append(devices, linuxNPUInventory("/sys/class/accel")...)
}

func linuxGPUInventory(root string) []gpuDevice {
	entries, _ := os.ReadDir(root)
	var devices []gpuDevice
	for _, entry := range entries {
		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "card"))
		if !strings.HasPrefix(entry.Name(), "card") || err != nil || index < 0 || len(devices) == model.MaxGPUs {
			continue
		}
		path := filepath.Join(root, entry.Name(), "device")
		vendor := map[string]string{"0x10de": "NVIDIA", "0x1002": "AMD", "0x8086": "Intel"}[gpuFile(filepath.Join(path, "vendor"))]
		if vendor == "" {
			continue
		}
		name := gpuText(gpuFile(filepath.Join(path, "product_name")), 128)
		if name == "" {
			name = vendor + " GPU"
		}
		devices = append(devices, gpuDevice{info: model.GPUInfo{Name: name, Vendor: vendor, Kind: gpuKind(name, vendor)}, path: path})
	}
	return devices
}

func linuxNPUInventory(root string) []gpuDevice {
	entries, _ := os.ReadDir(root)
	var devices []gpuDevice
	for _, entry := range entries {
		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "accel"))
		if !strings.HasPrefix(entry.Name(), "accel") || err != nil || index < 0 || len(devices) == model.MaxGPUs {
			continue
		}
		path := filepath.Join(root, entry.Name(), "device")
		driver, _ := os.Readlink(filepath.Join(path, "driver"))
		vendor := map[string]string{"intel_vpu": "Intel", "ivpu": "Intel", "amdxdna": "AMD", "qaic": "Qualcomm"}[filepath.Base(driver)]
		if vendor != "" {
			devices = append(devices, gpuDevice{info: model.GPUInfo{Name: vendor + " NPU", Vendor: vendor, Kind: model.GPUNPU}})
		}
	}
	return devices
}

func sampleGPUDevices(ctx context.Context, devices []gpuDevice) []model.GPUInfo {
	var result []model.GPUInfo
	for _, device := range devices {
		if ctx.Err() != nil {
			break
		}
		if device.info.Kind == model.GPUNPU {
			result = append(result, device.info)
		} else {
			result = append(result, sampleGPUDevice(device))
		}
	}
	return result
}

func sampleGPUDevice(device gpuDevice) model.GPUInfo {
	info := device.info
	if usage, err := strconv.ParseFloat(gpuFile(filepath.Join(device.path, "gpu_busy_percent")), 64); err == nil && usage >= 0 && usage <= 100 {
		info.UsagePercent = &usage
	}
	info.MemoryTotalBytes, _ = strconv.ParseUint(gpuFile(filepath.Join(device.path, "mem_info_vram_total")), 10, 64)
	if used, err := strconv.ParseUint(gpuFile(filepath.Join(device.path, "mem_info_vram_used")), 10, 64); err == nil && (info.MemoryTotalBytes == 0 || used <= info.MemoryTotalBytes) {
		info.MemoryUsedBytes = &used
	}
	return info
}

func gpuFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	data, _ := io.ReadAll(io.LimitReader(file, 256))
	return strings.TrimSpace(string(data))
}
