package model

type NodeSelector struct {
	Labels         map[string]string `json:"labels"`
	Capability     string            `json:"capability"`
	MinMemoryBytes uint64            `json:"minMemoryBytes,omitempty"`
	MinDiskBytes   uint64            `json:"minDiskBytes,omitempty"`
	MaxCPUPercent  *float64          `json:"maxCpuPercent,omitempty"`
	RequireFresh   bool              `json:"requireFresh,omitempty"`
}
