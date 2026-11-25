package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
)

// A policy travels inside the encrypted envelope through a real gateway to a
// real worker, which enforces it; value-only writes keep it.
func TestRemoteSecretPolicyEndToEnd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const account = "secret-policy-account-1234567890123456"
	const common = "secret-policy-common-1234567890123456"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: account})
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
	if err != nil || !listing.Policies {
		t.Fatalf("worker did not advertise policy support: %+v %v", listing, err)
	}
	policy, err := secrets.NewPolicy([]string{"http"}, []string{"api.example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetSecretWithPolicy(ctx, id, "api.token", "private-policy-token", policy, listing); err != nil {
		t.Fatal(err)
	}
	s := secrets.Store{Dir: filepath.Join(worker.Config.DataDir, "secrets"), WorkDir: worker.Config.WorkDir}
	entries, err := s.List()
	if err != nil || len(entries) != 1 || !reflect.DeepEqual(entries[0].Policy, policy) {
		t.Fatalf("policy did not reach the worker: %+v %v", entries, err)
	}
	r, _ := s.Resolver()
	if _, err := r.Resolve([]secrets.Reference{{Name: "api.token", Use: secrets.UseExec}}); err == nil {
		t.Fatal("worker did not enforce the remotely set policy")
	}
	_, listing, err = a.SecretTarget(ctx, id)
	if err != nil || len(listing.Secrets) != 1 || !reflect.DeepEqual(listing.Secrets[0].Policy, policy) {
		t.Fatalf("remote listing lacks the policy: %+v %v", listing, err)
	}
	// A value-only write, as the console and older clients send, keeps it.
	if err := a.SetSecret(ctx, id, "api.token", "private-rotated", listing.Key); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); !reflect.DeepEqual(entries[0].Policy, policy) {
		t.Fatal("value-only remote write removed the policy")
	}
	if err := a.SetSecretWithPolicy(ctx, id, "api.token", "private-rotated", nil, listing); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy != nil {
		t.Fatal("explicit remote clear ignored")
	}
}

// Without an advertisement the client sends nothing: an older worker would
// store the value unrestricted.
func TestRemoteSecretPolicyRefusedWithoutSupport(t *testing.T) {
	t.Parallel()
	key, _ := secrets.NewManagementKey()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	policy, _ := secrets.NewPolicy([]string{"exec"}, nil, false)
	err := (Admin{URL: server.URL, Key: "account"}).SetSecretWithPolicy(t.Context(), "worker", "api", "private-value", policy, secrets.RemoteList{Key: key.PublicKey().Bytes()})
	if !errors.Is(err, ErrSecretPolicyUnsupported) || calls != 0 {
		t.Fatalf("policy sent to a worker without support: %v, %d calls", err, calls)
	}
}
