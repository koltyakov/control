package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// backupAuditAction records an online backup in the operator's audit history.
const backupAuditAction = "gateway.backup"

// backupLockName serializes online backups of one state directory. It is
// separate from gateway.lock, which only the serving gateway holds.
const backupLockName = "gateway-backup.lock"

// BackupInfo describes a verified gateway database snapshot. It carries counts
// and a checksum, never record contents.
type BackupInfo struct {
	Path          string        `json:"path"`
	SchemaVersion int           `json:"schemaVersion"`
	Bytes         int64         `json:"bytes"`
	SHA256        string        `json:"sha256"`
	CreatedAt     time.Time     `json:"createdAt,omitzero"`
	Duration      time.Duration `json:"durationNs,omitempty"`
	Accounts      int           `json:"accounts"`
	Machines      int           `json:"machines"`
	Credentials   int           `json:"credentials"`
	Invitations   int           `json:"invitations"`
	Identities    int           `json:"identities"`
}

// backupSnapshotHook runs after the snapshot is written; tests use it to
// cancel between the snapshot and publication.
var backupSnapshotHook func()

// Backup writes a transactionally consistent snapshot of the gateway database
// in dataDir to output, while the gateway may be serving. It runs in the
// operator's process on the gateway host: authority is filesystem access to
// the state directory, and credential hashes never cross the network.
//
// The snapshot uses SQLite's VACUUM INTO on a separate connection. Under WAL,
// that connection's read transaction does not block the gateway's writers or
// touch its mutation mutex, and it observes exactly the committed
// transactions, so the copy holds a prefix of acknowledged mutations. The
// output is verified, synced, recorded in the audit log, and then published
// at output without overwriting anything. Any failure or cancellation removes
// the temporary file. Only the database is copied; release storage and
// rollout files are not (see docs/gateway.md).
func Backup(ctx context.Context, dataDir, output string) (BackupInfo, error) {
	started := time.Now()
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return BackupInfo{}, err
	}
	source := filepath.Join(dataDir, "gateway.db")
	stat, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, fmt.Errorf("no gateway database at %s; check --data", source)
	}
	if err != nil {
		return BackupInfo{}, err
	}
	if !stat.Mode().IsRegular() {
		return BackupInfo{}, fmt.Errorf("%s is not a regular file", source)
	}
	if err = checkDatabaseOwner(stat); err != nil {
		return BackupInfo{}, err
	}
	if output, err = filepath.Abs(output); err != nil {
		return BackupInfo{}, err
	}
	if _, err = os.Lstat(output); err == nil {
		return BackupInfo{}, fmt.Errorf("%s already exists; backups never overwrite: %w", output, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, err
	}
	dir := filepath.Dir(output)
	if err = outsideStateDirectory(dataDir, dir); err != nil {
		return BackupInfo{}, err
	}
	lock := flock.New(filepath.Join(dataDir, backupLockName))
	locked, err := lock.TryLock()
	if err != nil {
		return BackupInfo{}, err
	}
	if !locked {
		return BackupInfo{}, errors.New("another gateway backup of this state directory is in progress")
	}
	defer func() { _ = lock.Close() }()
	// mode=rw never creates a database. Durability and busy handling match the
	// gateway's own connection; journal_mode is persistent and left unchanged.
	db, err := sql.Open("sqlite", sqliteURI(source, "mode=rw&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"))
	if err != nil {
		return BackupInfo{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return BackupInfo{}, fmt.Errorf("read gateway database: %w", err)
	}
	if version > currentSchemaVersion {
		return BackupInfo{}, fmt.Errorf("gateway database schema %d is newer than this executable (%d); run the backup with the gateway's current Control version", version, currentSchemaVersion)
	}
	if version < baseSchemaVersion {
		return BackupInfo{}, errPreBaseState(fmt.Sprintf("gateway database schema %d", version))
	}
	f, err := os.CreateTemp(dir, ".control-backup-*")
	if err != nil {
		return BackupInfo{}, err
	}
	tmp := f.Name()
	published := false
	defer func() {
		if !published {
			removeBackupTemp(tmp)
		}
	}()
	err = f.Chmod(0600)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return BackupInfo{}, err
	}
	// VACUUM INTO accepts an existing empty file and refuses a non-empty one.
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return BackupInfo{}, fmt.Errorf("snapshot gateway database: %w", err)
	}
	if backupSnapshotHook != nil {
		backupSnapshotHook()
	}
	// SQLite does not sync VACUUM INTO output; sync before verifying so the
	// checksum and checks describe durable bytes.
	if err = syncFile(tmp); err != nil {
		return BackupInfo{}, err
	}
	info, err := VerifyBackup(ctx, tmp)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("verify snapshot: %w", err)
	}
	if info.SchemaVersion != version {
		return BackupInfo{}, fmt.Errorf("snapshot schema %d differs from source schema %d", info.SchemaVersion, version)
	}
	if err = ctx.Err(); err != nil {
		return BackupInfo{}, err
	}
	info.Path, info.CreatedAt, info.Duration = output, time.Now().UTC(), time.Since(started)
	// Record before publishing: a published online backup always has an
	// audit record. A failed publication can leave a record without a file.
	if err = recordBackup(ctx, db, info); err != nil {
		return BackupInfo{}, fmt.Errorf("record backup in audit log; backup not published: %w", err)
	}
	if err = store.PublishNew(tmp, output); err != nil {
		return BackupInfo{}, fmt.Errorf("publish backup (audit event already recorded): %w", err)
	}
	published = true
	return info, nil
}

// VerifyBackup checks a backup file without modifying it or creating SQLite
// sidecar files: integrity, foreign keys, a schema version this executable
// can open, and every table and column that version defines in schema.sql
// and schemaUpgrades. It is not for a database a gateway is serving.
func VerifyBackup(ctx context.Context, path string) (BackupInfo, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return BackupInfo{}, err
	}
	stat, err := os.Stat(path)
	if err != nil {
		return BackupInfo{}, err
	}
	if !stat.Mode().IsRegular() {
		return BackupInfo{}, fmt.Errorf("%s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro&immutable=1"))
	if err != nil {
		return BackupInfo{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	info := BackupInfo{Path: path}
	if err = db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&info.SchemaVersion); err != nil {
		return info, fmt.Errorf("open backup: %w", err)
	}
	if info.SchemaVersion == 0 {
		return info, errors.New("not a gateway database (schema version 0)")
	}
	if info.SchemaVersion < baseSchemaVersion {
		return info, fmt.Errorf("backup schema %d predates Control v0.14.0; restore it with v0.14.0 and start that gateway once to upgrade it", info.SchemaVersion)
	}
	if info.SchemaVersion > currentSchemaVersion {
		return info, fmt.Errorf("backup schema %d is newer than this executable (%d); use that Control version or newer", info.SchemaVersion, currentSchemaVersion)
	}
	var result string
	if err = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return info, fmt.Errorf("integrity check: %w", err)
	}
	if result != "ok" {
		return info, fmt.Errorf("integrity check failed: %s", clip(result, 256))
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return info, fmt.Errorf("foreign key check: %w", err)
	}
	violation := rows.Next()
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return info, fmt.Errorf("foreign key check: %w", err)
	}
	if violation {
		return info, errors.New("foreign key check failed")
	}
	// Restoring opens this version and applies the later steps, so the
	// backup must hold every table and column the gateway reads at it.
	if err = checkSchema(ctx, db, info.SchemaVersion); err != nil {
		return info, err
	}
	for table, count := range map[string]*int{"users": &info.Accounts, "nodes": &info.Machines, "credentials": &info.Credentials, "invitations": &info.Invitations, "identities": &info.Identities} {
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(count); err != nil {
			return info, fmt.Errorf("read %s: %w", table, err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if info.Bytes, err = io.Copy(hash, f); err != nil {
		return info, err
	}
	info.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return info, ctx.Err()
}

// recordBackup appends the operator audit event in its own transaction. The
// gateway's next retention pass bounds it like any other event.
func recordBackup(ctx context.Context, db *sql.DB, info BackupInfo) error {
	account := "unknown"
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	if sudo := os.Getenv("SUDO_USER"); sudo != "" && sudo != account {
		account += " via sudo from " + sudo
	}
	e := AuditEvent{At: info.CreatedAt, UserID: legacyUser, Actor: "@operator", Via: "local backup", Action: backupAuditAction,
		Target: "gateway database", TargetID: info.SHA256,
		Detail: clip(fmt.Sprintf("schema %d, %d bytes, host account %s", info.SchemaVersion, info.Bytes, account), 256)}
	_, err := db.ExecContext(ctx, `INSERT INTO audit_events(user_id,action,data) VALUES(?,?,?)`, e.UserID, e.Action, model.JSON(e))
	return err
}

// outsideStateDirectory rejects a destination inside the live state
// directory, compared by file identity so links and case-insensitive
// spellings cannot hide it.
func outsideStateDirectory(dataDir, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("backup destination directory: %w", err)
	}
	if st, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("backup destination directory: %w", err)
	} else if !st.IsDir() {
		return fmt.Errorf("backup destination directory %s is not a directory", dir)
	}
	state, err := os.Stat(dataDir)
	if err != nil {
		return err
	}
	for p := resolved; ; p = filepath.Dir(p) {
		if st, err := os.Stat(p); err == nil && os.SameFile(st, state) {
			return errors.New("backup destination must be outside the gateway state directory")
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// databaseOwnerMatches is the portable part of checkDatabaseOwner.
func databaseOwnerMatches(owner, current uint64) error {
	if owner != current {
		return fmt.Errorf("gateway database is owned by uid %d but this process runs as uid %d; run the backup as the gateway service account, for example with sudo -u", owner, current)
	}
	return nil
}

// sqliteURI names a file in SQLite URI form, so SQLite's own mode and
// immutable parameters apply. The driver consumes its underscore parameters.
func sqliteURI(path, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive paths become file:///C:/...
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// removeBackupTemp removes a partial snapshot and any sidecar SQLite may
// have created for it.
func removeBackupTemp(tmp string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(tmp + suffix)
	}
}
