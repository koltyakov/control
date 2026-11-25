package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
)

const stubSecret = "ctl_stub-secret-must-not-leak"

type keyStub struct {
	advertise bool
	create    func(w http.ResponseWriter, role string)
	posts     atomic.Int32
}

func (s *keyStub) serve(t *testing.T) Admin {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/auth":
			auth := map[string]any{"role": "user", "userId": "u1"}
			if s.advertise {
				auth["orchestratorKeys"] = true
			}
			_ = json.NewEncoder(w).Encode(auth)
		case "POST /v1/fleet/keys":
			s.posts.Add(1)
			var q struct{ Name, Role string }
			_ = json.NewDecoder(r.Body).Decode(&q)
			s.create(w, q.Role)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return Admin{URL: server.URL, Key: "owner"}
}

func issued(role string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"key": map[string]any{"id": "k1", "name": "ci", "role": role}, "token": stubSecret})
	}
}

func TestCreateOrchestratorKeyRefusesLegacyGatewayBeforeSending(t *testing.T) {
	// A legacy gateway ignores the role and would mint a common key.
	stub := &keyStub{create: issued("common")}
	_, err := stub.serve(t).CreateOrchestratorKey(context.Background(), "ci")
	if !errors.Is(err, ErrOrchestratorKeysUnsupported) || stub.posts.Load() != 0 {
		t.Fatal("legacy gateway received a creation request", stub.posts.Load(), err)
	}
}

func TestCreateOrchestratorKeyVerifiesOutcome(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(http.ResponseWriter, string)
		want   []string
	}{
		{"wrong role", issued("common"), []string{"k1", "control keys revoke k1", "withheld"}},
		{"missing role", issued(""), []string{"control keys revoke k1"}},
		{"unreadable", func(w http.ResponseWriter, _ string) { _, _ = w.Write([]byte(`{"key":`)) }, []string{"uncertain", "control keys list"}},
		{"bad gateway", func(w http.ResponseWriter, _ string) { http.Error(w, stubSecret, http.StatusBadGateway) }, []string{"uncertain", "not retried"}},
		{"refused", func(w http.ResponseWriter, _ string) { http.Error(w, stubSecret, http.StatusForbidden) }, []string{"403"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &keyStub{advertise: true, create: test.create}
			result, err := stub.serve(t).CreateOrchestratorKey(context.Background(), "ci")
			if err == nil || result != nil || stub.posts.Load() != 1 || strings.Contains(err.Error(), stubSecret) {
				t.Fatal("unverified creation", string(result), stub.posts.Load(), err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
		})
	}
	stub := &keyStub{advertise: true}
	stub.create = func(w http.ResponseWriter, role string) {
		if role != "orchestrator" {
			t.Error("role was not requested", role)
		}
		issued("orchestrator")(w, role)
	}
	result, err := stub.serve(t).CreateOrchestratorKey(context.Background(), "ci")
	if err != nil || !strings.Contains(string(result), stubSecret) || stub.posts.Load() != 1 {
		t.Fatal("supported creation", string(result), err)
	}
}

func TestCreateOrchestratorKeyOnCurrentGateway(t *testing.T) {
	const super = "orchestrator-create-superuser-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	ctx := context.Background()
	var user struct{ Token string }
	if err = (Admin{URL: s.URL, Key: super}).JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "owner"}, &user); err != nil {
		t.Fatal(err)
	}
	result, err := (Admin{URL: s.URL, Key: user.Token}).CreateOrchestratorKey(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Key   struct{ ID, Role string }
		Token string
	}
	if err = json.Unmarshal(result, &created); err != nil || created.Key.Role != "orchestrator" {
		t.Fatal(string(result), err)
	}
	if role, err := (Admin{URL: s.URL, Key: created.Token}).AuthRole(ctx); err != nil || role != "orchestrator" {
		t.Fatal("issued credential", role, err)
	}
}
