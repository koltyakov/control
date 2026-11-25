package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// backupNames reads the sequential account names a backup holds.
func backupNames(t *testing.T, path, prefix string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro&immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM users WHERE name LIKE ? ORDER BY name`, prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func backupAuditEvents(t *testing.T, g *Gateway) []string {
	t.Helper()
	rows, err := g.db.Query(`SELECT data FROM audit_events WHERE action=? ORDER BY id`, backupAuditAction)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var events []string
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		events = append(events, string(data))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// Backups taken while accounts are being created hold a gap-free prefix of
// the acknowledged mutations, pass verification, are audited without their
// path, and restore into a working gateway with accounts and registrations.
func TestOnlineBackupDuringMutationsRestoresIntoGateway(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	ctx := context.Background()
	alice, a := createTestUser(t, s, "alice")
	worker, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, a.Key, "worker", alice.ID, worker) == nil {
		t.Fatal("machine registration failed")
	}
	const total = 60
	done := make(chan error, 1)
	go func() {
		admin := testAdmin{URL: s.URL, Key: superKey}
		for i := range total {
			if err := admin.JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": fmt.Sprintf("seq-%03d", i)}, nil); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	out := t.TempDir()
	previous, backups := 0, 0
	check := func(path string) int {
		t.Helper()
		info, err := VerifyBackup(ctx, path)
		if err != nil || info.SchemaVersion != currentSchemaVersion {
			t.Fatalf("verify %s: %+v %v", path, info, err)
		}
		names := backupNames(t, path, "seq-")
		for i, name := range names {
			if name != fmt.Sprintf("seq-%03d", i) {
				t.Fatalf("backup holds a non-prefix of acknowledged mutations: %v", names)
			}
		}
		if len(names) < previous {
			t.Fatalf("later backup lost acknowledged accounts: %d < %d", len(names), previous)
		}
		previous = len(names)
		return len(names)
	}
	var mutationErr error
	for finished := false; !finished; {
		select {
		case mutationErr = <-done:
			finished = true
		default:
		}
		path := filepath.Join(out, fmt.Sprintf("during-%d.db", backups))
		info, err := Backup(ctx, dir, path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Path != path || info.Accounts == 0 || info.Machines != 1 || len(info.SHA256) != 64 {
			t.Fatalf("backup info: %+v", info)
		}
		if st, err := os.Stat(path); err != nil {
			t.Fatal(err)
		} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
			t.Fatalf("backup mode %v", st.Mode().Perm())
		}
		check(path)
		backups++
	}
	if mutationErr != nil {
		t.Fatal("gateway mutations failed during backups:", mutationErr)
	}
	final := filepath.Join(out, "final.db")
	if _, err := Backup(ctx, dir, final); err != nil {
		t.Fatal(err)
	}
	backups++
	if got := check(final); got != total {
		t.Fatalf("final backup holds %d of %d accounts", got, total)
	}
	events := backupAuditEvents(t, g)
	if len(events) != backups {
		t.Fatalf("audit events %d, backups %d", len(events), backups)
	}
	var event AuditEvent
	if err := json.Unmarshal([]byte(events[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.UserID != legacyUser || event.Actor != "@operator" || event.TargetID == "" || !strings.Contains(event.Detail, "schema 8") {
		t.Fatalf("audit event: %+v", event)
	}
	for _, e := range events {
		if strings.Contains(e, filepath.ToSlash(out)) || strings.Contains(e, out) || strings.Contains(e, ".db") {
			t.Fatalf("audit event records the destination: %s", e)
		}
	}
	if names := dirEntries(t, out); len(names) != backups {
		t.Fatalf("backup directory holds temporary files: %v", names)
	}

	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "gateway.db"), data, 0600); err != nil {
		t.Fatal(err)
	}
	r, server := gatewayAt(t, restored)
	defer func() { server.Close(); _ = r.Close() }()
	durable := func(g *Gateway) string {
		g.mu.Lock()
		defer g.mu.Unlock()
		names := map[string]string{}
		for id, n := range g.nodes {
			names[id] = n.UserID + "/" + n.Name
		}
		return string(model.JSON(map[string]any{"users": g.users, "keys": g.keys, "owners": g.owners, "nodes": names, "installations": g.installations}))
	}
	if before, after := durable(g), durable(r); before != after {
		t.Fatalf("restored state differs\nlive:     %s\nrestored: %s", before, after)
	}
	if err := (testAdmin{URL: server.URL, Key: a.Key}).JSON(ctx, "GET", "/v1/auth", nil, nil); err != nil {
		t.Fatal("restored account key does not authenticate:", err)
	}
	if rawPeer(t, server.URL, a.Key, "worker", alice.ID, worker) == nil {
		t.Fatal("restored registration refused its machine")
	}
}

func TestBackupIsCreateOnlyAndOutsideStateDirectory(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	ctx := context.Background()
	out := t.TempDir()
	existing := filepath.Join(out, "existing.db")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(ctx, dir, existing); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "keep" {
		t.Fatal("backup overwrote an existing file")
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "copy.db"), filepath.Join(dir, "sub", "copy.db")} {
		if _, err := Backup(ctx, dir, path); err == nil || !strings.Contains(err.Error(), "outside the gateway state directory") {
			t.Fatalf("backup inside the state directory %s: %v", path, err)
		}
	}
	link := filepath.Join(out, "state-link")
	if err := os.Symlink(dir, link); err == nil {
		if _, err := Backup(ctx, dir, filepath.Join(link, "copy.db")); err == nil || !strings.Contains(err.Error(), "outside the gateway state directory") {
			t.Fatalf("backup through a link into the state directory: %v", err)
		}
	}
	if _, err := Backup(ctx, dir, filepath.Join(out, "missing", "copy.db")); err == nil {
		t.Fatal("backup created a destination directory")
	}
	empty := t.TempDir()
	if _, err := Backup(ctx, empty, filepath.Join(out, "none.db")); err == nil || !strings.Contains(err.Error(), "no gateway database") {
		t.Fatalf("missing database: %v", err)
	}
	if names := dirEntries(t, empty); len(names) != 0 {
		t.Fatalf("backup created state in an empty directory: %v", names)
	}
	// One backup at a time per state directory.
	lock := flock.New(filepath.Join(dir, backupLockName))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatal("lock", ok, err)
	}
	if _, err := Backup(ctx, dir, filepath.Join(out, "busy.db")); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("concurrent backup: %v", err)
	}
	_ = lock.Close()
	for _, name := range dirEntries(t, out) {
		if name != "existing.db" && name != "state-link" {
			t.Fatalf("refused backup left %s", name)
		}
	}
	if events := backupAuditEvents(t, g); len(events) != 0 {
		t.Fatalf("refused backups were audited: %v", events)
	}
}

func TestBackupCancellationLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	out := t.TempDir()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Backup(cancelled, dir, filepath.Join(out, "early.db")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled backup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backupSnapshotHook = cancel
	defer func() { backupSnapshotHook = nil }()
	if _, err := Backup(ctx, dir, filepath.Join(out, "late.db")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled after snapshot: %v", err)
	}
	if names := dirEntries(t, out); len(names) != 0 {
		t.Fatalf("cancelled backups left files: %v", names)
	}
	if events := backupAuditEvents(t, g); len(events) != 0 {
		t.Fatalf("cancelled backups were audited: %v", events)
	}
}

func TestBackupSchemaAndOwnerChecks(t *testing.T) {
	if databaseOwnerMatches(1000, 0) == nil || databaseOwnerMatches(1000, 1000) != nil {
		t.Fatal("database owner check")
	}
	dir := t.TempDir()
	g, err := New(dir, commonKey)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("new database schema %d, backup expects %d: %v", version, currentSchemaVersion, err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	// An offline copy keeps the live database's WAL header; verify reads it
	// without creating sidecar files.
	out := t.TempDir()
	copied := filepath.Join(out, "offline.db")
	data, err := os.ReadFile(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, data, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := VerifyBackup(context.Background(), copied); err != nil || info.SchemaVersion != currentSchemaVersion {
		t.Fatalf("verify offline copy: %+v %v", info, err)
	}
	if names := dirEntries(t, out); len(names) != 1 {
		t.Fatalf("verify created files: %v", names)
	}
	truncated := filepath.Join(out, "truncated.db")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(context.Background(), truncated); err == nil {
		t.Fatal("verify accepted a truncated database")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, currentSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Backup(context.Background(), dir, filepath.Join(out, "newer.db")); err == nil || !strings.Contains(err.Error(), "newer than this executable") {
		t.Fatalf("newer schema backup: %v", err)
	}
	if _, err := VerifyBackup(context.Background(), filepath.Join(dir, "gateway.db")); err == nil || !strings.Contains(err.Error(), "newer than this executable") {
		t.Fatalf("newer schema verify: %v", err)
	}
	if _, err := New(dir, commonKey); err == nil {
		t.Fatal("gateway opened a newer schema")
	}
}
