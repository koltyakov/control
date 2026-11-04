package upload

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

type receiver struct {
	m          *Manager
	r          record
	parent     *os.Root
	f          *os.File
	h          hash.Hash
	buf        []byte
	position   int64
	checked    int64
	checkpoint time.Time
}

// Serve performs one attempt. No connection or mutation is replayed. The node
// keeps authorization and maintenance admission alive for the entire call.
func (m *Manager) Serve(ctx context.Context, owner string, conn net.Conn, q Request, beforePublish func() error, progress func(int64)) (err error) {
	acknowledged := false
	var current Result
	defer func() {
		if err == nil {
			return
		}
		if !acknowledged {
			_ = wire.WriteFrame(conn, model.ErrorResponse(err))
			return
		}
		e := model.OperationError(err, "unknown")
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = wire.WriteFrame(conn, Progress{Result: current, Error: e.Message, Code: e.Code})
	}()
	if q.Protocol != Protocol {
		return Error("upload_unsupported", "unsupported file upload protocol")
	}
	if err = q.Validate(); err != nil {
		return err
	}
	if err = m.CheckPath(filepath.FromSlash(q.Path)); err != nil {
		return err
	}
	// Validate the parent before reserving durable capacity. It must exist.
	parent, err := m.root.OpenRoot(filepath.FromSlash(path.Dir(q.Path)))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	r, release, err := m.reserve(owner, q.Spec, q.Resume)
	if err != nil {
		return err
	}
	defer release()
	if err = m.reconcile(ctx, &r); err != nil {
		return err
	}
	current = r.Result
	if r.State == "published" {
		return wire.WriteFrame(conn, model.Response{Result: model.JSON(Ready{Protocol: Protocol, Result: r.Result})})
	}
	if r.State != "staging" {
		return Error("upload_terminal", "upload was aborted or expired; it cannot be resumed")
	}
	if time.Since(r.Updated) > m.limits.Retention {
		return Error("upload_expired", "upload partial has expired")
	}
	if _, err = parent.Lstat(path.Base(q.Path)); !errors.Is(err, os.ErrNotExist) {
		return Error("destination_exists", "destination exists or cannot be inspected; uploads never overwrite")
	}
	if err = m.space(r, 0); err != nil {
		return err
	}
	if err = parent.Mkdir(StagingDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	stageInfo, err := parent.Lstat(StagingDir)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return Error("partial_corrupt", "upload staging directory is not a private directory")
	}
	metadata := filepath.Join(m.dir, key(owner, q.ID)+".json")
	_, statErr := os.Stat(metadata)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return statErr
	}
	flags := os.O_RDWR
	if fresh {
		// Persist the staging name before creating bytes so crash cleanup never
		// has to discover anonymous files by scanning workspace directories.
		if err = m.save(r); err != nil {
			return err
		}
		flags |= os.O_CREATE | os.O_EXCL
	} else {
		_, err = checkStage(parent, r)
		if errors.Is(err, os.ErrNotExist) && r.Offset == 0 {
			flags |= os.O_CREATE | os.O_EXCL
		} else if err != nil {
			return Error("partial_corrupt", "upload staging file is unavailable")
		}
	}
	f, err := parent.OpenFile(filepath.FromSlash(r.Temp), flags, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	stage, stageErr := checkStage(parent, r)
	if err != nil || stageErr != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, stage) || opened.Size() < r.Offset {
		return Error("partial_corrupt", "upload staging identity or length changed")
	}
	if flags&os.O_CREATE != 0 {
		if err = f.Sync(); err != nil {
			return err
		}
		if err = syncStage(parent); err != nil {
			return err
		}
		if err = syncParent(parent); err != nil {
			return err
		}
		if err = m.save(r); err != nil {
			return err
		}
	}
	// Only the speculative tail is discarded, and only on an explicit attempt.
	if err = f.Truncate(r.Offset); err != nil {
		return err
	}
	recv := receiver{m: m, r: r, parent: parent, f: f, h: sha256.New(), buf: make([]byte, BlockSize+8), position: r.Offset, checkpoint: time.Now()}
	if err = Feed(ctx, f, r.Offset, recv.h, recv.buf[:BlockSize]); err != nil {
		return err
	}
	if hex.EncodeToString(recv.h.Sum(nil)) != r.PrefixSHA256 {
		return Error("partial_corrupt", "durable upload prefix checksum mismatch")
	}
	generation := identity.NewID()
	if err = wire.WriteFrame(conn, model.Response{Result: model.JSON(Ready{Protocol: Protocol, Generation: generation, Result: r.Result})}); err != nil {
		return err
	}
	acknowledged = true
	kind, b, err := ReadRecord(conn, recv.buf)
	if err != nil {
		return err
	}
	var start Start
	if kind != StartRecord || json.Unmarshal(b, &start) != nil || start.Generation != generation || start.Offset != r.Offset || start.PrefixSHA256 != r.PrefixSHA256 {
		return Error("partial_mismatch", "sender did not verify the durable upload prefix")
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(IdleTimeout))
		kind, b, err = ReadRecord(conn, recv.buf)
		if err != nil {
			return err
		}
		switch kind {
		case DataRecord:
			if err = recv.write(b); err != nil {
				return err
			}
			progress(recv.position)
			if recv.position-recv.r.Offset >= CheckpointBytes || time.Since(recv.checkpoint) >= 2*time.Second {
				if err = recv.checkpointFile(); err != nil {
					return err
				}
				current = recv.r.Result
				_ = conn.SetWriteDeadline(time.Now().Add(IdleTimeout))
				if err = wire.WriteFrame(conn, Progress{Result: current}); err != nil {
					return err
				}
			}
		case FinishRecord:
			if len(b) != 0 || recv.position != r.Size {
				return Error("invalid_upload_record", "finish requires exactly the declared file size")
			}
			_ = conn.SetReadDeadline(time.Time{})
			// Finish authorizes publication, but a disconnected/cancelled caller
			// must still interrupt a long disk rehash before the atomic link.
			verifyCtx, cancelVerify := context.WithCancel(ctx)
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				var trailing [1]byte
				_, _ = conn.Read(trailing[:])
				cancelVerify()
			}()
			err = recv.publish(verifyCtx, beforePublish)
			_ = conn.SetReadDeadline(time.Now())
			<-readerDone
			cancelVerify()
			if err != nil {
				m.mu.Lock()
				current = m.entries[key(owner, q.ID)].record.Result
				m.mu.Unlock()
				return err
			}
			current = recv.r.Result
			_ = conn.SetWriteDeadline(time.Now().Add(IdleTimeout))
			return wire.WriteFrame(conn, Progress{Result: current})
		default:
			return Error("invalid_upload_record", "unexpected upload record")
		}
	}
}

func (r *receiver) write(b []byte) error {
	if len(b) <= 8 || int64(binary.BigEndian.Uint64(b[:8])) != r.position || int64(len(b)-8) > r.r.Size-r.position {
		return Error("invalid_upload_record", "upload data is out of order or exceeds the declared size")
	}
	if r.position >= r.checked {
		if err := r.m.space(r.r, int64(len(b)-8)); err != nil {
			return err
		}
		r.checked = r.position + (1 << 20)
	}
	b = b[8:]
	n, err := r.f.Write(b)
	if n > 0 {
		_, _ = r.h.Write(b[:n])
		r.position += int64(n)
	}
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (r *receiver) checkpointFile() error {
	if err := r.f.Sync(); err != nil {
		return err
	}
	next := r.r
	next.Offset = r.position
	next.PrefixSHA256 = hex.EncodeToString(r.h.Sum(nil))
	next.Updated = time.Now().UTC()
	if err := r.m.save(next); err != nil {
		return err
	}
	r.r = next
	r.checkpoint = time.Now()
	return nil
}

func (r *receiver) publish(ctx context.Context, beforePublish func() error) error {
	if err := r.checkpointFile(); err != nil {
		return err
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	if err := Feed(ctx, r.f, r.r.Size, h, r.buf[:BlockSize]); err != nil {
		return err
	}
	info, err := r.f.Stat()
	stage, stageErr := checkStage(r.parent, r.r)
	if err != nil || stageErr != nil || !os.SameFile(info, stage) || info.Size() != r.r.Size || hex.EncodeToString(h.Sum(nil)) != r.r.SHA256 {
		return Error("checksum_mismatch", "complete upload checksum, size, or identity mismatch")
	}
	if err = r.f.Close(); err != nil {
		return err
	}
	if err = beforePublish(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	r.r.State = "publishing"
	if err = r.m.save(r.r); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Never rename over an existing path or copy into the final name. The
	// retained staging link permits reconciliation after a lost final reply.
	if err = r.parent.Link(filepath.FromSlash(r.r.Temp), path.Base(r.r.Path)); err != nil {
		// A failed local link syscall did not create this destination. Persist
		// that known outcome so a collision can be aborted without ambiguity.
		r.r.State = "staging"
		if saveErr := r.m.save(r.r); saveErr != nil {
			return saveErr
		}
		if errors.Is(err, os.ErrExist) {
			return Error("destination_exists", "destination appeared during upload; it was not overwritten")
		}
		return Error("publication_failed", "atomic no-overwrite publication failed; query upload status before retrying")
	}
	// Once linked, finish journaling even if the caller disconnects. Shutdown
	// waits for this handler; a crash is reconciled from the publishing intent.
	if err = syncParent(r.parent); err != nil {
		return err
	}
	r.r.State = "published"
	r.r.Updated = time.Now().UTC()
	if err = r.m.save(r.r); err != nil {
		return err
	}
	_ = r.m.cleanup(&r.r)
	return nil
}
