package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestHybridSystemConfigPreservesProfile(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	data := []byte(`{"name":"worker-system","gateway":"https://example.test","token":"secret","dataDir":"state","workDir":"work","listen":"127.0.0.1:7331","futureSetting":{"keep":true},"allow":{}}`)
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureHybridSystem(config)
	if err != nil || !cfg.InstructionOnly || !cfg.HybridSystem || cfg.Listen != "127.0.0.1:7332" || cfg.MetricsIntervalSeconds != hybridSystemMetricsSeconds {
		t.Fatal(cfg, err)
	}
	updated, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(updated, &fields); err != nil {
		t.Fatal(err)
	}
	if !model.SameJSON(fields["futureSetting"], json.RawMessage(`{"keep":true}`)) || string(fields["allow"]) != `{}` {
		t.Fatal("unknown or restrictive settings lost", string(updated))
	}
	fields["listen"] = json.RawMessage(`"127.0.0.1:7339"`)
	delete(fields, "metricsIntervalSeconds")
	updated, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, updated, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = configureHybridSystem(config)
	if err != nil || cfg.Listen != "127.0.0.1:7339" {
		t.Fatal("recovery changed an existing companion API port", cfg, err)
	}
	if cfg.MetricsIntervalSeconds == hybridSystemMetricsSeconds {
		t.Fatal("recovery changed an existing companion's sampling interval")
	}
}

// Preflight must stop on a problem without consuming anything or writing state.
func TestEnrollPreflight(t *testing.T) {
	if err := installation.CheckHybridUser(); err != nil {
		t.Skip("Hybrid User preflight must run with a limited token:", err)
	}
	software := buildinfo.Current()
	for _, scenario := range []string{"accepted", "name-conflict", "old-gateway", "expired", "wrong-binary", "port-busy", "other-machine"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			config := filepath.Join(home, "node.json")
			var redeemed, checked atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/install/ticket/info":
					if scenario == "expired" {
						http.Error(w, "invitation expired", http.StatusGone)
						return
					}
					i := enrollment.Invitation{UserID: "fleet", Name: "worker", Gateway: "http://" + r.Host, ServiceMode: "hybrid", HybridRole: "user", Asset: update.Asset{OS: software.OS, Arch: software.Arch, SHA256: software.SHA256}}
					if scenario == "wrong-binary" {
						i.Asset.SHA256 = strings.Repeat("0", 64)
					}
					if scenario == "other-machine" {
						i.ReplaceID = "another-machine"
					}
					_ = json.NewEncoder(w).Encode(i)
				case "/install/ticket/check":
					checked.Add(1)
					switch scenario {
					case "name-conflict":
						http.Error(w, "name is already registered", http.StatusConflict)
					case "old-gateway":
						http.NotFound(w, r)
					default:
						w.WriteHeader(http.StatusNoContent)
					}
				case "/install/ticket/redeem":
					redeemed.Add(1)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			// Use a private port: the default may belong to a node on this machine.
			listen := freeLoopback(t)
			if err := createNodeConfig(config, "worker", server.URL, "active-token", listen, false); err != nil {
				t.Fatal(err)
			}
			if scenario == "port-busy" {
				listener, err := net.Listen("tcp", listen)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			}
			before, _ := os.ReadFile(config)
			link := server.URL + "/install/ticket"
			err := preflightEnrollment(context.Background(), config, link, "process", false)
			after, _ := os.ReadFile(config)
			if !bytes.Equal(before, after) {
				t.Fatal("preflight changed the active profile")
			}
			if _, statErr := os.Stat(filepath.Join(home, "pending-enrollment.json")); !os.IsNotExist(statErr) {
				t.Fatal("preflight wrote recovery state", statErr)
			}
			if redeemed.Load() != 0 {
				t.Fatal("preflight consumed the invitation")
			}
			switch scenario {
			case "accepted", "old-gateway":
				if err != nil || checked.Load() != 1 {
					t.Fatal("valid invitation rejected", err, checked.Load())
				}
			case "port-busy":
				if err == nil || !strings.Contains(err.Error(), "in use") {
					t.Fatal("busy local API port accepted", err)
				}
			default:
				if err == nil {
					t.Fatal("preflight accepted", scenario)
				}
			}
		})
	}
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}
