package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP task-wait progress. A control_task_get call with waitSeconds and a
// client progress token is split into a bounded schedule of long polls so the
// observed state and phase can be reported between them. Without a token the
// call is forwarded unchanged as one tasks.get.
const (
	// progressFirstSlice reports the initial state soon after the call starts.
	progressFirstSlice = 2
	// progressSlice bounds later polls: at most seven tasks.get requests cover
	// the 60-second maximum wait (2 + 5x10 + 8).
	progressSlice = 10
	// progressNotifyTimeout matches the upload path: a slow notification is
	// abandoned rather than delaying the next one.
	progressNotifyTimeout = 2 * time.Second
	// progressTextLimit bounds worker-supplied state and phase text.
	progressTextLimit = 64
)

// progressNotifier sends coalesced progress notifications for one tool call
// from a single goroutine, so reporting never blocks the caller. stop cancels
// any pending send and joins the goroutine before the tool result is returned.
type progressNotifier struct {
	session *mcp.ServerSession
	token   any
	parent  context.Context

	mu      sync.Mutex
	last    string // latest queued message, for duplicate suppression
	pending *mcp.ProgressNotificationParams
	started bool
	stopped bool
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	sent    float64
}

// newProgressNotifier returns nil when the request carries no progress token;
// every method is a no-op on nil.
func newProgressNotifier(ctx context.Context, request *mcp.CallToolRequest) *progressNotifier {
	if request == nil || request.Session == nil || request.Params == nil || request.Params.GetProgressToken() == nil {
		return nil
	}
	return &progressNotifier{session: request.Session, token: request.Params.GetProgressToken(), parent: ctx, wake: make(chan struct{}, 1)}
}

// report queues message unless it repeats the previous one. Only the newest
// queued message is kept while a send is in flight. A zero total is omitted.
func (p *progressNotifier) report(progress, total float64, message string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || message == p.last {
		return
	}
	p.last = message
	p.pending = &mcp.ProgressNotificationParams{ProgressToken: p.token, Progress: progress, Total: total, Message: message}
	if !p.started {
		p.started = true
		var ctx context.Context
		ctx, p.cancel = context.WithCancel(p.parent)
		p.done = make(chan struct{})
		go p.run(ctx)
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *progressNotifier) run(ctx context.Context) {
	defer close(p.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		p.mu.Lock()
		params := p.pending
		p.pending = nil
		// The MCP specification requires strictly increasing progress.
		if params != nil {
			if params.Progress <= p.sent {
				params.Progress = p.sent + 0.001
			}
			p.sent = params.Progress
		}
		p.mu.Unlock()
		if params == nil {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, progressNotifyTimeout)
		_ = p.session.NotifyProgress(sendCtx, params)
		cancel()
	}
}

// stop discards unsent messages, cancels an in-flight send and waits for the
// sender to exit, so no notification follows the tool result.
func (p *progressNotifier) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stopped = true
	started := p.started
	p.mu.Unlock()
	if started {
		p.cancel()
		<-p.done
	}
}

// progressText keeps a bounded, single-line copy of worker-supplied text.
func progressText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > progressTextLimit {
		s = strings.ToValidUTF8(s[:progressTextLimit], "")
	}
	return s
}

// taskProgressMessage describes only the task state and phase, never its
// arguments, result, error text or logs.
func taskProgressMessage(state, phase string) string {
	message := progressText(state)
	if phase = progressText(phase); phase != "" {
		message += ": " + phase
	}
	return message
}

// reconcileProgress reports workflow reconciliation as child counts. It adds
// no requests; nil means the request carried no progress token.
func reconcileProgress(notify *progressNotifier) func(queried, terminal, total int) {
	if notify == nil {
		return nil
	}
	return func(queried, terminal, total int) {
		notify.report(float64(queried), float64(total), fmt.Sprintf("queried %d of %d children, %d terminal", queried, total, terminal))
	}
}

// progressWaitSeconds returns the waitSeconds of a control_task_get call, or
// zero when the call should be forwarded unchanged.
func progressWaitSeconds(name string, args map[string]json.RawMessage) int {
	raw, ok := args["waitSeconds"]
	if name != "control_task_get" || !ok {
		return 0
	}
	var wait int
	if json.Unmarshal(raw, &wait) != nil || wait <= 0 {
		return 0
	}
	// Workers cap a wait at 60 seconds.
	return min(wait, int(maxProgressWait/time.Second))
}

const maxProgressWait = 60 * time.Second

// progressSecond is the worker's waitSeconds unit; tests shorten it.
var progressSecond = time.Second

// waitTaskWithProgress performs the same total worker-side wait as one
// tasks.get with waitSeconds, split into at most seven long polls. It returns
// the first terminal record, the record observed when the wait elapses, or the
// first early non-terminal reply. An early reply comes from an older worker
// that ignores waitSeconds, a worker shutting down or a request deadline; a
// single call would have returned it too, so the loop never polls rapidly.
// Errors are returned as from a single call; a lost reply still means
// reconciling the same task ID.
func (c Client) waitTaskWithProgress(ctx context.Context, target string, args map[string]json.RawMessage, wait int, notify *progressNotifier) (json.RawMessage, error) {
	started := time.Now()
	slice := min(wait, progressFirstSlice)
	for remaining := wait; ; slice = min(remaining, progressSlice) {
		params := make(map[string]json.RawMessage, len(args))
		for key, value := range args {
			params[key] = value
		}
		params["waitSeconds"] = json.RawMessage(fmt.Sprint(slice))
		polled := time.Now()
		var result json.RawMessage
		if err := c.Call(ctx, target, "tasks.get", params, &result); err != nil {
			return result, err
		}
		remaining -= slice
		var observed struct {
			State string `json:"state"`
			Phase string `json:"phase"`
		}
		if json.Unmarshal(result, &observed) != nil || (model.Task{State: observed.State}).Terminal() || remaining <= 0 {
			return result, nil
		}
		if time.Since(polled) < time.Duration(slice)*progressSecond/2 {
			return result, nil
		}
		notify.report(time.Since(started).Seconds(), float64(wait), taskProgressMessage(observed.State, observed.Phase))
	}
}
