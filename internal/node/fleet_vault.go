package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Every new peer RPC or stream admission asks the gateway live, so a policy
// change, revocation or allowance deadline applies to the first later
// admission, including on warm sessions. Decisions are never reused across
// requests, and an unavailable gateway refuses new work. Cost is bounded by the
// route's pooled HTTP connections rather than by reusing permission. Within
// one admission, nested checks of the same proven principal and machine set
// share one answer; see fleetVaultMemo.
const fleetVaultTimeout = 10 * time.Second

// fleetVaultBinding is the receiver-owned principal for one admission or
// grant. It is built from the authenticated caller and gateway decisions,
// never from request fields, and is immutable once a grant records it.
type fleetVaultBinding struct {
	// issuer is the authenticated account-client transport identity.
	issuer string
	// keyID and ownerID are the stable principal from the gateway decision.
	keyID, ownerID string
	// machines is the normalized set of every worker a grant implicates.
	machines []string
	// unchecked means the gateway did not advertise checks when this was
	// recorded. Enforcement refuses it later instead of guessing a principal.
	unchecked bool
}

func (b *fleetVaultBinding) request(machines []string) model.FleetVaultCheck {
	if b.keyID != "" || b.ownerID != "" {
		return model.FleetVaultCheck{KeyID: b.keyID, OwnerID: b.ownerID, MachineIDs: machines}
	}
	return model.FleetVaultCheck{Issuer: b.issuer, MachineIDs: machines}
}

// derive keeps this principal for a narrower set, such as artifact access
// derived from a task grant.
func (b *fleetVaultBinding) derive(machines []string) *fleetVaultBinding {
	return &fleetVaultBinding{issuer: b.issuer, keyID: b.keyID, ownerID: b.ownerID, machines: machines, unchecked: b.unchecked}
}

type fleetVaultChecks struct {
	// enforced and check default to the peer transport; tests replace them
	// before Start.
	enforced func() bool
	check    func(context.Context, model.FleetVaultCheck) (model.FleetVaultDecision, error)
}

// fleetVaultMemo holds the decisions made while admitting one request. Each
// acceptAuthority call installs a fresh memo, so neither another request nor
// a nested delegated acceptance can reuse it. Durable tasks do not inherit it.
type fleetVaultMemo struct {
	mu        sync.Mutex
	decisions map[string]model.FleetVaultDecision
}

type fleetVaultMemoKey struct{}

func withFleetVaultMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, fleetVaultMemoKey{}, &fleetVaultMemo{decisions: map[string]model.FleetVaultDecision{}})
}

func fleetVaultMemoKeyFor(q model.FleetVaultCheck) string { return string(model.JSON(q)) }

func (n *Node) fleetVaultEnforced() bool {
	return n.fleetVault.enforced != nil && n.fleetVault.enforced()
}

func fleetVaultDenied() error {
	return &model.RPCError{Code: model.FleetVaultDenied, Outcome: model.OutcomeNotAccepted, Message: "fleet vault policy does not allow this request on every implicated machine"}
}

func fleetVaultUnavailable(err error) error {
	return &model.RPCError{Code: model.FleetVaultUnavailable, Outcome: model.OutcomeNotAccepted, Message: "fleet vault decision unavailable; new peer work was refused before admission: " + err.Error(), Cause: err}
}

// decideFleetVault asks the gateway for q, bounded by the caller's context
// and fleetVaultTimeout. Only the current admission's memo can answer instead.
func (n *Node) decideFleetVault(ctx context.Context, q model.FleetVaultCheck) (model.FleetVaultDecision, error) {
	q = q.Normalize()
	if err := q.Validate(); err != nil {
		return model.FleetVaultDecision{}, fleetVaultUnavailable(err)
	}
	memo, _ := ctx.Value(fleetVaultMemoKey{}).(*fleetVaultMemo)
	key := fleetVaultMemoKeyFor(q)
	if memo != nil {
		memo.mu.Lock()
		decision, ok := memo.decisions[key]
		memo.mu.Unlock()
		if ok {
			return decision, nil
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, fleetVaultTimeout)
	decision, err := n.fleetVault.check(checkCtx, q)
	cancel()
	stable := q.KeyID != "" || q.OwnerID != ""
	if err == nil && stable && (decision.KeyID != "" && decision.KeyID != q.KeyID || decision.OwnerID != "" && decision.OwnerID != q.OwnerID) {
		// A stable recheck must not rebind a grant to another principal.
		err = errors.New("gateway returned a different principal for a stable recheck")
	}
	if err != nil {
		if ctx.Err() != nil {
			return model.FleetVaultDecision{}, ctx.Err()
		}
		return model.FleetVaultDecision{}, fleetVaultUnavailable(err)
	}
	if memo != nil {
		memo.mu.Lock()
		memo.decisions[key] = decision
		if !stable && (decision.KeyID != "" || decision.OwnerID != "") {
			// The gateway proved this issuer's stable principal for this set
			// within the same admission.
			memo.decisions[fleetVaultMemoKeyFor(model.FleetVaultCheck{KeyID: decision.KeyID, OwnerID: decision.OwnerID, MachineIDs: q.MachineIDs})] = decision
		}
		memo.mu.Unlock()
	}
	return decision, nil
}

// admitFleetVault checks b for this destination and machines, and for an
// issuer records the stable principal the gateway returned. Callers read
// enforcement once, compute machines under it, and call this only when it is
// enforced; otherwise they mark the binding unchecked. It must run after local
// authorization and before any effect.
func (n *Node) admitFleetVault(ctx context.Context, b *fleetVaultBinding, machines []string) error {
	if b.unchecked {
		return fleetVaultUnavailable(errors.New("authority was recorded before the gateway enforced fleet vault checks; request a new grant"))
	}
	stable := b.keyID != "" || b.ownerID != ""
	if !stable && b.issuer == "" {
		return fleetVaultUnavailable(errors.New("no authenticated account issuer"))
	}
	q := b.request(append([]string{n.Identity.ID}, machines...)).Normalize()
	decision, err := n.decideFleetVault(ctx, q)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fleetVaultDenied()
	}
	if !stable {
		b.keyID, b.ownerID = decision.KeyID, decision.OwnerID
	}
	b.machines = q.MachineIDs
	return nil
}

// recheckFleetVault repeats a grant's recorded decision without changing it.
func (n *Node) recheckFleetVault(ctx context.Context, b *fleetVaultBinding) error {
	if !n.fleetVaultEnforced() {
		return nil
	}
	if b == nil || b.unchecked {
		return fleetVaultUnavailable(errors.New("delegation was issued before the gateway enforced fleet vault checks; request a new grant"))
	}
	decision, err := n.decideFleetVault(ctx, b.request(b.machines))
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fleetVaultDenied()
	}
	return nil
}

// admitFleetVaultDiscovery gates a direct discovery request. An account
// client is the issuer; a worker calling another worker has no issuer and
// both machines must be covered by the gateway's answer.
func (n *Node) admitFleetVaultDiscovery(ctx context.Context, caller string) error {
	if caller == n.Identity.ID || !n.fleetVaultEnforced() {
		return nil
	}
	peer, err := n.Peer.Lookup(ctx, caller)
	if err != nil {
		return fleetVaultUnavailable(err)
	}
	q := model.FleetVaultCheck{MachineIDs: []string{n.Identity.ID}}
	switch {
	case peer.ExecutionAuthority:
		q.Issuer = caller
	case peer.ClientOwner == "":
		q.MachineIDs = append(q.MachineIDs, caller)
	}
	decision, err := n.decideFleetVault(ctx, q)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fleetVaultDenied()
	}
	return nil
}

// fleetVaultMachine resolves a worker an instruction names. Account clients
// are principals rather than machines and are omitted.
func (n *Node) fleetVaultMachine(ctx context.Context, name string) (string, error) {
	if name == "" || name == n.Identity.ID || name == n.Config.Name {
		return "", nil
	}
	peer, err := n.Peer.Lookup(ctx, name)
	if err != nil {
		return "", fmt.Errorf("resolve implicated machine %q: %w", name, err)
	}
	if peer.ClientOwner != "" || peer.ExecutionAuthority {
		return "", nil
	}
	return peer.ID, nil
}

// fleetVaultNesting bounds how deeply nested instructions are inspected.
// Nested coordination is rejected at destinations, so real plans stay shallow.
const fleetVaultNesting = 4

// fleetVaultImplicated lists every other worker an instruction would reach,
// directly or through instructions it carries: artifact sources and
// recipients, peer-call, connection-test and wake targets, and every workflow
// step's target and inputs. They are checked together with this destination
// before coordinator admission, so separate allowances for subsets never
// combine. Each later destination still checks its own admission. Including a
// requested name only narrows an allowance. Malformed parameters add nothing;
// their handlers reject them.
func (n *Node) fleetVaultImplicated(ctx context.Context, method string, params json.RawMessage) ([]string, error) {
	names, err := fleetVaultParticipants(method, params, 0)
	if err != nil {
		return nil, err
	}
	machines := make([]string, 0, len(names))
	for _, name := range names {
		id, err := n.fleetVaultMachine(ctx, name)
		if err != nil {
			return nil, err
		}
		if id != "" {
			machines = append(machines, id)
		}
	}
	return machines, nil
}

func fleetVaultParticipants(method string, params json.RawMessage, depth int) ([]string, error) {
	if depth > fleetVaultNesting {
		return nil, errors.New("instruction nesting is too deep for a fleet vault check")
	}
	var names []string
	nested := func(method string, params json.RawMessage) error {
		more, err := fleetVaultParticipants(method, params, depth+1)
		names = append(names, more...)
		return err
	}
	switch method {
	case "tasks.start", model.TaskCheckMethod:
		var spec model.TaskSpec
		if json.Unmarshal(params, &spec) != nil {
			return nil, nil
		}
		for _, input := range spec.Inputs {
			names = append(names, input.Artifact.Node)
		}
		if err := nested(spec.Capability, spec.Args); err != nil {
			return nil, err
		}
	case "peers.call":
		var q model.APICall
		if json.Unmarshal(params, &q) != nil {
			return nil, nil
		}
		names = append(names, q.Target)
		if err := nested(q.Method, q.Params); err != nil {
			return nil, err
		}
	case "workflow.run":
		var plan model.WorkflowPlan
		if json.Unmarshal(params, &plan) != nil {
			return nil, nil
		}
		if plan.HasPlacement() {
			return nil, model.ErrUnresolvedPlacement
		}
		for _, step := range plan.Steps {
			names = append(names, step.Target)
			if err := nested("tasks.start", model.JSON(step.Task)); err != nil {
				return nil, err
			}
		}
	case model.ConnectionTestMethod, "artifacts.grant", "artifacts.deliver", "network.wake":
		var q struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(params, &q)
		names = append(names, q.Target)
	case "artifacts.pull":
		var q struct {
			Artifact model.Artifact `json:"artifact"`
		}
		_ = json.Unmarshal(params, &q)
		names = append(names, q.Artifact.Node)
	}
	return names, nil
}

// bindFleetVaultTarget confirms that a worker resolved at effect time, such as
// a wake target read from the directory, is covered by the current peer
// admission. A name can resolve differently than when the request or task
// was admitted, so an ID outside the admitted set is checked live with the
// same principal. Without peer authority (the local API or a gateway-relayed
// wake) or gateway support, existing boundaries apply unchanged.
func (n *Node) bindFleetVaultTarget(ctx context.Context, id string) error {
	parent := fleetVaultBindingFor(ctx)
	if parent == nil || id == "" || id == n.Identity.ID || !n.fleetVaultEnforced() {
		return nil
	}
	if !parent.unchecked && slices.Contains(parent.machines, id) {
		return nil
	}
	return n.admitFleetVault(ctx, parent.derive(nil), []string{id})
}

// fleetVaultBindingFor returns the principal of the current authority.
func fleetVaultBindingFor(ctx context.Context) *fleetVaultBinding {
	a, ok := ctx.Value(authorityContextKey{}).(authority)
	if !ok {
		return nil
	}
	if a.grant != nil {
		return a.grant.vault
	}
	return a.vault
}
