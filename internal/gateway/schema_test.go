package gateway

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

//go:embed testdata/schema10-v0.14.0.sql
var schema10Fixture string

// databaseSchema returns every user object's definition with whitespace
// normalized, so formatting differences in the schema source do not matter.
func databaseSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	schema := map[string]string{}
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			t.Fatal(err)
		}
		definition = strings.Join(strings.Fields(definition), " ")
		definition = strings.NewReplacer("( ", "(", " )", ")", " ,", ",").Replace(definition)
		schema[kind+" "+name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return schema
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// rawDatabase opens dir's database without the gateway, for setting up or
// inspecting state around a gateway start.
func rawDatabase(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func freshSchema(t *testing.T) map[string]string {
	t.Helper()
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	return databaseSchema(t, g.db)
}

func TestNewDatabaseCreatesCurrentSchema(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	if v := schemaVersion(t, g.db); v != currentSchemaVersion {
		t.Fatalf("new database schema %d, want %d", v, currentSchemaVersion)
	}
	created := databaseSchema(t, g.db)
	if len(created) == 0 || created["table users"] == "" || created["table global_vault"] == "" {
		t.Fatalf("schema not created: %v", created)
	}
	if g.users[legacyUser].Name != "legacy" {
		t.Fatal("operator fleet not created with the database")
	}
	alice, a := createTestUser(t, s, "alice")
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening changes nothing and keeps the account; keys are stored hashed.
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if got := databaseSchema(t, g.db); !maps.Equal(got, created) || schemaVersion(t, g.db) != currentSchemaVersion {
		t.Fatalf("reopen changed the schema:\n%v\nwant\n%v", got, created)
	}
	if g.authenticate(a.Key).UserID != alice.ID {
		t.Fatal("account lost on restart")
	}
	var stored []byte
	if err := g.db.QueryRow(`SELECT data FROM credentials WHERE user_id=?`, alice.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stored) || bytes.Contains(stored, []byte(a.Key)) {
		t.Fatal("credential stored as plaintext")
	}
}

// A database written by v0.14.0 opens as is: the same schema a new database
// gets, with every kind of record loaded. Later schema steps must upgrade it
// to the schema of a new database too.
func TestOpensV014Database(t *testing.T) {
	dir := t.TempDir()
	if _, err := rawDatabase(t, dir).Exec(schema10Fixture); err != nil {
		t.Fatal(err)
	}
	// Pre-SQLite files beside an existing database are stale and ignored.
	if err := os.WriteFile(filepath.Join(dir, "nodes.json"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if v := schemaVersion(t, g.db); v != currentSchemaVersion {
		t.Fatalf("schema %d after open, want %d", v, currentSchemaVersion)
	}
	if got, want := databaseSchema(t, g.db), freshSchema(t); !maps.Equal(got, want) {
		t.Fatalf("v0.14.0 schema differs from a new database:\n%v\nwant\n%v", got, want)
	}
	var violations int
	if err := g.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations: %d %v", violations, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	var alice User
	for _, u := range g.users {
		if u.Name == "alice" {
			alice = u
		}
	}
	nodes := map[string]model.Node{}
	for _, n := range g.nodes {
		nodes[n.Name] = n
	}
	w1, w2 := nodes["w1"], nodes["w2"]
	if alice.ID == "" || len(g.users) != 2 || w1.UserID != alice.ID || w2.UserID != alice.ID || g.owners[w1.ID] != alice.ID {
		t.Fatalf("accounts or machines: users=%v nodes=%v", g.users, g.nodes)
	}
	var key keyRecord
	for _, k := range g.keys {
		key = k
	}
	if len(g.keys) != 1 || key.UserID != alice.ID || key.Role != roleUser || key.Hash == "" {
		t.Fatalf("credentials: %+v", g.keys)
	}
	// The usage marker matches each credential, so the time carries over.
	if g.keyLastUsed(key.ID).IsZero() || g.keyLastUsed(superuserKeyID).IsZero() {
		t.Fatal("key last-used times not loaded")
	}
	if len(g.clientOwners) != 1 || len(g.clientTransports) != 1 {
		t.Fatalf("client identities: owners=%v transports=%v", g.clientOwners, g.clientTransports)
	}
	for transport, owner := range g.clientTransports {
		if !g.clientOwners[owner] || g.owners[transport] != alice.ID {
			t.Fatal("client transport binding not loaded")
		}
	}
	if !g.machineStates[w2.ID].Disabled || g.labelOverlays[w1.ID].Set["site"] != "lab" || g.proxyDependencies[w2.ID].Worker != w1.ID {
		t.Fatalf("machine records: states=%v labels=%v proxies=%v", g.machineStates, g.labelOverlays, g.proxyDependencies)
	}
	if len(g.installations) != 1 || len(g.console.passwords) != 2 {
		t.Fatalf("invitations=%d console passwords=%d", len(g.installations), len(g.console.passwords))
	}
	policy := g.vaultPolicies[alice.ID]
	if !policy.Enabled || policy.UserID != alice.ID || len(policy.Allowances) != 1 || policy.Allowances[0].KeyID != key.ID || !slices.Equal(policy.Allowances[0].MachineIDs, []string{w1.ID}) {
		t.Fatalf("fleet lock: %+v", policy)
	}
	if !g.globalVault.Enabled || g.globalVault.Revision != 1 || g.globalVault.PausedUntil.IsZero() {
		t.Fatalf("gateway lock: %+v", g.globalVault)
	}
	var events int
	if err := g.db.QueryRow(`SELECT count(*) FROM audit_events WHERE user_id=?`, alice.ID).Scan(&events); err != nil || events == 0 {
		t.Fatalf("audit history: %d %v", events, err)
	}
}

// Databases older than the base schema are refused untouched by the gateway
// and by backups, and so are newer ones.
func TestOpenRefusesUnsupportedSchemas(t *testing.T) {
	for _, tc := range []struct {
		version int
		message string
	}{
		{1, "predates Control v0.14.0"},
		{baseSchemaVersion - 1, "predates Control v0.14.0"},
		{currentSchemaVersion + 1, "newer than this executable"},
	} {
		t.Run(fmt.Sprint(tc.version), func(t *testing.T) {
			dir := t.TempDir()
			db := rawDatabase(t, dir)
			if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, data BLOB NOT NULL); PRAGMA user_version=%d;`, tc.version)); err != nil {
				t.Fatal(err)
			}
			before := databaseSchema(t, db)
			if g, err := New(dir, commonKey, Options{SuperuserKey: superKey}); err == nil {
				_ = g.Close()
				t.Fatal("gateway opened an unsupported schema")
			} else if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("gateway error %q, want %q", err, tc.message)
			}
			if _, err := Backup(context.Background(), dir, filepath.Join(t.TempDir(), "backup.db")); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("backup error %v, want %q", err, tc.message)
			}
			if _, err := VerifyBackup(context.Background(), filepath.Join(dir, "gateway.db")); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("verify error %v, want %q", err, tc.message)
			}
			if schemaVersion(t, db) != tc.version || !maps.Equal(databaseSchema(t, db), before) {
				t.Fatal("refused database was modified")
			}
		})
	}
}

// Pre-SQLite JSON state is refused rather than ignored; v0.14.0 imports it.
func TestOpenRefusesPreSQLiteState(t *testing.T) {
	for _, name := range []string{"nodes.json", "keys.json", "installations.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, name)
			if err := os.WriteFile(legacy, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if g, err := New(dir, commonKey, Options{SuperuserKey: superKey}); err == nil {
				_ = g.Close()
				t.Fatal("gateway ignored pre-SQLite state")
			} else if !strings.Contains(err.Error(), "predates Control v0.14.0") {
				t.Fatal(err)
			}
			db := rawDatabase(t, dir)
			if schemaVersion(t, db) != 0 || len(databaseSchema(t, db)) != 0 {
				t.Fatal("refused state gained a database schema")
			}
			if data, err := os.ReadFile(legacy); err != nil || string(data) != "{}" {
				t.Fatal("pre-SQLite state changed", err)
			}
			_ = db.Close()
			if err := os.Remove(legacy); err != nil {
				t.Fatal(err)
			}
			g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
			if err != nil {
				t.Fatal("empty database not initialized once the state is gone", err)
			}
			_ = g.Close()
		})
	}
}
