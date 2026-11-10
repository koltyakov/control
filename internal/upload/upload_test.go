package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

func fixture(t *testing.T) *Manager {
	t.Helper()
	work := t.TempDir()
	root, err := os.OpenRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	m, err := New(root, work, t.TempDir(), "node-id", Limits{ReserveBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func request(id string, data []byte) Request {
	h := sha256.Sum256(data)
	return Request{Protocol: Protocol, Spec: Spec{ID: id, Path: id + ".bin", Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}}
}

func attempt(t *testing.T, m *Manager, owner string, q Request, data []byte, send int, finish bool, check func() error) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	done := make(chan error, 1)
	if check == nil {
		check = func() error { return nil }
	}
	go func() { defer func() { _ = a.Close() }(); done <- m.Serve(ctx, owner, a, q, check, func(int64) {}) }()
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	var ack model.Response
	if err := wire.ReadFrame(b, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Error != "" {
		return Result{}, <-done
	}
	var ready Ready
	if err := json.Unmarshal(ack.Result, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.State == "published" {
		return ready.Result, <-done
	}
	if err := WriteRecord(b, StartRecord, model.JSON(Start{Generation: ready.Generation, Offset: ready.Offset, PrefixSHA256: ready.PrefixSHA256})); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan Result, 1)
	go func() {
		last := ready.Result
		for {
			var p Progress
			if wire.ReadFrame(b, &p) != nil {
				readDone <- last
				return
			}
			last = p.Result
			if p.Error != "" || p.State == "published" {
				readDone <- last
				return
			}
		}
	}()
	for offset := int(ready.Offset); offset < send; {
		n := min(BlockSize, send-offset)
		block := make([]byte, 8+n)
		binary.BigEndian.PutUint64(block, uint64(offset))
		copy(block[8:], data[offset:offset+n])
		if err := WriteRecord(b, DataRecord, block); err != nil {
			break
		}
		offset += n
	}
	if finish {
		_ = WriteRecord(b, FinishRecord, nil)
	} else {
		_ = b.Close()
	}
	err := <-done
	_ = b.Close()
	return <-readDone, err
}

func TestUploadPublishAndReceipt(t *testing.T) {
	for _, size := range []int{0, 1, BlockSize - 1, BlockSize, BlockSize + 1, 3 << 20} {
		t.Run(stringID(size), func(t *testing.T) {
			m := fixture(t)
			data := bytes.Repeat([]byte{0, 0xff, 1}, (size+2)/3)[:size]
			q := request("file", data)
			r, err := attempt(t, m, "owner", q, data, len(data), true, nil)
			if err != nil || r.State != "published" || r.Offset != int64(size) {
				t.Fatal(r, err)
			}
			got, err := os.ReadFile(filepath.Join(m.workDir, q.Path))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("content", err)
			}
			r, err = attempt(t, m, "owner", q, data, 0, false, nil)
			if err != nil || r.State != "published" {
				t.Fatal("receipt", r, err)
			}
			r, err = m.Abort(t.Context(), "owner", q.ID)
			if err != nil || r.State != "published" {
				t.Fatal("abort published", r, err)
			}
			if _, err = os.Stat(filepath.Join(m.workDir, q.Path)); err != nil {
				t.Fatal("abort deleted published file", err)
			}
		})
	}
}

func stringID(n int) string { return time.Duration(n).String() }

func TestUploadResumeAfterRestart(t *testing.T) {
	m := fixture(t)
	data := bytes.Repeat([]byte("abcdef\x00\xff"), int(CheckpointBytes)/8+BlockSize)
	q := request("resume", data)
	_, err := attempt(t, m, "owner", q, data, int(CheckpointBytes)+12345, false, nil)
	if err == nil {
		t.Fatal("interrupted upload succeeded")
	}
	if _, err = os.Stat(filepath.Join(m.workDir, q.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial was published", err)
	}
	r, err := m.Status(t.Context(), "owner", q.ID)
	if err != nil || r.Offset < CheckpointBytes || r.Offset >= int64(len(data)) || r.Active {
		t.Fatal(r, err)
	}
	offset := r.Offset
	m, err = New(m.root, m.workDir, filepath.Dir(m.dir), m.nodeID, m.limits)
	if err != nil {
		t.Fatal("restart", err)
	}
	q.Resume = true
	r, err = attempt(t, m, "owner", q, data, len(data), true, nil)
	if err != nil || r.State != "published" || offset == 0 {
		t.Fatal(r, err)
	}
	got, err := os.ReadFile(filepath.Join(m.workDir, q.Path))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("resumed content", err)
	}
}

func TestUploadIntegrityOwnershipAndConflicts(t *testing.T) {
	m := fixture(t)
	data := []byte("expected contents")
	q := request("bad", data)
	bad := bytes.Repeat([]byte("x"), len(data))
	_, err := attempt(t, m, "owner", q, bad, len(bad), true, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal("accepted corrupt bytes", err)
	}
	if _, err = os.Stat(filepath.Join(m.workDir, q.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corruption published", err)
	}
	if _, err = m.Status(t.Context(), "other-owner", q.ID); err == nil {
		t.Fatal("foreign owner saw upload")
	}
	if _, err = m.Abort(t.Context(), "other-owner", q.ID); err == nil {
		t.Fatal("foreign owner aborted upload")
	}
	changed := q
	changed.Path = "different.bin"
	if _, err = attempt(t, m, "owner", changed, data, 0, false, nil); err == nil {
		t.Fatal("changed specification accepted")
	}
	if _, err = attempt(t, m, "owner", q, data, 0, false, nil); err == nil {
		t.Fatal("implicit resume accepted")
	}
	if _, err = m.Abort(t.Context(), "owner", q.ID); err != nil {
		t.Fatal(err)
	}
	q.Resume = true
	if _, err = attempt(t, m, "owner", q, data, 0, false, nil); err == nil {
		t.Fatal("aborted upload resumed")
	}
	q = request("collision", data)
	if err = os.WriteFile(filepath.Join(m.workDir, q.Path), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = attempt(t, m, "owner", q, data, len(data), true, nil); err == nil {
		t.Fatal("existing destination overwritten")
	}
	got, _ := os.ReadFile(filepath.Join(m.workDir, q.Path))
	if string(got) != "keep" {
		t.Fatal("existing file changed")
	}
}

func TestUploadPrefixCorruptionAndExpiry(t *testing.T) {
	m := fixture(t)
	data := bytes.Repeat([]byte("a"), int(CheckpointBytes)+BlockSize)
	q := request("prefix", data)
	_, _ = attempt(t, m, "owner", q, data, len(data)-1, false, nil)
	r := m.entries[key("owner", q.ID)].record
	f, err := os.OpenFile(filepath.Join(m.workDir, r.Temp), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("wrong"), 0)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	q.Resume = true
	if _, err = attempt(t, m, "owner", q, data, 0, false, nil); err == nil || !strings.Contains(err.Error(), "prefix checksum") {
		t.Fatal(err)
	}
	if err = m.Prune(t.Context(), time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s, err := m.Status(t.Context(), "owner", q.ID)
	if err != nil || s.State != "expired" {
		t.Fatal(s, err)
	}
	if _, err = os.Stat(filepath.Join(m.workDir, r.Temp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired staging retained", err)
	}
}

func TestUploadPublicationReconciliation(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-link", true: "after-link"}[linked], func(t *testing.T) {
			m := fixture(t)
			data := []byte("verified")
			q := request("intent", data)
			_, err := attempt(t, m, "owner", q, data, len(data), true, func() error { return errors.New("revoked") })
			if err == nil {
				t.Fatal("revoked publication succeeded")
			}
			r := m.entries[key("owner", q.ID)].record
			r.State = "publishing"
			if err = m.save(r); err != nil {
				t.Fatal(err)
			}
			if linked {
				if err = m.root.Link(r.Temp, r.Path); err != nil {
					t.Fatal(err)
				}
			}
			m, err = New(m.root, m.workDir, filepath.Dir(m.dir), m.nodeID, m.limits)
			if err != nil {
				t.Fatal(err)
			}
			s, err := m.Status(t.Context(), "owner", q.ID)
			if err != nil {
				t.Fatal(err)
			}
			if linked && s.State != "published" {
				t.Fatal(s)
			}
			if !linked {
				if s.State != "staging" {
					t.Fatal(s)
				}
				if _, err = os.Stat(filepath.Join(m.workDir, q.Path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("restart replayed publication", err)
				}
			}
		})
	}
}

func TestUploadBoundsAndPrivatePaths(t *testing.T) {
	m := fixture(t)
	q := request("quota", []byte("abc"))
	m.limits.OwnerBytes = 2
	if _, err := attempt(t, m, "owner", q, []byte("abc"), 3, true, nil); err == nil {
		t.Fatal("quota exceeded")
	}
	for _, name := range []string{"../escape", "/absolute", "a/../escape", "NUL", "dir/file:stream", "dir\\file", StagingDir + "/file"} {
		q.Path = name
		if q.Validate() == nil {
			t.Fatal("unsafe path", name)
		}
	}
	for _, name := range []string{StagingDir + "/file", "dir/" + StagingDir + "/file"} {
		if m.CheckPath(name) == nil {
			t.Fatal("private path exposed", name)
		}
	}
	var header [5]byte
	header[0] = DataRecord
	binary.BigEndian.PutUint32(header[1:], BlockSize+9)
	if _, _, err := ReadRecord(bytes.NewReader(header[:]), make([]byte, BlockSize+8)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, _, err := ReadRecord(bytes.NewReader([]byte{DataRecord}), make([]byte, BlockSize+8)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}

func TestUploadMalformedAttempts(t *testing.T) {
	for _, kind := range []string{"offset", "excess", "finish", "duplicate-start", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			m := fixture(t)
			q := request("malformed", []byte("abc"))
			a, b := net.Pipe()
			defer func() { _ = b.Close() }()
			done := make(chan error, 1)
			go func() {
				defer func() { _ = a.Close() }()
				done <- m.Serve(t.Context(), "owner", a, q, func() error { return nil }, func(int64) {})
			}()
			_ = b.SetDeadline(time.Now().Add(5 * time.Second))
			var ack model.Response
			if err := wire.ReadFrame(b, &ack); err != nil {
				t.Fatal(err)
			}
			var ready Ready
			if err := json.Unmarshal(ack.Result, &ready); err != nil {
				t.Fatal(err)
			}
			start := model.JSON(Start{Generation: ready.Generation, PrefixSHA256: ready.PrefixSHA256})
			if err := WriteRecord(b, StartRecord, start); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "offset":
				block := make([]byte, 9)
				binary.BigEndian.PutUint64(block, 42)
				_ = WriteRecord(b, DataRecord, block)
			case "excess":
				_ = WriteRecord(b, DataRecord, make([]byte, 12))
			case "finish":
				_ = WriteRecord(b, FinishRecord, nil)
			case "duplicate-start":
				_ = WriteRecord(b, StartRecord, start)
			case "oversize":
				var h [5]byte
				h[0] = DataRecord
				binary.BigEndian.PutUint32(h[1:], BlockSize+9)
				_, _ = b.Write(h[:])
			}
			var p Progress
			if err := wire.ReadFrame(b, &p); err != nil || p.Code != "invalid_upload_record" {
				t.Fatal(p, err)
			}
			if err := <-done; err == nil {
				t.Fatal("malformed attempt succeeded")
			}
			if _, err := os.Stat(filepath.Join(m.workDir, q.Path)); !os.IsNotExist(err) {
				t.Fatal("malformed upload published", err)
			}
		})
	}
}

func TestUploadPublicationCollisionAndPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"collision", "metadata", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			m := fixture(t)
			data := []byte("bytes")
			q := request("fault", data)
			_, err := attempt(t, m, "owner", q, data, len(data), true, func() error {
				switch kind {
				case "collision":
					return os.WriteFile(filepath.Join(m.workDir, q.Path), []byte("keep"), 0600)
				case "metadata":
					if err := os.Rename(m.dir, m.dir+".saved"); err != nil {
						return err
					}
					return os.WriteFile(m.dir, []byte("block metadata writes"), 0600)
				default:
					return context.Canceled
				}
			})
			if err == nil {
				t.Fatal("publication fault accepted")
			}
			if kind == "collision" {
				b, err := os.ReadFile(filepath.Join(m.workDir, q.Path))
				if err != nil || string(b) != "keep" {
					t.Fatal("collision overwritten", err)
				}
				if _, err = m.Abort(t.Context(), "owner", q.ID); err != nil {
					t.Fatal("known failed publication could not abort", err)
				}
			} else if _, err = os.Stat(filepath.Join(m.workDir, q.Path)); !os.IsNotExist(err) {
				t.Fatal("failed publication visible", err)
			}
		})
	}
}

func TestUploadCorruptMetadataIsPreserved(t *testing.T) {
	m := fixture(t)
	q := request("corrupt", []byte("data"))
	_, _ = attempt(t, m, "owner", q, []byte("data"), 2, false, nil)
	name := filepath.Join(m.dir, key("owner", q.ID)+".json")
	broken := []byte(`{"id":"broken"`)
	if err := os.WriteFile(name, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(m.root, m.workDir, filepath.Dir(m.dir), m.nodeID, m.limits); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatal("corrupt journal changed", err)
	}
}

func TestUploadActiveOwnershipAndReservationRelease(t *testing.T) {
	m := fixture(t)
	q := request("active", []byte("data"))
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = a.Close() }()
		done <- m.Serve(t.Context(), "owner", a, q, func() error { return nil }, func(int64) {})
	}()
	var ack model.Response
	if err := wire.ReadFrame(b, &ack); err != nil {
		t.Fatal(err)
	}
	s, err := m.Status(t.Context(), "owner", q.ID)
	if err != nil || !s.Active {
		t.Fatal(s, err)
	}
	if _, err = m.Abort(t.Context(), "owner", q.ID); err == nil {
		t.Fatal("active upload aborted")
	}
	if _, release, err := m.reserve("owner", q.Spec, true); err == nil {
		release()
		t.Fatal("concurrent writer accepted")
	}
	_ = b.Close()
	if err = <-done; err == nil {
		t.Fatal("disconnect succeeded")
	}
	if m.active != 0 || len(m.owners) != 0 {
		t.Fatal("active slots leaked")
	}
	if _, err = m.Abort(t.Context(), "owner", q.ID); err != nil {
		t.Fatal(err)
	}
	if m.active != 0 || len(m.owners) != 0 {
		t.Fatal("abort slots leaked")
	}
}

func TestUploadPrivateAliases(t *testing.T) {
	m := fixture(t)
	if err := os.Mkdir(filepath.Join(m.workDir, StagingDir), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(m.workDir, "alias")
	if err := os.Symlink(StagingDir, alias); err != nil {
		t.Skip("symlinks unavailable", err)
	}
	if m.CheckPath("alias/file") == nil {
		t.Fatal("symlink exposed staging")
	}
	if err := os.Symlink(m.dir, filepath.Join(m.workDir, "metadata")); err != nil {
		t.Fatal(err)
	}
	if m.CheckPath("metadata/file") == nil {
		t.Fatal("symlink exposed upload metadata")
	}
}

func TestUploadAdmissionAndDiskReserve(t *testing.T) {
	m := fixture(t)
	q := request("one", []byte("abc"))
	var releases []func()
	for _, id := range []string{"one", "two"} {
		q.ID = id
		_, release, err := m.reserve("owner", q.Spec, false)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	q.ID = "three"
	if _, release, err := m.reserve("owner", q.Spec, false); err == nil {
		release()
		t.Fatal("per-owner stream limit ignored")
	}
	for _, release := range releases {
		release()
	}
	if len(m.entries) != 0 || m.active != 0 {
		t.Fatal("unaccepted reservations retained")
	}
	m.limits.ReserveBytes = ^uint64(0)
	if _, err := attempt(t, m, "owner", q, []byte("abc"), 3, true, nil); err == nil {
		t.Fatal("disk reserve ignored")
	}
	if len(m.entries) != 0 {
		t.Fatal("disk rejection leaked a reservation")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(b []byte) (int, error) { return w.Buffer.Write(b[:min(3, len(b))]) }

func TestUploadRecordsReuseStorageAndHandleShortIO(t *testing.T) {
	w := &shortWriter{}
	payload := bytes.Repeat([]byte("binary\x00"), 100)
	if err := WriteRecord(w, DataRecord, payload); err != nil {
		t.Fatal(err)
	}
	scratch := make([]byte, BlockSize+8)
	kind, b, err := ReadRecord(w, scratch)
	if err != nil || kind != DataRecord || !bytes.Equal(b, payload) || &b[0] != &scratch[0] {
		t.Fatal("record did not reuse caller storage", err)
	}
	if err := WriteRecord(w, DataRecord, make([]byte, BlockSize+9)); err == nil {
		t.Fatal("oversized outgoing record accepted")
	}
}

func TestUploadHandshakeIdleDeadlines(t *testing.T) {
	for _, phase := range []string{"ready", "start", "receipt", "error"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := fixture(t)
				q := request("idle", []byte("contents"))
				if phase == "receipt" {
					if _, err := attempt(t, m, "owner", q, []byte("contents"), 8, true, nil); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "error" {
					q.Protocol = "invalid"
				}
				a, b := net.Pipe()
				defer func() { _ = a.Close(); _ = b.Close() }()
				done := make(chan error, 1)
				go func() { done <- m.Serve(t.Context(), "owner", a, q, func() error { return nil }, func(int64) {}) }()
				if phase == "start" {
					var ack model.Response
					if err := wire.ReadFrame(b, &ack); err != nil || ack.Error != "" {
						t.Fatal("ready", ack, err)
					}
				}
				// The peer neither reads the reply nor sends Start. Include the
				// separately bounded terminal-error write after the idle timeout.
				time.Sleep(IdleTimeout + 6*time.Second)
				select {
				case err := <-done:
					if phase != "receipt" && phase != "error" {
						var timeout net.Error
						if !errors.As(err, &timeout) || !timeout.Timeout() {
							t.Fatal("stalled handshake did not time out", err)
						}
					}
				case <-time.After(time.Second):
					t.Fatal("stalled handshake retained its stream")
				}
				if m.active != 0 || len(m.owners) != 0 {
					t.Fatal("stalled handshake leaked active upload slots")
				}
			})
		})
	}
}

func TestUploadDisconnectDuringVerificationDoesNotPublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := fixture(t)
		data := []byte("verified bytes")
		q := request("verify-cancel", data)
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			defer func() { _ = a.Close() }()
			done <- m.Serve(t.Context(), "owner", a, q, func() error { close(entered); <-release; return nil }, func(int64) {})
		}()
		var ack model.Response
		if err := wire.ReadFrame(b, &ack); err != nil {
			t.Fatal(err)
		}
		var ready Ready
		if err := json.Unmarshal(ack.Result, &ready); err != nil {
			t.Fatal(err)
		}
		if err := WriteRecord(b, StartRecord, model.JSON(Start{Generation: ready.Generation, PrefixSHA256: ready.PrefixSHA256})); err != nil {
			t.Fatal(err)
		}
		block := make([]byte, 8+len(data))
		copy(block[8:], data)
		if err := WriteRecord(b, DataRecord, block); err != nil {
			t.Fatal(err)
		}
		if err := WriteRecord(b, FinishRecord, nil); err != nil {
			t.Fatal(err)
		}
		<-entered
		_ = b.Close()
		synctest.Wait()
		close(release)
		if err := <-done; err == nil {
			t.Fatal("disconnected verification reported success")
		}
		if _, err := os.Stat(filepath.Join(m.workDir, q.Path)); !os.IsNotExist(err) {
			t.Fatal("disconnected caller published a file", err)
		}
	})
}
