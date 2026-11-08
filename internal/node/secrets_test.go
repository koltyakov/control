package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
)

func setTestSecret(t *testing.T, n *Node, name, value string) {
	t.Helper()
	s := secrets.Store{Dir: filepath.Join(n.Config.DataDir, "secrets"), WorkDir: n.Config.WorkDir}
	if err := s.Set(context.Background(), name, value); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSecretInjection(t *testing.T) {
	n := localNode(t, nil)
	const credential = "private-api-token"
	setTestSecret(t, n, "api.token", credential)
	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Authorization") == "Bearer "+credential
		w.Header().Set("X-Echo", credential)
		_, _ = fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	args := model.JSON(map[string]any{"url": server.URL, "headerSecrets": map[string]any{"Authorization": map[string]any{"secret": "api.token", "prefix": "Bearer "}}})
	value, err := n.httpRequest(context.Background(), args, Execution{Log: io.Discard})
	if err != nil || !received {
		t.Fatalf("injection failed: %v", err)
	}
	result := value.(map[string]any)
	body, _ := base64.StdEncoding.DecodeString(result["body"].(string))
	if string(body) != "Bearer [REDACTED]" || strings.Contains(string(model.JSON(value)), credential) {
		t.Fatal("response leaked credential")
	}
	for _, extra := range []map[string]any{
		{"headers": map[string]string{"authorization": "literal"}},
		{"headerSecrets": map[string]any{"Authorization": map[string]any{"secret": "missing"}}},
	} {
		var q map[string]any
		_ = json.Unmarshal(args, &q)
		for k, v := range extra {
			q[k] = v
		}
		if _, err := n.httpRequest(context.Background(), model.JSON(q), Execution{}); err == nil {
			t.Fatal("accepted invalid secret headers")
		}
	}
	redirected := false
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirected = true
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer redirect.Close()
	var q map[string]any
	_ = json.Unmarshal(args, &q)
	q["url"] = redirect.URL
	value, err = n.httpRequest(context.Background(), model.JSON(q), Execution{})
	if err != nil || redirected || value.(map[string]any)["status"] != http.StatusFound {
		t.Fatalf("secret-bearing redirect followed: %v", err)
	}
}

func TestSecretPeerTaskRetention(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Desktop = desktopCommand(t, "echo")
				}
			})
			worker.desktopLockPath = filepath.Join(t.TempDir(), "desktop.lock")
			const credential = desktopCredential
			setTestSecret(t, worker, "app.password", credential)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			spec := model.TaskSpec{ID: "secret-task", Capability: "desktop.run", Args: json.RawMessage(`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`)}
			var accepted model.Task
			call(t, source, "worker", "tasks.start", spec, &accepted)
			finished, err := testWaitTask(t, source, ctx, "worker", accepted.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatalf("task failed: %s, %v", finished.Error, err)
			}
			if hasDesktopCredential(model.JSON(finished)) || !strings.Contains(string(finished.Result), "[REDACTED]") || !bytes.Equal(finished.Spec.Args, spec.Args) {
				t.Fatal("task result leaked credential or changed reference arguments")
			}
			data, err := os.ReadFile(worker.taskPath(spec.ID))
			if err != nil || hasDesktopCredential(data) {
				t.Fatalf("task metadata leak: %v", err)
			}
			logs, err := os.ReadFile(worker.logPath(spec.ID))
			if err != nil || len(logs) != 0 {
				t.Fatalf("helper logs retained credentials: %v", err)
			}
			call(t, source, "worker", "tasks.start", spec, &accepted)
			if accepted.Updated != finished.Updated {
				t.Fatal("secret task replayed")
			}
			for _, method := range []string{"secrets.get", "secrets.list", "secrets.set", "secrets.delete"} {
				if err := testCall(t, source, ctx, "worker", method, map[string]any{"name": "app.password"}, nil); err == nil {
					t.Fatal("secret management exposed as RPC")
				}
			}
		})
	}
}
