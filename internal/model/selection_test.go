package model

import (
	"strings"
	"testing"
)

func TestDecodeNodeSelectorValidation(t *testing.T) {
	for input, want := range map[string]string{
		`{"gpuKind":"tpu"}`:                               "gpuKind must be",
		`{"gpuKind":"DISCRETE"}`:                          "gpuKind must be",
		`{"gpuVendor":"   "}`:                             "gpuVendor",
		`{"gpuVendor":"` + strings.Repeat("v", 33) + `"}`: "gpuVendor",
		`{"gpuVendor":"bad\u0007"}`:                       "gpuVendor",
		`{"minGpuMemoryBytes":-1}`:                        "minGpuMemoryBytes must be a non-negative integer",
		`{"minGpuFreeMemoryBytes":-5}`:                    "minGpuFreeMemoryBytes must be a non-negative integer",
		`{"minGpuMemoryBytes":1.5}`:                       "minGpuMemoryBytes must be a non-negative integer",
		`{"minMemoryBytes":-1}`:                           "minMemoryBytes must be a non-negative integer",
		`{"gpuKind":7}`:                                   "gpuKind has an invalid type",
		`{"maxCpuPercent":101}`:                           "maxCpuPercent must be 0..100",
	} {
		if _, err := DecodeNodeSelector([]byte(input)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", input, err, want)
		}
	}
	q, err := DecodeNodeSelector([]byte(`{"gpuKind":"npu","gpuVendor":"Intel","minGpuMemoryBytes":1,"minGpuFreeMemoryBytes":2}`))
	if err != nil || !q.GPUConstrained() || q.GPUKind != GPUNPU || q.MinGPUFreeMemoryBytes != 2 {
		t.Fatal(q, err)
	}
	if q, err = DecodeNodeSelector([]byte(`null`)); err != nil || q.GPUConstrained() {
		t.Fatal(q, err)
	}
}

func TestNodeSelectorGPUMatchesUnknownReadings(t *testing.T) {
	used := uint64(3 << 30)
	device := GPUInfo{Name: "RTX", Vendor: "NVIDIA", Kind: GPUDiscrete, MemoryTotalBytes: 8 << 30, MemoryUsedBytes: &used}
	for _, tc := range []struct {
		q    NodeSelector
		g    GPUInfo
		want bool
	}{
		{NodeSelector{GPUKind: GPUDiscrete}, device, true},
		{NodeSelector{GPUKind: GPUDiscrete}, GPUInfo{Name: "unclassified"}, false},
		{NodeSelector{GPUVendor: " nvidia "}, device, true},
		{NodeSelector{GPUVendor: "NVIDIA"}, GPUInfo{Name: "no vendor", Kind: GPUDiscrete}, false},
		{NodeSelector{MinGPUMemoryBytes: 8 << 30}, device, true},
		{NodeSelector{MinGPUMemoryBytes: 1}, GPUInfo{Name: "no capacity", Kind: GPUDiscrete}, false},
		{NodeSelector{MinGPUFreeMemoryBytes: 5 << 30}, device, true},
		{NodeSelector{MinGPUFreeMemoryBytes: 5<<30 + 1}, device, false},
		{NodeSelector{MinGPUFreeMemoryBytes: 1}, GPUInfo{Name: "no usage", MemoryTotalBytes: 8 << 30}, false},
		{NodeSelector{MinGPUFreeMemoryBytes: 1}, GPUInfo{Name: "no capacity", MemoryUsedBytes: &used}, false},
	} {
		if got := tc.q.GPUMatches(tc.g); got != tc.want {
			t.Errorf("%+v on %+v: got %v", tc.q, tc.g, got)
		}
	}
}
