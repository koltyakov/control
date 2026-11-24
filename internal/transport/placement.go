package transport

import (
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Health is a placement hint, never a reservation. Explicit resource constraints
// require fresh samples; unconstrained discovery still works without health access.
func resourcePlacement(nodes []model.Node, health map[string]model.NodeActivitySnapshot, q model.NodeSelector, now time.Time) (model.Node, error) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	var best model.Node
	score := math.Inf(1)
	gpu := q.GPUConstrained()
	constrained := q.RequireFresh || q.MinMemoryBytes > 0 || q.MinDiskBytes > 0 || q.MaxCPUPercent != nil || gpu
	sampled := false
	for _, n := range nodes {
		h, ok := health[n.ID]
		if ok && h.Status == "update" {
			continue
		}
		fresh := ok && !h.ObservedAt.IsZero() && !h.ObservedAt.After(now.Add(time.Second)) && now.Sub(h.ObservedAt) <= model.NodeHealthTTL
		if fresh && h.Leased {
			continue
		}
		resources := fresh && h.System != nil && !h.System.SampledAt.IsZero() && now.Sub(h.System.SampledAt) <= model.NodeHealthTTL && !h.System.SampledAt.After(now.Add(time.Second))
		if constrained && !resources {
			continue
		}
		sampled = sampled || resources
		s := 1e9
		if fresh {
			s = float64(h.ActiveCount)
		}
		if resources {
			sys := h.System
			if sys.MemoryAvailableBytes < q.MinMemoryBytes {
				continue
			}
			if q.MaxCPUPercent != nil && (sys.CPUUsagePercent == nil || *sys.CPUUsagePercent > *q.MaxCPUPercent) {
				continue
			}
			if q.MinDiskBytes > 0 {
				if len(sys.Disks) == 0 {
					continue
				}
				enough := true
				for _, disk := range sys.Disks {
					if disk.Error != "" || disk.FreeBytes < q.MinDiskBytes {
						enough = false
					}
				}
				if !enough {
					continue
				}
			}
			var pressure float64
			if gpu {
				var ok bool
				if pressure, ok = gpuPressure(sys.GPUs, q); !ok {
					continue
				}
			}
			s /= float64(max(1, sys.LogicalCPUs))
			if sys.CPUUsagePercent != nil {
				s += *sys.CPUUsagePercent / 100
			}
			if sys.MemoryTotalBytes > 0 {
				s += 1 - float64(min(sys.MemoryAvailableBytes, sys.MemoryTotalBytes))/float64(sys.MemoryTotalBytes)
			}
			s += pressure
		}
		if best.ID == "" || s < score {
			best, score = n, s
		}
	}
	if best.ID == "" && constrained && !sampled {
		return best, errors.New("resource constraints require fresh owner-visible health samples, and none are available for the matching unleased nodes")
	}
	if best.ID == "" {
		return best, errors.New("no unleased node satisfies fresh resource constraints")
	}
	return best, nil
}

// gpuPressure returns the lowest load among devices satisfying every GPU
// constraint. Dedicated memory is never summed across devices, since one
// workload cannot generally use several devices' memory as one pool. Unknown
// utilization or memory use counts as fully loaded, so it never outranks a
// known reading.
func gpuPressure(gpus []model.GPUInfo, q model.NodeSelector) (float64, bool) {
	best, found := 0.0, false
	for _, g := range gpus {
		if !q.GPUMatches(g) {
			continue
		}
		p := 2.0
		if g.UsagePercent != nil {
			p += *g.UsagePercent/100 - 1
		}
		if g.MemoryTotalBytes > 0 && g.MemoryUsedBytes != nil {
			p += float64(min(*g.MemoryUsedBytes, g.MemoryTotalBytes))/float64(g.MemoryTotalBytes) - 1
		}
		if !found || p < best {
			best, found = p, true
		}
	}
	return best, found
}
