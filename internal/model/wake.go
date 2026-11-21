package model

import "time"

// WakeInterface records an IPv4 interface address with prefix length and its MAC.
// Nodes report eligible interfaces automatically, independently of labels.
type WakeInterface struct {
	Address string `json:"address"`
	MAC     string `json:"mac"`
}

type WakeResult struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	Packets int    `json:"packets"`
}

const MaxLANPeers = 128
const LANPeerTTL = 7 * 24 * time.Hour

// LANPeer is gateway-owned evidence that ID broadcast to this node's Local
// interface from Remote. It authorizes sender selection, never execution.
type LANPeer struct {
	ID         string        `json:"id"`
	Local      WakeInterface `json:"local"`
	Remote     WakeInterface `json:"remote"`
	VerifiedAt time.Time     `json:"verifiedAt"`
}

// LANView is discovery input, not a directory for membership or execution.
// LANPeers contains only the receiver's own verified relationships.
type LANView struct {
	UserID   string    `json:"userId"`
	Receiver string    `json:"receiver"`
	Nodes    []LANNode `json:"nodes"`
	LANPeers []LANPeer `json:"lanPeers,omitempty"`
}

type LANNode struct {
	ID             string          `json:"id"`
	PublicKey      []byte          `json:"publicKey"`
	WakeInterfaces []WakeInterface `json:"wakeInterfaces"`
}

// LANProbeNode projects the online nodes eligible to participate in discovery.
func (n Node) LANProbeNode() (LANNode, bool) {
	if !n.Online || !n.LANDiscovery || len(n.WakeInterfaces) == 0 {
		return LANNode{}, false
	}
	return LANNode{ID: n.ID, PublicKey: n.PublicKey, WakeInterfaces: n.WakeInterfaces}, true
}

type LANProbe struct {
	Receiver    string        `json:"receiver"`
	Sender      string        `json:"sender"`
	ReceiverNIC WakeInterface `json:"receiverInterface"`
	SenderNIC   WakeInterface `json:"senderInterface"`
	Challenge   string        `json:"challenge"`
	Port        int           `json:"port"`
}

// LANProof is delivered only in a UDP broadcast, not in gateway signaling.
// The receiver must obtain the sender's signature before reporting the edge.
type LANProof struct {
	LANProbe
	Nonce     string    `json:"nonce"`
	IssuedAt  time.Time `json:"issuedAt"`
	Signature []byte    `json:"signature"`
}

func (p LANProof) Message() []byte {
	p.Signature = nil
	return append([]byte("control-lan-broadcast-v1\x00"), JSON(p)...)
}
