package model

import "time"

const NodeHealthInterval = 5 * time.Second
const NodeHealthTTL = 20 * time.Second
const MaxNodeHealthBytes = 16 << 10

// NodeHealth reports aggregate machine health to the fleet owner. It contains
// no task records, identities, arguments, credentials, or local filesystem paths.
type NodeHealth struct {
	ActiveCount int        `json:"activeCount"`
	Leased      bool       `json:"leased"`
	System      SystemInfo `json:"system"`
	// Aggregate counts and times only. Older nodes omit them.
	Work             *WorkCounts            `json:"work,omitempty"`
	Tunnels          *TunnelCounts          `json:"tunnels,omitempty"`
	ConnectionRoutes *ConnectionRouteCounts `json:"connectionRoutes,omitempty"`
	LeaseExpires     time.Time              `json:"leaseExpires,omitzero"`
	LastActive       time.Time              `json:"lastActive,omitzero"`
}

// WorkCounts splits a machine's active operations by kind. Tunnels are
// counted separately by TunnelCounts.
type WorkCounts struct {
	Tasks     int `json:"tasks"`
	Exec      int `json:"exec"`
	Transfers int `json:"transfers"`
	Other     int `json:"other"`
}

// Valid rejects negative counts from a malformed report.
func (h NodeHealth) Valid() bool {
	counts := []int{h.ActiveCount}
	if h.Work != nil {
		counts = append(counts, h.Work.Tasks, h.Work.Exec, h.Work.Transfers, h.Work.Other)
	}
	if h.Tunnels != nil {
		counts = append(counts, h.Tunnels.Forward, h.Tunnels.Reverse)
	}
	if r := h.ConnectionRoutes; r != nil {
		counts = append(counts, r.Direct, r.TURN, r.Relay, r.Unknown)
	}
	for _, count := range counts {
		if count < 0 {
			return false
		}
	}
	return true
}
