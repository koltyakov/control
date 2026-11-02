package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
)

func TestRunbooksRequireAuthorityButNotWorkAdmission(t *testing.T) {
	for _, method := range []string{"runbooks.list", "runbooks.read"} {
		if discoveryMethod(method) || !observational(method) || requiresAdmission(method) || trackOperation(method) {
			t.Fatalf("incorrect authorization/admission classification: %s", method)
		}
	}
	for _, item := range []struct {
		method string
		args   any
	}{
		{"runbooks.list", map[string]any{"node": "other"}},
		{"runbooks.list", map[string]any{"local": true}},
		{"runbooks.list", map[string]any{"file": "shared/run.md"}},
		{"runbooks.read", map[string]any{}},
	} {
		if _, err := (&Node{}).runbooks(context.Background(), item.method, model.JSON(item.args)); err == nil {
			t.Fatal("invalid worker query accepted", item)
		}
	}
}

func TestRunbooksPeerDiscovery(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Allow = map[string][]string{"source": {"node.describe", "runbooks.*"}, "consumer": {"node.describe"}, "*": {"node.describe"}}
				}
			})
			user, common := t.TempDir(), t.TempDir()
			worker.runbookRoots = []runbooks.Root{{Source: "worker-user", Path: user}, {Source: "worker-common", Path: common}}
			for _, item := range []struct{ root, file, text string }{
				{user, "machines/" + worker.Identity.ID + "/vpn.md", "# Worker VPN\nReferences: vpn.portal vpn.username vpn.password"},
				{common, "shared/vpn.md", "---\nrequires:\n  os: [windows]\n  apps: [ffmpeg]\n  gpu: true\n  vpn:\n    vendor: Palo Alto Networks\n    state: connected\n---\n# VPN\nGeneric instructions."},
				{common, "machines/consumer/private.md", "Other machine's instructions."},
			} {
				p := filepath.Join(item.root, filepath.FromSlash(item.file))
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(item.text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var description map[string]any
			// Node.Call is the raw worker-to-worker path. The account-client test
			// adapter intentionally enriches descriptions with authorized content.
			if err := consumer.Call(ctx, "worker", "node.describe", map[string]any{}, &description); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(model.JSON(description)), "vpn.password") || !strings.Contains(string(model.JSON(description)), runbooks.Protocol) {
				t.Fatal("discovery-only description exposed instructions or omitted protocol")
			}
			var catalog runbooks.Catalog
			call(t, source, "worker", "runbooks.list", map[string]any{}, &catalog)
			if len(catalog.Runbooks) != 2 || catalog.Runbooks[0].Machine != worker.Identity.ID || !catalog.Runbooks[0].Preferred {
				t.Fatalf("worker discovery: %+v", catalog)
			}
			shared := catalog.Runbooks[1]
			if shared.Machine != "" || !reflect.DeepEqual(shared.Requires, map[string]any{"gpu": true, "os": []any{"windows"}, "apps": []any{"ffmpeg"}, "vpn": map[string]any{"vendor": "Palo Alto Networks", "state": "connected"}}) {
				t.Fatalf("worker discovery omitted shared applicability metadata: %+v", shared)
			}
			call(t, source, "worker", "runbooks.read", map[string]any{"source": "worker-user", "file": "machines/" + worker.Identity.ID + "/vpn.md"}, &catalog)
			if len(catalog.Runbooks) != 1 || !strings.Contains(catalog.Runbooks[0].Content, "vpn.password") {
				t.Fatal("complete worker read omitted references")
			}
			if err := testCall(t, consumer, ctx, "worker", "runbooks.list", map[string]any{}, nil); err == nil {
				t.Fatal("node.describe permission granted runbook access")
			}
			if err := consumer.Call(ctx, "worker", "runbooks.list", map[string]any{}, nil); err == nil {
				t.Fatal("worker identity gained ambient runbook reads")
			}
			if err := testCall(t, source, ctx, "worker", "runbooks.read", map[string]any{"source": "worker-common", "file": "machines/consumer/private.md"}, nil); err == nil {
				t.Fatal("read another machine's instructions")
			}
			if err := testCall(t, source, ctx, "worker", "runbooks.read", map[string]any{"source": "worker-user", "file": "../private.md"}, nil); err == nil {
				t.Fatal("escaped approved worker store")
			}
		})
	}
}
