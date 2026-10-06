package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

const delegationIdleTimeout = time.Hour

type delegationState struct {
	grant        model.Delegation
	ctx          context.Context
	cancel       context.CancelFunc
	lastActivity time.Time
	activeTasks  int
	used         bool
}

type authorityContextKey struct{}
type authority struct {
	owner string
	grant *delegationState
}

func discoveryMethod(method string) bool {
	switch method {
	case "nodes.list", "nodes.select", "node.describe", "capabilities.list", "mcp.discover":
		return true
	}
	return false
}

func (n *Node) executionOwner(ctx context.Context, caller string) string {
	if a, ok := ctx.Value(authorityContextKey{}).(authority); ok {
		return a.owner
	}
	return n.Peer.Owner(caller)
}

// acceptAuthority runs before every streaming and JSON handler. Grants never
// bypass fleet membership and are not installed as ambient worker permissions.
func (n *Node) acceptAuthority(ctx context.Context, caller string, request model.Request) (context.Context, func(), error) {
	ctx = model.WithDelegations(ctx, request.Delegations)
	if request.Delegation == "" {
		if request.Method == "artifacts.open" {
			var q struct {
				Grant string `json:"grant"`
				ID    string `json:"id"`
			}
			if json.Unmarshal(request.Params, &q) == nil && n.validGrant(q.Grant, caller, q.ID) {
				var g artifactGrant
				if n.decodeGrant(q.Grant, &g) && g.Access != "" {
					request.Delegation = g.Access
				}
			}
		}
	}
	if request.Delegation == "" {
		if discoveryMethod(request.Method) {
			return ctx, func() {}, nil
		}
		peer, err := n.Peer.Lookup(ctx, caller)
		if err != nil {
			return ctx, nil, err
		}
		if !peer.ExecutionAuthority {
			return ctx, nil, errors.New("peer execution requires an account-authenticated orchestrator or an instruction-bound delegation")
		}
		ctx = context.WithValue(ctx, authorityContextKey{}, authority{owner: n.Peer.Owner(caller)})
		return ctx, func() {}, nil
	}
	n.delegationMu.Lock()
	s := n.delegations[request.Delegation]
	if s == nil || s.ctx.Err() != nil || s.grant.Subject != caller || !s.grant.Matches(request.Method, request.Params) || !n.delegationLiveLocked(s, time.Now()) {
		n.delegationMu.Unlock()
		return ctx, nil, errors.New("delegation is missing, expired, revoked, or does not authorize this instruction")
	}
	if request.Method == "tasks.start" && len(s.grant.InputsFrom) != 0 {
		// Validate signed output provenance outside the grant lock, then recheck
		// its lifetime before admitting the instruction.
		grant := s.grant
		n.delegationMu.Unlock()
		var expected, actual model.TaskSpec
		_ = json.Unmarshal(grant.Params, &expected)
		_ = json.Unmarshal(request.Params, &actual)
		for i, input := range grant.InputsFrom {
			artifact := actual.Inputs[len(expected.Inputs)+i].Artifact
			peer, err := n.Peer.Lookup(ctx, input.Node)
			var proof artifactGrant
			if err != nil || !decodeArtifactGrant(artifact.Grant, peer.PublicKey, &proof) || proof.TaskID != input.TaskID || proof.Output != input.Artifact || proof.Subject != n.Identity.ID || proof.Artifact != artifact.ID || proof.Expires <= time.Now().Unix() {
				return ctx, nil, errors.New("delegated input is not the authorized upstream task output")
			}
		}
		n.delegationMu.Lock()
		if s.ctx.Err() != nil || !n.delegationLiveLocked(s, time.Now()) {
			n.delegationMu.Unlock()
			return ctx, nil, errors.New("delegation was revoked or expired")
		}
	}
	if s.grant.Method != "tasks.start" && s.grant.Method != "artifacts.open" {
		if s.used {
			n.delegationMu.Unlock()
			return ctx, nil, errors.New("delegated instruction has already been used; reconcile its effects before requesting new work")
		}
		s.used = true
	}
	// Output grants are restricted to artifacts produced by this delegated task.
	if request.Method == "artifacts.grant" {
		var q struct {
			ID          string `json:"id"`
			OutputIndex *int   `json:"outputIndex"`
		}
		_ = json.Unmarshal(request.Params, &q)
		n.mu.Lock()
		task := n.tasks[s.grant.TaskID()]
		valid := false
		if task != nil && task.Owner == s.grant.Owner {
			for index, artifact := range task.Artifacts {
				if artifact.ID == q.ID && (q.OutputIndex == nil || *q.OutputIndex == index) {
					valid = true
				}
			}
		}
		n.mu.Unlock()
		if !valid {
			n.delegationMu.Unlock()
			return ctx, nil, errors.New("delegation does not authorize this artifact")
		}
	}
	s.lastActivity = time.Now()
	n.delegationMu.Unlock()
	ctx = context.WithValue(ctx, authorityContextKey{}, authority{owner: s.grant.Owner, grant: s})
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }, nil
}

func (n *Node) delegationLiveLocked(s *delegationState, now time.Time) bool {
	if s.ctx.Err() != nil {
		delete(n.delegations, s.grant.ID)
		return false
	}
	if s.activeTasks != 0 || now.Before(s.lastActivity.Add(delegationIdleTimeout)) {
		return true
	}
	s.cancel()
	delete(n.delegations, s.grant.ID)
	return false
}

func (n *Node) expireDelegations(now time.Time) {
	n.delegationMu.Lock()
	defer n.delegationMu.Unlock()
	for _, s := range n.delegations {
		n.delegationLiveLocked(s, now)
	}
}

func (n *Node) runDelegations(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			n.expireDelegations(now)
		}
	}
}

func (n *Node) delegationMethod(ctx context.Context, caller, method string, params json.RawMessage) (any, error) {
	a, ok := ctx.Value(authorityContextKey{}).(authority)
	if !ok || a.grant != nil || caller == n.Identity.ID {
		return nil, errors.New("only an account-authenticated orchestrator can manage delegations")
	}
	if method == "access.grant" {
		var q model.Delegation
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		if q.Method == "" || q.Method == "workflow.run" || q.Method == "peers.call" || q.Method == "access.grant" || q.Method == "access.revoke" || q.Method == "access.list" || discoveryMethod(q.Method) {
			return nil, errors.New("delegate an execution instruction, not discovery, delegation management, or nested coordination")
		}
		if err := n.authorize(ctx, caller, q.Method); err != nil {
			return nil, err
		}
		if q.Method == "tasks.start" {
			var spec model.TaskSpec
			if err := json.Unmarshal(q.Params, &spec); err != nil {
				return nil, err
			}
			if !safeID.MatchString(spec.ID) || spec.Capability == "workflow.run" || spec.Capability == "peers.call" {
				return nil, errors.New("delegated tasks require an explicit ID and cannot create further coordination")
			}
			if _, exists := n.providers[spec.Capability]; !exists {
				return nil, fmt.Errorf("unknown capability %q", spec.Capability)
			}
			if err := n.authorize(ctx, caller, spec.Capability); err != nil {
				return nil, err
			}
			for _, permission := range []string{"tasks.get", "tasks.cancel", "tasks.logs"} {
				if err := n.authorize(ctx, caller, permission); err != nil {
					return nil, err
				}
			}
			if len(q.DeliverTo) != 0 {
				if err := n.authorize(ctx, caller, "artifacts.grant"); err != nil {
					return nil, err
				}
			}
		} else if len(q.InputsFrom) != 0 || len(q.DeliverTo) != 0 {
			return nil, errors.New("input/output delegation is only supported for tasks")
		}
		if !json.Valid(q.Params) || len(q.InputsFrom) > 100 || len(q.DeliverTo) > 100 {
			return nil, errors.New("invalid delegation parameters or limits")
		}
		subject, err := n.Peer.Resolve(ctx, q.Subject)
		if err != nil {
			return nil, err
		}
		if subject.ClientOwner != "" || subject.ExecutionAuthority {
			return nil, errors.New("delegation subject must be a worker node")
		}
		for _, input := range q.InputsFrom {
			if !filepath.IsLocal(input.Path) || !safeID.MatchString(input.TaskID) || input.Artifact < 0 {
				return nil, errors.New("delegated input paths must be workspace-relative")
			}
			peer, err := n.Peer.Lookup(ctx, input.Node)
			if err != nil || peer.ID != input.Node {
				return nil, errors.New("delegated inputs require same-fleet source identities")
			}
		}
		for i, target := range q.DeliverTo {
			peer, err := n.Peer.Lookup(ctx, target)
			if err != nil {
				return nil, err
			}
			q.DeliverTo[i] = peer.ID
		}
		q.ID, q.Subject, q.Target, q.Owner = identity.NewID(), subject.ID, n.Identity.ID, a.owner
		q.Expires = time.Now().Add(delegationIdleTimeout).UTC()
		grantCtx, cancel := context.WithCancel(n.ctx)
		s := &delegationState{grant: q, ctx: grantCtx, cancel: cancel, lastActivity: time.Now()}
		n.delegationMu.Lock()
		defer n.delegationMu.Unlock()
		if len(n.delegations) >= 4096 {
			cancel()
			return nil, errors.New("delegation limit reached")
		}
		n.delegations[q.ID] = s
		return q, nil
	}
	n.delegationMu.Lock()
	defer n.delegationMu.Unlock()
	if method == "access.list" {
		grants := []model.Delegation{}
		for _, s := range n.delegations {
			if s.grant.Owner == a.owner && n.delegationLiveLocked(s, time.Now()) {
				q := s.grant
				q.Expires = s.lastActivity.Add(delegationIdleTimeout).UTC()
				grants = append(grants, q)
			}
		}
		sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
		return grants, nil
	}
	var q struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &q); err != nil {
		return nil, err
	}
	s := n.delegations[q.ID]
	if s != nil && s.grant.Owner != a.owner {
		return nil, errors.New("delegation belongs to another orchestrator")
	}
	if s != nil {
		s.cancel()
		delete(n.delegations, q.ID)
	}
	return map[string]bool{"revoked": true}, nil
}

// Grant activity is driven by real stream IO, not discovery or keepalives.
type delegationConn struct {
	net.Conn
	node  *Node
	state *delegationState
}

func (c *delegationConn) touch(size int) {
	if size <= 0 {
		return
	}
	c.node.delegationMu.Lock()
	if c.state.ctx.Err() == nil {
		c.state.lastActivity = time.Now()
	}
	c.node.delegationMu.Unlock()
}
func (c *delegationConn) Read(b []byte) (int, error) {
	size, err := c.Conn.Read(b)
	c.touch(size)
	return size, err
}
func (c *delegationConn) Write(b []byte) (int, error) {
	size, err := c.Conn.Write(b)
	c.touch(size)
	return size, err
}

func (n *Node) peerCall(ctx context.Context, args json.RawMessage, _ Execution) (any, error) {
	var q struct {
		Target string          `json:"target"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Target == "" || q.Method == "" {
		return nil, errors.New("target and method are required")
	}
	if q.Method == "artifacts.open" || q.Method == "tcp.open" || q.Method == "tcp.listen" || q.Method == "clipboard.open" || q.Method == "clipboard.paste" {
		return nil, errors.New("peers.call supports JSON instructions, not raw streaming methods")
	}
	var result json.RawMessage
	err := n.Call(ctx, q.Target, q.Method, q.Params, &result)
	return result, err
}
