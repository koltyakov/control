package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/client"
)

func TestMachineRenameCommand(t *testing.T) {
	for _, target := range []string{"hostname.local", "stable-id"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth":
					_, _ = w.Write([]byte(`{"role":"user"}`))
				case "/v1/nodes":
					_, _ = w.Write([]byte(`[{"id":"stable-id","name":"hostname.local"}]`))
				case "/v1/fleet/nodes/stable-id":
					var q map[string]string
					if r.Method != http.MethodPatch || json.NewDecoder(r.Body).Decode(&q) != nil || q["name"] != "render-01" || len(q) != 1 {
						t.Error("incorrect rename request", q)
					}
					calls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Error("unexpected request", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer s.Close()
			c := client.Admin{URL: s.URL, Key: "account-key"}
			if err := machineCLI(context.Background(), c, []string{"rename", target, "render-01"}); err != nil || calls.Load() != 1 {
				t.Fatal("rename failed", err, calls.Load())
			}
			if err := machineCLI(context.Background(), c, []string{"rename", target}); err == nil || calls.Load() != 1 {
				t.Fatal("missing alias did not fail without submission", err)
			}
		})
	}
}
