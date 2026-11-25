package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
)

func secretStdin(t *testing.T, value string) {
	t.Helper()
	input, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	if _, err := input.WriteString(value); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = old })
}

func TestSecretsPolicyCLI(t *testing.T) {
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
	s := secrets.Store{Dir: filepath.Join(cfg.DataDir, "secrets"), WorkDir: cfg.WorkDir}
	ctx := context.Background()
	secretStdin(t, "private-policy-cli\n")
	if _, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(ctx, []string{"set", "api.token", "--stdin", "--use", "http", "--host", "API.example.com", "--host", "*.api.example.com:8443"}, profile)
	}); err != nil {
		t.Fatal(err)
	}
	want, _ := secrets.NewPolicy([]string{"http"}, []string{"api.example.com", "*.api.example.com:8443"}, false)
	output, err := captureUpdatePushOutput(t, func() error { return secretsCLI(ctx, []string{"list", "--metadata"}, profile) })
	var listed []secrets.Metadata
	if err != nil || json.Unmarshal([]byte(output), &listed) != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Policy, want) || strings.Contains(output, "private-") {
		t.Fatalf("policy not listed: %q %v", output, err)
	}
	// Metadata-only change: the value and timestamps stay.
	if _, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(ctx, []string{"policy", "api.token", "--use", "exec,desktop"}, profile)
	}); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	values, _ := s.Snapshot()
	if !reflect.DeepEqual(entries[0].Policy.Uses, []secrets.Use{secrets.UseDesktop, secrets.UseExec}) || values["api.token"] != "private-policy-cli" || !entries[0].UpdatedAt.Equal(listed[0].UpdatedAt) {
		t.Fatalf("policy-only update changed the value or dates: %+v", entries[0])
	}
	// A value-only set keeps the policy.
	secretStdin(t, "private-rotated\n")
	if _, err := captureUpdatePushOutput(t, func() error { return secretsCLI(ctx, []string{"set", "api.token", "--stdin"}, profile) }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy == nil {
		t.Fatal("value-only set dropped the policy")
	}
	for _, args := range [][]string{
		{"policy", "api.token"},
		{"policy", "api.token", "--clear", "--use", "exec"},
		{"policy", "api.token", "--host", "api.example.com"},
		{"policy", "api.token", "--use", "http"},
		{"policy", "api.token", "--use", "exec", "--plain-http"},
		{"policy", "api.token", "--use", "ssh"},
		{"policy", "api.token", "--use", "http", "--host", "*.com"},
		{"policy", "api.token", "--use", "exec", "--node", "worker"},
		{"policy", "missing", "--use", "exec"},
		{"set", "api.token", "--stdin", "--clear-policy", "--host", "api.example.com"},
	} {
		if _, err := captureUpdatePushOutput(t, func() error { return secretsCLI(ctx, args, profile) }); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if entries, _ := s.List(); entries[0].Policy == nil {
		t.Fatal("refused commands changed the policy")
	}
	if _, err := captureUpdatePushOutput(t, func() error { return secretsCLI(ctx, []string{"policy", "api.token", "--clear"}, profile) }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy != nil {
		t.Fatal("policy not cleared")
	}
	secretStdin(t, "private-again\n")
	if _, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(ctx, []string{"set", "api.token", "--stdin", "--use", "exec"}, profile)
	}); err != nil {
		t.Fatal(err)
	}
	secretStdin(t, "private-again\n")
	if _, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(ctx, []string{"set", "api.token", "--stdin", "--clear-policy"}, profile)
	}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy != nil {
		t.Fatal("set --clear-policy did not clear")
	}
}

func TestSecretsRemotePolicyCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONTROL_HOME", root)
	key, _ := secrets.NewManagementKey()
	supported := false
	var writes []secrets.Write
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/nodes":
			_ = json.NewEncoder(w).Encode([]model.Node{{ID: "worker-id", Name: "worker"}})
		case r.Method == "GET" && r.URL.Path == "/v1/fleet/nodes/worker-id/secrets":
			_ = json.NewEncoder(w).Encode(secrets.RemoteList{Key: key.PublicKey().Bytes(), Secrets: []secrets.Metadata{}, Policies: supported})
		case r.Method == "PUT" && r.URL.Path == "/v1/fleet/nodes/worker-id/secrets/api":
			var envelope secrets.Envelope
			_ = json.NewDecoder(r.Body).Decode(&envelope)
			written, err := secrets.OpenWrite(key, "worker-id", "api", envelope)
			if err != nil {
				t.Error(err)
			}
			writes = append(writes, written)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("CONTROL_GATEWAY", server.URL)
	token := "account"
	profile := filepath.Join(root, "none.json")
	secretStdin(t, "private-remote\n")
	_, err := captureUpdatePushOutput(t, func() error {
		return secretsCLI(t.Context(), []string{"set", "api", "--node", "worker", "--stdin", "--use", "exec"}, profile, &token)
	})
	if !errors.Is(err, client.ErrSecretPolicyUnsupported) || len(writes) != 0 {
		t.Fatalf("policy sent to a worker without support: %v %d", err, len(writes))
	}
	if rest, _ := io.ReadAll(os.Stdin); string(rest) != "private-remote\n" {
		t.Fatal("value was read before refusing the unsupported policy")
	}
	supported = true
	for _, args := range [][]string{{"--use", "exec"}, {"--clear-policy"}, {}} {
		secretStdin(t, "private-remote\n")
		if _, err := captureUpdatePushOutput(t, func() error {
			return secretsCLI(t.Context(), append([]string{"set", "api", "--node", "worker", "--stdin"}, args...), profile, &token)
		}); err != nil {
			t.Fatal(err)
		}
	}
	exec, _ := secrets.NewPolicy([]string{"exec"}, nil, false)
	if len(writes) != 3 || !writes[0].ReplacePolicy || !reflect.DeepEqual(writes[0].Policy, exec) ||
		!writes[1].ReplacePolicy || writes[1].Policy != nil || writes[2].ReplacePolicy || writes[2].Value != "private-remote" {
		t.Fatalf("remote policy writes: %+v", writes)
	}
}
