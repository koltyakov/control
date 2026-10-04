// Package update implements verified binary staging and managed rollout messages.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
)

const GatewaySender = "@gateway"
const MaxBinarySize int64 = 128 << 20

type Asset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	Assets    []Asset   `json:"assets"`
}

type Deployment struct {
	ID           string    `json:"id"`
	Manifest     Manifest  `json:"manifest"`
	Source       string    `json:"source"`
	Phase        string    `json:"phase"`
	Error        string    `json:"error,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Participants []string  `json:"participants,omitempty"`
}

type Command struct {
	ID         string    `json:"id"`
	Version    string    `json:"version,omitempty"`
	Asset      Asset     `json:"asset,omitempty"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
}

type Status struct {
	Software   buildinfo.Info `json:"software"`
	ID         string         `json:"id,omitempty"`
	State      string         `json:"state"`
	Busy       bool           `json:"busy"`
	Paused     bool           `json:"paused"`
	LeaseUntil time.Time      `json:"leaseUntil,omitempty"`
	Error      string         `json:"error,omitempty"`
	SeenAt     time.Time      `json:"seenAt"`
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var versionPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,127}$`)

func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

func (m Manifest) Validate() error {
	if !versionPattern.MatchString(m.Version) {
		return errors.New("invalid update version")
	}
	if len(m.Assets) == 0 || len(m.Assets) > 6 {
		return errors.New("manifest requires 1..6 platform assets")
	}
	seen := map[string]bool{}
	for _, a := range m.Assets {
		if a.OS != "windows" && a.OS != "linux" && a.OS != "darwin" {
			return fmt.Errorf("unsupported OS %q", a.OS)
		}
		if a.Arch != "amd64" && a.Arch != "arm64" {
			return fmt.Errorf("unsupported architecture %q", a.Arch)
		}
		if a.Size <= 0 || a.Size > MaxBinarySize || !ValidDigest(a.SHA256) {
			return errors.New("invalid asset size or checksum")
		}
		if a.File != AssetName(a.OS, a.Arch) {
			return fmt.Errorf("expected asset filename %s", AssetName(a.OS, a.Arch))
		}
		key := a.OS + "/" + a.Arch
		if seen[key] {
			return fmt.Errorf("duplicate platform %s", key)
		}
		seen[key] = true
	}
	return nil
}

func AssetName(os, arch string) string {
	name := "control_" + os + "_" + arch
	if os == "windows" {
		name += ".exe"
	}
	return name
}

func (m Manifest) Asset(os, arch string) (Asset, bool) {
	for _, a := range m.Assets {
		if a.OS == os && a.Arch == arch {
			return a, true
		}
	}
	return Asset{}, false
}

func (m Manifest) ID() string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Matches(info buildinfo.Info, asset Asset) bool {
	return info.OS == asset.OS && info.Arch == asset.Arch && info.SHA256 == asset.SHA256
}

func ShortError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 512 {
		s = strings.ToValidUTF8(s[:512], "")
	}
	return s
}
