// Package vaultwire implements a loopback approval broker and its client wire.
//
// A broker accepts pinned TLS connections on an externally provided loopback
// listener. Each physical connection is its own principal: its first attempted
// fleet operation returns approval_required with a short random code instead
// of dispatching or queueing the operation. A human then grants that code once
// with a key derived from the vault passphrase. The broker verifies an Ed25519
// signature over a single-use random challenge bound to the action, its exact
// parameters, the broker certificate pin, and the TLS session through keying
// material export. Neither the passphrase nor a reusable proof crosses the
// connection. Listing and revoking grants require the same proof.
//
// A grant lives only as long as its connection and its absolute TTL. It is
// never transferred to a reconnecting caller. Revocation or expiry cancels
// active operations and refuses new ones; resources a handler deliberately
// bound to its connection context, such as established forwards, remain until
// the caller disconnects.
//
// Caller labels are untrusted, sanitized metadata for display only. They do not
// identify a particular chat, agent, or process.
package vaultwire

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	protocolVersion = 1
	alpnProtocol    = "control-vault/1"
	exporterLabel   = "EXPORTER-control-vault-approval-v1"
	signatureDomain = "control-vault-approval-v1"
	serverName      = "control-vault"

	// DefaultGrantTTL is the absolute grant lifetime when Grant receives zero.
	DefaultGrantTTL = 24 * time.Hour
	// MaxGrantTTL bounds every grant regardless of the requested TTL.
	MaxGrantTTL = 24 * time.Hour
	// CodeTTL bounds how long a pending approval code can be granted.
	CodeTTL = 5 * time.Minute

	maxLabelRunes = 64
	maxOpBytes    = 128
	maxIDBytes    = 64
)

// Entry states.
const (
	StatePending = "pending"
	StateActive  = "active"
	StateNone    = "none"
	StateExpired = "expired"
	StateRevoked = "revoked"
	StateDenied  = "denied"
)

// Error codes returned by the broker.
const (
	CodeApprovalRequired = "approval_required"
	CodeApprovalDenied   = "approval_denied"
	CodeApprovalFailed   = "approval_failed"
	CodeGrantRevoked     = "grant_revoked"
	CodeGrantExpired     = "grant_expired"
	CodeRateLimited      = "rate_limited"
	CodeLimitExceeded    = "limit_exceeded"
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeOpFailed         = "op_failed"
	CodeCanceled         = "canceled"
	CodeResultTooLarge   = "result_too_large"
	CodeUnavailable      = "unavailable"
)

// Action names bound into approval signatures.
const (
	actionGrant  = "grant"
	actionList   = "list"
	actionRevoke = "revoke"
)

// Frame types.
const (
	frameHello     = "hello"
	framePing      = "ping"
	frameDo        = "do"
	frameCancel    = "cancel"
	frameStatus    = "status"
	frameRequest   = "request"
	frameChallenge = "challenge"
	frameAuthorize = "authorize"
	frameResult    = "result"
	frameError     = "error"
)

// Entry describes one connection principal as seen by the broker.
//
// ID identifies the connection and is the Revoke target. Code is set while an
// approval is pending. Label is sanitized caller metadata and is informational
// only. Expires is the code expiry while pending and the absolute grant expiry
// while active.
type Entry struct {
	ID      string    `json:"id"`
	Code    string    `json:"code,omitempty"`
	Label   string    `json:"label"`
	State   string    `json:"state"`
	Expires time.Time `json:"expires,omitzero"`
}

// Error is a broker error. Entry is set for authorization outcomes such as
// approval_required, which are never dispatched.
//
// Outcome is set on errors from a dispatched Do. OutcomeNotSent means the
// operation provably did not reach its backend: the handler reported a
// trusted not_sent outcome, or no handler was available. Every other Do error,
// including cancellation, grant revocation or expiry, disconnects, and
// oversized results, is OutcomeUnknown: the operation may have partly run, so
// callers must not retry it automatically.
type Error struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome,omitempty"`
	Message string `json:"message"`
	Entry   *Entry `json:"entry,omitempty"`
}

// Error outcomes.
const (
	OutcomeNotSent = "not_sent"
	OutcomeUnknown = "unknown"
)

func (e *Error) Error() string {
	if e.Entry != nil && e.Entry.Code != "" {
		return fmt.Sprintf("%s: %s (code %s, expires %s)", e.Code, e.Message, e.Entry.Code, e.Entry.Expires.UTC().Format(time.RFC3339))
	}
	return e.Code + ": " + e.Message
}

// ApprovalRequired reports whether err requests approval and returns the
// pending entry that carries the code.
func ApprovalRequired(err error) (Entry, bool) {
	var e *Error
	if errors.As(err, &e) && e.Code == CodeApprovalRequired && e.Entry != nil {
		return *e.Entry, true
	}
	return Entry{}, false
}

// ErrClosed reports use of a closed or broken session.
var ErrClosed = errors.New("vault session closed")

// Pin returns the certificate pin used by Dial: lowercase hex SHA-256 of the
// leaf certificate DER.
func Pin(certDER []byte) string {
	sum := sha256.Sum256(certDER)
	return hex.EncodeToString(sum[:])
}

type clientFrame struct {
	ID        uint64          `json:"id"`
	Type      string          `json:"type"`
	Version   int             `json:"version,omitempty"`
	Label     string          `json:"label,omitempty"`
	Op        string          `json:"op,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Action    string          `json:"action,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Challenge string          `json:"challenge,omitempty"`
	Signature []byte          `json:"signature,omitempty"`
}

type serverFrame struct {
	ID         uint64          `json:"id"`
	Type       string          `json:"type"`
	Version    int             `json:"version,omitempty"`
	Connection string          `json:"connection,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Entry      *Entry          `json:"entry,omitempty"`
	Entries    []Entry         `json:"entries,omitempty"`
	Challenge  *challengeData  `json:"challenge,omitempty"`
	Error      *Error          `json:"error,omitempty"`
}

// challengeData is what the approver signs. Params holds the exact bytes the
// broker bound, including details it resolved such as the target connection.
type challengeData struct {
	ID      string          `json:"id"`
	Action  string          `json:"action"`
	Params  json.RawMessage `json:"params"`
	Nonce   []byte          `json:"nonce"`
	Expires time.Time       `json:"expires"`
}

type grantParams struct {
	Code       string `json:"code"`
	Connection string `json:"connection,omitempty"`
	Label      string `json:"label,omitempty"`
	TTLMillis  int64  `json:"ttl_ms"`
}

type revokeParams struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// signedMessage is an unambiguous length-prefixed encoding. The broker pin and
// TLS exporter value come from each side's own view of the session, so a proof
// cannot be replayed to another broker or over another connection.
func signedMessage(pin string, binding []byte, c *challengeData) []byte {
	var b []byte
	put := func(v []byte) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = append(b, v...)
	}
	put([]byte(signatureDomain))
	put([]byte(pin))
	put(binding)
	put([]byte(c.ID))
	put(c.Nonce)
	put([]byte(c.Action))
	put(c.Params)
	b = binary.BigEndian.AppendUint64(b, uint64(c.Expires.UnixMilli()))
	return b
}

// SanitizeLabel makes caller-supplied text safe to display: it removes control,
// format, and bidirectional override characters, collapses whitespace, and
// truncates to 64 runes.
func SanitizeLabel(s string) string {
	var out strings.Builder
	runes, space := 0, false
	for i := 0; i < len(s) && runes < maxLabelRunes; {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size <= 1:
			continue
		case unicode.IsSpace(r):
			space = out.Len() > 0
			continue
		case !unicode.IsPrint(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Bidi_Control, r):
			continue
		}
		if space {
			out.WriteByte(' ')
			runes++
			space = false
			if runes >= maxLabelRunes {
				break
			}
		}
		out.WriteRune(r)
		runes++
	}
	if out.Len() == 0 {
		return "unnamed"
	}
	return strings.TrimSpace(out.String())
}

const codeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// normalizeCode accepts codes in any case, with or without separators.
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == '-' || r == ' ' {
			continue
		}
		if !strings.ContainsRune(codeAlphabet, r) || b.Len() >= 8 {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != 8 {
		return ""
	}
	v := b.String()
	return v[:4] + "-" + v[4:]
}

func validOp(op string) bool {
	if op == "" || len(op) > maxOpBytes {
		return false
	}
	for i := 0; i < len(op); i++ {
		if c := op[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// writeFrame writes one length-prefixed JSON frame in a single Write.
func writeFrame(w io.Writer, v any, limit int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > limit {
		return errFrameTooLarge
	}
	frame := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	frame = append(frame, body...)
	_, err = w.Write(frame)
	return err
}

var errFrameTooLarge = errors.New("vault frame exceeds limit")

// readFrame grows its buffer with received bytes rather than the claimed size.
func readFrame(r io.Reader, v any, limit int) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if uint64(n) > uint64(limit) {
		return errFrameTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if err != nil {
		return err
	}
	if len(b) != int(n) {
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(b, v)
}
