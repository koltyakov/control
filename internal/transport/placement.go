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
	constrained := q.RequireFresh || q.MinMemoryBytes > 0 || q.MinDiskBytes > 0 || q.MaxCPUPercent != nil
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
			s /= float64(max(1, sys.LogicalCPUs))
			if sys.CPUUsagePercent != nil {
				s += *sys.CPUUsagePercent / 100
			}
			if sys.MemoryTotalBytes > 0 {
				s += 1 - float64(min(sys.MemoryAvailableBytes, sys.MemoryTotalBytes))/float64(sys.MemoryTotalBytes)
			}
		}
		if best.ID == "" || s < score {
			best, score = n, s
		}
	}
	if best.ID == "" {
		return best, errors.New("no unleased node satisfies fresh resource constraints")
	}
	return best, nil
}
