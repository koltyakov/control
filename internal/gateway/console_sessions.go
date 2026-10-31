package gateway

import (
	"errors"
	"time"
)

const (
	consoleSessionIdleTTL = 24 * time.Hour
	consoleSessionTTL     = 7 * 24 * time.Hour
)

const consoleSessionSchema = `
CREATE TABLE console_sessions (hash TEXT PRIMARY KEY, data BLOB NOT NULL);
`

// Caller holds g.mu, except during startup before requests are served.
func (g *Gateway) validConsoleSession(s consoleSession, now time.Time) bool {
	p, exists := g.console.passwords[s.Username]
	u := g.users[p.UserID]
	return exists && p.Revision == s.Revision && !u.Disabled && u.ID != "" &&
		(p.Role == "user" || (p.Role == "superuser" && g.superuser != "")) &&
		!s.Created.IsZero() && !s.Seen.IsZero() &&
		now.Sub(s.Seen) < consoleSessionIdleTTL && now.Sub(s.Created) < consoleSessionTTL
}

func (g *Gateway) loadConsoleSessions() error {
	if err := readRecords(g.db, "(SELECT * FROM console_sessions LIMIT 4097)", "hash", g.console.sessions); err != nil {
		return err
	}
	if len(g.console.sessions) > 4096 {
		return errors.New("gateway console session limit exceeded")
	}
	var removed []string
	now := time.Now()
	for key, session := range g.console.sessions {
		if !g.validConsoleSession(session, now) {
			delete(g.console.sessions, key)
			removed = append(removed, key)
			continue
		}
		session.saved = session.Seen
		g.console.sessions[key] = session
	}
	if len(removed) > 0 {
		return g.commit(change{sessions: removed})
	}
	return nil
}

// Graceful restarts save the latest activity without extending absolute expiry.
// A crash can lose less than a minute of activity since the last checkpoint.
// Caller holds g.mu.
func (g *Gateway) checkpointConsoleSessions() error {
	var changed []string
	for key, session := range g.console.sessions {
		if session.Seen.After(session.saved) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return g.commit(change{sessions: changed})
}
