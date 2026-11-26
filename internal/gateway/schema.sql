-- Gateway database schema, version 10 (baseSchemaVersion). A new database is
-- created from this file; later changes are appended to schemaUpgrades.
-- Records are JSON in data columns; key columns repeat the fields SQLite must
-- index, join, or constrain.

-- Accounts and their fleet-scoped credentials. The operator fleet is the
-- "legacy" user, created with the database.
CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, data BLOB NOT NULL);
CREATE TABLE credentials (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), hash TEXT NOT NULL UNIQUE, data BLOB NOT NULL);
CREATE INDEX credentials_user ON credentials(user_id);
-- Each access key's last successful authentication. key_id also covers the
-- built-in superuser and bootstrap keys, so it has no foreign key.
-- credential_hash marks the secret's generation, so rotating a
-- gateway-supplied key starts a new interval.
CREATE TABLE access_key_times (key_id TEXT PRIMARY KEY, credential_hash TEXT NOT NULL, last_used_ms INTEGER NOT NULL);

-- Permanent identity ownership. A row is never reassigned to another fleet.
CREATE TABLE identities (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), UNIQUE(id,user_id));
-- Registered machines; the composite key keeps a machine in its identity's fleet.
CREATE TABLE nodes (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL, data BLOB NOT NULL, UNIQUE(user_id,name), FOREIGN KEY(id,user_id) REFERENCES identities(id,user_id));
-- Owner lifecycle decisions (disable, unregister, rename) by machine identity.
CREATE TABLE machine_states (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL);
-- Fleet owners' label overlays, kept apart from the signed registration.
CREATE TABLE machine_labels (id TEXT PRIMARY KEY REFERENCES identities(id), user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
-- Managed proxy listener dependencies of enrolled and retiring children,
-- keyed by the child's identity.
CREATE TABLE proxy_dependencies (id TEXT PRIMARY KEY REFERENCES identities(id), user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
CREATE INDEX proxy_dependencies_user ON proxy_dependencies(user_id);

-- Stable account-client owners, and the immutable binding of each transport
-- identity to its owner. Both are insert-only.
CREATE TABLE client_identities (id TEXT PRIMARY KEY REFERENCES identities(id), data BLOB NOT NULL);
CREATE INDEX client_identity_prefix ON client_identities(substr(id,1,16));
CREATE TABLE client_transports (id TEXT PRIMARY KEY REFERENCES identities(id), owner_id TEXT NOT NULL REFERENCES client_identities(id), data BLOB NOT NULL);

-- Installation invitations by ticket digest.
CREATE TABLE invitations (hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
CREATE INDEX invitations_user ON invitations(user_id);

-- Web console passwords (salted Argon2id) and sessions by cookie digest.
CREATE TABLE console_passwords (username TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
CREATE TABLE console_sessions (hash TEXT PRIMARY KEY, data BLOB NOT NULL);

-- Bounded per-fleet history of sign-ins and management changes.
CREATE TABLE audit_events (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT NOT NULL, action TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX audit_user ON audit_events(user_id, id);
CREATE INDEX audit_action ON audit_events(action, id);

-- Each fleet's opt-in lock: enabled, an optional pause, and key allowances.
-- No row means an unlocked fleet.
CREATE TABLE fleet_vault (user_id TEXT PRIMARY KEY REFERENCES users(id), data BLOB NOT NULL);
-- The operator's gateway-wide lock. No row means it was never configured.
CREATE TABLE global_vault (
	id INTEGER PRIMARY KEY CHECK(id=1),
	enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
	paused_until_ns INTEGER NOT NULL CHECK(paused_until_ns>=0 AND (enabled=1 OR paused_until_ns=0)),
	revision INTEGER NOT NULL CHECK(revision>0)
);
