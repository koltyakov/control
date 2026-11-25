package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/vaultwire"
)

type frontRequest struct {
	op     string
	params json.RawMessage
}

type frontPeer struct {
	mu       sync.Mutex
	requests []frontRequest
	respond  func(op string, params json.RawMessage) (json.RawMessage, error)
}

func (p *frontPeer) Do(_ context.Context, op string, params json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.requests = append(p.requests, frontRequest{op: op, params: append(json.RawMessage(nil), params...)})
	respond := p.respond
	p.mu.Unlock()
	if respond == nil {
		return json.RawMessage(`{}`), nil
	}
	return respond(op, params)
}

func (p *frontPeer) sent() []frontRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]frontRequest(nil), p.requests...)
}

func runFront(t *testing.T, peer vaultPeer, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runVaultFrontend(t.Context(), args, peer, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestVaultFrontendRefusesWithoutSending(t *testing.T) {
	peer := &frontPeer{}
	for _, args := range [][]string{
		{"shell", "w1"}, {"upload", "start", "w1", "a", "b"}, {"clipboard", "paste", "w1"}, {"speedtest", "w1"}, {"wake", "w1"},
		{"login"}, {"secrets", "list"}, {"keys", "list"}, {"users"}, {"audit"}, {"update", "push"}, {"console"}, {"setup"},
		{"service", "start"}, {"install-mcp", "opencode"}, {"dashboard"}, {"doctor"},
		{"artifact", "get", "w1", "id", "out"}, {"upload", "resume", "w1", "id", "f"}, {"upload", "status", "w1", "bad id!"},
		{"call", "w1", "shell.open"}, {"call", "w1", "secrets.set", `{}`}, {"call", "w1", "tcp.listen", `{}`},
		{"mcp", "--scope", "chat"}, {"nonsense"},
	} {
		if _, _, err := runFront(t, peer, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
	if sent := peer.sent(); len(sent) != 0 {
		t.Fatalf("refused commands reached the broker: %v", sent)
	}
	if _, _, err := runFront(t, peer, "mcp", "--scope", "subagent"); !errors.Is(err, client.ErrVaultSelectiveScope) {
		t.Fatalf("selective scope error = %v", err)
	}
	if _, _, err := runFront(t, nil, "session"); err == nil {
		t.Fatal("missing broker accepted")
	}
}

func TestVaultFrontendSystemUsesBrokerCall(t *testing.T) {
	peer := &frontPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"os":"linux"}`), nil
	}}
	stdout, _, err := runFront(t, peer, "system", "w1", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	var call client.VaultCall
	sent := peer.sent()
	if len(sent) != 1 || sent[0].op != client.VaultOpCall || json.Unmarshal(sent[0].params, &call) != nil || call.Target != "w1" || call.Method != "system.info" || string(call.Params) != `{"refresh":true}` {
		t.Fatalf("sent %v", sent)
	}
	if !strings.Contains(stdout, `"os": "linux"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestVaultFrontendExecPrintsIDBeforeSubmitting(t *testing.T) {
	var submitted string
	peer := &frontPeer{respond: func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var call client.VaultCall
		_ = json.Unmarshal(params, &call)
		var q struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(call.Params, &q)
		if call.Method == "tasks.start" {
			submitted = q.ID
			return model.JSON(model.Task{ID: q.ID, State: "queued"}), nil
		}
		return model.JSON(model.Task{ID: q.ID, State: "succeeded", Result: json.RawMessage(`{"exitCode":0}`)}), nil
	}}
	stdout, stderr, err := runFront(t, peer, "exec", "w1", "--", "echo", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if submitted == "" || !strings.Contains(stderr, "task "+submitted) || !strings.Contains(stdout, `"succeeded"`) {
		t.Fatalf("stdout %q stderr %q id %q", stdout, stderr, submitted)
	}
	if sent := peer.sent(); len(sent) != 2 {
		t.Fatalf("sent %d requests, want start and one wait", len(sent))
	}
}

func TestVaultFrontendBoundsJSONInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "params.json")
	if err := os.WriteFile(path, []byte(`{"x":"`+strings.Repeat("a", client.VaultMaxPayload)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	peer := &frontPeer{}
	if _, _, err := runFront(t, peer, "call", "w1", "files.list", "@"+path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized input error = %v", err)
	}
	if sent := peer.sent(); len(sent) != 1 || sent[0].op != client.VaultOpCheck {
		t.Fatalf("oversized input sent %v, want only the grant check", sent)
	}
}

func TestVaultFrontendFileInputNeedsLiveGrant(t *testing.T) {
	present := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(present, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent.json")
	for _, args := range [][]string{
		{"call", "w1", "files.list", "@" + present}, {"call", "w1", "files.list", "@" + missing},
		{"task", "start", "w1", "@" + present}, {"task", "start", "w1", "@" + missing},
		{"runbooks"}, {"runbooks", "--local"},
	} {
		peer := &frontPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
			return nil, &vaultwire.Error{Code: vaultwire.CodeApprovalRequired, Message: "grant needed", Entry: &vaultwire.Entry{Code: "ABCD-EFGH"}}
		}}
		_, _, err := runFront(t, peer, args...)
		// The same answer whether or not the file exists.
		if !client.VaultAuthorizationError(err) || strings.Contains(err.Error(), "no such file") {
			t.Errorf("%v = %v", args, err)
		}
		if sent := peer.sent(); len(sent) != 1 || sent[0].op != client.VaultOpCheck {
			t.Errorf("%v sent %v, want one grant check", args, sent)
		}
	}
}

func TestVaultFrontendTunnelManagement(t *testing.T) {
	peer := &frontPeer{respond: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		if op == client.VaultOpTunnelList {
			return json.RawMessage(`[]`), nil
		}
		return json.RawMessage(`{"id":"t1","node":"w1","address":"127.0.0.1:80","listen":"127.0.0.1:8080","nodeId":"n","created":"2026-01-01T00:00:00Z","activeConnections":0}`), nil
	}}
	if _, _, err := runFront(t, peer, "tunnel", "list"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFront(t, peer, "tunnel", "start", "w1", "127.0.0.1:80", "--listen", "127.0.0.1:8080", "--id", "t1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFront(t, peer, "tunnel", "dispose", "t1"); err != nil {
		t.Fatal(err)
	}
	sent := peer.sent()
	if len(sent) != 3 || sent[0].op != client.VaultOpTunnelList || sent[1].op != client.VaultOpTunnelStart || sent[2].op != client.VaultOpTunnelDispose || string(sent[2].params) != `{"id":"t1"}` {
		t.Fatalf("sent %v", sent)
	}
}

// donePeer reports a physical broker disconnect like *vaultwire.Session.
type donePeer struct {
	*frontPeer
	done chan struct{}
}

func (p donePeer) Done() <-chan struct{} { return p.done }

const forwardJSON = `{"id":"f1","node":"w1","listen":"127.0.0.1:9000","activeConnections":0,"capacityRejections":0}`

// forwardPeer starts f1 and answers forward.list with list, signalling each
// listing on listed.
func forwardPeer(list func() (json.RawMessage, error), listed chan<- struct{}) *frontPeer {
	return &frontPeer{respond: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		switch op {
		case client.VaultOpMachines:
			return json.RawMessage(`[{"id":"stable-1","name":"w1"}]`), nil
		case client.VaultOpForwardStart:
			return json.RawMessage(forwardJSON), nil
		case client.VaultOpForwardList:
			select {
			case listed <- struct{}{}:
			default:
			}
			return list()
		}
		return json.RawMessage(`{"stopped":true}`), nil
	}}
}

func fastForwardPoll(t *testing.T) {
	previous := vaultForwardPoll
	vaultForwardPoll = time.Millisecond
	t.Cleanup(func() { vaultForwardPoll = previous })
}

func countOps(requests []frontRequest, op string) int {
	n := 0
	for _, r := range requests {
		if r.op == op {
			n++
		}
	}
	return n
}

func TestVaultFrontendForwardEndsWhenBrokerReportsItGone(t *testing.T) {
	fastForwardPoll(t)
	peer := forwardPeer(func() (json.RawMessage, error) { return json.RawMessage(`[]`), nil }, nil)
	_, _, err := runFront(t, peer, "tunnel", "w1", "127.0.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "no longer held") {
		t.Fatalf("error = %v", err)
	}
	if n := countOps(peer.sent(), client.VaultOpForwardStop); n != 0 {
		t.Fatalf("sent %d stops for an ended forward", n)
	}
}

func TestVaultFrontendForwardSurvivesRevokedGrant(t *testing.T) {
	fastForwardPoll(t)
	for _, code := range []string{vaultwire.CodeGrantRevoked, vaultwire.CodeGrantExpired, vaultwire.CodeApprovalRequired, vaultwire.CodeOpFailed} {
		t.Run(code, func(t *testing.T) {
			listed := make(chan struct{}, 1)
			peer := forwardPeer(func() (json.RawMessage, error) {
				return nil, &vaultwire.Error{Code: code, Message: "grant ended"}
			}, listed)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stderr bytes.Buffer
			result := make(chan error, 1)
			go func() {
				result <- runVaultFrontend(ctx, []string{"tunnel", "w1", "127.0.0.1:80"}, peer, io.Discard, &stderr)
			}()
			<-listed
			select {
			case err := <-result:
				t.Fatalf("a failed observation ended the command: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			if err := <-result; err != nil {
				t.Fatalf("caller cancellation = %v", err)
			}
			sent := peer.sent()
			if n := countOps(sent, client.VaultOpForwardStop); n != 0 {
				t.Fatalf("stop sent after an uncertain grant: %v", sent)
			}
			if n := countOps(sent, client.VaultOpForwardList); n != 1 {
				t.Fatalf("listed %d times after the grant failed, want 1", n)
			}
			if !strings.Contains(stderr.String(), "stays open") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestVaultFrontendForwardEndsOnPhysicalDisconnect(t *testing.T) {
	fastForwardPoll(t)
	listed := make(chan struct{}, 1)
	inner := forwardPeer(func() (json.RawMessage, error) {
		return nil, &vaultwire.Error{Code: vaultwire.CodeGrantRevoked, Message: "revoked"}
	}, listed)
	peer := donePeer{frontPeer: inner, done: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- runVaultFrontend(t.Context(), []string{"browser", "view", "w1"}, peer, io.Discard, io.Discard)
	}()
	<-listed
	close(peer.done)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("disconnect = %v", err)
	}
	if n := countOps(inner.sent(), client.VaultOpForwardStop); n != 0 {
		t.Fatal("stop sent after disconnect")
	}
}

func TestVaultFrontendForwardStopsWhenCallerEndsHealthy(t *testing.T) {
	fastForwardPoll(t)
	listed := make(chan struct{}, 1)
	peer := forwardPeer(func() (json.RawMessage, error) { return json.RawMessage(`[` + forwardJSON + `]`), nil }, listed)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- runVaultFrontend(ctx, []string{"tunnel", "w1", "127.0.0.1:80"}, peer, io.Discard, io.Discard)
	}()
	<-listed
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	sent := peer.sent()
	if last := sent[len(sent)-1]; last.op != client.VaultOpForwardStop || string(last.params) != `{"id":"f1"}` {
		t.Fatalf("healthy forward not stopped on exit: %v", sent)
	}
}

func TestVaultWireErrorOutcomes(t *testing.T) {
	if client.VaultApprovalRequired != vaultwire.CodeApprovalRequired {
		t.Fatal("approval codes diverged")
	}
	for _, c := range []struct{ code, wire, outcome string }{
		{vaultwire.CodeApprovalRequired, "", "not_sent"}, {vaultwire.CodeApprovalDenied, "", "not_sent"},
		{vaultwire.CodeOpFailed, "", "unknown"}, {vaultwire.CodeOpFailed, vaultwire.OutcomeUnknown, "unknown"},
		{vaultwire.CodeOpFailed, vaultwire.OutcomeNotSent, "not_sent"}, {vaultwire.CodeOpFailed, "bogus", "unknown"},
		{vaultwire.CodeGrantRevoked, "", "unknown"}, {vaultwire.CodeInvalidRequest, "", "unknown"},
	} {
		code, outcome := c.code, c.outcome
		peer := &frontPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
			return nil, &vaultwire.Error{Code: code, Outcome: c.wire, Message: "m", Entry: &vaultwire.Entry{Code: "ABCD-EFGH"}}
		}}
		_, _, err := runFront(t, peer, "diagnostics", "w1")
		var rpc *model.RPCError
		if !errors.As(err, &rpc) || rpc.Code != code || rpc.Outcome != outcome {
			t.Errorf("%s = %#v", code, err)
		}
		if code == vaultwire.CodeApprovalRequired && !strings.Contains(err.Error(), "ABCD-EFGH") {
			t.Errorf("approval error lacks its code: %v", err)
		}
		if len(peer.sent()) != 1 {
			t.Errorf("%s was replayed", code)
		}
	}
}
