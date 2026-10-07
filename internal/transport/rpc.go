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
	"slices"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

// OpenRPC sends one request. The returned stream may contain a bulk payload.
// It never replays a request after writing application bytes.
func (p *Peer) OpenRPC(ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, error) {
	if len(model.Delegations(ctx)) != 0 {
		peer, err := p.Lookup(ctx, target)
		if err != nil {
			return nil, nil, err
		}
		target = peer.ID
	}
	lane := ControlLane
	if method == "artifacts.open" || method == "clipboard.open" || method == "clipboard.paste" {
		lane = BulkLane
	}
	if method == "tcp.open" || method == "tcp.listen" {
		lane = InteractiveLane
	}
	if method == "tasks.logs" || method == "tasks.get" {
		// Long-held requests stay off the control lane's stream budget.
		var q struct {
			Follow      bool `json:"follow"`
			WaitSeconds int  `json:"waitSeconds"`
		}
		if json.Unmarshal(model.JSON(params), &q) == nil && (q.Follow || q.WaitSeconds > 0) {
			lane = InteractiveLane
		}
	}
	conn, err := p.OpenLane(ctx, target, lane)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() {
		// yamux Close sends FIN but can leave reads waiting for the peer.
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
	defer stop()
	deadline, _ := ctx.Deadline()
	request := model.Request{Version: model.Version, Method: method, Params: model.JSON(params), Deadline: deadline, Delegations: model.Delegations(ctx)}
	for _, grant := range request.Delegations {
		if (grant.Target == target || grant.Alias == target) && grant.Matches(method, request.Params) {
			request.Delegation = grant.ID
			break
		}
	}
	if err = wire.WriteFrame(conn, request); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	var response model.Response
	if err = wire.ReadFrame(conn, &response); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if response.Error != "" {
		_ = conn.Close()
		return nil, nil, errors.New(response.Error)
	}
	return conn, response.Result, nil
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
func (p *Peer) Select(ctx context.Context, args json.RawMessage) (model.Node, error) {
	var query struct {
		Labels     map[string]string `json:"labels"`
		Capability string            `json:"capability"`
	}
	if err := json.Unmarshal(args, &query); err != nil {
		return model.Node{}, err
	}
	nodes, err := p.Nodes(ctx)
	if err != nil {
		return model.Node{}, err
	}
	candidates := []model.Node{}
	for _, peer := range nodes {
		if !peer.Online || peer.Disabled || peer.ControlPending {
			continue
		}
		matches := true
		for k, v := range query.Labels {
			if peer.Labels[k] != v {
				matches = false
				break
			}
		}
		if matches && query.Capability != "" {
			matches = slices.ContainsFunc(peer.Capabilities, func(c model.Capability) bool { return c.Name == query.Capability })
		}
		if matches {
			candidates = append(candidates, peer)
		}
	}
	if len(candidates) == 0 {
		return model.Node{}, errors.New("no online enabled node matches the selector")
	}
	return leastLoaded(candidates, p.activeCounts(ctx)), nil
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
func (p *Peer) activeCounts(ctx context.Context) map[string]int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/status"), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var status model.PoolActivitySnapshot
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&status) != nil {
		return nil
	}
	counts := map[string]int{}
	for _, n := range status.Nodes {
		if !n.ObservedAt.IsZero() && n.ActiveCount >= 0 {
			counts[n.ID] = n.ActiveCount
		}
	}
	return counts
}
