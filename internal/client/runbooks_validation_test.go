package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
)

func TestRunbooksRejectForgedWorkerOriginsAndBindings(t *testing.T) {
	for _, book := range []runbooks.Book{
		{ID: "vpn", Source: "project", File: "shared/vpn.md"},
		{ID: "vpn", Source: "worker-user", File: "machines/other/vpn.md"},
		{ID: "forged-id", Source: "worker-user", File: "shared/vpn.md"},
		{ID: "vpn", Source: "worker-user", File: "../vpn.md"},
		{ID: "vpn", Source: "worker-user", File: "shared/vpn.md", Requires: map[string]any{"custom": strings.Repeat("x", 2049)}},
		{ID: "vpn", Source: "worker-user", File: "shared/vpn.md", Requires: map[string]any{"apps": make([]any, 65)}},
	} {
		t.Run(book.Source+":"+book.File+":"+book.ID, func(t *testing.T) {
			root := t.TempDir()
			writeScenarioFixture(t, filepath.Join(root, "shared", "local.md"), "Local instructions stay available.")
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var q model.APICall
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
					t.Error(err)
					return
				}
				if q.Method == "node.describe" {
					_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "stable-id", "name": "trader", "runbooks": map[string]any{"protocol": runbooks.Protocol}}})
					return
				}
				catalog := runbooks.Empty(runbooks.Selector{Node: "trader", ID: "stable-id"})
				catalog.Sources = []runbooks.Source{{Name: book.Source, Available: true}}
				catalog.Runbooks = []runbooks.Book{book}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": catalog})
			}))
			defer s.Close()
			c := Client{URL: s.URL, runbookRoots: []runbooks.Root{{Source: "project", Path: root}}}
			catalog, err := c.Runbooks(context.Background(), runbooks.Query{Node: "trader"})
			if err != nil || len(catalog.Runbooks) != 1 || catalog.Runbooks[0].ID != "local" || len(catalog.Sources) != 2 || catalog.Sources[1].Error == "" {
				t.Fatalf("invalid worker metadata displaced local content: %+v, %v", catalog, err)
			}
		})
	}
}
