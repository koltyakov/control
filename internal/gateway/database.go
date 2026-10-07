package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	_ "modernc.org/sqlite"
)

const legacyUser = "legacy"

// SQLite is authoritative. Maps are a process-local cache protected by g.mu;
// each mutation commits before its HTTP/registration acknowledgement.
func (g *Gateway) openDatabase(dir string) error {
	path := filepath.Join(dir, "gateway.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	g.db, err = sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	g.db.SetMaxOpenConns(1)
	if _, err = g.db.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`); err != nil {
		return err
	}
	var version int
	if err = g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 4 {
		return errors.New("gateway database is newer than this executable")
	}
	if version == 0 {
		if err = g.importLegacy(dir); err != nil {
			return err
		}
		tx, err := g.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = tx.Exec(`
CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, data BLOB NOT NULL);
CREATE TABLE identities (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), UNIQUE(id,user_id));
CREATE TABLE credentials (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), hash TEXT NOT NULL UNIQUE, data BLOB NOT NULL);
CREATE TABLE nodes (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL, data BLOB NOT NULL, UNIQUE(user_id,name), FOREIGN KEY(id,user_id) REFERENCES identities(id,user_id));
CREATE TABLE invitations (hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
CREATE INDEX credentials_user ON credentials(user_id);
CREATE INDEX invitations_user ON invitations(user_id);
CREATE TABLE machine_states (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL);
CREATE TABLE client_identities (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL);
CREATE TABLE client_transports (id TEXT PRIMARY KEY REFERENCES identities(id), owner_id TEXT NOT NULL REFERENCES client_identities(id), data BLOB NOT NULL);
PRAGMA user_version=4;`); err != nil {
			return err
		}
		if err = g.writeState(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version == 1 {
		tx, err := g.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`CREATE TABLE machine_states (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL); PRAGMA user_version=2;`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		tx, err := g.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`CREATE TABLE client_identities (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL); PRAGMA user_version=3;`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 3
	}
	if version == 3 {
		tx, err := g.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`CREATE TABLE client_transports (id TEXT PRIMARY KEY REFERENCES identities(id), owner_id TEXT NOT NULL REFERENCES client_identities(id), data BLOB NOT NULL); PRAGMA user_version=4;`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	g.users = map[string]User{}
	g.owners = map[string]string{}
	g.clientOwners = map[string]bool{}
	g.clientTransports = map[string]string{}
	g.keys = map[string]keyRecord{}
	g.nodes = map[string]model.Node{}
	g.installations = map[string]installation{}
	g.machineStates = map[string]model.MachineState{}
	if err = readRecords(g.db, "client_identities", "id", g.clientOwners); err != nil {
		return err
	}
	if err = readRecords(g.db, "client_transports", "id", g.clientTransports); err != nil {
		return err
	}
	if err = readRecords(g.db, "machine_states", "id", g.machineStates); err != nil {
		return err
	}
	if err = readRecords(g.db, "users", "id", g.users); err != nil {
		return err
	}
	if err = readRecords(g.db, "credentials", "id", g.keys); err != nil {
		return err
	}
	if err = readRecords(g.db, "nodes", "id", g.nodes); err != nil {
		return err
	}
	if err = readRecords(g.db, "invitations", "hash", g.installations); err != nil {
		return err
	}
	rows, err := g.db.Query(`SELECT id,user_id FROM identities`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, user string
		if err = rows.Scan(&id, &user); err != nil {
			return err
		}
		g.owners[id] = user
	}
	return rows.Err()
}

func readRecords[T any](db *sql.DB, table, key string, dst map[string]T) error {
	rows, err := db.Query("SELECT " + key + ",data FROM " + table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var data []byte
		var value T
		if err = rows.Scan(&id, &data); err != nil {
			return err
		}
		if err = json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("decode %s: %w", table, err)
		}
		dst[id] = value
	}
	return rows.Err()
}

func (g *Gateway) importLegacy(dir string) error {
	g.clientOwners = map[string]bool{}
	g.clientTransports = map[string]string{}
	g.machineStates = map[string]model.MachineState{}
	g.users = map[string]User{legacyUser: {ID: legacyUser, Name: "legacy", CreatedAt: time.Now().UTC()}}
	g.owners = map[string]string{}
	g.keys = map[string]keyRecord{}
	g.nodes = map[string]model.Node{}
	g.installations = map[string]installation{}
	for name, dst := range map[string]any{"nodes.json": &g.nodes, "keys.json": &g.keys, "installations.json": &g.installations} {
		if err := store.Read(filepath.Join(dir, name), dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("migrate %s: %w", name, err)
		}
	}
	for id, n := range g.nodes {
		n.UserID = legacyUser
		g.nodes[id] = n
		g.owners[id] = legacyUser
	}
	for id, k := range g.keys {
		k.UserID = legacyUser
		g.keys[id] = k
	}
	for hash, i := range g.installations {
		i.UserID = legacyUser
		g.installations[hash] = i
		if i.RedeemedID != "" {
			g.owners[i.RedeemedID] = legacyUser
		}
	}
	return nil
}

// persist rewrites every record. It runs only during startup migration; request
// paths commit the records they change with commit.
// Caller holds g.mu, except during initialization before serving requests.
func (g *Gateway) persist() error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = g.writeState(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	g.reindexCredentialsLocked()
	return nil
}

// change names the records one mutation touched. commit writes each named
// record from the in-memory cache, or deletes it when the cache no longer holds
// it. Identity, client-owner, and transport bindings are insert-only and keep
// their immutability checks.
type change struct {
	users, identities, clients, transports  []string
	nodes, keys, invitations, machineStates []string
}

// commit makes one mutation durable in a single transaction before the caller
// acknowledges it. Its cost is proportional to the change, not to the gateway's
// accumulated state. Caller holds g.mu.
func (g *Gateway) commit(c change) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = g.writeChange(tx, c); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(c.users)+len(c.keys)+len(c.invitations) != 0 {
		g.reindexCredentialsLocked()
	}
	return nil
}

func (g *Gateway) writeChange(tx *sql.Tx, c change) error {
	exec := func(query string, args ...any) error {
		_, err := tx.Exec(query, args...)
		return err
	}
	// Deletions precede inserts so a released name can be reused in one change.
	for _, id := range c.nodes {
		if _, ok := g.nodes[id]; !ok {
			if err := exec(`DELETE FROM nodes WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, id := range c.keys {
		if _, ok := g.keys[id]; !ok {
			if err := exec(`DELETE FROM credentials WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, hash := range c.invitations {
		if _, ok := g.installations[hash]; !ok {
			if err := exec(`DELETE FROM invitations WHERE hash=?`, hash); err != nil {
				return err
			}
		}
	}
	for _, id := range c.machineStates {
		if _, ok := g.machineStates[id]; !ok {
			if err := exec(`DELETE FROM machine_states WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, id := range c.users {
		u, ok := g.users[id]
		if !ok {
			continue
		}
		if err := exec(`INSERT INTO users(id,name,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,data=excluded.data`, u.ID, u.Name, model.JSON(u)); err != nil {
			return err
		}
	}
	for _, id := range c.identities {
		if user := g.owners[id]; user != "" {
			if err := writeIdentity(tx, id, user); err != nil {
				return err
			}
		}
	}
	for _, id := range c.clients {
		if g.clientOwners[id] {
			if err := exec(`INSERT INTO client_identities(id,data) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, model.JSON(true)); err != nil {
				return err
			}
		}
	}
	for _, id := range c.transports {
		if owner := g.clientTransports[id]; owner != "" {
			if err := writeTransport(tx, id, owner); err != nil {
				return err
			}
		}
	}
	for _, id := range c.nodes {
		if n, ok := g.nodes[id]; ok {
			if err := exec(`INSERT INTO nodes(id,user_id,name,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,name=excluded.name,data=excluded.data`, n.ID, n.UserID, n.Name, model.JSON(n)); err != nil {
				return err
			}
		}
	}
	for _, id := range c.keys {
		if k, ok := g.keys[id]; ok {
			if err := exec(`INSERT INTO credentials(id,user_id,hash,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,hash=excluded.hash,data=excluded.data`, k.ID, k.UserID, k.Hash, model.JSON(k)); err != nil {
				return err
			}
		}
	}
	for _, hash := range c.invitations {
		if i, ok := g.installations[hash]; ok {
			if err := exec(`INSERT INTO invitations(hash,user_id,data) VALUES(?,?,?) ON CONFLICT(hash) DO UPDATE SET user_id=excluded.user_id,data=excluded.data`, hash, i.UserID, model.JSON(i)); err != nil {
				return err
			}
		}
	}
	for _, id := range c.machineStates {
		if state, ok := g.machineStates[id]; ok {
			if err := exec(`INSERT INTO machine_states(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, id, model.JSON(state)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeIdentity(tx *sql.Tx, id, user string) error {
	if _, err := tx.Exec(`INSERT INTO identities(id,user_id) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, user); err != nil {
		return err
	}
	var stored string
	if err := tx.QueryRow(`SELECT user_id FROM identities WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	if stored != user {
		return errors.New("identity ownership is immutable")
	}
	return nil
}

func writeTransport(tx *sql.Tx, id, owner string) error {
	if _, err := tx.Exec(`INSERT INTO client_transports(id,owner_id,data) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`, id, owner, model.JSON(owner)); err != nil {
		return err
	}
	var stored string
	if err := tx.QueryRow(`SELECT owner_id FROM client_transports WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	if stored != owner {
		return errors.New("client transport ownership is immutable")
	}
	return nil
}

func (g *Gateway) writeState(tx *sql.Tx) error {
	for _, u := range g.users {
		if _, err := tx.Exec(`INSERT INTO users(id,name,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,data=excluded.data`, u.ID, u.Name, model.JSON(u)); err != nil {
			return err
		}
	}
	for id, user := range g.owners {
		if err := writeIdentity(tx, id, user); err != nil {
			return err
		}
	}
	for id, client := range g.clientOwners {
		if _, err := tx.Exec(`INSERT INTO client_identities(id,data) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, model.JSON(client)); err != nil {
			return err
		}
	}
	for id, owner := range g.clientTransports {
		if err := writeTransport(tx, id, owner); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM nodes; DELETE FROM credentials; DELETE FROM invitations;`); err != nil {
		return err
	}
	for _, n := range g.nodes {
		if _, err := tx.Exec(`INSERT INTO nodes(id,user_id,name,data) VALUES(?,?,?,?)`, n.ID, n.UserID, n.Name, model.JSON(n)); err != nil {
			return err
		}
	}
	for _, k := range g.keys {
		if _, err := tx.Exec(`INSERT INTO credentials(id,user_id,hash,data) VALUES(?,?,?,?)`, k.ID, k.UserID, k.Hash, model.JSON(k)); err != nil {
			return err
		}
	}
	for hash, i := range g.installations {
		if _, err := tx.Exec(`INSERT INTO invitations(hash,user_id,data) VALUES(?,?,?)`, hash, i.UserID, model.JSON(i)); err != nil {
			return err
		}
	}
	for id, state := range g.machineStates {
		if _, err := tx.Exec(`INSERT INTO machine_states(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, id, model.JSON(state)); err != nil {
			return err
		}
	}
	return nil
}
