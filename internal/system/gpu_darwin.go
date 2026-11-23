package system

import (
	"context"

	"github.com/koltyakov/control/internal/model"
)

type gpuDevice struct{ info model.GPUInfo }

func gpuInventory(ctx context.Context) ([]gpuDevice, error) {
	data, err := gpuCommand(ctx, "/usr/sbin/system_profiler", "SPDisplaysDataType", "-json")
	if err != nil {
		return nil, err
	}
	return parseDarwinGPUs(data), nil
}

func sampleGPUDevices(_ context.Context, devices []gpuDevice) []model.GPUInfo {
	var result []model.GPUInfo
	for _, device := range devices {
		result = append(result, device.info)
	}
	return result
}
