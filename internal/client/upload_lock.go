package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/upload"
)

// Local upload recovery records are guarded by 64 fixed stripe lock files
// that are never removed. Removing a lock path that another process holds or
// waits on would let a third process create and lock a new file at the same
// path while the first still believes it holds the lock. The single-file and
// directory records of one ID share a stripe.
//
// A directory transfer attempt, or abort, also holds a short-lived attempt
// lock file for its ID. It is created, locked without waiting, checked and
// removed only while holding the ID's stripe, so nobody else has it open when
// it is removed, and at most the active and crashed attempts' files exist.
//
// Older releases locked each single-file record through "<record>.lock" and
// each directory transfer through "<manifest>.lock", and removed those files
// themselves. Those paths are never created or removed here; when one exists,
// it is also locked, so a running older client is still excluded.
const uploadLockStripes = 64

// lockPollInterval is how often a contended stripe is retried. Stripes are
// held only around local record checks and writes.
const lockPollInterval = 10 * time.Millisecond

var errDirUploadBusy = upload.Error("upload_busy", "directory upload is active in another process")

func (c Client) uploadLockDir() string {
	return filepath.Join(c.uploadScopeDir(), "upload-locks")
}

func (c Client) uploadStripePath(id string) string {
	path, _ := uploadStripePath(c.uploadLockDir(), identity.ID([]byte(id)))
	return path
}

func (c Client) dirAttemptPath(id string) string {
	return dirAttemptPath(c.uploadScopeDir(), identity.ID([]byte(id)))
}

func dirAttemptPath(scope, hashed string) string {
	return filepath.Join(scope, "upload-attempts", hashed+".lock")
}

// uploadStripePath maps a record's hashed file name to its stripe, so pruning
// finds the stripe from a directory listing without trusting record content.
func uploadStripePath(locks, hashed string) (string, bool) {
	if len(hashed) != 64 || strings.ToLower(hashed) != hashed {
		return "", false
	}
	b, err := strconv.ParseUint(hashed[:2], 16, 8)
	if err != nil {
		return "", false
	}
	return filepath.Join(locks, fmt.Sprintf("%02x.lock", b%uploadLockStripes)), true
}

// lockStripe waits for a stripe until ctx ends.
func lockStripe(ctx context.Context, path string) (*flock.Flock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	locked, err := lock.TryLockContext(ctx, lockPollInterval)
	if err == nil && !locked {
		err = errors.New("cannot lock upload recovery records")
	}
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// tryLockStripe takes a stripe without waiting; pruning skips a busy stripe.
func tryLockStripe(path string) (*flock.Flock, bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, false
	}
	lock := flock.New(path)
	if locked, err := lock.TryLock(); err != nil || !locked {
		_ = lock.Close()
		return nil, false
	}
	return lock, true
}

// lockExisting locks path only if it already exists, never creating it. It
// returns nil for a missing file, and busy while another process holds it.
// With wait set it retries until ctx ends, as older clients briefly held
// record locks.
func lockExisting(ctx context.Context, path string, wait bool) (lock *flock.Flock, busy bool, err error) {
	lock = flock.New(path, flock.SetFlag(os.O_RDONLY))
	var locked bool
	if wait {
		locked, err = lock.TryLockContext(ctx, lockPollInterval)
	} else {
		locked, err = lock.TryLock()
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !locked {
		_ = lock.Close()
		return nil, err == nil, err
	}
	return lock, false, nil
}

// closeLock releases a lock that may be nil.
func closeLock(lock *flock.Flock) {
	if lock != nil {
		_ = lock.Close()
	}
}

// dirUploadLock is a held directory transfer attempt lock.
type dirUploadLock struct {
	stripe, attempt string
	lock, legacy    *flock.Flock
}

// acquireDirAttempt takes the attempt lock of a directory transfer while the
// caller holds its stripe, or returns errDirUploadBusy while another attempt
// or abort, of this or an older release, holds it.
func acquireDirAttempt(stripe, attempt, legacy string) (*dirUploadLock, error) {
	if err := os.MkdirAll(filepath.Dir(attempt), 0700); err != nil {
		return nil, err
	}
	l := &dirUploadLock{stripe: stripe, attempt: attempt, lock: flock.New(attempt)}
	locked, err := l.lock.TryLock()
	if err == nil && !locked {
		err = errDirUploadBusy
	}
	if err != nil {
		_ = l.lock.Close()
		return nil, err
	}
	var busy bool
	if l.legacy, busy, err = lockExisting(context.Background(), legacy, false); err != nil || busy {
		l.closeHeld()
		if busy {
			err = errDirUploadBusy
		}
		return nil, err
	}
	return l, nil
}

// lockDirUpload takes the attempt lock of directory transfer id. Under the
// same stripe it refuses an ID a single-file upload holds, so a transfer that
// goes on to create its remote root cannot lose its ID to a racing file
// upload before its recovery record is written.
func (c Client) lockDirUpload(ctx context.Context, id string) (*dirUploadLock, error) {
	stripePath := c.uploadStripePath(id)
	stripe, err := lockStripe(ctx, stripePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stripe.Close() }()
	if _, err = os.Lstat(c.uploadRecordPath(id)); err == nil {
		return nil, errors.New("upload ID belongs to a single-file upload; choose another ID for the directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return acquireDirAttempt(stripePath, c.dirAttemptPath(id), c.dirUploadPath(id)+".lock")
}

// dirAttemptActive reports, under the ID's stripe, whether a directory
// transfer attempt of this or an older release holds id.
func (c Client) dirAttemptActive(id string) (bool, error) {
	for _, path := range []string{c.dirAttemptPath(id), c.dirUploadPath(id) + ".lock"} {
		lock, busy, err := lockExisting(context.Background(), path, false)
		closeLock(lock)
		if err != nil || busy {
			return busy, err
		}
	}
	return false, nil
}

// closeHeld unlocks without removing the attempt file.
func (l *dirUploadLock) closeHeld() {
	closeLock(l.legacy)
	_ = l.lock.Close()
}

// removeHeld unlocks and removes the attempt file; the caller holds the
// stripe, so no other process has the file open.
func (l *dirUploadLock) removeHeld() {
	l.closeHeld()
	// After unlocking: Windows cannot remove a file this process holds open.
	_ = os.Remove(l.attempt)
}

// release ends the attempt and removes its lock file under the stripe. If
// the stripe stays busy, the file is left for pruning.
func (l *dirUploadLock) release(ctx context.Context) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stripe, err := lockStripe(cleanup, l.stripe)
	if err != nil {
		l.closeHeld()
		return
	}
	defer func() { _ = stripe.Close() }()
	l.removeHeld()
}

// pruneDirAttempts removes attempt lock files that crashed attempts left. A
// file is removed only under its stripe after this process locked it.
func pruneDirAttempts(ctx context.Context, scope string) {
	dir := filepath.Join(scope, "upload-attempts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	locks := filepath.Join(scope, "upload-locks")
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		hashed, ok := strings.CutSuffix(entry.Name(), ".lock")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		stripePath, ok := uploadStripePath(locks, hashed)
		if !ok {
			continue
		}
		stripe, ok := tryLockStripe(stripePath)
		if !ok {
			continue
		}
		if l, err := acquireDirAttempt(stripePath, dirAttemptPath(scope, hashed), filepath.Join(scope, "upload-dirs", hashed+".json.lock")); err == nil {
			l.removeHeld()
		}
		_ = stripe.Close()
	}
}
