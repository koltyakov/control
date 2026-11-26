package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/koltyakov/control/internal/model"
	_ "modernc.org/sqlite"
)

const legacyUser = "legacy"

// SQLite is authoritative. Maps are a process-local cache protected by g.mu;
// each mutation commits before its HTTP/registration acknowledgement.
const databasePragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"

func (g *Gateway) openDatabase(dir string) error {
	path := filepath.Join(dir, "gateway.db")
	// The driver applies DSN pragmas to every connection, so a replaced pooled
	// connection keeps foreign keys, durability and busy handling.
	if strings.Contains(path, "?") {
		return errors.New("gateway state directory must not contain '?'")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	g.db, err = sql.Open("sqlite", path+"?"+databasePragmas)
	if err != nil {
		return err
	}
	g.db.SetMaxOpenConns(1)
	if err = g.db.Ping(); err != nil {
		return err
	}
	var version int
	if err = g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case version > currentSchemaVersion:
		return errors.New("gateway database is newer than this executable")
	case version == 0:
		if err = g.createSchema(dir); err != nil {
			return err
		}
		version = baseSchemaVersion
	case version < baseSchemaVersion:
		return errPreBaseState(fmt.Sprintf("gateway database schema %d", version))
	}
	for ; version < currentSchemaVersion; version++ {
		if err = g.upgradeSchema(version); err != nil {
			return fmt.Errorf("upgrade gateway database to schema %d: %w", version+1, err)
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
	g.proxyDependencies = map[string]proxyDependency{}
	g.labelOverlays = map[string]labelOverlay{}
	g.vaultPolicies = map[string]fleetVaultPolicy{}
	if err = readRecords(g.db, "(SELECT * FROM client_identities ORDER BY id DESC LIMIT 4096)", "id", g.clientOwners); err != nil {
		return err
	}
	if err = readRecords(g.db, "(SELECT * FROM client_transports ORDER BY id DESC LIMIT 4096)", "id", g.clientTransports); err != nil {
		return err
	}
	if err = readRecords(g.db, "machine_states", "id", g.machineStates); err != nil {
		return err
	}
	if err = readRecords(g.db, "proxy_dependencies", "id", g.proxyDependencies); err != nil {
		return err
	}
	if err = readRecords(g.db, "machine_labels", "id", g.labelOverlays); err != nil {
		return err
	}
	if err = readRecords(g.db, "fleet_vault", "user_id", g.vaultPolicies); err != nil {
		return err
	}
	for user, p := range g.vaultPolicies {
		p.UserID = user
		g.vaultPolicies[user] = p
	}
	if err = readRecords(g.db, "users", "id", g.users); err != nil {
		return err
	}
	if err = readRecords(g.db, "credentials", "id", g.keys); err != nil {
		return err
	}
	if err = g.loadKeyTimes(); err != nil {
		return err
	}
	if err = readRecords(g.db, "nodes", "id", g.nodes); err != nil {
		return err
	}
	if err = readRecords(g.db, "invitations", "hash", g.installations); err != nil {
		return err
	}
	rows, err := g.db.Query(`SELECT id,user_id FROM identities WHERE (id NOT IN (SELECT id FROM client_identities) AND id NOT IN (SELECT id FROM client_transports)) OR id IN (SELECT id FROM client_identities ORDER BY id DESC LIMIT 4096) OR id IN (SELECT id FROM client_transports ORDER BY id DESC LIMIT 4096)`)
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

// persist rewrites every record. It runs only for the startup client cleanup;
// request paths commit the records they change with commit.
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
	passwords, sessions                     []string
	// proxyDependencies are keyed by the child's identity.
	proxyDependencies []string
	// labels are owner label overlays, keyed by machine identity.
	labels []string
	// vaultPolicies are fleet lock policies, keyed by user.
	vaultPolicies []string
	// keyUses are last-used times in Unix milliseconds, keyed by access key.
	keyUses map[string]int64
	// audit events commit in the same transaction as the change they describe.
	audit []AuditEvent
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
	for id, seen := range c.keyUses {
		g.keyTimes[id].persisted = seen
	}
	if len(c.users)+len(c.keys)+len(c.invitations) != 0 {
		g.reindexCredentialsLocked()
	}
	g.publishObservations(slices.Concat(c.nodes, c.labels)...)
	g.publishProxyObservations(c)
	return nil
}

func (g *Gateway) writeChange(tx *sql.Tx, c change) error {
	exec := func(query string, args ...any) error {
		_, err := tx.Exec(query, args...)
		return err
	}
	if err := g.writeAudit(tx, c.audit); err != nil {
		return err
	}
	for id, seen := range c.keyUses {
		if err := exec(`INSERT INTO access_key_times(key_id,credential_hash,last_used_ms) VALUES(?,?,?) ON CONFLICT(key_id) DO UPDATE SET credential_hash=excluded.credential_hash,last_used_ms=excluded.last_used_ms`, id, g.keyTimes[id].credential, seen); err != nil {
			return err
		}
	}
	for _, username := range c.passwords {
		p := g.console.passwords[username]
		if err := exec(`INSERT INTO console_passwords(username,user_id,data) VALUES(?,?,?) ON CONFLICT(username) DO UPDATE SET data=excluded.data`, username, p.UserID, model.JSON(p)); err != nil {
			return err
		}
		if err := exec(`DELETE FROM console_sessions WHERE json_extract(data,'$.Username')=?`, username); err != nil {
			return err
		}
	}
	for _, hash := range c.sessions {
		if s, ok := g.console.sessions[hash]; ok {
			if err := exec(`INSERT INTO console_sessions(hash,data) VALUES(?,?) ON CONFLICT(hash) DO UPDATE SET data=excluded.data`, hash, model.JSON(s)); err != nil {
				return err
			}
		} else if err := exec(`DELETE FROM console_sessions WHERE hash=?`, hash); err != nil {
			return err
		}
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
		if _, exists := g.keys[id]; !exists {
			if err := exec(`DELETE FROM access_key_times WHERE key_id=?`, id); err != nil {
				return err
			}
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
	for _, id := range c.proxyDependencies {
		if _, ok := g.proxyDependencies[id]; !ok {
			if err := exec(`DELETE FROM proxy_dependencies WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, id := range c.labels {
		if _, ok := g.labelOverlays[id]; !ok {
			if err := exec(`DELETE FROM machine_labels WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for _, id := range c.vaultPolicies {
		if _, ok := g.vaultPolicies[id]; !ok {
			if err := exec(`DELETE FROM fleet_vault WHERE user_id=?`, id); err != nil {
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
	for _, id := range c.proxyDependencies {
		if d, ok := g.proxyDependencies[id]; ok {
			if err := exec(`INSERT INTO proxy_dependencies(id,user_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,data=excluded.data`, id, d.UserID, model.JSON(d)); err != nil {
				return err
			}
		}
	}
	for _, id := range c.labels {
		if o, ok := g.labelOverlays[id]; ok {
			if err := exec(`INSERT INTO machine_labels(id,user_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,data=excluded.data`, id, o.UserID, model.JSON(o)); err != nil {
				return err
			}
		}
	}
	// Users are written above, so a policy's fleet reference is satisfied.
	for _, id := range c.vaultPolicies {
		if p, ok := g.vaultPolicies[id]; ok {
			if err := exec(`INSERT INTO fleet_vault(user_id,data) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data`, id, model.JSON(p)); err != nil {
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
	for id, d := range g.proxyDependencies {
		if _, err := tx.Exec(`INSERT INTO proxy_dependencies(id,user_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,data=excluded.data`, id, d.UserID, model.JSON(d)); err != nil {
			return err
		}
	}
	for id, o := range g.labelOverlays {
		if _, err := tx.Exec(`INSERT INTO machine_labels(id,user_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET user_id=excluded.user_id,data=excluded.data`, id, o.UserID, model.JSON(o)); err != nil {
			return err
		}
	}
	for id, p := range g.vaultPolicies {
		if _, err := tx.Exec(`INSERT INTO fleet_vault(user_id,data) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data`, id, model.JSON(p)); err != nil {
			return err
		}
	}
	return nil
}
