package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
)

func TestSecretsCLI(t *testing.T) {
	root := t.TempDir()
	cfg := node.Config{DataDir: filepath.Join(root, "state"), WorkDir: filepath.Join(root, "work")}
	if err := os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "node.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(profile, data, 0600); err != nil {
		t.Fatal(err)
	}
	secretDir := filepath.Join(cfg.DataDir, "secrets")
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "values.json"), []byte(`{"legacy":"private-legacy-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(root, "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString("  private-cli-password  \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	if err := secretsCLI(context.Background(), []string{"set", "app.password", "--stdin"}, profile); err != nil {
		t.Fatal(err)
	}
	s := secrets.Store{Dir: filepath.Join(cfg.DataDir, "secrets"), WorkDir: cfg.WorkDir}
	values, err := s.Snapshot()
	if err != nil || values["app.password"] != "  private-cli-password  " {
		t.Fatalf("value not preserved: %v", err)
	}
	for _, args := range [][]string{{"get", "app.password"}, {"set", "app.password", "secret-as-argument"}, {"list", "extra"}, {"list", "--metadata", "extra"}} {
		if err := secretsCLI(context.Background(), args, profile); err == nil || strings.Contains(err.Error(), "private-cli-password") {
			t.Fatal("invalid secret command accepted or value exposed")
		}
	}
	output, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(context.Background(), []string{"list"}, profile)
	})
	var listedNames []string
	if err != nil || json.Unmarshal([]byte(output), &listedNames) != nil || !reflect.DeepEqual(listedNames, []string{"app.password", "legacy"}) {
		t.Fatalf("names-only listing changed: %q, %v", output, err)
	}
	output, err = captureUpdatePushOutput(t, func() error {
		return secretsCLI(context.Background(), []string{"list", "--metadata"}, profile)
	})
	var metadata []secrets.Metadata
	if err != nil || json.Unmarshal([]byte(output), &metadata) != nil || len(metadata) != 2 {
		t.Fatalf("invalid metadata listing: %v", err)
	}
	if metadata[0].Name != "app.password" || metadata[0].CreatedAt.IsZero() || !metadata[0].CreatedAt.Equal(metadata[0].UpdatedAt) ||
		metadata[1] != (secrets.Metadata{Name: "legacy"}) || strings.Contains(output, "private-") || strings.Contains(output, `"value"`) {
		t.Fatal("metadata listing exposes values or has invalid timestamps")
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &raw); err != nil || len(raw[0]) != 3 || len(raw[1]) != 1 {
		t.Fatal("metadata must omit unknown dates and return only names and known timestamps")
	}
	if err := secretsCLI(context.Background(), []string{"delete", "app.password"}, profile); err != nil {
		t.Fatal(err)
	}
	if err := secretsCLI(context.Background(), []string{"delete", "legacy"}, profile); err != nil {
		t.Fatal(err)
	}
	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Fatalf("secret not deleted: %v", err)
	}
	output, err = captureUpdatePushOutput(t, func() error {
		return secretsCLI(context.Background(), []string{"list", "--metadata"}, profile)
	})
	if err != nil || strings.TrimSpace(output) != "[]" {
		t.Fatalf("empty metadata must be an array: %q, %v", output, err)
	}
}

func TestSecretsRemoteCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONTROL_HOME", root)
	t.Setenv("CONTROL_USER_KEY", "wrong-environment-key")
	key, _ := secrets.NewManagementKey()
	var saved bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer explicit-account" {
			t.Error("explicit token did not override environment")
		}
		switch {
		case r.URL.Path == "/v1/nodes":
			_ = json.NewEncoder(w).Encode([]model.Node{{ID: "worker-id", Name: "worker"}})
		case r.Method == "GET" && r.URL.Path == "/v1/fleet/nodes/worker-id/secrets":
			_ = json.NewEncoder(w).Encode(secrets.RemoteList{Key: key.PublicKey().Bytes(), Secrets: []secrets.Metadata{}})
		case r.Method == "PUT" && r.URL.Path == "/v1/fleet/nodes/worker-id/secrets/api":
			var envelope secrets.Envelope
			if json.NewDecoder(r.Body).Decode(&envelope) != nil {
				t.Error("remote set did not send encrypted envelope")
			}
			if got, err := secrets.Open(key, "worker-id", "api", envelope); err != nil || got != "  private-remote-cli  " {
				t.Error("remote CLI input was not preserved")
			}
			saved = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("CONTROL_GATEWAY", server.URL)
	input, err := os.CreateTemp(root, "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	_, _ = input.WriteString("  private-remote-cli  \n")
	_, _ = input.Seek(0, 0)
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	token := "explicit-account"
	profile := filepath.Join(root, "no-local-profile.json")
	output, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(t.Context(), []string{"set", "api", "--node", "worker", "--stdin"}, profile, &token)
	})
	if err != nil || !saved || strings.Contains(output, "private-remote-cli") {
		t.Fatalf("remote CLI set failed or exposed value: %v", err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("remote management created a local worker profile")
	}
	if err := secretsCLI(t.Context(), []string{"set", "api", "--node", "", "--stdin"}, profile, &token); err == nil {
		t.Fatal("empty target fell back to local storage")
	}
}
