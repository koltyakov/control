// Package enrollment defines single-machine installation invitations.
package enrollment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/update"
)

type Request struct {
	Name       string `json:"name"`
	AutoName   bool   `json:"autoName,omitempty"`
	OS         string `json:"os"`
	Arch       string `json:"arch,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
	Gateway    string `json:"gateway"`
}

type Invitation struct {
	UserID     string         `json:"userId,omitempty"`
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	AutoName   bool           `json:"autoName,omitempty"` // Hostname selected and signed by the target installer.
	Gateway    string         `json:"gateway"`
	Asset      update.Asset   `json:"asset"`            // Selected binary; first candidate until architecture selection.
	Assets     []update.Asset `json:"assets,omitempty"` // Pinned candidates for architecture-detecting installers.
	Version    string         `json:"version"`
	ExpiresAt  time.Time      `json:"expiresAt"`
	CreatedAt  time.Time      `json:"createdAt"`
	RedeemedID string         `json:"redeemedId,omitempty"`
	ReplaceID  string         `json:"replaceId,omitempty"` // Existing identity reserved by a same-name invitation.
	Revoked    bool           `json:"revoked"`
}

type Link struct {
	Invitation
	URL     string `json:"url"`
	Command string `json:"command"`
}

type Redemption struct {
	Arch           string `json:"arch,omitempty"`
	Name           string `json:"name,omitempty"`
	PublicKey      []byte `json:"publicKey"`
	CredentialHash string `json:"credentialHash"`
	Signature      []byte `json:"signature"`
}

func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func Message(ticket string, r Redemption) []byte {
	b, _ := json.Marshal(struct {
		Purpose, Ticket, Credential string
		PublicKey                   []byte
		Arch                        string `json:"arch,omitempty"`
		Name                        string `json:"name,omitempty"`
	}{"control-enrollment-v1", Hash(ticket), r.CredentialHash, r.PublicKey, r.Arch, r.Name})
	return b
}

var machineName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// ResolveName runs on the target. The resolved name is persisted with the
// pending enrollment so a retry cannot rename the machine if its hostname changes.
func (i Invitation) ResolveName(hostname string) (string, error) {
	name := i.Name
	if i.AutoName {
		name = hostname
	}
	if !machineName.MatchString(name) {
		return "", fmt.Errorf("machine name %q must be 1..63 letters, digits, dots, underscores, or hyphens and start with a letter or digit; use an explicit invitation name if the hostname is invalid", name)
	}
	return name, nil
}

func (i Invitation) Candidates() []update.Asset {
	if len(i.Assets) > 0 {
		return i.Assets
	}
	return []update.Asset{i.Asset}
}

func (i Invitation) Select(arch string) (update.Asset, bool) {
	for _, asset := range i.Candidates() {
		if asset.Arch == arch {
			return asset, true
		}
	}
	return update.Asset{}, false
}

func GatewayURL(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("gateway must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func ShellQuote(s string) string      { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func PowerShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
