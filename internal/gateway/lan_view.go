package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

const LANViewSessionHeader = "Control-Lan-Session"

// LANViewSession binds HTTP LAN reads to the signed registration on the live
// gateway socket. Shared fleet credentials alone do not identify a worker.
func LANViewSession(signature []byte) string {
	sum := sha256.Sum256(append([]byte("control-lan-view-v1\x00"), signature...))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (g *Gateway) lanView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := g.authenticate(bearer(r))
	if p.Role == "" {
		g.counters.authFailures.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	c := g.route(id, p.UserID)
	if c == nil || c.keyID != p.KeyID || c.lanViewSession == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get(LANViewSessionHeader)), []byte(c.lanViewSession)) != 1 {
		http.NotFound(w, r)
		return
	}
	view, etag, unchanged := g.conditionalLANView(p.UserID, id, r.Header.Get("If-None-Match"))
	if etag == "" || g.route(id, p.UserID) != c {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization, "+LANViewSessionHeader+", Accept-Encoding")
	writeConditionalMetadataJSON(w, r, view, etag, unchanged)
}

// Only discovery inputs change the topology revision. Proofs are versioned
// separately for each receiver. Caller holds observationMu for writing.
func publishLANObservation(fleet *observedFleet, previous observation, next *observation) {
	if node, ok := next.node.LANProbeNode(); ok {
		next.lanNodeHash = sha256.Sum256(model.JSON(node))
	}
	if previous.lanNodeHash != next.lanNodeHash {
		fleet.lanTopology = identity.NewID()
	}
	if len(next.node.LANPeers) != 0 {
		next.lanProofHash = sha256.Sum256(model.JSON(next.node.LANPeers))
	}
	next.lanProofRev = previous.lanProofRev
	if next.lanProofRev == "" || previous.lanProofHash != next.lanProofHash {
		next.lanProofRev = identity.NewID()
	}
}

func (g *Gateway) conditionalLANView(user, id, condition string) (model.LANView, string, bool) {
	g.observationMu.RLock()
	defer g.observationMu.RUnlock()
	fleet := g.observed[user]
	if fleet == nil {
		return model.LANView{}, "", false
	}
	receiver, ok := fleet.nodes[id]
	if !ok {
		return model.LANView{}, "", false
	}
	etag := `"lan-` + fleet.lanTopology + `-` + receiver.lanProofRev + `"`
	if matchesDirectoryETag(condition, etag) {
		return model.LANView{}, etag, true
	}
	view := model.LANView{UserID: user, Receiver: id, Nodes: []model.LANNode{}, LANPeers: receiver.node.LANPeers}
	for _, entry := range fleet.nodes {
		if node, ok := entry.node.LANProbeNode(); ok {
			view.Nodes = append(view.Nodes, node)
		}
	}
	// Keep response order stable without coupling discovery to machine names.
	slices.SortFunc(view.Nodes, func(a, b model.LANNode) int { return strings.Compare(a.ID, b.ID) })
	return view, etag, false
}
