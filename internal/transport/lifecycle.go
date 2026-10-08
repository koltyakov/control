package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func (p *Peer) MachineState(ctx context.Context, revision uint64) (model.MachineState, bool, error) {
	return ReadMachineState(ctx, p.cfg.Gateway, p.cfg.Identity, revision, false)
}

// ReadMachineState reads only the signing identity's policy, even after its bearer
// credential has been revoked. The bool reports gateway lifecycle support.
func ReadMachineState(ctx context.Context, base string, id *identity.Identity, revision uint64, inspectOnly bool) (model.MachineState, bool, error) {
	proof := model.MachineStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC(), Revision: revision, InspectOnly: inspectOnly}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/node/state", bytes.NewReader(model.JSON(proof)))
	if err != nil {
		return model.MachineState{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return model.MachineState{}, false, gatewayNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return model.MachineState{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return model.MachineState{}, true, gatewayResponseError(resp.StatusCode, "machine state")
	}
	var state model.MachineState
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&state)
	return state, true, gatewayNetworkError(err)
}
