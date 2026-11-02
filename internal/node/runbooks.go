package node

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/koltyakov/control/internal/runbooks"
)

func (n *Node) runbooks(ctx context.Context, method string, args json.RawMessage) (any, error) {
	var query runbooks.Query
	if err := json.Unmarshal(args, &query); err != nil {
		return nil, err
	}
	if query.Node != "" || query.Local {
		return nil, errors.New("worker runbooks are scoped to this node; node and local are client-only fields")
	}
	if method == "runbooks.read" && (query.File == "" || query.Source == "") {
		return nil, errors.New("runbooks.read requires source and file")
	}
	if method == "runbooks.list" && query.File != "" {
		return nil, errors.New("use runbooks.read to read a complete file")
	}
	roots := n.runbookRoots
	if roots == nil {
		roots = runbooks.WorkerRoots()
	}
	// Use the gateway-owned current alias, not the installation's original name.
	self, err := n.Peer.Lookup(ctx, n.Identity.ID)
	if err != nil {
		return nil, err
	}
	return runbooks.Discover(ctx, roots, runbooks.Selector{Node: self.Name, ID: n.Identity.ID}, query)
}
