package model

import (
	"errors"
	"time"
)

const (
	ConnectionTestMethod   = "connection.test"
	ConnectionOpenMethod   = "connection.open"
	ConnectionTestProtocol = "connection-test-v1"
	ConnectionTestTimeout  = 2 * time.Minute
)

// ConnectionTestOptions bounds synthetic traffic independently of network speed.
type ConnectionTestOptions struct {
	Bytes   int64 `json:"bytes"`
	Samples int   `json:"samples"`
}

func (q ConnectionTestOptions) Normalize() (ConnectionTestOptions, error) {
	if q.Bytes == 0 {
		q.Bytes = 16 << 20
	}
	if q.Samples == 0 {
		q.Samples = 10
	}
	if q.Bytes < 1 || q.Bytes > 256<<20 {
		return q, errors.New("connection test bytes must be 1..268435456 per direction")
	}
	if q.Samples < 1 || q.Samples > 100 {
		return q, errors.New("connection test samples must be 1..100")
	}
	return q, nil
}

type ConnectionTestRequest struct {
	Target string `json:"target"`
	ConnectionTestOptions
}

type ConnectionOpenRequest struct {
	Protocol string `json:"protocol"`
	ConnectionTestOptions
}

type ConnectionTestResult struct {
	Source      string               `json:"source"`
	SourceID    string               `json:"sourceId"`
	Target      string               `json:"target"`
	TargetID    string               `json:"targetId"`
	Transport   string               `json:"transport"`
	StartedAt   time.Time            `json:"startedAt"`
	SetupMillis float64              `json:"setupMillis"`
	Latency     ConnectionLatency    `json:"latency"`
	Upload      ConnectionThroughput `json:"upload"`
	Download    ConnectionThroughput `json:"download"`
}

type ConnectionLatency struct {
	Samples      int     `json:"samples"`
	MinMillis    float64 `json:"minMillis"`
	MeanMillis   float64 `json:"meanMillis"`
	MaxMillis    float64 `json:"maxMillis"`
	JitterMillis float64 `json:"jitterMillis"`
}

type ConnectionThroughput struct {
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Mbps    float64 `json:"mbps"`
}
