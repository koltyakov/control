package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
)

func TestRemoteSecretsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const account = "secret-management-account-123456789012"
	const common = "secret-management-common-123456789012"
	dir := t.TempDir()
	g, err := gateway.New(dir, common, gateway.Options{SuperuserKey: account})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	worker, err := node.New(node.Config{Name: "worker", Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), InstructionOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a := Admin{URL: server.URL, Key: account}
	id, listing, err := a.SecretTarget(ctx, "worker")
	if err != nil || id != worker.Identity.ID || len(listing.Secrets) != 0 {
		t.Fatalf("metadata/key discovery failed: %v", err)
	}
	const credential = "remote-private-世界-password"
	if err := a.SetSecret(ctx, id, "app.password", credential, listing.Key); err != nil {
		t.Fatal(err)
	}
	s := secrets.Store{Dir: filepath.Join(worker.Config.DataDir, "secrets"), WorkDir: worker.Config.WorkDir}
	values, err := s.Snapshot()
	if err != nil || values["app.password"] != credential {
		t.Fatal("value did not reach destination store")
	}
	_, listing, err = a.SecretTarget(ctx, id)
	if err != nil || len(listing.Secrets) != 1 || listing.Secrets[0].UpdatedAt.IsZero() {
		t.Fatal("remote metadata unavailable", err)
	}
	data, _ := json.Marshal(listing)
	if bytes.Contains(data, []byte(credential)) || bytes.Contains(data, []byte(`"value"`)) {
		t.Fatal("remote listing leaked a value")
	}
	if _, _, err := (Admin{URL: server.URL, Key: common}).SecretTarget(ctx, id); err == nil {
		t.Fatal("common worker key gained secret management authority")
	}
	var other struct{ Token string }
	if err := a.JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": "other"}, &other); err != nil {
		t.Fatal(err)
	}
	if err := (Admin{URL: server.URL, Key: other.Token}).DeleteSecret(ctx, id, "app.password"); err == nil {
		t.Fatal("cross-fleet deletion succeeded")
	}
	if err := a.JSON(ctx, "PUT", secretRoute(id, "app.password"), map[string]string{"value": credential}, nil); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatal("plaintext request accepted or echoed")
	}
	if err := a.DeleteSecret(ctx, id, "app.password"); err != nil {
		t.Fatal(err)
	}
	if entries, err := s.List(); err != nil || len(entries) != 0 {
		t.Fatal("remote delete failed")
	}
	// Nothing except worker-local storage may retain plaintext, including audit,
	// task state and gateway SQLite/WAL files.
	for _, root := range []string{dir, filepath.Join(worker.Config.DataDir, "tasks")} {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err == nil && bytes.Contains(data, []byte(credential)) {
				t.Errorf("plaintext retained outside worker secret storage: %s", path)
			}
			return nil
		})
	}
}

func TestRemoteSecretClientDoesNotReplay(t *testing.T) {
	t.Parallel()
	key, _ := secrets.NewManagementKey()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.Header.Get("Authorization") != "Bearer account" {
			t.Error("invalid remote secret request")
		}
		var envelope secrets.Envelope
		if json.NewDecoder(r.Body).Decode(&envelope) != nil {
			t.Error("missing encrypted envelope")
		}
		if value, err := secrets.Open(key, "worker", "api", envelope); err != nil || value != "private-client-value" {
			t.Error("invalid client encryption")
		}
		w.Header().Set("Location", "/would-replay")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	err := (Admin{URL: server.URL, Key: "account"}).SetSecret(t.Context(), "worker", "api", "private-client-value", key.PublicKey().Bytes())
	if err == nil || calls != 1 || strings.Contains(err.Error(), "private-client-value") {
		t.Fatal("redirect followed, value leaked, or failed request retried")
	}
	if err := (Admin{URL: "http://public.example.com", Key: "account"}).SetSecret(t.Context(), "worker", "api", "private-client-value", key.PublicKey().Bytes()); err == nil {
		t.Fatal("insecure gateway accepted")
	}
}
