package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

type NodeSelector struct {
	Labels         map[string]string `json:"labels"`
	Capability     string            `json:"capability"`
	MinMemoryBytes uint64            `json:"minMemoryBytes,omitempty"`
	MinDiskBytes   uint64            `json:"minDiskBytes,omitempty"`
	MaxCPUPercent  *float64          `json:"maxCpuPercent,omitempty"`
	RequireFresh   bool              `json:"requireFresh,omitempty"`
	// GPU constraints must all be satisfied by one reported device. Absent
	// kind, vendor, and memory readings are unknown and never satisfy them.
	GPUKind               string `json:"gpuKind,omitempty"`
	GPUVendor             string `json:"gpuVendor,omitempty"`
	MinGPUMemoryBytes     uint64 `json:"minGpuMemoryBytes,omitempty"`
	MinGPUFreeMemoryBytes uint64 `json:"minGpuFreeMemoryBytes,omitempty"`
}

// NodeSelection is the nodes.select result. GPUConstraints confirms that the
// selector applied GPU constraints; older selectors ignore those fields and
// omit it, so callers can reject their answer instead of trusting it.
type NodeSelection struct {
	Node
	GPUConstraints bool `json:"gpuConstraints,omitempty"`
}

// DecodeNodeSelector decodes and validates a nodes.select request, naming the
// offending field instead of reporting a Go type.
func DecodeNodeSelector(data []byte) (NodeSelector, error) {
	var q NodeSelector
	if err := json.Unmarshal(data, &q); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" && typeErr.Type != nil {
			if typeErr.Type.Kind() == reflect.Uint64 {
				return q, fmt.Errorf("%s must be a non-negative integer", typeErr.Field)
			}
			return q, fmt.Errorf("%s has an invalid type", typeErr.Field)
		}
		return q, err
	}
	return q, q.Validate()
}

func (q NodeSelector) Validate() error {
	if q.MaxCPUPercent != nil && (*q.MaxCPUPercent < 0 || *q.MaxCPUPercent > 100) {
		return errors.New("maxCpuPercent must be 0..100")
	}
	switch q.GPUKind {
	case "", GPUDiscrete, GPUIntegrated, GPUVirtual, GPUNPU:
	default:
		return fmt.Errorf("gpuKind must be %s, %s, %s, or %s", GPUDiscrete, GPUIntegrated, GPUVirtual, GPUNPU)
	}
	if q.GPUVendor != "" && (strings.TrimSpace(q.GPUVendor) == "" || len(q.GPUVendor) > 32 || !utf8.ValidString(q.GPUVendor) || strings.ContainsFunc(q.GPUVendor, unicode.IsControl)) {
		return errors.New("gpuVendor must be a non-blank vendor name of at most 32 bytes")
	}
	return nil
}

func (q NodeSelector) GPUConstrained() bool {
	return q.GPUKind != "" || q.GPUVendor != "" || q.MinGPUMemoryBytes > 0 || q.MinGPUFreeMemoryBytes > 0
}

// GPUMatches reports whether one device satisfies every GPU constraint.
// Unclassified kinds, unreported vendors, and missing dedicated-memory
// readings are unknown and fail any constraint that needs them.
func (q NodeSelector) GPUMatches(g GPUInfo) bool {
	if q.GPUKind != "" && g.Kind != q.GPUKind {
		return false
	}
	if q.GPUVendor != "" && (g.Vendor == "" || !strings.EqualFold(g.Vendor, strings.TrimSpace(q.GPUVendor))) {
		return false
	}
	if q.MinGPUMemoryBytes > 0 && g.MemoryTotalBytes < q.MinGPUMemoryBytes {
		return false
	}
	if q.MinGPUFreeMemoryBytes > 0 {
		if g.MemoryTotalBytes == 0 || g.MemoryUsedBytes == nil || *g.MemoryUsedBytes > g.MemoryTotalBytes || g.MemoryTotalBytes-*g.MemoryUsedBytes < q.MinGPUFreeMemoryBytes {
			return false
		}
	}
	return true
}
