//go:build compose

package compose_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func environment(t *testing.T) (context.Context, client.Client) {
	t.Helper()
	api, token := os.Getenv("CONTROL_API"), os.Getenv("CONTROL_TOKEN")
	if api == "" || token == "" {
		t.Fatal("run these tests with Docker Compose; CONTROL_API and CONTROL_TOKEN are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx, client.Client{URL: api, Token: token}
}

func call(t *testing.T, ctx context.Context, c client.Client, target, method string, args, result any) {
	t.Helper()
	if err := c.Call(ctx, target, method, args, result); err != nil {
		t.Fatalf("%s on %s: %v", method, target, err)
	}
}

func cli(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(ctx, "control", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("control %v: %v\n%s\n%s", args, err, stderr.String(), b)
	}
	return b
}

func nodes(t *testing.T, ctx context.Context, c client.Client) map[string]model.Node {
	t.Helper()
	var pool []model.Node
	call(t, ctx, c, "", "nodes.list", map[string]any{}, &pool)
	result := map[string]model.Node{}
	for _, node := range pool {
		result[node.Name] = node
	}
	for _, name := range []string{"source", "worker", "consumer"} {
		if !result[name].Online || result[name].ID == "" {
			t.Fatalf("%s is not enrolled and online: %+v", name, pool)
		}
	}
	return result
}

func assertTransport(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	expected := os.Getenv("CONTROL_EXPECT_TRANSPORT")
	if expected != "webrtc" && expected != "relay" {
		t.Fatal("CONTROL_EXPECT_TRANSPORT must be webrtc or relay")
	}
	pool := nodes(t, ctx, c)
	var description struct {
		Connections map[string]string `json:"connections"`
	}
	call(t, ctx, c, "worker", "node.describe", map[string]any{}, &description)
	for _, name := range []string{"source", "consumer"} {
		if actual := description.Connections[pool[name].ID]; actual != expected {
			t.Fatalf("worker → %s used %q, expected %q", name, actual, expected)
		}
	}
	t.Logf("verified worker/source and worker/consumer sessions use %s", expected)
}

func TestPoolAndAuthentication(t *testing.T) {
	ctx, c := environment(t)
	pool := nodes(t, ctx, c)
	seen := map[string]bool{}
	for name, node := range pool {
		if seen[node.ID] {
			t.Fatal("nodes share an identity")
		}
		seen[node.ID] = true
		if node.Labels["role"] != name {
			t.Fatalf("unexpected node labels: %+v", node)
		}
	}
	c.Token = "wrong-token"
	if err := c.Call(ctx, "", "nodes.list", map[string]any{}, nil); err == nil {
		t.Fatal("local API accepted an invalid token")
	}
}

func TestFFmpegWorkflowAndResumableCLIDownload(t *testing.T) {
	ctx, c := environment(t)
	b, err := os.ReadFile("../../examples/workflow-task.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec model.TaskSpec
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	spec.ID = "compose-video-" + identity.NewID()
	// Exercise the shipped CLI against the independently running source node.
	var accepted model.Task
	if err = json.Unmarshal(cli(t, ctx, "task", "start", "source", string(model.JSON(spec))), &accepted); err != nil {
		t.Fatal(err)
	}
	finished, err := c.Wait(ctx, "source", accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "succeeded" {
		t.Fatalf("workflow failed: %s; result=%s", finished.Error, finished.Result)
	}
	var steps map[string]model.Task
	if err = json.Unmarshal(finished.Result, &steps); err != nil {
		t.Fatal(err)
	}
	encoded := steps["encode"]
	if len(encoded.Artifacts) != 1 {
		t.Fatalf("missing encoded artifact: %+v", encoded)
	}
	a := encoded.Artifacts[0]
	if a.Size <= 512 {
		t.Fatalf("encoded video is unexpectedly small: %+v", a)
	}
	var delivered model.Artifact
	if err = json.Unmarshal(cli(t, ctx, "artifact", "deliver", "worker", a.ID, "consumer"), &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Node != nodes(t, ctx, c)["consumer"].ID {
		t.Fatal("consumer does not own delivered artifact")
	}

	// Stop a download early, keep its prefix, then let the CLI resume it.
	prefix := &prefixWriter{remaining: 512}
	if err = c.Download(ctx, delivered, 0, prefix); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected interrupted prefix download, got %v", err)
	}
	output := filepath.Join(t.TempDir(), "encoded.mp4")
	if err = os.WriteFile(output+".partial", prefix.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cli(t, ctx, "artifact", "get", "consumer", a.ID, output)
	video, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(video)
	if int64(len(video)) != a.Size || hex.EncodeToString(digest[:]) != a.SHA256 {
		t.Fatal("download size or checksum differs")
	}
	if _, err = os.Stat(output + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CLI did not finalize the partial download")
	}

	var probe struct {
		Stdout string `json:"stdout"`
	}
	call(t, ctx, c, "worker", "exec.run", map[string]any{
		"command": "ffprobe", "args": []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "json", "tasks/" + encoded.ID + "/output.mp4"},
	}, &probe)
	var media struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err = json.Unmarshal([]byte(probe.Stdout), &media); err != nil {
		t.Fatal(err)
	}
	if len(media.Streams) != 1 || media.Streams[0].Width != 160 || media.Streams[0].Height != 120 {
		t.Fatalf("unexpected encoded dimensions: %s", probe.Stdout)
	}
	var sourceArtifacts []model.Artifact
	call(t, ctx, c, "source", "artifacts.list", map[string]any{}, &sourceArtifacts)
	for _, artifact := range sourceArtifacts {
		if artifact.ID == a.ID {
			t.Fatal("encoded output was stored on the orchestrator")
		}
	}
	assertTransport(t, ctx, c)
	t.Logf("encoded, delivered, resumed and verified %d video bytes", len(video))
}

type prefixWriter struct {
	buffer    bytes.Buffer
	remaining int
}

func (w *prefixWriter) Bytes() []byte { return w.buffer.Bytes() }
func (w *prefixWriter) Write(b []byte) (int, error) {
	count := min(len(b), w.remaining)
	n, _ := w.buffer.Write(b[:count])
	w.remaining -= n
	if count < len(b) || w.remaining == 0 {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func TestLargeArtifactPeerDelivery(t *testing.T) {
	ctx, c := environment(t)
	payload := bytes.Repeat([]byte("cross-container-artifact\n"), 150000)
	path := "payload-" + identity.NewID() + ".bin"
	for offset := 0; offset < len(payload); {
		end := min(offset+(1<<20), len(payload))
		call(t, ctx, c, "source", "files.write", map[string]any{"path": path, "offset": offset, "truncate": offset == 0, "data": base64.StdEncoding.EncodeToString(payload[offset:end])}, nil)
		offset = end
	}
	var a, workerCopy, consumerCopy model.Artifact
	call(t, ctx, c, "source", "artifacts.export", map[string]any{"path": path}, &a)
	call(t, ctx, c, "source", "artifacts.deliver", map[string]any{"id": a.ID, "target": "worker"}, &workerCopy)
	call(t, ctx, c, "worker", "artifacts.deliver", map[string]any{"id": workerCopy.ID, "target": "consumer"}, &consumerCopy)
	var downloaded bytes.Buffer
	if err := c.Download(ctx, consumerCopy, 0, &downloaded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded.Bytes(), payload) {
		t.Fatal("multi-megabyte artifact changed during delivery")
	}
	assertTransport(t, ctx, c)
}

func TestPrivateNetworkHTTPAndTCPTunnel(t *testing.T) {
	ctx, c := environment(t)
	const body = "response from the isolated Compose network"
	fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/private" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	_ = fixture.Listener.Close()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Listener = listener
	fixture.Start()
	defer fixture.Close()
	address := fmt.Sprintf("tests:%d", listener.Addr().(*net.TCPAddr).Port)
	var result struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	call(t, ctx, c, "worker", "http.request", map[string]any{"url": "http://" + address + "/private"}, &result)
	decoded, err := base64.StdEncoding.DecodeString(result.Body)
	if err != nil || result.Status != http.StatusOK || string(decoded) != body {
		t.Fatalf("remote HTTP response: %+v, %v", result, err)
	}
	conn, err := c.Tunnel(ctx, "worker", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/private", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Close = true
	if err = req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != body {
		t.Fatalf("TCP tunnel response: %q, %v", b, err)
	}
}

func TestReverseDevServerTunnel(t *testing.T) {
	ctx, c := environment(t)
	c = c.WithLifetime(ctx)
	defer c.Close()
	const body = "dev server bound only to the orchestrator loopback"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer app.Close()
	f, err := c.StartForward(ctx, client.ForwardSpec{Node: "worker", Address: strings.TrimPrefix(app.URL, "http://"), Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var result struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	// This localhost belongs to the worker container, not the test runner or
	// the source node relaying the client's local API connection.
	call(t, ctx, c, "worker", "http.request", map[string]any{"url": "http://" + f.Info().Listen}, &result)
	decoded, err := base64.StdEncoding.DecodeString(result.Body)
	if err != nil || result.Status != http.StatusOK || string(decoded) != body {
		t.Fatalf("reverse HTTP response: %+v, %v", result, err)
	}
	if err := c.StopForward(f.Info().ID); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardObservesPoolWorkAndSampledResources(t *testing.T) {
	ctx, c := environment(t)
	pool := nodes(t, ctx, c)
	consumer := client.Client{URL: "http://consumer:7331", Token: c.Token}
	// A file larger than socket buffers keeps the peer transfer observable
	// while a deliberately slow downloader holds the receiver open.
	call(t, ctx, c, "consumer", "exec.run", map[string]any{"command": "dd", "args": []string{"if=/dev/zero", "of=dashboard.bin", "bs=1048576", "count=32"}}, nil)
	var artifact model.Artifact
	call(t, ctx, c, "consumer", "artifacts.export", map[string]any{"path": "dashboard.bin"}, &artifact)
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	writer := &pausedWriter{started: make(chan struct{}), release: make(chan struct{})}
	transferDone := make(chan error, 1)
	go func() { transferDone <- c.Download(streamCtx, artifact, 0, writer) }()
	defer func() {
		stop()
		close(writer.release)
		select {
		case <-transferDone:
		case <-time.After(5 * time.Second):
			t.Error("download did not stop")
		}
	}()
	select {
	case <-writer.started:
	case err := <-transferDone:
		t.Fatalf("download ended before observation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	ids := []string{}
	defer func() {
		for _, id := range ids {
			_ = consumer.Call(ctx, "worker", "tasks.cancel", map[string]any{"id": id}, nil)
		}
		for _, id := range ids {
			_, _ = consumer.Wait(ctx, "worker", id)
		}
	}()
	for range 3 {
		id := "dashboard-" + identity.NewID()
		call(t, ctx, consumer, "worker", "tasks.start", model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(map[string]any{"command": "sleep", "args": []string{"60"}})}, nil)
		ids = append(ids, id)
	}
	// Hold one synchronous HTTP operation open on a different worker.
	httpStarted, releaseHTTP := make(chan struct{}), make(chan struct{})
	fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(httpStarted)
			select {
			case <-releaseHTTP:
			case <-r.Context().Done():
			}
		}
		_, _ = io.WriteString(w, "ok")
	}))
	_ = fixture.Listener.Close()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Listener = listener
	fixture.Start()
	defer fixture.Close()
	address := fmt.Sprintf("tests:%d", listener.Addr().(*net.TCPAddr).Port)
	httpDone := make(chan error, 1)
	go func() {
		httpDone <- c.Call(ctx, "consumer", "http.request", map[string]any{"url": "http://" + address + "/hold"}, nil)
	}()
	defer func() {
		close(releaseHTTP)
		select {
		case <-httpDone:
		case <-time.After(5 * time.Second):
			t.Error("HTTP operation did not stop")
		}
	}()
	select {
	case <-httpStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tunnel, err := c.Tunnel(ctx, "worker", address)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	// Leave the HTTP connection alive so the dashboard can observe its tunnel.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = req.Write(tunnel); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tunnel), req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	var snapshot model.PoolActivitySnapshot
	if err = json.Unmarshal(cli(t, ctx, "dashboard", "--once", "--json", "--recent", "0"), &snapshot); err != nil {
		t.Fatal(err)
	}
	if g := snapshot.Gateway; g == nil || g.URL != os.Getenv("CONTROL_GATEWAY") || g.Software.Version == "" || g.System == nil || g.System.SampledAt.IsZero() || g.System.MemoryTotalBytes == 0 {
		t.Fatalf("missing remote gateway status: %+v", g)
	}
	found := map[string]bool{}
	for _, node := range snapshot.Nodes {
		if node.Status != "ready" || !node.Online || node.System == nil || node.System.MemoryTotalBytes == 0 || len(node.System.Disks) == 0 {
			t.Fatalf("missing machine/resource status: %+v", node)
		}
		for _, a := range node.Active {
			if a.Kind == "task" && a.Owner == pool["consumer"].ID {
				found["foreign task"] = true
				found[a.State] = true
			}
			if a.Kind == "operation" && a.Operation == "http.request" {
				found["HTTP"] = true
			}
			if a.Kind == "transfer" && a.TotalBytes == artifact.Size && a.BytesDone > 0 {
				found["transfer"] = true
			}
			if a.Kind == "tunnel" && a.BytesSent > 0 && a.BytesReceived > 0 {
				found["tunnel"] = true
			}
		}
	}
	for _, kind := range []string{"foreign task", "running", "queued", "HTTP", "transfer", "tunnel"} {
		if !found[kind] {
			t.Fatalf("dashboard omitted %s: %s", kind, model.JSON(snapshot))
		}
	}
	text := string(cli(t, ctx, "dashboard", "--once", "--recent", "0"))
	for _, label := range []string{"Gateway", os.Getenv("CONTROL_GATEWAY"), "Machines", "Activity", "CPU", "RAM", "Disk free", "source", "worker", "consumer"} {
		if !strings.Contains(text, label) {
			t.Fatalf("dashboard text missing %q", label)
		}
	}
	var metrics model.SystemInfo
	if err = json.Unmarshal(cli(t, ctx, "system", "worker", "--refresh"), &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.SampledAt.IsZero() || metrics.CPUUsagePercent == nil || metrics.LogicalCPUs == 0 || metrics.MemoryTotalBytes == 0 {
		t.Fatalf("requested resource sample incomplete: %+v", metrics)
	}
	// A logged-in owner also gets machine health without a local observer.
	admin := client.Admin{URL: os.Getenv("CONTROL_GATEWAY"), Key: os.Getenv("CONTROL_SUPERUSER_KEY")}
	healthCtx, cancelHealth := context.WithTimeout(ctx, 12*time.Second)
	defer cancelHealth()
	waitUpdate(t, healthCtx, func() bool {
		owner, err := (client.Client{}).Dashboard(healthCtx, admin, model.PoolActivityQuery{Nodes: []string{"worker"}})
		if err != nil || len(owner.Nodes) != 1 {
			return false
		}
		n := owner.Nodes[0]
		if n.Status != "summary" || n.ActiveCount < 4 || n.System == nil || n.System.CPUUsagePercent == nil {
			return false
		}
		if len(n.Active) != 0 || len(n.Recent) != 0 || owner.Notice != "" {
			t.Fatal("owner health exposed peer records or lost availability")
		}
		for _, id := range ids {
			if strings.Contains(string(model.JSON(owner)), id) {
				t.Fatal("owner summary exposed task IDs")
			}
		}
		return true
	})
	t.Log("observed foreign-owner tasks, queue, HTTP call, transfer progress, tunnel counters, and host resources")
}

type pausedWriter struct {
	started, release chan struct{}
	calls            int
}

func (w *pausedWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 2 {
		close(w.started)
		<-w.release
	}
	return len(b), nil
}
