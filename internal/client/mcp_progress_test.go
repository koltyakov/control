package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressUnit shortens a worker's waitSeconds in these tests.
const progressUnit = 10 * time.Millisecond

// fakeWaitWorker answers tasks.get through the local API. Each observation is
// returned for one request; the last repeats. A holding worker keeps each
// non-terminal request open for waitSeconds units, as current workers do.
type fakeWaitWorker struct {
	t            *testing.T
	observations []model.Task
	ignoreWait   bool

	mu           sync.Mutex
	waits        []int
	terminalGate <-chan struct{}
}

func (f *fakeWaitWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var call model.APICall
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil || call.Method != "tasks.get" {
		f.t.Error("unexpected request", call.Method, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var q struct {
		ID          string `json:"id"`
		WaitSeconds int    `json:"waitSeconds"`
	}
	if err := json.Unmarshal(call.Params, &q); err != nil || q.ID != "job" {
		f.t.Error("unexpected params", string(call.Params), err)
	}
	f.mu.Lock()
	index := min(len(f.waits), len(f.observations)-1)
	f.waits = append(f.waits, q.WaitSeconds)
	terminalGate := f.terminalGate
	f.mu.Unlock()
	task := f.observations[index]
	task.ID = "job"
	if task.Terminal() && terminalGate != nil {
		select {
		case <-terminalGate:
		case <-r.Context().Done():
			return
		}
	} else if !f.ignoreWait && !task.Terminal() && q.WaitSeconds > 0 {
		select {
		case <-time.After(time.Duration(q.WaitSeconds) * progressUnit):
		case <-r.Context().Done():
			return
		}
	}
	_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(task)})
}

func (f *fakeWaitWorker) requests() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.waits...)
}

type progressRecorder struct {
	mu      sync.Mutex
	records []mcp.ProgressNotificationParams
	first   chan struct{}
}

func (p *progressRecorder) handle(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, *r.Params)
	if len(p.records) == 1 {
		close(p.first)
	}
}

func (p *progressRecorder) messages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	messages := make([]string, len(p.records))
	for i, record := range p.records {
		messages[i] = record.Message
	}
	return messages
}

// settled returns the messages once n have arrived, or fails. Clients may
// handle notifications concurrently with the tool result.
func (p *progressRecorder) settled(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(p.messages()) < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * progressUnit)
	return p.messages()
}

// connectProgress starts an MCP session against a Client whose local API is
// worker. middleware, if set, wraps the server's outgoing messages.
func connectProgress(t *testing.T, ctx context.Context, worker http.Handler, middleware mcp.Middleware) (*mcp.ClientSession, *progressRecorder) {
	t.Helper()
	previous := progressSecond
	progressSecond = progressUnit
	t.Cleanup(func() { progressSecond = previous })
	api := httptest.NewServer(worker)
	t.Cleanup(api.Close)
	server := (Client{URL: api.URL, Token: "test"}).MCPServer()
	if middleware != nil {
		server.AddSendingMiddleware(middleware)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	recorder := &progressRecorder{first: make(chan struct{})}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "progress-test", Version: "1"}, &mcp.ClientOptions{ProgressNotificationHandler: recorder.handle}).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, recorder
}

func taskGetParams(wait int, token any) *mcp.CallToolParams {
	params := &mcp.CallToolParams{Name: "control_task_get", Arguments: map[string]any{"node": "worker", "id": "job", "waitSeconds": wait}}
	if token != nil {
		params.SetProgressToken(token)
	}
	return params
}

func toolTask(t *testing.T, ctx context.Context, session *mcp.ClientSession, params *mcp.CallToolParams) model.Task {
	t.Helper()
	result, err := session.CallTool(ctx, params)
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatal("tool call failed", result, err)
	}
	var task model.Task
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestMCPTaskWaitWithoutTokenSendsOneRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker := &fakeWaitWorker{t: t, observations: []model.Task{{State: "queued", Phase: "waiting for worker slot"}, {State: "succeeded"}}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	task := toolTask(t, ctx, session, taskGetParams(45, nil))
	if task.State != "queued" {
		t.Fatal("unexpected task", task)
	}
	if waits := worker.requests(); len(waits) != 1 || waits[0] != 45 {
		t.Fatal("a call without a progress token must forward one unchanged wait", waits)
	}
	if messages := recorder.messages(); len(messages) != 0 {
		t.Fatal("notifications without a progress token", messages)
	}
}

func TestMCPTaskWaitReportsEachStateChangeOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker := &fakeWaitWorker{t: t, observations: []model.Task{
		{State: "queued", Phase: "waiting for worker slot", Spec: model.TaskSpec{Args: model.JSON(map[string]string{"token": "secret-argument"})}},
		{State: "running", Phase: "executing"},
		{State: "running", Phase: "executing"},
		{State: "running", Phase: "publishing outputs"},
		{State: "succeeded", Result: model.JSON("secret-result")},
	}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	task := toolTask(t, ctx, session, taskGetParams(60, "wait-progress"))
	if task.State != "succeeded" || string(task.Result) != `"secret-result"` || task.Spec.Args != nil {
		t.Fatal("the final result differs from a single wait", task)
	}
	if waits := worker.requests(); fmt.Sprint(waits) != "[2 10 10 10 10]" {
		t.Fatal("unexpected long-poll schedule", waits)
	}
	want := []string{"queued: waiting for worker slot", "running: executing", "running: publishing outputs"}
	if messages := recorder.settled(t, len(want)); fmt.Sprint(messages) != fmt.Sprint(want) {
		t.Fatalf("notifications %q, want %q", messages, want)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	last := 0.0
	for _, record := range recorder.records {
		if record.ProgressToken != "wait-progress" || record.Progress <= last || record.Total != 60 {
			t.Fatal("progress must use the token, increase strictly and report the wait budget", record)
		}
		if strings.Contains(record.Message, "secret") {
			t.Fatal("progress exposed task content", record.Message)
		}
		last = record.Progress
	}
}

func TestMCPTaskWaitBoundsRequestsWithoutDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker := &fakeWaitWorker{t: t, observations: []model.Task{{State: "running", Phase: "executing"}}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	// Waits above the worker cap are bounded as the worker would bound them.
	task := toolTask(t, ctx, session, taskGetParams(600, 7))
	if task.State != "running" {
		t.Fatal("unexpected task", task)
	}
	waits := worker.requests()
	total := 0
	for _, wait := range waits {
		total += wait
	}
	if len(waits) != 7 || total != 60 {
		t.Fatal("a 60-second wait must take at most seven polls totalling 60 seconds", waits)
	}
	if messages := recorder.settled(t, 1); fmt.Sprint(messages) != "[running: executing]" {
		t.Fatal("unchanged state was reported more than once", messages)
	}
}

func TestMCPTaskWaitOlderWorkerAnswersOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// An older worker ignores waitSeconds and omits phase.
	worker := &fakeWaitWorker{t: t, ignoreWait: true, observations: []model.Task{{State: "running"}}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	task := toolTask(t, ctx, session, taskGetParams(60, "older"))
	if task.State != "running" {
		t.Fatal("unexpected task", task)
	}
	if waits := worker.requests(); len(waits) != 1 {
		t.Fatal("an older worker must not be polled in a loop", waits)
	}
	if messages := recorder.messages(); len(messages) != 0 {
		t.Fatal("unexpected notifications", messages)
	}
}

func TestMCPTaskWaitReportsStateWithoutPhase(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker := &fakeWaitWorker{t: t, observations: []model.Task{{State: "running"}, {State: "succeeded"}}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	// Keep the call open until the client receives progress. Otherwise the
	// terminal reply can cancel the pending notification before it is sent.
	worker.mu.Lock()
	worker.terminalGate = recorder.first
	worker.mu.Unlock()
	if task := toolTask(t, ctx, session, taskGetParams(30, "no-phase")); task.State != "succeeded" {
		t.Fatal("unexpected task", task)
	}
	if messages := recorder.messages(); len(messages) != 1 || messages[0] != "running" {
		t.Fatal("unexpected notifications", messages)
	}
}

func TestMCPTaskWaitNotificationFailureDoesNotFailCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var attempts atomic.Int32
	failing := func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "notifications/progress" {
				attempts.Add(1)
				return nil, errors.New("notification transport failed")
			}
			return next(ctx, method, req)
		}
	}
	worker := &fakeWaitWorker{t: t, observations: []model.Task{{State: "queued"}, {State: "running", Phase: "executing"}, {State: "succeeded"}}}
	session, _ := connectProgress(t, ctx, worker, failing)
	if task := toolTask(t, ctx, session, taskGetParams(60, "failing")); task.State != "succeeded" {
		t.Fatal("unexpected task", task)
	}
	if attempts.Load() == 0 {
		t.Fatal("the failing notification was not attempted")
	}

	// A notification that never completes is abandoned when the call ends:
	// its send is cancelled and joined before the result is returned.
	var blocked, released atomic.Int32
	stuck := func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "notifications/progress" {
				blocked.Add(1)
				<-ctx.Done()
				released.Add(1)
				return nil, ctx.Err()
			}
			return next(ctx, method, req)
		}
	}
	worker = &fakeWaitWorker{t: t, observations: []model.Task{{State: "queued"}, {State: "running"}, {State: "running", Phase: "publishing outputs"}, {State: "succeeded"}}}
	session, _ = connectProgress(t, ctx, worker, stuck)
	started := time.Now()
	if task := toolTask(t, ctx, session, taskGetParams(60, "stuck")); task.State != "succeeded" {
		t.Fatal("unexpected task", task)
	}
	if elapsed := time.Since(started); elapsed > progressNotifyTimeout {
		t.Fatal("a stuck notification delayed the call", elapsed)
	}
	if blocked.Load() == 0 || released.Load() != blocked.Load() {
		t.Fatal("a notification send outlived the tool call", blocked.Load(), released.Load())
	}
}

func TestMCPTaskWaitCancellationStopsPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	worker := &fakeWaitWorker{t: t, observations: []model.Task{{State: "queued"}, {State: "running", Phase: "executing"}}}
	session, recorder := connectProgress(t, ctx, worker, nil)
	callCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(callCtx, taskGetParams(60, "cancelled"))
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(recorder.messages()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("missing progress before cancellation", recorder.messages())
		}
		time.Sleep(progressUnit)
	}
	stop()
	if err := <-done; err == nil {
		t.Fatal("a cancelled call reported success")
	}
	// The server observes the cancellation asynchronously; afterwards it
	// sends no further polls or notifications.
	time.Sleep(20 * progressUnit)
	requests, messages := len(worker.requests()), len(recorder.messages())
	time.Sleep(60 * progressUnit)
	if len(worker.requests()) != requests || len(recorder.messages()) != messages || requests >= 7 {
		t.Fatal("polling continued after cancellation", worker.requests(), recorder.messages())
	}
}

func TestMCPWorkflowReconcileReportsChildCounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	steps := []model.WorkflowStep{
		{Name: "a", Target: "worker", Task: model.TaskSpec{ID: "child-a"}},
		{Name: "b", Target: "worker", Task: model.TaskSpec{ID: "child-b"}},
		{Name: "c", Target: "worker", Task: model.TaskSpec{ID: "child-c"}},
	}
	var requests atomic.Int32
	worker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var call model.APICall
		_ = json.NewDecoder(r.Body).Decode(&call)
		var q struct{ ID string }
		_ = json.Unmarshal(call.Params, &q)
		task := model.Task{ID: q.ID, State: "succeeded"}
		switch q.ID {
		case "parent":
			task = model.Task{ID: "parent", State: "running", Spec: model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}}
		case "child-c":
			// A slower child lets earlier counts be sent before the call ends.
			time.Sleep(20 * progressUnit)
			task.State = "running"
		}
		_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(task)})
	})
	session, recorder := connectProgress(t, ctx, worker, nil)
	arguments := map[string]any{"node": "coordinator", "id": "parent"}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_workflow_reconcile", Arguments: arguments})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	if messages := recorder.messages(); len(messages) != 0 {
		t.Fatal("notifications without a progress token", messages)
	}
	withoutToken := requests.Load()
	params := &mcp.CallToolParams{Name: "control_workflow_reconcile", Arguments: arguments}
	params.SetProgressToken("reconcile")
	if result, err = session.CallTool(ctx, params); err != nil || result.IsError {
		t.Fatal(result, err)
	}
	if requests.Load() != 2*withoutToken {
		t.Fatal("progress added requests", withoutToken, requests.Load())
	}
	// The notifier keeps only the newest unsent message, so the first count
	// may be replaced by the second before it is sent, and the final count
	// may be discarded when the result is ready first.
	messages := recorder.settled(t, 1)
	if len(messages) == 0 || (messages[0] != "queried 1 of 3 children, 1 terminal" && messages[0] != "queried 2 of 3 children, 2 terminal") {
		t.Fatal("unexpected reconciliation progress", messages)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	last := 0.0
	for _, record := range recorder.records {
		var queried, terminal int
		if _, err := fmt.Sscanf(record.Message, "queried %d of 3 children, %d terminal", &queried, &terminal); err != nil || record.Progress != float64(queried) || record.Progress <= last || record.Total != 3 || terminal > queried {
			t.Fatal("reconciliation progress must count children", record, err)
		}
		last = record.Progress
	}
}
