package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
)

func TestMachineForceCommands(t *testing.T) {
	for _, action := range []string{"add", "rename"} {
		t.Run(action, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if action == "rename" {
				if err := os.MkdirAll(filepath.Join(".control-scenarios", "original"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(".control-scenarios", "original", "run.md"), []byte("control call original node.describe\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth":
					_, _ = w.Write([]byte(`{"role":"user"}`))
				case "/v1/nodes":
					_, _ = w.Write([]byte(`[{"id":"id","name":"original"}]`))
				default:
					calls++
					var q struct {
						Name  string `json:"name"`
						Force bool   `json:"force"`
					}
					if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Name != "reserved" || !q.Force {
						t.Error("force missing from request", q, err)
					}
					if action == "add" {
						_ = json.NewEncoder(w).Encode(enrollment.Link{})
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				}
			}))
			defer s.Close()
			args := []string{"add", "reserved", "--platform", "linux", "--force", "--json"}
			if action == "rename" {
				args = []string{"rename", "original", "reserved", "--force"}
			}
			if err := machineCLI(context.Background(), client.Admin{URL: s.URL, Key: "key"}, args); err != nil || calls != 1 {
				t.Fatal("force command failed", err, calls)
			}
			if action == "rename" {
				if _, err := os.Stat(filepath.Join(".control-scenarios", "reserved", "run.md")); err != nil {
					t.Fatal("force bypassed scenario migration", err)
				}
			}
		})
	}
}
