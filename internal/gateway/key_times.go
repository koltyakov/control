package gateway

import "time"

// Key IDs outlive their first access. Use the credential's digest as a
// generation marker so rotating a gateway-supplied key starts a new interval.
const keyTimeSchema = `CREATE TABLE IF NOT EXISTS access_key_times (target_id TEXT PRIMARY KEY, credential TEXT NOT NULL, seen INTEGER NOT NULL);`

func (g *Gateway) loadKeyTimes() error {
	g.keyTimes = make(map[string]*keyTime)
	rows, err := g.db.Query(`SELECT target_id, credential, seen FROM access_key_times`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, target string
		var seen int64
		if err := rows.Scan(&id, &target, &seen); err != nil {
			return err
		}
		k := &keyTime{credential: target, persisted: seen}
		k.last.Store(seen)
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
