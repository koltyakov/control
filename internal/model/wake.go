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
