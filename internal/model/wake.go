package model

// WakeInterface records an IPv4 interface address with prefix length and its MAC.
// It is advertised only by nodes explicitly assigned a labels.lan identifier.
type WakeInterface struct {
	Address string `json:"address"`
	MAC     string `json:"mac"`
}

type WakeResult struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	Packets int    `json:"packets"`
}
