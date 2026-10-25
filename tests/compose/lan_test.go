//go:build compose

package compose_test

import (
	"testing"
	"time"

	"github.com/koltyakov/control/internal/wol"
)

func TestAutomaticLANRelationships(t *testing.T) {
	ctx, c := environment(t)
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool := nodes(t, ctx, c)
		complete := true
		for _, receiver := range []string{"source", "worker", "consumer"} {
			if !pool[receiver].LANDiscovery || len(pool[receiver].WakeInterfaces) == 0 {
				t.Fatal("automatic interface metadata missing", receiver)
			}
			for _, sender := range []string{"source", "worker", "consumer"} {
				if receiver != sender && !wol.CanSend(pool[sender], pool[receiver]) {
					complete = false
				}
			}
		}
		if complete {
			t.Log("all three nodes automatically verified broadcast reachability without LAN labels")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("LAN discovery did not establish all directed relationships", pool)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
