package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func refusedAPI(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + address
}

type ownerProvider struct{}

func (ownerProvider) Capability() model.Capability {
	return model.Capability{Name: "test.owner", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (ownerProvider) Run(_ context.Context, _ json.RawMessage, e node.Execution) (any, error) {
	return e.Owner, nil
}

func TestStandalonePeerOperations(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			t.Cleanup(cancel)
			const super = "standalone-test-superuser-1234567890"
			g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = g.Close() })
			s := httptest.NewServer(g.Handler())
			t.Cleanup(s.Close)
			accounts := make([]Admin, 2)
			workers := make([]*node.Node, 2)
			for i := range accounts {
				var issued struct{ Token string }
				if err = (Admin{URL: s.URL, Key: super}).JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": fmt.Sprintf("user%d", i)}, &issued); err != nil {
					t.Fatal(err)
				}
				accounts[i] = Admin{URL: s.URL, Key: issued.Token}
				var allow map[string][]string
				if i == 1 {
					allow = map[string][]string{"*": {"node.describe"}}
				}
				w, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: issued.Token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay, Allow: allow})
				if err != nil {
					t.Fatal(err)
				}
				workers[i] = w
				t.Cleanup(func() { _ = w.Close() })
				if err = w.Register(ownerProvider{}); err != nil {
					t.Fatal(err)
				}
				if err = w.Register(longProvider{}); err != nil {
					t.Fatal(err)
				}
				if err = w.Start(ctx); err != nil {
					t.Fatal(err)
				}
			}
			local := Client{URL: refusedAPI(t)}
			cfg := StandaloneConfig{Gateway: accounts[0], StateDir: t.TempDir(), RelayOnly: relay}
			c := local.WithStandalone(ctx, cfg)
			t.Cleanup(func() { _ = c.Close() })
			var described struct{ ID string }
			if err := c.Call(ctx, "worker", "node.describe", map[string]any{}, &described); err != nil || described.ID != workers[0].Identity.ID {
				t.Fatal("remote describe", described, err)
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if got := c.routing.peer.Connections()[described.ID]; got != mode {
				t.Fatalf("wanted %s, got %s", mode, got)
			}
			var nodes []model.Node
			if err := c.Call(ctx, "", "nodes.list", map[string]any{}, &nodes); err != nil || len(nodes) != 1 || nodes[0].ID != described.ID {
				t.Fatal("fleet directory", nodes, err)
			}
			var selected model.Node
			if err := c.Call(ctx, "", "nodes.select", map[string]any{"capability": "test.owner"}, &selected); err != nil || selected.ID != described.ID {
				t.Fatal("selector", selected, err)
			}
			if err := c.Call(ctx, "", "nodes.select", map[string]any{}, &selected); err != nil || selected.ID != described.ID {
				t.Fatal("selector chose an outbound-only caller", selected, err)
			}
			for _, method := range []string{"node.describe", "test.owner", "tasks.list"} {
				if err := c.Call(ctx, workers[1].Identity.ID, method, map[string]any{}, nil); err == nil {
					t.Fatal("cross-fleet call accepted", method)
				}
			}
			if err := c.Call(ctx, "", "exec.run", map[string]any{}, nil); err == nil {
				t.Fatal("standalone peer exposed local execution")
			}
			if err := os.WriteFile(filepath.Join(workers[0].Config.WorkDir, "result.txt"), []byte("artifact-content"), 0600); err != nil {
				t.Fatal(err)
			}
			var artifact model.Artifact
			if err := c.Call(ctx, "worker", "artifacts.export", map[string]any{"path": "result.txt"}, &artifact); err != nil {
				t.Fatal(err)
			}
			var downloaded bytes.Buffer
			if err := c.Download(ctx, artifact, 4, &downloaded); err != nil || downloaded.String() != "fact-content" {
				t.Fatal("resumed download", downloaded.String(), err)
			}
			if err := c.Download(ctx, artifact, -1, io.Discard); err == nil {
				t.Fatal("negative offset accepted")
			}
			echo, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = echo.Close() })
			go func() {
				conn, err := echo.Accept()
				if err == nil {
					defer func() { _ = conn.Close() }()
					_, _ = io.Copy(conn, conn)
				}
			}()
			tunnelCtx, stopTunnel := context.WithCancel(ctx)
			conn, err := c.Tunnel(tunnelCtx, "worker", echo.Addr().String())
			if err != nil {
				stopTunnel()
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			var reply [4]byte
			if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "ping" {
				t.Fatal("tunnel", reply, err)
			}
			// A dashboard using only an account login must observe another process's
			// open socket and idle reverse listener without a local API or token.
			reverse, err := c.StartForward(ctx, ForwardSpec{Node: "worker", Address: echo.Addr().String(), Reverse: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reverse.Close()
			observerCfg := cfg
			observerCfg.AccountRouting = true
			observer := (Client{}).WithStandalone(ctx, observerCfg)
			t.Cleanup(func() { _ = observer.Close() })
			observed, err := observer.Dashboard(ctx, accounts[0], model.PoolActivityQuery{Recent: 5})
			if err != nil || len(observed.Nodes) != 1 || observed.Nodes[0].Status != "ready" || observed.Nodes[0].ActiveCount != 2 {
				t.Fatalf("standalone dashboard lost tunnel activity: %+v %v", observed, err)
			}
			if observed.Nodes[0].Tunnels == nil || *observed.Nodes[0].Tunnels != (model.TunnelCounts{Forward: 1, Reverse: 1}) {
				t.Fatalf("standalone dashboard lost live tunnel counts: %+v", observed.Nodes[0])
			}
			operations := map[string]model.Activity{}
			for _, activity := range observed.Nodes[0].Active {
				operations[activity.Operation] = activity
			}
			if socket, listener := operations["tcp.accept"], operations["tcp.listen"]; socket.Kind != "tunnel" || socket.BytesSent != 4 || socket.BytesReceived != 4 || listener.Kind != "tunnel" || listener.Phase != "listening" {
				t.Fatalf("missing tunnel traffic or idle listener: %+v", operations)
			}
			if err := c.StopForward(reverse.Info().ID); err != nil {
				t.Fatal(err)
			}
			stopTunnel()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := conn.Read(reply[:]); err == nil {
				t.Fatal("tunnel remained open after cancellation")
			}
			_ = conn.Close()
			var task model.Task
			spec := model.TaskSpec{ID: "standalone-owner", Capability: "test.owner", Args: model.JSON(map[string]any{})}
			if err := c.Call(ctx, "worker", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			if task, err = c.Wait(ctx, "worker", spec.ID); err != nil || task.State != "succeeded" {
				t.Fatal("task", task, err)
			}
			owner := task.Owner
			transportID := c.routing.peer.IdentityID()
			var clientPeer model.Node
			if err := accounts[0].JSON(ctx, http.MethodGet, "/v1/peers/"+transportID, nil, &clientPeer); err != nil || clientPeer.ID != transportID || clientPeer.ClientOwner != owner || owner == transportID || len(clientPeer.Capabilities) != 0 {
				t.Fatal("live client identity not attested", clientPeer, err)
			}
			if err := accounts[1].JSON(ctx, http.MethodGet, "/v1/peers/"+transportID, nil, nil); err == nil {
				t.Fatal("client identity leaked across accounts")
			}
			var pool model.PoolActivitySnapshot
			if err := accounts[0].JSON(ctx, http.MethodGet, "/v1/status", nil, &pool); err != nil || len(pool.Nodes) != 1 || pool.Nodes[0].ID != described.ID {
				t.Fatal("client session appeared in the dashboard", pool.Nodes, err)
			}
			if err := workers[0].Call(ctx, transportID, "exec.run", map[string]any{"command": "must-not-run"}, nil); err == nil {
				t.Fatal("standalone peer accepted incoming execution")
			}
			busy := local.WithStandalone(ctx, cfg)
			t.Cleanup(func() { _ = busy.Close() })
			if err := busy.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, &task); err != nil || task.Owner != owner {
				t.Fatal("concurrent client could not recover task", task, err)
			}
			info, err := busy.Session(ctx)
			if err != nil || info.OwnerID != owner || info.TransportID == transportID {
				t.Fatal("concurrent TLS/task identities", info, err)
			}
			if err := c.Call(ctx, "worker", "node.describe", map[string]any{}, nil); err != nil {
				t.Fatal("second client displaced the first", err)
			}
			independentCfg := cfg
			independentCfg.StateDir = t.TempDir()
			independent := local.WithStandalone(ctx, independentCfg)
			if err := independent.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, nil); err == nil {
				t.Fatal("account credential impersonated a different task owner")
			}
			_ = independent.Close()
			verifyReverseForward(t, ctx, c)
			if _, err := c.StartForward(ctx, ForwardSpec{Node: workers[1].Identity.ID, Address: "127.0.0.1:3000", Reverse: true}); err == nil {
				t.Fatal("cross-fleet reverse listener accepted")
			}
			verifyForwardAndLongTask(t, ctx, c, busy)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			// Wait for gateway disconnect reconciliation before reconnecting.
			for {
				if err := accounts[0].JSON(ctx, http.MethodGet, "/v1/peers/"+transportID, nil, nil); err != nil {
					if !strings.Contains(err.Error(), "404") {
						t.Fatal(err)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			resumed := local.WithStandalone(ctx, cfg)
			t.Cleanup(func() { _ = resumed.Close() })
			if err := resumed.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, &task); err != nil || task.Owner != owner {
				t.Fatal("owner was not retained across commands", task, err)
			}
			otherCfg := cfg
			otherCfg.Gateway = accounts[1]
			other := local.WithStandalone(ctx, otherCfg)
			t.Cleanup(func() { _ = other.Close() })
			if err := other.Call(ctx, "worker", "node.describe", map[string]any{}, nil); err != nil {
				t.Fatal("account change reused a foreign identity", err)
			}
			if err := other.Call(ctx, "worker", "test.owner", map[string]any{}, nil); err == nil {
				t.Fatal("standalone account key bypassed worker access rules")
			}
			if _, err := other.StartForward(ctx, ForwardSpec{Node: "worker", Address: "127.0.0.1:3000", Reverse: true}); err == nil {
				t.Fatal("reverse listener bypassed tcp.listen permission")
			}
			if err := accounts[0].ManageMachine(ctx, workers[0].Identity.ID, "disable"); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Call(ctx, "", "nodes.select", map[string]any{}, nil); err == nil {
				t.Fatal("selector chose a disabled worker or outbound-only peer")
			}
		})
	}
}

func TestStandaloneNeverSwitchesAfterLocalErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var q node.APICall
				_ = json.NewDecoder(r.Body).Decode(&q)
				if q.Method == "node.describe" && status == http.StatusOK {
					_, _ = w.Write([]byte(`{"result":{}}`))
					return
				}
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"uncertain application error"}`))
			}))
			defer api.Close()
			var gatewayCalls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { gatewayCalls.Add(1) }))
			defer remote.Close()
			c := (Client{URL: api.URL}).WithStandalone(context.Background(), StandaloneConfig{Gateway: Admin{URL: remote.URL, Key: "key"}, StateDir: t.TempDir()})
			defer func() { _ = c.Close() }()
			for range 2 {
				if err := c.Call(context.Background(), "worker", "tasks.start", map[string]any{}, nil); err == nil {
					t.Fatal("local error lost")
				}
			}
			if gatewayCalls.Load() != 0 {
				t.Fatal("error caused gateway fallback")
			}
			want := int32(1)
			if status == http.StatusOK {
				want = 2
			}
			if calls.Load() != want {
				t.Fatal("unexpected replay", calls.Load())
			}
		})
	}
}
