package client

import "testing"

func TestSafeCredentialURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:7331":     true,
		"http://localhost:7331/":    true,
		"http://[::1]:7331":         true,
		"https://node.example:7331": true,
		"http://0.0.0.0:7331":       false,
		"http://10.0.0.5:7331":      false,
		"http://source:7331":        false,
		"ws://127.0.0.1:7331":       false,
		"127.0.0.1:7331":            false,
		"":                          false,
	} {
		if got := SafeCredentialURL(raw); got != want {
			t.Errorf("SafeCredentialURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
