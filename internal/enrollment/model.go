// Package enrollment defines single-machine installation invitations.
package enrollment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/update"
)

type Request struct {
	Name       string `json:"name"`
	OS         string `json:"os"`
	Arch       string `json:"arch,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
	Gateway    string `json:"gateway"`
}

type Invitation struct {
	UserID     string         `json:"userId,omitempty"`
	ID         string         `json:"id"`
	Name       string         `json:"name"`
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
	}{"control-enrollment-v1", Hash(ticket), r.CredentialHash, r.PublicKey, r.Arch})
	return b
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
