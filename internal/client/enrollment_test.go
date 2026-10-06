package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
)

func TestInviteAutoNameUsesExplicitProtocolFlag(t *testing.T) {
	for _, name := range []string{"", "auto", "explicit"} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request enrollment.Request
				if r.URL.Path != "/v1/fleet/installations" || json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("wrong invitation request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				auto := name == "" || name == "auto"
				if request.AutoName != auto || (auto && request.Name != "") || (!auto && request.Name != name) || request.Gateway == "" {
					t.Errorf("wrong naming request: %+v", request)
				}
				_ = json.NewEncoder(w).Encode(enrollment.Link{Invitation: enrollment.Invitation{Name: request.Name, AutoName: request.AutoName}})
			}))
			defer s.Close()
			if _, err := (Admin{URL: s.URL, Key: "key"}).Invite(context.Background(), enrollment.Request{Name: name, OS: "windows"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvitationStatusMatchesIDInAuthenticatedRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/fleet/installations" || r.Header.Get("Authorization") != "Bearer fleet-key" {
			t.Error("status lookup did not use the authenticated fleet registry")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode([]enrollment.Invitation{
			{ID: "old", Name: "worker", RedeemedID: "old-identity"},
			{ID: "pending", Name: "worker"},
			{ID: "automatic", AutoName: true, Name: "target-hostname", RedeemedID: "new-identity"},
		})
	}))
	defer server.Close()
	client := Admin{URL: server.URL, Key: "fleet-key"}
	for _, test := range []struct{ id, redeemed string }{{"pending", ""}, {"automatic", "new-identity"}} {
		invitation, err := client.Invitation(context.Background(), test.id)
		if err != nil || invitation.ID != test.id || invitation.RedeemedID != test.redeemed {
			t.Fatalf("wrong invitation status for %s: %+v, %v", test.id, invitation, err)
		}
	}
	if _, err := client.Invitation(context.Background(), "missing"); err == nil {
		t.Fatal("missing invitation was treated as redeemed")
	}
	server.Close()
	if _, err := client.Invitation(context.Background(), "automatic"); err == nil {
		t.Fatal("failed lookup was treated as redeemed")
	}
}
