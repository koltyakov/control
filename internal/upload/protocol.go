// Package upload implements bounded, resumable, create-only file uploads.
package upload

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
)

const (
	Protocol              = "file-upload-v1"
	OpenMethod            = "files.upload.open"
	StatusMethod          = "files.upload.status"
	AbortMethod           = "files.upload.abort"
	BlockSize             = 256 << 10
	MetadataLimit         = 16 << 10
	MaxSize         int64 = 1 << 40
	CheckpointBytes int64 = 8 << 20
	IdleTimeout           = 60 * time.Second
	StagingDir            = ".control-uploads"
	StartRecord     byte  = 1
	DataRecord      byte  = 2
	FinishRecord    byte  = 3
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Spec struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Request struct {
	Spec
	Protocol string `json:"protocol"`
	Resume   bool   `json:"resume,omitempty"`
}

type Result struct {
	Spec
	NodeID       string    `json:"nodeId"`
	State        string    `json:"state"`
	Offset       int64     `json:"offset"`
	PrefixSHA256 string    `json:"prefixSHA256"`
	Active       bool      `json:"active,omitempty"`
	ResumedFrom  int64     `json:"resumedFrom,omitempty"`
	BytesSent    int64     `json:"bytesSent,omitempty"`
	Updated      time.Time `json:"updated"`
}

type Ready struct {
	Protocol   string `json:"protocol"`
	Generation string `json:"generation"`
	Result
}

type Start struct {
	Generation   string `json:"generation"`
	Offset       int64  `json:"offset"`
	PrefixSHA256 string `json:"prefixSHA256"`
}

type Progress struct {
	Result
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

func ValidID(id string) bool { return safeID.MatchString(id) }

func (m *Manager) Description() map[string]any {
	return map[string]any{"protocol": Protocol, "methods": []string{OpenMethod, StatusMethod, AbortMethod}, "maxBytes": MaxSize, "blockBytes": BlockSize, "maxActive": 8, "maxActivePerOwner": 2, "createOnly": true, "resume": true, "maxStagingBytes": m.limits.StagingBytes, "maxOwnerBytes": m.limits.OwnerBytes, "reserveBytes": m.limits.ReserveBytes, "retentionHours": int(m.limits.Retention / time.Hour), "maxRecords": 4096, "maxRecordsPerOwner": 1024, "maxPartials": 256, "maxPartialsPerOwner": 64}
}

// Validate requires portable slash-separated workspace paths on every OS.
func (s Spec) Validate() error {
	if !ValidID(s.ID) || s.Size < 0 || s.Size > MaxSize || !digest.MatchString(s.SHA256) {
		return Error("invalid_upload", "invalid upload ID, size, or SHA-256")
	}
	if s.Path == "" || len(s.Path) > 4096 || !utf8.ValidString(s.Path) || path.IsAbs(s.Path) || path.Clean(s.Path) != s.Path {
		return Error("invalid_upload", "destination must be a normalized workspace-relative file path")
	}
	for _, part := range strings.Split(s.Path, "/") {
		if !ValidName(part) || strings.EqualFold(part, StagingDir) || part == "." || part == ".." {
			return Error("invalid_upload", "invalid or reserved destination component")
		}
	}
	return nil
}

func ValidName(name string) bool {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, `/\:<>"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
		return false
	}
	chars := []rune(base)
	if len(chars) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return !strings.ContainsRune("123456789¹²³", chars[3])
	}
	return true
}

func Error(code, message string) error {
	return &model.RPCError{Code: code, Outcome: "unknown", Message: message}
}

// Records have a fixed five-byte header. The caller owns and reuses scratch;
// length is validated before reading or allocating any payload.
func ReadRecord(r io.Reader, scratch []byte) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	kind, size := header[0], binary.BigEndian.Uint32(header[1:])
	limit := uint32(MetadataLimit)
	if kind == DataRecord {
		limit = BlockSize + 8
	}
	if kind < StartRecord || kind > FinishRecord || size > limit || uint64(size) > uint64(len(scratch)) {
		return 0, nil, Error("invalid_upload_record", "invalid upload record type or size")
	}
	b := scratch[:size]
	_, err := io.ReadFull(r, b)
	return kind, b, err
}

func WriteRecord(w io.Writer, kind byte, b []byte) error {
	limit := MetadataLimit
	if kind == DataRecord {
		limit = BlockSize + 8
	}
	if kind < StartRecord || kind > FinishRecord || len(b) > limit {
		return Error("invalid_upload_record", "invalid outgoing upload record")
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(b)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return errors.New("invalid writer count")
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Hash hashes exactly size bytes from the reader's current position.
func Hash(ctx context.Context, r io.Reader, size int64) (string, error) {
	h := sha256.New()
	b := make([]byte, BlockSize)
	if err := Feed(ctx, r, size, h, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), ctx.Err()
}

// Feed extends a hash with exactly size bytes, using caller-owned storage.
func Feed(ctx context.Context, r io.Reader, size int64, h hash.Hash, b []byte) error {
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(r, b[:min(size, int64(len(b)))])
		if n > 0 {
			_, _ = h.Write(b[:n])
			size -= int64(n)
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
