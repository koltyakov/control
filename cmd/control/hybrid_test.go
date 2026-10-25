package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestHybridSystemConfigPreservesProfile(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	data := []byte(`{"name":"worker-system","gateway":"https://example.test","token":"secret","dataDir":"state","workDir":"work","listen":"127.0.0.1:7331","futureSetting":{"keep":true},"allow":{}}`)
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureHybridSystem(config)
	if err != nil || !cfg.InstructionOnly || !cfg.HybridSystem || cfg.Listen != "127.0.0.1:7332" {
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
}
