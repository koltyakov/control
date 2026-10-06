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

func TestMachineAddStartupContext(t *testing.T) {
	for _, mode := range []string{"user", "system", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			var created atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth" {
					_ = json.NewEncoder(w).Encode(map[string]string{"role": "user"})
					return
				}
				created.Add(1)
				var q enrollment.Request
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.ServiceMode != mode {
					t.Error("startup context missing", err)
				}
				_ = json.NewEncoder(w).Encode(enrollment.Link{Invitation: enrollment.Invitation{ServiceMode: q.ServiceMode}})
			}))
			defer server.Close()
			err := machineCLI(context.Background(), client.Admin{URL: server.URL, Key: "key"}, []string{"add", "auto", "--platform", "linux", "--service", mode, "--json"})
			if mode == "invalid" {
				if err == nil || created.Load() != 0 {
					t.Fatal("invalid context created an invitation")
				}
			} else if err != nil || created.Load() != 1 {
				t.Fatalf("context not requested once: %v", err)
			}
		})
	}
}
