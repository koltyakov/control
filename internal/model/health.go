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
}
