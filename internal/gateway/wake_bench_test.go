package gateway

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestWakeSenderSortedPreservesSelection(t *testing.T) {
	nodes := []model.Node{
		{ID: "lower", Name: "alpha", UserID: "u", Online: true, GatewayWake: true, WakeInterfaces: []model.WakeInterface{wakeLocal}},
		{ID: "upper", Name: "Zulu", UserID: "u", Online: true, GatewayWake: true, WakeInterfaces: []model.WakeInterface{wakeLocal}},
	}
	target := sleepingTarget("u", "lower", "upper")
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b model.Node) int { return strings.Compare(a.Name, b.Name) })
	got, ok := wakeSenderSorted(sorted, target)
	if !ok || got.ID != "upper" {
		t.Fatal("case-sensitive Wake ordering changed", got, ok)
	}
	if nodes[0].ID != "lower" {
		t.Fatal("display order mutated")
	}
	for _, target := range []model.Node{target, {ID: "online", Online: true}, sleepingTarget("foreign", "upper")} {
		a, okA := wakeSender(nodes, target)
		b, okB := wakeSenderSorted(sorted, target)
		if a.ID != b.ID || okA != okB {
			t.Fatal("selection differs", a, b)
		}
	}
}

func BenchmarkStatusWakeCandidates(b *testing.B) {
	for _, count := range []int{100, 1000} {
		nodes := make([]model.Node, count)
		for i := range nodes {
			nodes[i] = model.Node{ID: fmt.Sprint(i), Name: fmt.Sprintf("node-%06d", count-i), UserID: "u"}
		}
		target := sleepingTarget("u")
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("nodes=%d/reuse=%v", count, reuse), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if reuse {
						candidates := slices.Clone(nodes)
						slices.SortFunc(candidates, func(a, b model.Node) int { return strings.Compare(a.Name, b.Name) })
						for range 32 {
							wakeSenderSorted(candidates, target)
						}
					} else {
						for range 32 {
							wakeSender(nodes, target)
						}
					}
				}
			})
		}
	}
}
