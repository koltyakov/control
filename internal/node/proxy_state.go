package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// syncProxyState applies the gateway's durable decision about which managed
// listeners must run. A failed read keeps the current listeners: an outage is
// never treated as the absence of dependencies. A gateway without managed
// proxy support leaves the last persisted decision unchanged.
func (n *Node) syncProxyState(ctx context.Context) error {
	if n.managedProxy == nil {
		return nil
	}
	state, supported, err := readProxyState(ctx, n.Config.Gateway, n.route, n.Identity)
	if err != nil {
		return err
	}
	if n.managedProxy.setSupported(supported) {
		n.proxyChecks.recheck()
	}
	if supported {
		return n.managedProxy.apply(state.Listeners, true)
	}
	return nil
}

// readProxyState reads only the signing worker's own desired listeners. The
// bool reports gateway support.
func readProxyState(ctx context.Context, base string, route gatewayroute.Route, id *identity.Identity) (model.ProxyState, bool, error) {
	proof := model.ProxyStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC()}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/node/proxy", bytes.NewReader(model.JSON(proof)))
	if err != nil {
		return model.ProxyState{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := route.Client(0, false).Do(req)
	if err != nil {
		return model.ProxyState{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return model.ProxyState{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return model.ProxyState{}, true, fmt.Errorf("managed proxy state returned %s", resp.Status)
	}
	var state model.ProxyState
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&state); err != nil {
		return model.ProxyState{}, true, err
	}
	if len(state.Listeners) > model.MaxProxyEndpoints {
		return model.ProxyState{}, true, fmt.Errorf("managed proxy state lists too many listeners")
	}
	return state, true, nil
}
