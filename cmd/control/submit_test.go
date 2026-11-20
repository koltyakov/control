package main

import (
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

// An ID every node rejects must fail before contacting a worker, without
// suggesting that an uncertain submission needs reconciliation.
func TestSubmitRejectsInvalidTaskIDLocally(t *testing.T) {
	for _, id := range []string{"perf-arvo.dev", strings.Repeat("a", 129), "a/b"} {
		err := submit(t.Context(), client.Client{}, "worker", model.TaskSpec{ID: id, Capability: "exec.run"}, false, false)
		if err == nil || !strings.Contains(err.Error(), "nothing was submitted") || strings.Contains(err.Error(), "query this ID") {
			t.Fatalf("submit(%q) error = %v", id, err)
		}
	}
}
