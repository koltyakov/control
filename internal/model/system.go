package model

import "time"

type DiskUsage struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
	Error      string `json:"error,omitempty"`
}

type SystemInfo struct {
	OS                   string      `json:"os"`
	Arch                 string      `json:"arch"`
	Platform             string      `json:"platform,omitempty"`
	PlatformVersion      string      `json:"platformVersion,omitempty"`
	KernelVersion        string      `json:"kernelVersion,omitempty"`
	CPUModel             string      `json:"cpuModel,omitempty"`
	LogicalCPUs          int         `json:"logicalCPUs"`
	PhysicalCPUs         int         `json:"physicalCPUs"`
	CPUUsagePercent      *float64    `json:"cpuUsagePercent,omitempty"`
	MemoryTotalBytes     uint64      `json:"memoryTotalBytes"`
	MemoryUsedBytes      uint64      `json:"memoryUsedBytes"`
	MemoryAvailableBytes uint64      `json:"memoryAvailableBytes"`
	Disks                []DiskUsage `json:"disks"`
	UptimeSeconds        uint64      `json:"uptimeSeconds"`
	SampledAt            time.Time   `json:"sampledAt"`
	IntervalSeconds      int         `json:"intervalSeconds"`
	Errors               []string    `json:"errors,omitempty"`
}
