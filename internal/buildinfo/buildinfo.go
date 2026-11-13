// Package buildinfo identifies the running binary, independently of protocol version.
package buildinfo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/koltyakov/control/internal/peimage"
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
	Runtime     string `json:"runtime,omitempty"`
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
			GatewayProtocol: 1, NodeGatewayMin: 0, NodeGatewayMax: 1, Runtime: runtimeMode()}
		if sha, err := ExecutableSHA256(); err == nil {
			current.SHA256 = sha
		}
	})
	return current
}

func runtimeMode() string {
	if len(os.Args) > 1 && (os.Args[1] == "__managed" || os.Args[1] == "__user" || os.Args[1] == "__user-launch") {
		return "supervised"
	}
	return "single"
}

// ExecutableSHA256 hashes actual on-disk bytes, including a derived GUI image.
// Use it for copying this executable, not the normalized managed software ID.
func ExecutableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// UseWindowsGUISource reports the original release checksum for a console-free
// runtime only after proving that its entire image differs solely in Subsystem.
// Call before Current during direct Windows User startup. Ordinary version
// inspection continues to report the actual executable's checksum.
func UseWindowsGUISource(source string) error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, (128<<20)+1))
		if err == nil && len(data) > 128<<20 {
			err = errors.New("GUI runtime exceeds the executable size limit")
		}
		return data, err
	}
	image, err := read(source)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(image)
	if err = peimage.SetGUI(image); err != nil {
		return err
	}
	running, err := read(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(image, running) {
		return errors.New("GUI runtime does not match its original executable")
	}
	digest := hex.EncodeToString(sum[:])
	once.Do(func() {
		current = Info{Version: Version, ReleaseRepo: ReleaseRepo, BuildTime: BuildTime, OS: runtime.GOOS, Arch: runtime.GOARCH,
			GatewayProtocol: 1, NodeGatewayMin: 0, NodeGatewayMax: 1, SHA256: digest, Runtime: "single"}
	})
	if current.SHA256 != digest {
		return errors.New("software identity initialized before GUI runtime validation")
	}
	return nil
}
