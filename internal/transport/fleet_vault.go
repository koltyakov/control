package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

// FleetVaultEnforced reports whether the last authenticated gateway advertised
// fleet vault checks. Receivers then refuse new peer work without a decision.
// It stays set while the gateway is unreachable, so an outage fails closed;
// only a later authentication to a gateway without the check clears it.
func (p *Peer) FleetVaultEnforced() bool { return p.fleetVaultCheck.Load() }

// CheckFleetVault asks the authenticated gateway for one decision. Transport
// and non-200 failures are errors; a decision is returned only from a complete
// JSON reply. The caller owns caching and fail-closed admission.
func (p *Peer) CheckFleetVault(ctx context.Context, q model.FleetVaultCheck) (model.FleetVaultDecision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL(p.cfg.Gateway, model.FleetVaultCheckPath), bytes.NewReader(model.JSON(q)))
	if err != nil {
		return model.FleetVaultDecision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.Route.Client(10*time.Second, false).Do(req)
	if err != nil {
		return model.FleetVaultDecision{}, gatewayNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return model.FleetVaultDecision{}, directoryStatusError("fleet vault check", resp)
	}
	var decision model.FleetVaultDecision
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&decision); err != nil {
		return model.FleetVaultDecision{}, err
	}
	return decision, nil
}
