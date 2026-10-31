package main

import (
	"strings"
	"testing"
)

func TestGatewayRejectsInvalidLimitsBeforeStartup(t *testing.T) {
	for _, flag := range []string{"max-connections", "max-connections-per-account", "max-clients-per-account", "max-nodes-per-account",
		"max-handshakes", "max-handshakes-per-account", "new-identities-per-minute", "max-queued-bytes", "max-queued-bytes-per-account"} {
		t.Run(flag, func(t *testing.T) {
			err := run(t.Context(), []string{"gateway", "--" + flag + "=0", "--data", t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "must be positive") {
				t.Fatalf("invalid gateway limit: %v", err)
			}
		})
	}
}
