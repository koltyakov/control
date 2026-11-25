package gateway

import "time"

// keyTimeSchema records each access key's last successful authentication.
// key_id also covers the built-in superuser and bootstrap keys, so it has no
// foreign key to credentials. Key IDs outlive a rotation of a gateway-supplied
// key; credential_hash marks the generation so a new secret starts a new
// interval.
const keyTimeSchema = `
CREATE TABLE access_key_times (key_id TEXT PRIMARY KEY, credential_hash TEXT NOT NULL, last_used_ms INTEGER NOT NULL);
`

func (g *Gateway) loadKeyTimes() error {
	g.keyTimes = make(map[string]*keyTime)
	rows, err := g.db.Query(`SELECT key_id, credential_hash, last_used_ms FROM access_key_times`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, credential string
		var lastUsed int64
		if err := rows.Scan(&id, &credential, &lastUsed); err != nil {
			return err
		}
		k := &keyTime{credential: credential, persisted: lastUsed}
		k.last.Store(lastUsed)
		g.keyTimes[id] = k
	}
	return rows.Err()
}

// Only mark a timestamp persisted after the enclosing transaction commits.
func (g *Gateway) pendingKeyUses() map[string]int64 {
	var seenKeys map[string]int64
	for id, key := range g.keyTimes {
		seen := key.last.Load()
		if seen <= key.persisted {
			continue
		}
		if seenKeys == nil {
			seenKeys = make(map[string]int64)
		}
		seenKeys[id] = seen
	}
	return seenKeys
}

func (g *Gateway) keyLastUsed(id string) time.Time {
	if t := g.keyTimes[id]; t != nil {
		if seen := t.last.Load(); seen > 0 {
			return time.UnixMilli(seen).UTC()
		}
	}
	return time.Time{}
}
