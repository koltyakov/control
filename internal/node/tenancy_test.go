package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

func TestPrivateFleetsWithMultipleHostAgents(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			const super = "tenant-test-superuser-key-123456789"
			g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
			if err != nil {
				t.Fatal(err)
			}
			s := httptest.NewServer(g.Handler())
			t.Cleanup(s.Close)
			t.Cleanup(func() { _ = g.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			var fleets [2][]*Node
			for u := range fleets {
				req, err := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/admin/users", bytes.NewReader(model.JSON(map[string]string{"name": fmt.Sprintf("user%d", u)})))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+super)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var account struct{ Token string }
				err = json.NewDecoder(resp.Body).Decode(&account)
				_ = resp.Body.Close()
				if err != nil || account.Token == "" {
					t.Fatal("account creation failed", err)
				}
				for _, name := range []string{"agent-one", "agent-two", "worker"} {
					n, err := New(Config{Name: name, Gateway: s.URL, Token: account.Token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = n.Close() })
					if err = n.Start(ctx); err != nil {
						t.Fatal(err)
					}
					fleets[u] = append(fleets[u], n)
				}
			}
			for u, fleet := range fleets {
				worker := fleet[2]
				private := fmt.Sprintf("user-%d-private-artifact", u)
				if err = os.WriteFile(filepath.Join(worker.Config.WorkDir, "private.txt"), []byte(private), 0600); err != nil {
					t.Fatal(err)
				}
				var artifact model.Artifact
				for _, agent := range fleet[:2] {
					var nodes []model.Node
					call(t, agent, "", "nodes.list", map[string]any{}, &nodes)
					if len(nodes) != 3 {
						t.Fatalf("foreign nodes visible: %+v", nodes)
					}
					call(t, agent, "worker", "exec.run", executable(t, "agent"), nil)
					call(t, agent, "worker", "artifacts.export", map[string]any{"path": "private.txt"}, &artifact)
					var content bytes.Buffer
					if err = agent.Download(ctx, artifact, 0, &content); err != nil || content.String() != private {
						t.Fatalf("same-fleet download: %q %v", content.String(), err)
					}
					want := "webrtc"
					if relay {
						want = "relay"
					}
					if mode := agent.Peer.Connections()[worker.Identity.ID]; mode != want {
						t.Fatalf("want %s, got %s", want, mode)
					}
				}
				attacker := fleets[1-u][0]
				for _, method := range []string{"node.describe", "activities.list", "tasks.list", "files.read", "artifacts.list", "exec.run"} {
					if err = attacker.Call(ctx, worker.Identity.ID, method, map[string]any{}, nil); err == nil {
						t.Fatal("foreign operation accepted", method)
					}
				}
				// Even a correctly signed artifact grant is not a cross-user route.
				foreignGrant := worker.grantArtifact(artifact, attacker.Identity.ID, time.Minute)
				var stolen bytes.Buffer
				if err = attacker.Download(ctx, foreignGrant, 0, &stolen); err == nil || stolen.Len() != 0 {
					t.Fatal("cross-user artifact download succeeded")
				}
				if conn, err := attacker.OpenTCP(ctx, worker.Identity.ID, "127.0.0.1:7331"); err == nil {
					_ = conn.Close()
					t.Fatal("cross-user tunnel opened")
				}
				if err = worker.authorize(ctx, attacker.Identity.ID, "node.describe"); err == nil {
					t.Fatal("default trust bypassed fleet membership")
				}
			}
		})
	}
}
