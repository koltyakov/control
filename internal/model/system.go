package model

import (
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxGPUs = 16

const (
	GPUDiscrete   = "discrete"
	GPUIntegrated = "integrated"
	GPUVirtual    = "virtual"
	GPUNPU        = "npu"
)

// GPUInfo contains GPU/NPU inventory and optional readings. Shared host RAM is not
// reported as dedicated GPU memory. Nil readings are unknown, not zero.
type GPUInfo struct {
	Name             string   `json:"name"`
	Vendor           string   `json:"vendor,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	UsagePercent     *float64 `json:"usagePercent,omitempty"`
	MemoryTotalBytes uint64   `json:"memoryTotalBytes,omitempty"`
	MemoryUsedBytes  *uint64  `json:"memoryUsedBytes,omitempty"`
}

func (g GPUInfo) Valid() bool {
	switch g.Kind {
	case "", GPUDiscrete, GPUIntegrated, GPUVirtual, GPUNPU:
	default:
		return false
	}
	for _, field := range []struct {
		value string
		limit int
	}{{g.Name, 128}, {g.Vendor, 32}} {
		if len(field.value) > field.limit || !utf8.ValidString(field.value) || strings.ContainsFunc(field.value, unicode.IsControl) {
			return false
		}
	}
	if strings.TrimSpace(g.Name) == "" {
		return false
	}
	if p := g.UsagePercent; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 100) {
		return false
	}
	return g.MemoryUsedBytes == nil || g.MemoryTotalBytes == 0 || *g.MemoryUsedBytes <= g.MemoryTotalBytes
}

// Displayable excludes integrated, virtual, and unclassified adapters without
// discarding their inventory from the system API.
func (g GPUInfo) Displayable() bool { return g.Kind == GPUDiscrete || g.Kind == GPUNPU }

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
	GPUs                 []GPUInfo   `json:"gpus,omitempty"`
	UptimeSeconds        uint64      `json:"uptimeSeconds"`
	SampledAt            time.Time   `json:"sampledAt"`
	IntervalSeconds      int         `json:"intervalSeconds"`
	Errors               []string    `json:"errors,omitempty"`
}
