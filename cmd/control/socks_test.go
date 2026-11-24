package main

import (
	"context"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
)

func TestSOCKSTunnelCLIArguments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: "http://unused.example", Key: "account-key"}, StateDir: t.TempDir(), AccountRouting: true}
	c := (client.Client{}).WithStandalone(ctx, cfg)
	defer func() { _ = c.Close() }()
	// Each combination fails before any gateway or peer connection.
	for _, args := range [][]string{
		{"worker", "db.internal:5432", "--socks"},
		{"worker", "--listen", "127.0.0.1:0"},
		{"worker", "--socks", "extra"},
		{"worker", "--socks", "--reverse"},
		{"worker", "--socks", "--listen", "0.0.0.0:0"},
		{"worker", "db.internal:5432", "--allow-non-loopback"},
	} {
		if err := tunnelCLI(ctx, c, args); err == nil {
			t.Fatal("invalid SOCKS arguments accepted", args)
		}
	}
}
