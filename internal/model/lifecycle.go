package model

import "time"

// MachineState is gateway-owned policy. Revisions are monotonic per identity.
type MachineState struct {
	Revision     uint64 `json:"revision"`
	Disabled     bool   `json:"disabled"`
	Unregistered bool   `json:"unregistered"`
	Name         string `json:"name,omitempty"` // Owner-selected routing name, overriding node configuration.
}

// MachineStateProof permits a node to learn that it was unregistered even
// after its enrollment credential has been revoked. It authorizes no mutation.
type MachineStateProof struct {
	PublicKey   []byte    `json:"publicKey"`
	SignedAt    time.Time `json:"signedAt"`
	Revision    uint64    `json:"revision"`
	Signature   []byte    `json:"signature"`
	InspectOnly bool      `json:"inspectOnly,omitempty"`
}

func (p MachineStateProof) Message() []byte {
	return JSON(struct {
		Purpose     string
		PublicKey   []byte
		SignedAt    time.Time
		Revision    uint64
		InspectOnly bool `json:"inspectOnly,omitempty"`
	}{"control-machine-state-v1", p.PublicKey, p.SignedAt, p.Revision, p.InspectOnly})
}
