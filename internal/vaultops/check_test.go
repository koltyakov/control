package vaultops

import (
	"context"
	"testing"

	"github.com/koltyakov/control/internal/client"
)

// The gateway refuses connections, so a check that touched the network or
// the account would fail instead of answering.
func TestCheckPerformsNoIO(t *testing.T) {
	r, _ := openTest(t, client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: private(t)})
	h := r.NewSession(context.Background())
	defer func() { _ = h.Close() }()
	if got := do[map[string]bool](t, h, OpCheck, map[string]any{}); !got["granted"] {
		t.Fatalf("check = %v", got)
	}
	if _, err := h.Do(context.Background(), OpCheck, []byte(`{"path":"/"}`)); outcome(err) != "not_sent" {
		t.Fatalf("check with arguments = %v", err)
	}
}
