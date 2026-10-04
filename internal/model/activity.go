package model

import "time"

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
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	OS       string            `json:"os"`
	Labels   map[string]string `json:"labels,omitempty"`
	Online   bool              `json:"online"`
	LastSeen time.Time         `json:"lastSeen"`
	// Status is ready, offline, or unavailable. Unavailable includes denied
	// access, timeouts, and peers that do not implement activities.list.
	Status         string      `json:"status"`
	Error          string      `json:"error,omitempty"`
	ObservedAt     time.Time   `json:"observedAt,omitempty"`
	Active         []Activity  `json:"active"`
	Recent         []Activity  `json:"recent"`
	ActiveCount    int         `json:"activeCount"`
	Omitted        int         `json:"omitted,omitempty"`
	DirectSessions int         `json:"directSessions"`
	RelaySessions  int         `json:"relaySessions"`
	LeaseOwner     string      `json:"leaseOwner,omitempty"`
	LeaseExpires   time.Time   `json:"leaseExpires,omitempty"`
	System         *SystemInfo `json:"system,omitempty"`
}

type PoolActivitySnapshot struct {
	ObservedAt time.Time              `json:"observedAt"`
	Nodes      []NodeActivitySnapshot `json:"nodes"`
}
