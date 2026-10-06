package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestClientRefusesOlderGatewayWithoutMachineRegistration(t *testing.T) {
	var connects atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth" {
			_, _ = w.Write([]byte(`{"userId":"legacy","nodeHealth":true}`))
			return
		}
		connects.Add(1)
		http.Error(w, "unexpected connection", http.StatusInternalServerError)
	}))
	defer s.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Client: true, Gateway: s.URL, Token: "token", Identity: id, Node: model.Node{ID: id.ID, Name: "cli-" + id.ID[:16], PublicKey: id.Public}}, nil)
	defer func() { _ = p.Close() }()
	if err := p.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "does not support client sessions") {
		t.Fatal("old gateway was not rejected", err)
	}
	if connects.Load() != 0 {
		t.Fatal("unsupported client tried to enroll as a machine")
	}
}

func TestMachineNegotiatesClientSupportForOlderGateway(t *testing.T) {
	const token = "older-gateway-machine-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth" {
			_, _ = w.Write([]byte(`{"userId":"legacy","nodeHealth":true}`))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "worker", PublicKey: id.Public, ClientSessions: true}}, nil)
	defer func() { _ = p.Close() }()
	if err := p.Start(context.Background()); err != nil {
		t.Fatal("new machine failed against an older gateway", err)
	}
	if strings.Contains(string(model.JSON(p.cfg.Node)), "clientSessions") {
		t.Fatal("new field broke legacy registration encoding")
	}
}

func TestClientRejectsUnsupportedTargetBeforeOpeningStream(t *testing.T) {
	const token = "unsupported-target-test-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes" {
			_, _ = w.Write([]byte(`[{"id":"old-worker","userId":"legacy","name":"worker","online":true}]`))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Client: true, Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "cli-" + id.ID[:16], PublicKey: id.Public}}, nil)
	defer func() { _ = p.Close() }()
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(context.Background(), "worker"); err == nil || !strings.Contains(err.Error(), "upgrade the target node") {
		t.Fatal("old worker accepted without client-session support", err)
	}
	if len(p.Connections()) != 0 {
		t.Fatal("unsupported target received a peer stream")
	}
}

func TestConcurrentOwnerNegotiationRejectsOlderDeployments(t *testing.T) {
	for _, olderGateway := range []bool{true, false} {
		t.Run(map[bool]string{true: "gateway", false: "worker"}[olderGateway], func(t *testing.T) {
			const token = "client-owner-negotiation-token-123456"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			var connects atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth" && olderGateway {
					_, _ = w.Write([]byte(`{"userId":"legacy","clientSessions":true}`))
					return
				}
				if r.URL.Path == "/v1/nodes" {
					_, _ = w.Write([]byte(`[{"id":"old-worker","userId":"legacy","name":"worker","online":true,"clientSessions":true}]`))
					return
				}
				if r.URL.Path == "/v1/client/connect" || r.URL.Path == "/v1/connect" {
					connects.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			owner, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			id, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			p := New(Config{Client: true, ClientOwner: owner, Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, PublicKey: id.Public, Name: "cli-" + owner.ID[:16]}}, nil)
			defer func() { _ = p.Close() }()
			err = p.Start(context.Background())
			if olderGateway {
				if err == nil || !strings.Contains(err.Error(), "concurrent client owners") || connects.Load() != 0 {
					t.Fatal("unsupported gateway not rejected before connect", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Open(context.Background(), "worker"); err == nil || !strings.Contains(err.Error(), "concurrent client owners") {
				t.Fatal("unsupported worker accepted", err)
			}
			if len(p.Connections()) != 0 {
				t.Fatal("unsupported worker received a stream")
			}
		})
	}
}
