package model

import (
	"errors"
	"slices"
)

// FleetVaultCheckPath is the authenticated gateway route a receiver asks
// before admitting new fleet peer work once the gateway advertises
// fleetVaultCheck in GET /v1/auth.
const FleetVaultCheckPath = "/v1/fleet/vault/check"

// Error codes for refusals before any effect. Both use OutcomeNotAccepted.
const (
	// FleetVaultDenied means the gateway decided the request is not allowed.
	FleetVaultDenied = "fleet_vault_denied"
	// FleetVaultUnavailable means the receiver could not obtain a decision and
	// refused the new request rather than admitting it unchecked.
	FleetVaultUnavailable = "fleet_vault_unavailable"
)

// FleetVaultCheck asks whether one principal may use every listed worker
// together. A single allowance must cover the whole set; receivers never
// combine separate answers for subsets.
//
// Issuer is the authenticated transport identity of an account client the
// receiver observed directly. KeyID and OwnerID are the stable principal a
// prior gateway decision returned for that issuer, recorded by the receiver
// in a grant it owns. A request names Issuer or KeyID/OwnerID, never values
// supplied by a caller. A request with neither is a worker calling another
// worker directly with no account issuer, such as discovery.
type FleetVaultCheck struct {
	Issuer     string   `json:"issuer,omitempty"`
	KeyID      string   `json:"keyId,omitempty"`
	OwnerID    string   `json:"ownerId,omitempty"`
	MachineIDs []string `json:"machineIds"`
}

// FleetVaultDecision is the gateway's answer. KeyID and OwnerID identify the
// stable principal behind an issuer for later receiver-owned rechecks.
type FleetVaultDecision struct {
	Allowed bool   `json:"allowed"`
	KeyID   string `json:"keyId,omitempty"`
	OwnerID string `json:"ownerId,omitempty"`
}

// Normalize returns a copy with sorted, unique, non-empty machine IDs.
func (q FleetVaultCheck) Normalize() FleetVaultCheck {
	ids := make([]string, 0, len(q.MachineIDs))
	for _, id := range q.MachineIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	q.MachineIDs = slices.Compact(ids)
	return q
}

// Validate rejects ambiguous principals and empty machine sets.
func (q FleetVaultCheck) Validate() error {
	if len(q.MachineIDs) == 0 {
		return errors.New("fleet vault check requires at least one machine")
	}
	if q.Issuer != "" && (q.KeyID != "" || q.OwnerID != "") {
		return errors.New("fleet vault check names either an issuer or a stable key and owner")
	}
	return nil
}
