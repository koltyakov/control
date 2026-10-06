package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/store"
)

func TestPersistentTunnelCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: "http://unused.example", Key: "account-key"}, StateDir: t.TempDir(), AccountRouting: true}
	root := t.TempDir()
	dir := filepath.Join(root, identity.ID([]byte(cfg.Gateway.URL+"\x00"+cfg.Gateway.Key)))
	if err := store.Write(filepath.Join(dir, "service.json"), cfg); err != nil {
		t.Fatal(err)
	}
	submissions := make(chan client.PersistentTunnelSpec, 1)
	disposals := make(chan bool, 1)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost:
			var submitted client.PersistentTunnelSpec
			_ = json.NewDecoder(r.Body).Decode(&submitted)
			submissions <- submitted
			_ = json.NewEncoder(w).Encode(client.PersistentTunnelInfo{PersistentTunnelSpec: submitted, State: "starting"})
		case r.Method == http.MethodDelete:
			disposals <- r.URL.Path == "/tunnels/frontend"
			_, _ = w.Write([]byte(`{"disposed":true}`))
		default:
			_ = json.NewEncoder(w).Encode([]client.PersistentTunnelInfo{})
		}
	}))
	defer h.Close()
	if err := store.Write(filepath.Join(dir, "endpoint.json"), map[string]string{"address": strings.TrimPrefix(h.URL, "http://"), "token": "local-token"}); err != nil {
		t.Fatal(err)
	}
	c := (client.Client{}).WithStandalone(ctx, cfg).WithPersistentTunnels(root, nil)
	defer func() { _ = c.Close() }()
	text, err := captureUpdatePushOutput(t, func() error {
		return tunnelCLI(ctx, c, []string{"start", "tj-vm", "[::1]:5173", "--reverse", "--listen", "127.0.0.1:5173", "--id", "frontend", "--ttl", "2h"})
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted := <-submissions
	if submitted.ID != "frontend" || submitted.Node != "tj-vm" || !submitted.Reverse || submitted.TTLSeconds != 7200 || !strings.Contains(text, `"state": "starting"`) {
		t.Fatal("incorrect durable start", submitted, text, err)
	}
	if _, err := captureUpdatePushOutput(t, func() error { return tunnelCLI(ctx, c, []string{"list"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureUpdatePushOutput(t, func() error { return tunnelCLI(ctx, c, []string{"dispose", "frontend"}) }); err != nil {
		t.Fatal("dispose not routed", err)
	}
	if !<-disposals {
		t.Fatal("wrong disposal ID")
	}
	for _, args := range [][]string{
		{"start"}, {"list", "worker"}, {"dispose"},
		{"start", "worker", "127.0.0.1:80", "--listen", "127.0.0.1:0"},
		{"start", "worker", "127.0.0.1:80", "--listen", "127.0.0.1:8080", "--ttl", "500ms"},
	} {
		if err := tunnelCLI(ctx, c, args); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}
