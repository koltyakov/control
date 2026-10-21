package transport

import (
	"github.com/koltyakov/control/internal/model"
	"testing"
	"time"
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
