package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"
)

const RemoteProtocol = "control-secrets-v1"
const MaxRemoteMessage = 12 << 10

// Envelope carries only ciphertext. Its key is the sender's ephemeral P-256
// public key, not the worker's private decryption key.
type Envelope struct {
	ID         string `json:"id"`
	ExpiresAt  int64  `json:"expiresAt"`
	Key        []byte `json:"key"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type ManagementRequest struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Name      string    `json:"name,omitempty"`
	ExpiresAt int64     `json:"expiresAt"`
	Envelope  *Envelope `json:"envelope,omitempty"`
}

// ManagementReply has no value field or arbitrary diagnostic text.
type ManagementReply struct {
	ID      string     `json:"id"`
	Error   string     `json:"error,omitempty"`
	Secrets []Metadata `json:"secrets,omitempty"`
}

type RemoteList struct {
	Key     []byte     `json:"key"`
	Secrets []Metadata `json:"secrets"`
}

func NewManagementKey() (*ecdh.PrivateKey, error) { return ecdh.P256().GenerateKey(rand.Reader) }

func ValidManagementKey(key []byte) bool {
	_, err := ecdh.P256().NewPublicKey(key)
	return err == nil
}

func ValidRequestID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && len(id) == 32
}

func ValidDeadline(expires int64, now time.Time) bool {
	return expires > now.Unix() && expires <= now.Add(time.Minute).Unix()
}

func (e Envelope) Valid(now time.Time) bool {
	return ValidRequestID(e.ID) && ValidDeadline(e.ExpiresAt, now) && ValidManagementKey(e.Key) &&
		len(e.Nonce) == 12 && len(e.Ciphertext) >= 17 && len(e.Ciphertext) <= MaxValue+16
}

func (q ManagementRequest) Valid(now time.Time) bool {
	if !ValidRequestID(q.ID) || !ValidDeadline(q.ExpiresAt, now) {
		return false
	}
	switch q.Operation {
	case "list":
		return q.Name == "" && q.Envelope == nil
	case "delete":
		return ValidName(q.Name) && q.Envelope == nil
	case "set":
		return ValidName(q.Name) && q.Envelope != nil && q.Envelope.ID == q.ID && q.Envelope.ExpiresAt == q.ExpiresAt && q.Envelope.Valid(now)
	}
	return false
}

func envelopeAAD(node, name string, e Envelope) []byte {
	return []byte(RemoteProtocol + "\x00" + node + "\x00" + name + "\x00" + e.ID + "\x00" + strconv.FormatInt(e.ExpiresAt, 10))
}

func managementCipher(private *ecdh.PrivateKey, public []byte, node string) (cipher.AEAD, error) {
	key, err := ecdh.P256().NewPublicKey(public)
	if err != nil {
		return nil, errors.New("invalid secret management key")
	}
	shared, err := private.ECDH(key)
	if err != nil {
		return nil, errors.New("cannot derive secret management key")
	}
	derived, err := hkdf.Key(sha256.New, shared, nil, RemoteProtocol+"\x00"+node, 32)
	clear(shared)
	if err != nil {
		return nil, errors.New("cannot derive secret management key")
	}
	block, err := aes.NewCipher(derived)
	clear(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Seal(node, name, value string, public []byte) (Envelope, error) {
	if node == "" || !ValidName(name) || !validValue(value) {
		return Envelope{}, errors.New("secret requires a valid name and 1..4096 UTF-8 bytes without NUL")
	}
	private, err := NewManagementKey()
	if err != nil {
		return Envelope{}, err
	}
	aead, err := managementCipher(private, public, node)
	if err != nil {
		return Envelope{}, err
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	e := Envelope{ID: hex.EncodeToString(id[:]), ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Key: private.PublicKey().Bytes(), Nonce: make([]byte, aead.NonceSize())}
	_, _ = rand.Read(e.Nonce)
	e.Ciphertext = aead.Seal(nil, e.Nonce, []byte(value), envelopeAAD(node, name, e))
	return e, nil
}

func Open(private *ecdh.PrivateKey, node, name string, e Envelope) (string, error) {
	if private == nil || !ValidName(name) || !e.Valid(time.Now()) {
		return "", errors.New("invalid or expired secret envelope")
	}
	aead, err := managementCipher(private, e.Key, node)
	if err != nil {
		return "", errors.New("invalid secret envelope")
	}
	value, err := aead.Open(nil, e.Nonce, e.Ciphertext, envelopeAAD(node, name, e))
	if err != nil {
		return "", errors.New("invalid secret envelope")
	}
	defer clear(value)
	if !validValue(string(value)) {
		return "", errors.New("invalid secret value")
	}
	return string(value), nil
}

// SecureURL permits authenticated HTTPS or direct loopback development only.
// Remote HTTP permits key substitution even when secret values are encrypted.
func SecureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}
