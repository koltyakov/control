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
	if version > 2 {
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
PRAGMA user_version=2;`); err != nil {
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
	}
	g.users = map[string]User{}
	g.owners = map[string]string{}
	g.keys = map[string]keyRecord{}
	g.nodes = map[string]model.Node{}
	g.installations = map[string]installation{}
	g.machineStates = map[string]model.MachineState{}
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
	return tx.Commit()
}

func (g *Gateway) writeState(tx *sql.Tx) error {
	for _, u := range g.users {
		if _, err := tx.Exec(`INSERT INTO users(id,name,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,data=excluded.data`, u.ID, u.Name, model.JSON(u)); err != nil {
			return err
		}
	}
	for id, user := range g.owners {
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
