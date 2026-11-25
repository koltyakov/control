package gateway

import "time"

// Keep historical client observations separate from the fleet's access keys.
const clientProfileSchema = `
CREATE TABLE IF NOT EXISTS client_profiles (
	owner_id TEXT PRIMARY KEY REFERENCES client_identities(id),
	label TEXT NOT NULL DEFAULT '', last_seen_ms INTEGER NOT NULL DEFAULT 0
);`

func (g *Gateway) noteProfileSeenLocked(id string, when time.Time) {
	if g.profileSeenDirty == nil {
		g.profileSeenDirty = make(map[string]time.Time)
	}
	g.profileSeenDirty[id] = when
}
