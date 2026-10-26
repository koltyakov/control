package system

import (
	"context"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/model"
	"golang.org/x/sys/windows"
)

type gpuDevice struct {
	info   model.GPUInfo
	luid   windows.LUID
	native bool
}

func gpuInventory(ctx context.Context) []gpuDevice {
	if devices := dxCoreInventory(ctx); len(devices) > 0 {
		return devices
	}
	// Win32_VideoController.AdapterRAM is a 32-bit value and is not reliable for
	// modern VRAM capacities. Do not expose it as a GPU memory measurement.
	data, err := gpuPowerShell(ctx, `ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_VideoController | Select-Object Name,AdapterCompatibility)`)
	if err != nil {
		return nil
	}
	return parseWindowsGPUs(data)
}

func gpuPowerShell(ctx context.Context, script string) ([]byte, error) {
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	return gpuCommand(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		`$ErrorActionPreference='Stop'; $env:PSModulePath=$PSHOME+'\Modules'; [Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false); `+script)
}

func sampleGPUDevices(ctx context.Context, devices []gpuDevice) []model.GPUInfo {
	var memory map[string]uint64
	for _, device := range devices {
		if device.native && device.info.Displayable() && device.info.MemoryTotalBytes > 0 {
			// AdapterMemory is node-wide. GPUProcessMemory and DXGI process
			// budgets would incorrectly show only this Control process's usage.
			data, err := gpuPowerShell(ctx, `ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory | Select-Object Name,DedicatedUsage)`)
			if err == nil {
				memory = parseWindowsGPUMemory(data)
			}
			break
		}
	}
	var result []model.GPUInfo
	for _, device := range devices {
		info := device.info
		if used, ok := memory[windowsGPUKey(uint32(device.luid.HighPart), device.luid.LowPart)]; device.native && ok && info.MemoryTotalBytes > 0 && used <= info.MemoryTotalBytes {
			info.MemoryUsedBytes = &used
		}
		result = append(result, info)
	}
	return result
}
