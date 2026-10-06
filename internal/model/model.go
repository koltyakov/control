package model

import (
	"encoding/json"
	"github.com/koltyakov/control/internal/buildinfo"
	"time"
)

const Version = "1"

type Capability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Node struct {
	UserID         string            `json:"userId,omitempty"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	PublicKey      []byte            `json:"publicKey"`
	OS             string            `json:"os"`
	Labels         map[string]string `json:"labels,omitempty"`
	Capabilities   []Capability      `json:"capabilities"`
	Online         bool              `json:"online"`
	LastSeen       time.Time         `json:"lastSeen"`
	System         *SystemInfo       `json:"system,omitempty"`
	Software       buildinfo.Info    `json:"software"`
	Disabled       bool              `json:"disabled,omitempty"`
	ControlPending bool              `json:"controlPending,omitempty"`
	Managed        bool              `json:"managed,omitempty"`
	ClientSessions bool              `json:"clientSessions,omitempty"`
	ClientOwners   bool              `json:"clientOwners,omitempty"`
	ClientOwner    string            `json:"clientOwner,omitempty"`
	PeerChannels   bool              `json:"peerChannels,omitempty"`
	// ExecutionAuthority is assigned by the gateway, never by enrollment metadata.
	ExecutionAuthority    bool `json:"executionAuthority,omitempty"`
	InstructionDelegation bool `json:"instructionDelegation,omitempty"`
}

type Artifact struct {
	Node    string    `json:"node"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
	Grant   string    `json:"grant,omitempty"`
}

type Input struct {
	Artifact Artifact `json:"artifact"`
	Path     string   `json:"path"`
}

type TaskSpec struct {
	ID             string          `json:"id,omitempty"`
	Capability     string          `json:"capability"`
	Args           json.RawMessage `json:"args"`
	Inputs         []Input         `json:"inputs,omitempty"`
	Outputs        []string        `json:"outputs,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
	LeaseID        string          `json:"leaseId,omitempty"`
}

type Lease struct {
	ID      string    `json:"id"`
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

type PeerSession struct {
	ID       string `json:"id"`
	Peer     string `json:"peer"`
	Lane     string `json:"lane"`
	Mode     string `json:"mode"`
	Outgoing int    `json:"outgoingStreams"`
	Incoming int    `json:"incomingStreams"`
}

type TaskLogChunk struct {
	Data     []byte `json:"data,omitempty"`
	Offset   int64  `json:"offset"`
	Terminal bool   `json:"terminal"`
	Error    string `json:"error,omitempty"`
}

type Task struct {
	ID        string          `json:"id"`
	Owner     string          `json:"owner"`
	Spec      TaskSpec        `json:"spec"`
	State     string          `json:"state"`
	Phase     string          `json:"phase,omitempty"`
	Created   time.Time       `json:"created"`
	Updated   time.Time       `json:"updated"`
	Result    json.RawMessage `json:"result,omitempty"`
	Artifacts []Artifact      `json:"artifacts,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (t Task) Terminal() bool {
	return t.State == "succeeded" || t.State == "failed" || t.State == "cancelled" || t.State == "interrupted"
}

type Request struct {
	Version string          `json:"version"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Deadline is enforced by the receiving node, including through a relay.
	Deadline    time.Time    `json:"deadline,omitempty"`
	Delegation  string       `json:"delegation,omitempty"`
	Delegations []Delegation `json:"delegations,omitempty"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func JSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
