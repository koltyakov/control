package transport

import (
	"cmp"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Health is a placement hint, never a reservation. Explicit resource constraints
// require fresh samples; unconstrained discovery still works without health access.
func resourcePlacement(nodes []model.Node, health map[string]model.NodeActivitySnapshot, q model.NodeSelector, now time.Time) (model.Node, error) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	var best model.Node
	score := math.Inf(1)
	sampled := false
	for _, n := range nodes {
		s, resources, ok := placementScore(n, health, q, now)
		sampled = sampled || resources
		if ok && (best.ID == "" || s < score) {
			best, score = n, s
		}
	}
	if best.ID == "" {
		return best, noPlacement(q, sampled)
	}
	return best, nil
}

// eligiblePlacements returns every candidate that resourcePlacement could
// choose, ordered by name and then ID so fan-out expansion is deterministic.
func eligiblePlacements(nodes []model.Node, health map[string]model.NodeActivitySnapshot, q model.NodeSelector, now time.Time) ([]model.Node, error) {
	var eligible []model.Node
	sampled := false
	for _, n := range nodes {
		_, resources, ok := placementScore(n, health, q, now)
		sampled = sampled || resources
		if ok {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		return nil, noPlacement(q, sampled)
	}
	slices.SortFunc(eligible, func(a, b model.Node) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return eligible, nil
}

func resourceConstrained(q model.NodeSelector) bool {
	return q.RequireFresh || q.MinMemoryBytes > 0 || q.MinDiskBytes > 0 || q.MaxCPUPercent != nil || q.GPUConstrained()
}

func noPlacement(q model.NodeSelector, sampled bool) error {
	if resourceConstrained(q) && !sampled {
		return errors.New("resource constraints require fresh owner-visible health samples, and none are available for the matching unleased nodes")
	}
	return errors.New("no unleased node satisfies fresh resource constraints")
}

// placementScore applies the update, lease and resource filters to one
// candidate and returns its preference score, lower being better. sampled
// reports whether fresh resources were visible, even when they did not fit.
func placementScore(n model.Node, health map[string]model.NodeActivitySnapshot, q model.NodeSelector, now time.Time) (score float64, sampled, ok bool) {
	h, known := health[n.ID]
	if known && h.Status == "update" {
		return 0, false, false
	}
	fresh := known && !h.ObservedAt.IsZero() && !h.ObservedAt.After(now.Add(time.Second)) && now.Sub(h.ObservedAt) <= model.NodeHealthTTL
	if fresh && h.Leased {
		return 0, false, false
	}
	resources := fresh && h.System != nil && !h.System.SampledAt.IsZero() && now.Sub(h.System.SampledAt) <= model.NodeHealthTTL && !h.System.SampledAt.After(now.Add(time.Second))
	if resourceConstrained(q) && !resources {
		return 0, false, false
	}
	s := 1e9
	if fresh {
		s = float64(h.ActiveCount)
	}
	if resources {
		sys := h.System
		if sys.MemoryAvailableBytes < q.MinMemoryBytes {
			return 0, true, false
		}
		if q.MaxCPUPercent != nil && (sys.CPUUsagePercent == nil || *sys.CPUUsagePercent > *q.MaxCPUPercent) {
			return 0, true, false
		}
		if q.MinDiskBytes > 0 {
			if len(sys.Disks) == 0 {
				return 0, true, false
			}
			for _, disk := range sys.Disks {
				if disk.Error != "" || disk.FreeBytes < q.MinDiskBytes {
					return 0, true, false
				}
			}
		}
		var pressure float64
		if q.GPUConstrained() {
			var fits bool
			if pressure, fits = gpuPressure(sys.GPUs, q); !fits {
				return 0, true, false
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
	return s, resources, true
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
