package model

// ConcurrencyPool reports admission, not CPU usage or a capacity reservation.
type ConcurrencyPool struct {
	Limit int `json:"limit"`
	InUse int `json:"inUse"`
	// Older workers report limit/inUse without queue counts. Nil means unknown.
	Queued *int `json:"queued,omitempty"`
}

type NodeConcurrency struct {
	Execution  ConcurrencyPool `json:"execution"`
	Operations ConcurrencyPool `json:"operations"`
	// Older workers have no separate desktop admission or desktop report.
	Desktop *ConcurrencyPool `json:"desktop,omitempty"`
}
