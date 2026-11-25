package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
	"github.com/koltyakov/control/internal/wire"
)

// OpenRPC sends one request. The returned stream may contain a bulk payload.
// It never replays a request after writing application bytes.
func (p *Peer) OpenRPC(ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, error) {
	conn, result, _, err := p.OpenRPCWithReceipt(ctx, target, method, params)
	return conn, result, err
}

// OpenRPCWithReceipt also returns a stream-bound task acceptance receipt, when
// supported. Lost or legacy replies never imply that authority was unused.
func (p *Peer) OpenRPCWithReceipt(ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, *model.TaskSubmissionReceipt, error) {
	if len(model.Delegations(ctx)) != 0 {
		peer, err := p.Lookup(ctx, target)
		if err != nil {
			return nil, nil, nil, err
		}
		target = peer.ID
	}
	lane := ControlLane
	if method == "artifacts.open" || method == "clipboard.open" || method == "clipboard.paste" || method == model.ConnectionOpenMethod || method == upload.OpenMethod {
		lane = BulkLane
	}
	if method == "tcp.open" || method == "tcp.listen" {
		lane = InteractiveLane
	}
	raw := model.JSON(params)
	if method == "tasks.logs" || method == "tasks.get" {
		// Long-held requests stay off the control lane's stream budget.
		var q struct {
			Follow      bool `json:"follow"`
			WaitSeconds int  `json:"waitSeconds"`
		}
		if json.Unmarshal(raw, &q) == nil && (q.Follow || q.WaitSeconds > 0) {
			lane = InteractiveLane
		}
	}
	conn, err := p.OpenLane(ctx, target, lane)
	if err != nil {
		return nil, nil, nil, model.OperationError(err, "not_sent")
	}
	stop := context.AfterFunc(ctx, func() {
		// yamux Close sends FIN but can leave reads waiting for the peer.
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
	defer stop()
	deadline, _ := ctx.Deadline()
	request := model.Request{Version: model.Version, Method: method, Params: raw, Deadline: deadline}
	if method == "tasks.start" || method == "peers.call" {
		request.SubmissionID = identity.NewID()
	}
	for _, grant := range model.Delegations(ctx) {
		// Only the coordinator needs instructions issued for its own
		// downstream calls. A leaf validates the selected ID in its own store.
		if grant.Subject == target {
			request.Delegations = append(request.Delegations, grant)
		}
		// Locally reconstructed artifact access IDs can omit Subject; the
		// destination still verifies the bound subject, expiry and instruction.
		if request.Delegation == "" && (grant.Subject == "" || grant.Subject == p.IdentityID()) && (grant.Target == target || grant.Alias == target) && grant.Matches(method, request.Params) {
			request.Delegation = grant.ID
		}
	}
	if err = wire.WriteFrame(conn, request); err != nil {
		_ = conn.Close()
		outcome := "unknown"
		if errors.Is(err, wire.ErrFrameTooLarge) {
			outcome = "not_sent"
		}
		return nil, nil, nil, model.OperationError(err, outcome)
	}
	var response model.Response
	if err = wire.ReadFrame(conn, &response); err != nil {
		_ = conn.Close()
		return nil, nil, nil, model.OperationError(err, "unknown")
	}
	receipt := response.Submission
	if receipt != nil && (request.SubmissionID == "" || receipt.ID != request.SubmissionID ||
		(receipt.Status != "accepted" && receipt.Status != "existing" && receipt.Status != "rejected")) {
		receipt = nil
	}
	if response.Error != "" {
		_ = conn.Close()
		return nil, nil, receipt, response.Err()
	}
	return conn, response.Result, receipt, nil
}

// OpenTCP negotiates duplex records. Older receivers return a raw stream.
func (p *Peer) OpenTCP(ctx context.Context, target, address string) (net.Conn, error) {
	conn, result, err := p.OpenRPC(ctx, target, "tcp.open", map[string]any{"address": address, "duplex": DuplexVersion})
	if err != nil {
		return nil, err
	}
	var ack struct {
		Duplex int `json:"duplex"`
	}
	if err := json.Unmarshal(result, &ack); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack.Duplex == DuplexVersion {
		return NewDuplexConn(conn), nil
	}
	if ack.Duplex != 0 {
		_ = conn.Close()
		return nil, errors.New("unsupported TCP duplex version")
	}
	return conn, nil
}

// Select uses the same admission filters for node and standalone callers.
// Among matching machines it prefers the one reporting the fewest active
// operations, choosing randomly among ties or when load is not visible.
// GPU constraints are confirmed in the result for callers that may reach an
// older selector.
func (p *Peer) Select(ctx context.Context, args json.RawMessage) (model.NodeSelection, error) {
	query, candidates, err := p.selectCandidates(ctx, args)
	if err != nil {
		return model.NodeSelection{}, err
	}
	selected, err := resourcePlacement(candidates, p.placementHealth(ctx), query, time.Now())
	if err != nil {
		return model.NodeSelection{}, err
	}
	return model.NodeSelection{Node: selected, GPUConstraints: query.GPUConstrained()}, nil
}

// leastLoaded treats load as a placement preference, not a reservation;
// admission remains the destination's decision.
func leastLoaded(candidates []model.Node, loads map[string]int) model.Node {
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	load := func(n model.Node) int {
		if count, ok := loads[n.ID]; ok {
			return count
		}
		return math.MaxInt
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if load(candidate) < load(best) {
			best = candidate
		}
	}
	return best
}

// activeCounts reads the owner-visible health summaries the gateway already
// caches. Credentials without summary access, or any failure, yield no counts.
func (p *Peer) placementHealth(ctx context.Context) map[string]model.NodeActivitySnapshot {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/status"), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := p.cfg.Route.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var status model.PoolActivitySnapshot
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&status) != nil {
		return nil
	}
	counts := map[string]model.NodeActivitySnapshot{}
	for _, n := range status.Nodes {
		if n.Status == "update" || (!n.ObservedAt.IsZero() && n.ActiveCount >= 0) {
			counts[n.ID] = n
		}
	}
	return counts
}
