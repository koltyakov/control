package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrOrchestratorKeysUnsupported means the gateway does not advertise
// orchestratorKeys. Nothing was sent: an older gateway ignores the role field
// and would issue a common key, which can enroll machines.
var ErrOrchestratorKeysUnsupported = errors.New("gateway does not support execution-only orchestrator keys; upgrade the gateway. No key was created")

// CreateOrchestratorKey issues an execution-only fleet key. It refuses before
// sending unless /v1/auth advertises orchestratorKeys, and it never retries.
// The returned bytes are the gateway's verified creation response, including
// the one-time secret. Any other outcome returns an error and no secret:
// a refusal, an uncertain result to inspect, or a key with a different role to
// revoke by ID.
func (c Admin) CreateOrchestratorKey(ctx context.Context, name string) (json.RawMessage, error) {
	return c.CreateExecutionKey(ctx, name, "orchestrator")
}

// CreateExecutionKey issues the requested execution role only after checking
// that the gateway understands it. Never retry a key issuance.
func (c Admin) CreateExecutionKey(ctx context.Context, name, role string) (json.RawMessage, error) {
	if role != "orchestrator" && role != "orchestrator-enroll" {
		return nil, fmt.Errorf("unknown execution key role %q", role)
	}
	var auth struct {
		OrchestratorKeys       bool `json:"orchestratorKeys"`
		OrchestratorEnrollKeys bool `json:"orchestratorEnrollKeys"`
	}
	authCtx, cancel := context.WithTimeout(ctx, accountTimeout)
	err := c.request(authCtx, http.MethodGet, "/v1/auth", nil, 0, "application/json", &auth)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("authenticate with gateway %s: %w", c.URL, err)
	}
	if role == "orchestrator-enroll" && !auth.OrchestratorEnrollKeys {
		return nil, errors.New("upgrade the gateway to issue a key that can enroll and run work; no key was created")
	}
	if role == "orchestrator" && !auth.OrchestratorKeys {
		return nil, ErrOrchestratorKeysUnsupported
	}
	body, err := json.Marshal(map[string]string{"name": name, "role": role})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/v1/fleet/keys", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	uncertain := func(cause error) error {
		return fmt.Errorf("orchestrator key creation outcome is uncertain (%w); it was not retried. Run control keys list, and revoke any unexpected key named %q with control keys revoke ID before issuing another", cause, name)
	}
	resp, err := c.Route.Client(3*time.Minute, false).Do(req)
	if err != nil {
		return nil, uncertain(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// Never echo the body or a proxy's custom reason text from a
		// credential issuance endpoint. Either can contain credential data.
		return nil, fmt.Errorf("gateway refused key creation: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, uncertain(fmt.Errorf("gateway HTTP %d", resp.StatusCode))
	}
	const maxResponse = 64 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return nil, uncertain(errors.New("invalid creation response"))
	}
	var created struct {
		Key struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"key"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.Key.ID == "" {
		return nil, uncertain(errors.New("unreadable creation response"))
	}
	if created.Key.Role != role || created.Token == "" {
		return nil, fmt.Errorf("gateway issued key %s with role %q instead of %q; its secret was withheld. Revoke it with control keys revoke %s; it was not retried", created.Key.ID, created.Key.Role, role, created.Key.ID)
	}
	return data, nil
}
