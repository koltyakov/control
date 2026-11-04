package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/koltyakov/control/internal/upload"
	"github.com/koltyakov/control/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUploadClientResumeAndMCP(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, admin := delegationClusterWithTransport(t, relay, 90*time.Second, transporttest.Loopback(), true)
			worker := workers[1]
			data := bytes.Repeat([]byte("streamed\x00\xff"), 2<<20)
			source := filepath.Join(t.TempDir(), "local.bin")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			var controlErr error
			var checkControl sync.Once
			result, err := c.Upload(attemptCtx, UploadSpec{Node: "second", ID: "resume", Path: "remote.bin", LocalPath: source, Progress: func(p upload.Result) {
				if p.Offset >= upload.CheckpointBytes {
					checkControl.Do(func() {
						controlCtx, stop := context.WithTimeout(ctx, 5*time.Second)
						defer stop()
						controlErr = c.Call(controlCtx, "second", "system.info", map[string]any{}, nil)
					})
					cancel()
				}
			}})
			cancel()
			if err == nil || result.State == "published" {
				t.Fatal("cancelled upload completed", result, err)
			}
			if controlErr != nil {
				t.Fatal("control traffic stalled during bulk upload", controlErr)
			}
			for {
				result, err = c.UploadStatus(ctx, "second", "resume", false)
				if err != nil {
					t.Fatal(err)
				}
				if !result.Active {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if result.Offset < upload.CheckpointBytes || result.State != "staging" {
				t.Fatal("missing durable checkpoint", result)
			}
			resumeOffset := result.Offset
			if _, err = os.Stat(filepath.Join(worker.Config.WorkDir, "remote.bin")); !os.IsNotExist(err) {
				t.Fatal("partial published", err)
			}
			// A different owner with the same account does not inherit this upload.
			other := (Client{}).WithStandalone(ctx, StandaloneConfig{Gateway: admin, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true, PeerOptions: c.routing.cfg.PeerOptions})
			defer func() { _ = other.Close() }()
			if _, err = other.UploadStatus(ctx, "second", "resume", false); err == nil {
				t.Fatal("foreign owner saw upload")
			}
			if _, err = other.UploadStatus(ctx, "second", "resume", true); err == nil {
				t.Fatal("foreign owner aborted upload")
			}
			if err = workers[0].Call(ctx, "second", upload.StatusMethod, map[string]string{"id": "resume"}, nil); err == nil {
				t.Fatal("worker credentials acquired upload authority")
			}
			// Restart both endpoints. The worker's grants disappear, but data and
			// checkpoints persist; the fresh client obtains exact new System grants.
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			if err = worker.Close(); err != nil {
				t.Fatal(err)
			}
			worker, err = node.New(worker.Config, transporttest.Loopback())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = worker.Close() })
			if err = worker.Start(ctx); err != nil {
				t.Fatal(err)
			}
			fresh := (Client{}).WithStandalone(ctx, c.routing.cfg)
			defer func() { _ = fresh.Close() }()
			result, err = fresh.Upload(ctx, UploadSpec{Node: "second", ID: "resume", LocalPath: source, Resume: true})
			if err != nil || result.State != "published" || result.ResumedFrom != resumeOffset || result.BytesSent != int64(len(data))-resumeOffset {
				t.Fatal(result, err)
			}
			got, err := os.ReadFile(filepath.Join(worker.Config.WorkDir, "remote.bin"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("uploaded bytes", err)
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if fresh.routing.peer.Connections()[worker.Identity.ID] != mode {
				t.Fatal("wrong actual upload transport", fresh.routing.peer.Connections())
			}
			// Losing a final response is reconciled without sending the file again.
			result, err = fresh.Upload(ctx, UploadSpec{Node: "second", ID: "resume", LocalPath: source, Resume: true})
			if err != nil || result.State != "published" || result.BytesSent != 0 {
				t.Fatal("receipt reconciliation", result, err)
			}
			for _, method := range []string{"files.read", "files.write", "artifacts.export"} {
				if err = fresh.Call(ctx, "second", method, map[string]any{"path": upload.StagingDir + "/private", "data": "YQ=="}, nil); err == nil {
					t.Fatal("private staging exposed", method)
				}
			}
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			server, err := fresh.MCPServer().Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = server.Close() }()
			progressNotifications := make(chan struct{}, 1)
			session, err := mcp.NewClient(&mcp.Implementation{Name: "upload-test", Version: "1"}, &mcp.ClientOptions{ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
				if r.Params.ProgressToken == "upload-progress" && r.Params.Progress > 0 {
					select {
					case progressNotifications <- struct{}{}:
					default:
					}
				}
			}}).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			identity := fresh.routing.peer.IdentityID()
			var beforeMetrics struct {
				Connects int64 `json:"connects"`
			}
			if err = admin.JSON(ctx, http.MethodGet, "/v1/admin/metrics", nil, &beforeMetrics); err != nil {
				t.Fatal(err)
			}
			mcpSource := filepath.Join(t.TempDir(), "mcp.bin")
			if err = os.WriteFile(mcpSource, []byte("MCP source bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			params := &mcp.CallToolParams{Name: "control_upload", Arguments: map[string]any{"node": "second", "id": "mcp", "localPath": mcpSource, "path": "mcp.bin"}}
			params.SetProgressToken("upload-progress")
			tool, err := session.CallTool(ctx, params)
			if err != nil || tool.IsError {
				t.Fatal(tool, err)
			}
			if err = json.Unmarshal([]byte(tool.Content[0].(*mcp.TextContent).Text), &result); err != nil || result.State != "published" {
				t.Fatal(result, err)
			}
			select {
			case <-progressNotifications:
			case <-ctx.Done():
				t.Fatal("missing MCP upload progress", ctx.Err())
			}
			tool, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_upload_status", Arguments: map[string]any{"node": "second", "id": "mcp"}})
			if err != nil || tool.IsError {
				t.Fatal(tool, err)
			}
			if fresh.routing.peer.IdentityID() != identity {
				t.Fatal("MCP upload created extra client identities")
			}
			var afterMetrics struct {
				Connects int64 `json:"connects"`
			}
			if err = admin.JSON(ctx, http.MethodGet, "/v1/admin/metrics", nil, &afterMetrics); err != nil || afterMetrics.Connects != beforeMetrics.Connects {
				t.Fatal("MCP uploads introduced gateway connections", beforeMetrics, afterMetrics, err)
			}
			// A changed local file must not request Finish, even if earlier bytes
			// have already reached a durable worker checkpoint.
			var mutate sync.Once
			var mutateErr error
			_, err = fresh.Upload(ctx, UploadSpec{Node: "second", ID: "changing", Path: "changing.bin", LocalPath: source, Progress: func(p upload.Result) {
				if p.Offset >= upload.CheckpointBytes {
					mutate.Do(func() {
						f, e := os.OpenFile(source, os.O_WRONLY, 0600)
						if e != nil {
							mutateErr = e
							return
						}
						_, mutateErr = f.WriteAt([]byte("changed"), int64(len(data)-7))
						_ = f.Close()
					})
				}
			}})
			if err == nil || mutateErr != nil {
				t.Fatal("source mutation not rejected", err, mutateErr)
			}
			if _, e := os.Stat(filepath.Join(worker.Config.WorkDir, "changing.bin")); !os.IsNotExist(e) {
				t.Fatal("changed source was published", e)
			}
			var grants []model.Delegation
			if err = fresh.Call(ctx, "second", "access.list", map[string]any{}, &grants); err != nil || len(grants) != 0 {
				t.Fatal("temporary upload grants retained", grants, err)
			}
		})
	}
}

func TestUploadRejectsExplicitAPIWithoutReadingSource(t *testing.T) {
	c := Client{URL: "http://127.0.0.1:1"}
	_, err := c.Upload(t.Context(), UploadSpec{Node: "worker", ID: "test", Path: "remote.bin", LocalPath: "does-not-exist"})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("account-client routing")) {
		t.Fatal(err)
	}
}

func TestUploadRejectsIncompleteCompletionReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	data := []byte("payload")
	sum, err := upload.Hash(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := upload.Hash(ctx, bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	ready := upload.Ready{Protocol: upload.Protocol, Generation: "attempt", Result: upload.Result{Spec: upload.Spec{ID: "receipt", Path: "file.bin", Size: int64(len(data)), SHA256: sum}, NodeID: "worker-id", State: "staging", PrefixSHA256: empty}}
	file := filepath.Join(t.TempDir(), "source")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = a.Close() }()
		buf := make([]byte, upload.BlockSize+8)
		for {
			kind, _, err := upload.ReadRecord(a, buf)
			if err != nil {
				return
			}
			if kind == upload.FinishRecord {
				break
			}
		}
		invalid := ready.Result
		invalid.State = "published"
		_ = wire.WriteFrame(a, upload.Progress{Result: invalid})
	}()
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	if _, err = sendUpload(ctx, b, f, info, ready, nil); err == nil {
		t.Fatal("incomplete receipt accepted as successful publication")
	}
	<-done
}

func TestUploadRecoveryFilenamesArePortable(t *testing.T) {
	c := (Client{}).WithStandalone(t.Context(), StandaloneConfig{StateDir: t.TempDir(), Gateway: Admin{URL: "https://gateway.example"}, AccountRouting: true})
	defer func() { _ = c.Close() }()
	if c.uploadRecordPath("BUILD") == c.uploadRecordPath("build") {
		t.Fatal("case-sensitive upload IDs share a recovery file")
	}
	if base := filepath.Base(c.uploadRecordPath("CON")); base == "CON.json" || len(base) != 69 {
		t.Fatal("Windows device name used as recovery filename", base)
	}
}
