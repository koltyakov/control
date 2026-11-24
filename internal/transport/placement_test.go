package transport

import (
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestPlacementFreshResourcesAndLease(t *testing.T) {
	now := time.Now()
	nodes := []model.Node{{ID: "a"}, {ID: "b"}}
	health := map[string]model.NodeActivitySnapshot{
		"a": {ObservedAt: now, Leased: true, System: &model.SystemInfo{SampledAt: now, MemoryAvailableBytes: 1024}},
		"b": {ObservedAt: now, System: &model.SystemInfo{SampledAt: now, MemoryAvailableBytes: 1024}},
	}
	q := model.NodeSelector{MinMemoryBytes: 512}
	got, err := resourcePlacement(nodes, health, q, now)
	if err != nil || got.ID != "b" {
		t.Fatal(got, err)
	}
	h := health["b"]
	h.System.SampledAt = now.Add(-time.Hour)
	health["b"] = h
	if _, err = resourcePlacement(nodes, health, q, now); err == nil {
		t.Fatal("used stale resources")
	}
	if _, err = resourcePlacement(nodes, nil, q, now); err == nil {
		t.Fatal("constraints accepted without visibility")
	}
	if _, err = resourcePlacement(nodes, nil, model.NodeSelector{}, now); err != nil {
		t.Fatal("unconstrained discovery unavailable", err)
	}
}

func gpuHealth(now time.Time, gpus ...model.GPUInfo) model.NodeActivitySnapshot {
	return model.NodeActivitySnapshot{ObservedAt: now, System: &model.SystemInfo{SampledAt: now, LogicalCPUs: 4, MemoryTotalBytes: 1 << 30, MemoryAvailableBytes: 1 << 29, GPUs: gpus}}
}

func TestPlacementGPUConstraints(t *testing.T) {
	now := time.Now()
	used := func(v uint64) *uint64 { return &v }
	usage := func(v float64) *float64 { return &v }
	const gib = uint64(1) << 30
	nodes := func() []model.Node { return []model.Node{{ID: "cpu"}, {ID: "igpu"}, {ID: "multi"}, {ID: "unknown"}} }
	health := map[string]model.NodeActivitySnapshot{
		"cpu":  gpuHealth(now),
		"igpu": gpuHealth(now, model.GPUInfo{Name: "Intel UHD", Vendor: "Intel", Kind: model.GPUIntegrated}),
		// Neither device alone has 20 GiB; memory is not summed.
		"multi": gpuHealth(now,
			model.GPUInfo{Name: "RTX A", Vendor: "NVIDIA", Kind: model.GPUDiscrete, MemoryTotalBytes: 12 * gib, MemoryUsedBytes: used(10 * gib), UsagePercent: usage(90)},
			model.GPUInfo{Name: "RTX B", Vendor: "NVIDIA", Kind: model.GPUDiscrete, MemoryTotalBytes: 16 * gib, MemoryUsedBytes: used(2 * gib), UsagePercent: usage(5)},
		),
		// Unclassified kind and unknown memory readings.
		"unknown": gpuHealth(now, model.GPUInfo{Name: "Mystery 3D", MemoryTotalBytes: 0}),
	}
	cases := []struct {
		name string
		q    model.NodeSelector
		want string
	}{
		{"kind", model.NodeSelector{GPUKind: model.GPUDiscrete}, "multi"},
		{"integrated", model.NodeSelector{GPUKind: model.GPUIntegrated}, "igpu"},
		{"vendor case-insensitive", model.NodeSelector{GPUVendor: "intel"}, "igpu"},
		{"total on one device", model.NodeSelector{MinGPUMemoryBytes: 16 * gib}, "multi"},
		{"free on one device", model.NodeSelector{GPUKind: model.GPUDiscrete, MinGPUFreeMemoryBytes: 13 * gib}, "multi"},
		{"not summed", model.NodeSelector{MinGPUMemoryBytes: 20 * gib}, ""},
		{"free exceeds every device", model.NodeSelector{MinGPUFreeMemoryBytes: 15 * gib}, ""},
		{"same device must meet all", model.NodeSelector{GPUVendor: "Intel", MinGPUMemoryBytes: gib}, ""},
		{"no npu", model.NodeSelector{GPUKind: model.GPUNPU}, ""},
		{"unknown vendor", model.NodeSelector{GPUVendor: "AMD"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for range 8 {
				got, err := resourcePlacement(nodes(), health, tc.q, now)
				if tc.want == "" {
					if err == nil {
						t.Fatal("unsatisfiable GPU constraint selected", got.ID)
					}
					return
				}
				if err != nil || got.ID != tc.want {
					t.Fatal(got.ID, err)
				}
			}
		})
	}
	// Without GPU constraints GPUs neither help nor hurt: equal load ties randomly.
	seen := map[string]bool{}
	for range 200 {
		got, err := resourcePlacement(nodes(), health, model.NodeSelector{}, now)
		if err != nil {
			t.Fatal(err)
		}
		seen[got.ID] = true
	}
	if !seen["cpu"] || !seen["multi"] {
		t.Fatal("unconstrained selection preferred GPU inventory", seen)
	}
}

func TestPlacementGPUUnknownStaleAndPreference(t *testing.T) {
	now := time.Now()
	used := func(v uint64) *uint64 { return &v }
	usage := func(v float64) *float64 { return &v }
	nodes := []model.Node{{ID: "busy"}, {ID: "idle"}, {ID: "unread"}}
	busy := model.GPUInfo{Name: "RTX", Vendor: "NVIDIA", Kind: model.GPUDiscrete, MemoryTotalBytes: 8 << 30, MemoryUsedBytes: used(7 << 30), UsagePercent: usage(95)}
	idle := busy
	idle.MemoryUsedBytes, idle.UsagePercent = used(1<<30), usage(3)
	unread := model.GPUInfo{Name: "RTX", Vendor: "NVIDIA", Kind: model.GPUDiscrete, MemoryTotalBytes: 8 << 30}
	health := map[string]model.NodeActivitySnapshot{"busy": gpuHealth(now, busy), "idle": gpuHealth(now, idle), "unread": gpuHealth(now, unread)}
	q := model.NodeSelector{GPUKind: model.GPUDiscrete}
	for range 20 {
		if got, err := resourcePlacement(append([]model.Node(nil), nodes...), health, q, now); err != nil || got.ID != "idle" {
			t.Fatal("GPU load did not break the tie", got.ID, err)
		}
	}
	// Unknown used memory cannot satisfy a free-memory constraint.
	if got, err := resourcePlacement([]model.Node{{ID: "unread"}}, health, model.NodeSelector{MinGPUFreeMemoryBytes: 1}, now); err == nil {
		t.Fatal("unknown used memory satisfied a free-memory constraint", got.ID)
	}
	// Unknown readings count as fully loaded, so they never outrank a known reading.
	delete(health, "idle")
	for range 20 {
		if got, err := resourcePlacement(append([]model.Node(nil), nodes...), health, q, now); err != nil || got.ID != "busy" {
			t.Fatal(got.ID, err)
		}
	}
	stale := gpuHealth(now, idle)
	stale.System.SampledAt = now.Add(-time.Minute)
	if _, err := resourcePlacement([]model.Node{{ID: "idle"}}, map[string]model.NodeActivitySnapshot{"idle": stale}, q, now); err == nil {
		t.Fatal("stale GPU sample satisfied a constraint")
	}
	old := gpuHealth(now)
	old.ObservedAt = now.Add(-time.Minute)
	old.System.GPUs = []model.GPUInfo{idle}
	if _, err := resourcePlacement([]model.Node{{ID: "idle"}}, map[string]model.NodeActivitySnapshot{"idle": old}, q, now); err == nil {
		t.Fatal("stale health satisfied a GPU constraint")
	}
	// Older gateways or workers without GPU reports remain unknown.
	if _, err := resourcePlacement([]model.Node{{ID: "idle"}}, map[string]model.NodeActivitySnapshot{"idle": gpuHealth(now)}, q, now); err == nil {
		t.Fatal("missing GPU report satisfied a constraint")
	}
	if _, err := resourcePlacement([]model.Node{{ID: "idle"}}, nil, q, now); err == nil || !strings.Contains(err.Error(), "fresh owner-visible health") {
		t.Fatal("constraint without health visibility", err)
	}
}
