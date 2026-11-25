package client

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/upload"
	"github.com/koltyakov/control/internal/wire"
)

type UploadSpec struct {
	Node           string              `json:"node"`
	ID             string              `json:"id"`
	LocalPath      string              `json:"localPath"`
	Path           string              `json:"path,omitempty"`
	TimeoutSeconds int                 `json:"timeoutSeconds,omitempty"`
	Resume         bool                `json:"-"`
	Progress       func(upload.Result) `json:"-"`
}

type localUpload struct {
	upload.Spec
	NodeID    string `json:"nodeId"`
	Node      string `json:"node"`
	LocalPath string `json:"localPath"`
	// Published is when this client saw the upload complete; see
	// pruneUploadRecords.
	Published time.Time `json:"published,omitzero"`
}

func (c Client) uploadPeer(ctx context.Context) (*transport.Peer, error) {
	if c.routing == nil || !c.routing.cfg.AccountRouting {
		return nil, errors.New("uploads require account-client routing; omit --api and CONTROL_API")
	}
	return c.backend(ctx)
}

func (c Client) uploadRecordPath(id string) string {
	scope := identity.ID([]byte(strings.TrimRight(c.routing.cfg.Gateway.URL, "/") + "\x00" + c.routing.ownerID))
	// Hash IDs for case-insensitive filesystems and Windows device names.
	return filepath.Join(c.routing.cfg.StateDir, scope, "uploads", identity.ID([]byte(id))+".json")
}

func (c Client) uploadTarget(ctx context.Context, peer *transport.Peer, node, id string) (string, error) {
	var local localUpload
	err := store.Read(c.uploadRecordPath(id), &local)
	if err == nil {
		if local.ID != id || local.Validate() != nil || local.NodeID == "" {
			return "", errors.New("invalid local upload recovery record")
		}
		if node != local.Node && node != local.NodeID {
			destination, err := peer.Lookup(ctx, node)
			if err != nil || destination.ID != local.NodeID {
				return "", errors.New("upload belongs to a different worker identity")
			}
		}
		return local.NodeID, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	destination, err := peer.Lookup(ctx, node)
	return destination.ID, err
}

// Upload owns exactly one attempt. Cancellation retains the worker's durable
// prefix; only an explicit Resume invocation can open another attempt.
func (c Client) Upload(ctx context.Context, s UploadSpec) (upload.Result, error) {
	var result upload.Result
	if s.Node == "" || !upload.ValidID(s.ID) || s.LocalPath == "" {
		return result, errors.New("upload requires node, valid id, and localPath")
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 3600
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 86400 {
		return result, errors.New("upload timeoutSeconds must be 1..86400")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutSeconds)*time.Second)
	defer cancel()
	ctx, closeLifetime := c.lifetimeContext(ctx)
	defer closeLifetime()
	peer, err := c.uploadPeer(ctx)
	if err != nil {
		return result, err
	}
	if info, err := os.Lstat(s.LocalPath); err == nil && info.IsDir() {
		return result, errors.New("localPath is a directory; upload directory trees with the CLI: control upload start NODE LOCAL_DIR REMOTE_DIR --recursive")
	}
	if err = c.refuseDirectoryUploadID(s.ID); err != nil {
		return result, err
	}
	target, err := c.uploadTarget(ctx, peer, s.Node, s.ID)
	if err != nil {
		return result, err
	}
	if _, err = describeUploadTarget(ctx, peer, target, false); err != nil {
		return result, err
	}
	records := filepath.Dir(c.uploadRecordPath(s.ID))
	if err = os.MkdirAll(records, 0700); err != nil {
		return result, err
	}
	pruneUploadRecords(ctx, records, time.Now())
	return c.uploadFile(ctx, peer, target, s)
}

// uploadFile performs one attempt for one file after the caller has checked
// the target's upload protocol. Directory uploads call it once per file.
func (c Client) uploadFile(ctx context.Context, peer *transport.Peer, target string, s UploadSpec) (upload.Result, error) {
	var result upload.Result
	info, err := os.Lstat(s.LocalPath)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > upload.MaxSize {
		return result, errors.New("upload source must be a regular file, not a symlink, and at most 1 TiB")
	}
	f, err := os.Open(s.LocalPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return result, errors.New("upload source changed while opening")
	}
	sum, err := upload.Hash(ctx, f, info.Size())
	if err != nil {
		return result, err
	}
	if err = sourceUnchanged(f, info); err != nil {
		return result, err
	}
	rec := localUpload{Spec: upload.Spec{ID: s.ID, Path: s.Path, Size: info.Size(), SHA256: sum}, NodeID: target, Node: s.Node, LocalPath: s.LocalPath}
	var prior localUpload
	localPath := c.uploadRecordPath(s.ID)
	if err = os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
		return result, err
	}
	lock := flock.New(localPath + ".lock")
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return result, err
	}
	if !locked {
		return result, errors.New("cannot lock upload recovery record")
	}
	if err = store.Read(localPath, &prior); err == nil {
		if s.Resume {
			rec.Path = prior.Path
		}
		if prior.Spec != rec.Spec || prior.NodeID != rec.NodeID {
			return result, upload.Error("upload_spec_conflict", "local upload ID binds a different destination or content")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	} else if s.Resume {
		return result, errors.New("local upload recovery record is missing; inspect remote status rather than starting a replacement")
	}
	if err = rec.Validate(); err != nil {
		return result, err
	}
	if err = store.Write(localPath, rec); err != nil {
		return result, err
	}
	if err = lock.Unlock(); err != nil {
		return result, err
	}
	conn, data, err := openPeerRPC(ctx, peer, target, upload.OpenMethod, upload.Request{Spec: rec.Spec, Protocol: upload.Protocol, Resume: s.Resume})
	if err != nil {
		return result, fmt.Errorf("upload %s; query status before resuming: %w", s.ID, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	defer stop()
	var ready upload.Ready
	if err = json.Unmarshal(data, &ready); err != nil {
		return result, err
	}
	if ready.Protocol != upload.Protocol || ready.Spec != rec.Spec || ready.NodeID != target || ready.Offset < 0 || ready.Offset > rec.Size {
		return result, errors.New("upload acknowledgement metadata mismatch")
	}
	if ready.State == "published" {
		if ready.Offset != ready.Size || ready.PrefixSHA256 != ready.SHA256 {
			return result, errors.New("invalid upload completion receipt")
		}
		markUploadPublished(ctx, localPath, rec)
		return ready.Result, nil
	}
	if ready.State != "staging" || ready.Generation == "" {
		return result, errors.New("invalid upload acknowledgement state")
	}
	result, err = sendUpload(ctx, conn, f, info, ready, s.Progress)
	if err != nil {
		return result, fmt.Errorf("upload %s; query status before explicitly resuming: %w", s.ID, err)
	}
	if result.State == "published" {
		markUploadPublished(ctx, localPath, rec)
	}
	return result, nil
}

func sourceUnchanged(f *os.File, before os.FileInfo) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) || !os.SameFile(info, before) {
		return upload.Error("source_changed", "upload source changed; no publication was requested")
	}
	return nil
}

func sendUpload(ctx context.Context, conn net.Conn, f *os.File, info os.FileInfo, ready upload.Ready, progress func(upload.Result)) (upload.Result, error) {
	result := ready.Result
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	buf := make([]byte, upload.BlockSize+8)
	h := sha256.New()
	if err := upload.Feed(ctx, f, ready.Offset, h, buf[8:]); err != nil {
		return result, err
	}
	if hex.EncodeToString(h.Sum(nil)) != ready.PrefixSHA256 {
		return result, upload.Error("partial_mismatch", "local source does not match the verified worker prefix")
	}
	if err := upload.WriteRecord(conn, upload.StartRecord, model.JSON(upload.Start{Generation: ready.Generation, Offset: ready.Offset, PrefixSHA256: ready.PrefixSHA256})); err != nil {
		return result, err
	}
	// Reverse checkpoint traffic must drain while sending. Its result channel
	// holds one terminal value, not an accumulating progress queue.
	type completion struct {
		result upload.Result
		err    error
	}
	done := make(chan completion, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		last := ready.Result
		for {
			var p upload.Progress
			err := wire.ReadFrameLimit(conn, &p, upload.MetadataLimit)
			if err == nil && (p.Spec != ready.Spec || p.NodeID != ready.NodeID || p.Offset < last.Offset || p.Offset > ready.Size) {
				err = errors.New("invalid upload progress metadata")
			}
			if err == nil && p.Error == "" && p.State == "published" && (p.Offset != ready.Size || p.PrefixSHA256 != ready.SHA256) {
				err = errors.New("invalid upload completion receipt")
			}
			if err != nil {
				_ = conn.SetDeadline(time.Now())
				_ = conn.Close()
				done <- completion{last, err}
				return
			}
			last = p.Result
			if progress != nil {
				progress(last)
			}
			if p.Error != "" {
				_ = conn.SetDeadline(time.Now())
				_ = conn.Close()
				done <- completion{last, upload.Error(p.Code, p.Error)}
				return
			}
			if p.State == "published" {
				done <- completion{last, nil}
				return
			}
		}
	}()
	// sent counts payload bytes written to the stream by this attempt. After
	// an interruption the worker may hold fewer; its durable offset says so.
	var sent int64
	sendErr := func() error {
		for offset := ready.Offset; offset < ready.Size; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := int(min(int64(upload.BlockSize), ready.Size-offset))
			if _, err := io.ReadFull(f, buf[8:8+n]); err != nil {
				return err
			}
			_, _ = h.Write(buf[8 : 8+n])
			binary.BigEndian.PutUint64(buf[:8], uint64(offset))
			_ = conn.SetWriteDeadline(time.Now().Add(upload.IdleTimeout))
			if err := upload.WriteRecord(conn, upload.DataRecord, buf[:8+n]); err != nil {
				return err
			}
			sent += int64(n)
			offset += int64(n)
		}
		if err := sourceUnchanged(f, info); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != ready.SHA256 {
			return upload.Error("source_changed", "transmitted source digest changed; no publication was requested")
		}
		return upload.WriteRecord(conn, upload.FinishRecord, nil)
	}()
	if sendErr != nil {
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	}
	completed := <-done
	<-exited
	// Failed attempts report what they sent, so an interrupted directory
	// transfer accounts for its partial files too.
	result = completed.result
	result.ResumedFrom = ready.Offset
	result.BytesSent = sent
	if sendErr != nil {
		var remote *model.RPCError
		if errors.As(completed.err, &remote) {
			return result, completed.err
		}
		return result, sendErr
	}
	return result, completed.err
}

func (c Client) UploadStatus(ctx context.Context, node, id string, abort bool) (upload.Result, error) {
	var result upload.Result
	if node == "" || !upload.ValidID(id) {
		return result, errors.New("upload status/abort requires node and valid id")
	}
	peer, err := c.uploadPeer(ctx)
	if err != nil {
		return result, err
	}
	target, err := c.uploadTarget(ctx, peer, node, id)
	if err != nil {
		return result, err
	}
	method := upload.StatusMethod
	if abort {
		method = upload.AbortMethod
	}
	err = peerCall(ctx, peer, target, method, map[string]string{"id": id}, &result)
	if err == nil && (result.ID != id || result.NodeID != target) {
		err = errors.New("upload status identity mismatch")
	}
	return result, err
}

// uploadRecordRetention keeps a published upload's recovery record long enough
// to reconcile a lost final response with an explicit resume.
const uploadRecordRetention = 7 * 24 * time.Hour

// markUploadPublished records completion so pruneUploadRecords can later
// remove the record. A record replaced meanwhile by another attempt for
// different content is left unchanged.
func markUploadPublished(ctx context.Context, path string, published localUpload) {
	lock := flock.New(path + ".lock")
	defer func() { _ = lock.Close() }()
	if locked, err := lock.TryLockContext(ctx, 50*time.Millisecond); err != nil || !locked {
		return
	}
	var current localUpload
	if store.Read(path, &current) == nil && current.Spec == published.Spec && current.NodeID == published.NodeID && current.Published.IsZero() {
		current.Published = time.Now().UTC()
		_ = store.Write(path, current)
	}
}

// pruneUploadRecords removes recovery records of uploads published before the
// retention period, and lock files left without a record. Unpublished records
// are kept: they are the only evidence for a safe explicit resume.
func pruneUploadRecords(ctx context.Context, dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		name := entry.Name()
		if !entry.Type().IsRegular() {
			continue
		}
		if record, ok := strings.CutSuffix(name, ".lock"); ok {
			// A fresh lock may belong to an attempt about to write its record.
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) < time.Hour {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, record)); errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		var record localUpload
		if store.Read(path, &record) != nil || record.Published.IsZero() || now.Sub(record.Published) < uploadRecordRetention {
			continue
		}
		lock := flock.New(path + ".lock")
		if locked, err := lock.TryLock(); err == nil && locked {
			_ = os.Remove(path)
			_ = lock.Unlock()
		}
		_ = lock.Close()
		_ = os.Remove(path + ".lock")
	}
}
