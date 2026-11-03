// Package buildinfo identifies the running binary, independently of protocol version.
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"
)

// Build metadata is set by the release/development builder using -ldflags.
var Version = "dev"
var ReleaseRepo = "koltyakov/control"
var BuildTime string

type Info struct {
	Version     string `json:"version"`
	ReleaseRepo string `json:"releaseRepo,omitempty"`
	BuildTime   string `json:"buildTime,omitempty"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	SHA256      string `json:"sha256,omitempty"`
	// Epoch zero denotes gateways predating explicit compatibility declarations.
	GatewayProtocol int `json:"gatewayProtocol,omitempty"`
	NodeGatewayMin  int `json:"nodeGatewayMin,omitempty"`
	NodeGatewayMax  int `json:"nodeGatewayMax,omitempty"`
}

// SupportsGateway is an explicit executable declaration, not a version guess.
// An absent range must retain gateway-first update ordering.
func (i Info) SupportsGateway(epoch int) bool {
	return epoch >= 0 && i.NodeGatewayMin >= 0 && i.NodeGatewayMax > 0 &&
		i.NodeGatewayMin <= epoch && epoch <= i.NodeGatewayMax
}

var once sync.Once
var current Info

func Current() Info {
	once.Do(func() {
		current = Info{Version: Version, ReleaseRepo: ReleaseRepo, BuildTime: BuildTime, OS: runtime.GOOS, Arch: runtime.GOARCH,
			GatewayProtocol: 1, NodeGatewayMin: 0, NodeGatewayMax: 1}
		path, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err == nil {
			current.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
	})
	return current
}
