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

func TestRPASecretInjection(t *testing.T) {
	const credential = "private-\"password-世界"
	for _, mode := range []string{"secret-echo", "secret-fail"} {
		t.Run(mode, func(t *testing.T) {
			n := rpaNode(t, mode)
			setTestSecret(t, n, "app.password", credential)
			var log bytes.Buffer
			args := json.RawMessage(`{"actions":[{"type":"setValue","target":{"name":"Password"},"secret":"app.password"}]}`)
			value, err := n.rpaRun(context.Background(), args, Execution{Dir: n.Config.WorkDir, Log: &log})
			if mode == "secret-fail" && err == nil {
				t.Fatal("expected helper failure")
			}
			if mode == "secret-echo" {
				if err != nil {
					t.Fatal(err)
				}
				response := value.(rpaResponse)
				if string(response.Results[0]["echo"]) != `"[REDACTED]"` {
					t.Fatalf("unmasked response: %s", model.JSON(response))
				}
			}
			if log.Len() != 0 || strings.Contains(string(model.JSON(value)), credential) || (err != nil && strings.Contains(err.Error(), credential)) {
				t.Fatal("credential leaked in result, error, or logs")
			}
			if _, err := n.rpaRun(context.Background(), json.RawMessage(`{"actions":[{"type":"type","secret":"missing"}]}`), Execution{Log: &log}); err == nil {
				t.Fatal("accepted missing secret")
			}
		})
	}
}

func TestHTTPSecretInjection(t *testing.T) {
	n := rpaNode(t, "success")
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
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.RPA = rpaCommand(t, "secret-echo")
				}
			})
			worker.rpaLockPath = filepath.Join(t.TempDir(), "rpa.lock")
			const credential = "retained-private-password"
			setTestSecret(t, worker, "app.password", credential)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			spec := model.TaskSpec{ID: "secret-task", Capability: "rpa.run", Args: json.RawMessage(`{"actions":[{"type":"type","secret":"app.password"}]}`)}
			var accepted model.Task
			call(t, source, "worker", "tasks.start", spec, &accepted)
			finished, err := source.WaitTask(ctx, "worker", accepted.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatalf("task failed: %s, %v", finished.Error, err)
			}
			if strings.Contains(string(model.JSON(finished)), credential) || !strings.Contains(string(finished.Result), "[REDACTED]") {
				t.Fatal("task result leaked credential")
			}
			data, err := os.ReadFile(worker.taskPath(spec.ID))
			if err != nil || bytes.Contains(data, []byte(credential)) {
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
				if err := source.Call(ctx, "worker", method, map[string]any{"name": "app.password"}, nil); err == nil {
					t.Fatal("secret management exposed as RPC")
				}
			}
		})
	}
}
