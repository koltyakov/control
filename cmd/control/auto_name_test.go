package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
)

func TestMachineAddAutoName(t *testing.T) {
	for _, args := range [][]string{
		{"add", "auto", "--platform", "windows", "--json"},
		{"add", "--platform", "windows", "--json"},
		{"add", "specific-name", "--platform", "windows", "--json"},
	} {
		t.Run(args[1], func(t *testing.T) {
			var requested atomic.Bool
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth" {
					_ = json.NewEncoder(w).Encode(map[string]string{"role": "user"})
					return
				}
				var q enrollment.Request
				if r.URL.Path != "/v1/fleet/installations" || json.NewDecoder(r.Body).Decode(&q) != nil {
					t.Error("wrong invitation request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requested.Store(true)
				auto := args[1] != "specific-name"
				if q.AutoName != auto || (auto && q.Name != "") || (!auto && q.Name != "specific-name") || q.OS != "windows" {
					t.Errorf("wrong naming request: %+v", q)
				}
				_ = json.NewEncoder(w).Encode(enrollment.Link{Invitation: enrollment.Invitation{Name: q.Name, AutoName: q.AutoName}})
			}))
			defer s.Close()
			if err := machineCLI(context.Background(), client.Admin{URL: s.URL, Key: "account-key"}, args); err != nil || !requested.Load() {
				t.Fatalf("invitation not requested: %v", err)
			}
		})
	}
}
