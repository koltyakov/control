package client

import (
	"maps"
	"sync"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// maxNameHints bounds the persisted routing-name map for one gateway account.
const maxNameHints = 4096

// nameHints remembers the identity last listed for each routing name, so a
// new CLI or MCP process can resolve a name with one scoped gateway record
// instead of the whole fleet directory. It stores only names and public
// identity IDs. The transport accepts a hint only when the gateway's current
// record for that identity still carries the requested name.
type nameHints struct {
	path   string
	mu     sync.Mutex
	loaded bool
	ids    map[string]string
}

func (h *nameHints) loadLocked() {
	if h.loaded {
		return
	}
	h.loaded = true
	var ids map[string]string
	if store.Read(h.path, &ids) == nil {
		h.ids = ids
	}
}

func (h *nameHints) Hint(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	return h.ids[name]
}

func (h *nameHints) Remember(nodes []model.Node) {
	next := make(map[string]string, min(len(nodes), maxNameHints))
	for _, node := range nodes {
		if len(next) >= maxNameHints {
			break
		}
		// Keep the directory's first match, which is what lookup selects.
		if _, seen := next[node.Name]; !seen && node.Name != "" && len(node.ID) == 64 {
			next[node.Name] = node.ID
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	if maps.Equal(next, h.ids) {
		return
	}
	h.ids = next
	// Hints are an optimization; a failed write only costs a directory read.
	_ = store.Write(h.path, next)
}
