package model

import (
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
)

// Activity contains operational metadata, never command arguments, prompts,
// environment variables, credentials, results, or log contents.
type Activity struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Operation     string    `json:"operation"`
	Owner         string    `json:"owner"`
	Peer          string    `json:"peer,omitempty"`
	TaskID        string    `json:"taskId,omitempty"`
	State         string    `json:"state"`
	Phase         string    `json:"phase,omitempty"`
	Started       time.Time `json:"started"`
	Updated       time.Time `json:"updated"`
	Finished      time.Time `json:"finished,omitempty"`
	BytesDone     int64     `json:"bytesDone,omitempty"`
	TotalBytes    int64     `json:"totalBytes,omitempty"`
	BytesSent     int64     `json:"bytesSent,omitempty"`
	BytesReceived int64     `json:"bytesReceived,omitempty"`
}

type ActivityQuery struct {
	Recent int `json:"recent,omitempty"`
}

type PoolActivityQuery struct {
	Nodes  []string `json:"nodes,omitempty"`
	Recent int      `json:"recent,omitempty"`
}

type NodeActivitySnapshot struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	OS             string            `json:"os"`
	Labels         map[string]string `json:"labels,omitempty"`
	Online         bool              `json:"online"`
	LastSeen       time.Time         `json:"lastSeen"`
	Software       buildinfo.Info    `json:"software"`
	Disabled       bool              `json:"disabled,omitempty"`
	ControlPending bool              `json:"controlPending,omitempty"`
	// Status is ready for peer activity, summary for fresh owner health,
	// online for directory-only presence, update during managed updates,
	// offline, or unavailable.
	Status          string        `json:"status"`
	Error           string        `json:"error,omitempty"`
	ObservedAt      time.Time     `json:"observedAt,omitempty"`
	Active          []Activity    `json:"active"`
	Recent          []Activity    `json:"recent"`
	ActiveCount     int           `json:"activeCount"`
	Omitted         int           `json:"omitted,omitempty"`
	DirectSessions  int           `json:"directSessions"`
	RelaySessions   int           `json:"relaySessions"`
	Tunnels         *TunnelCounts `json:"tunnels,omitempty"`
	RetainedTunnels *TunnelCounts `json:"retainedTunnels,omitempty"`
	LeaseOwner      string        `json:"leaseOwner,omitempty"`
	LeaseExpires    time.Time     `json:"leaseExpires,omitempty"`
	Leased          bool          `json:"leased,omitempty"`
	System          *SystemInfo   `json:"system,omitempty"`
}

// TunnelCounts separates forward connections from reverse listeners for live
// activity, or forward/reverse definitions when used for RetainedTunnels.
type TunnelCounts struct {
	Forward int `json:"forward"`
	Reverse int `json:"reverse"`
}

// ActiveTunnelCounts excludes recent completions and non-tunnel operations.
func ActiveTunnelCounts(active []Activity) *TunnelCounts {
	counts := &TunnelCounts{}
	for _, a := range active {
		if a.Kind != "tunnel" {
			continue
		}
		switch a.Operation {
		case "tcp.open", "tcp.accept":
			counts.Forward++
		case "tcp.listen":
			counts.Reverse++
		}
	}
	return counts
}

type PoolActivitySnapshot struct {
	ObservedAt time.Time              `json:"observedAt"`
	Nodes      []NodeActivitySnapshot `json:"nodes"`
	Gateway    *GatewaySnapshot       `json:"gateway,omitempty"`
	Notice     string                 `json:"notice,omitempty"`
}

// GatewaySnapshot describes the gateway service, independently of fleet nodes.
type GatewaySnapshot struct {
	URL       string         `json:"url"`
	Software  buildinfo.Info `json:"software"`
	StartedAt time.Time      `json:"startedAt"`
	System    *SystemInfo    `json:"system,omitempty"`
}
