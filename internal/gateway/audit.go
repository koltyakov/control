package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

// AuditEvent records one management action or browser sign-in. It never
// contains passwords, tokens, installation links, or execution arguments.
type AuditEvent struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	UserID string    `json:"userId"`
	// Fleet is the account name for UserID, resolved when events are listed.
	Fleet string `json:"fleet,omitempty"`
	// Actor is a browser login, an account name for its key, or "@operator".
	Actor    string `json:"actor"`
	Via      string `json:"via"`
	KeyID    string `json:"keyId,omitempty"`
	Action   string `json:"action"`
	Target   string `json:"target,omitempty"`
	TargetID string `json:"targetId,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// SourceIP is the direct TCP peer. Behind a reverse proxy it is the proxy.
	SourceIP string `json:"sourceIp,omitempty"`
}

// IF NOT EXISTS keeps the migration idempotent for state rewound by tests or
// restored from an older backup that already carried the table.
const auditSchema = `CREATE TABLE IF NOT EXISTS audit_events (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT NOT NULL, action TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS audit_user ON audit_events(user_id, id);
CREATE INDEX IF NOT EXISTS audit_action ON audit_events(action, id);`

// Retention keeps the newest events. Each fleet has its own budget so one
// tenant's activity cannot push out another fleet's history; the gateway-wide
// ceiling bounds the database. Failed sign-ins have their own smaller budget
// so a password-guessing burst cannot push out management history.
const (
	maxAuditEvents      = 100000
	maxAuditFleetEvents = 10000
	maxAuditFailures    = 2000
)

const auditSignInFailed = "session.signin_failed"

var auditCategories = map[string]bool{"session": true, "key": true, "invitation": true, "machine": true, "account": true, "update": true, "vault": true}

// auditPruneSlack is how many events may be written between retention passes.
// Each pass walks up to a budget of index entries, so pruning on every
// audited commit cost far more than the insert; budgets may be exceeded by at
// most this many events.
const auditPruneSlack = 256

// writeAudit inserts events and periodically enforces retention. Caller holds
// g.mu. The fleets and failure flag accumulate across commits until a pass.
func (g *Gateway) writeAudit(tx *sql.Tx, events []AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	if g.auditPruneFleets == nil {
		g.auditPruneFleets = map[string]bool{}
	}
	for _, e := range events {
		g.auditPruneFailed = g.auditPruneFailed || e.Action == auditSignInFailed
		if len(g.auditPruneFleets) < 4096 {
			g.auditPruneFleets[e.UserID] = true
		}
		if _, err := tx.Exec(`INSERT INTO audit_events(user_id,action,data) VALUES(?,?,?)`, e.UserID, e.Action, model.JSON(e)); err != nil {
			return err
		}
	}
	g.auditPending += len(events)
	if g.auditPending < auditPruneSlack {
		return nil
	}
	fleets, failed := g.auditPruneFleets, g.auditPruneFailed
	g.auditPending, g.auditPruneFleets, g.auditPruneFailed = 0, nil, false
	for fleet := range fleets {
		if _, err := tx.Exec(`DELETE FROM audit_events WHERE user_id=? AND id <= (SELECT id FROM audit_events WHERE user_id=? ORDER BY id DESC LIMIT 1 OFFSET ?)`, fleet, fleet, maxAuditFleetEvents); err != nil {
			return err
		}
	}
	if failed {
		if _, err := tx.Exec(`DELETE FROM audit_events WHERE action=? AND id <= (SELECT id FROM audit_events WHERE action=? ORDER BY id DESC LIMIT 1 OFFSET ?)`, auditSignInFailed, auditSignInFailed, maxAuditFailures); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM audit_events WHERE id <= (SELECT id FROM audit_events ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxAuditEvents)
	return err
}

func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// auditEventLocked attributes an action to the request's browser login or
// bearer key. fleet selects whose history holds it; empty means the caller's.
// Caller holds g.mu.
func (g *Gateway) auditEventLocked(r *http.Request, fleet, action, target, targetID, detail string) AuditEvent {
	p := g.requestPrincipal(r)
	e := AuditEvent{At: time.Now().UTC(), UserID: fleet, Action: action, Target: clip(target, 128), TargetID: clip(targetID, 128), Detail: clip(detail, 256), SourceIP: sourceIP(r)}
	if e.UserID == "" {
		e.UserID = p.UserID
	}
	if c, ok := r.Context().Value(consoleContextKey{}).(consoleContext); ok {
		e.Actor, e.Via = c.Username, "browser"
		return e
	}
	e.KeyID = p.KeyID
	switch p.Role {
	case "superuser":
		e.Actor, e.Via = "@operator", "superuser key"
	case "user":
		e.Actor, e.Via = g.users[p.UserID].Name, "account key"
	default:
		e.Actor, e.Via = g.users[p.UserID].Name, p.Role+" key"
	}
	return e
}

// listAudit returns newest-first pages. Account owners see their fleet; the
// operator sees every fleet, including failed sign-ins for unknown names.
func (g *Gateway) listAudit(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	before, _ := strconv.ParseInt(query.Get("before"), 10, 64)
	category := query.Get("category")
	if category != "" && !auditCategories[category] {
		http.Error(w, "unknown category", http.StatusBadRequest)
		return
	}
	fleet := p.UserID
	if p.Role == "superuser" {
		fleet = ""
	}
	// Separate statements let tenant pages use the (user_id, id) index.
	// A category is a fixed prefix; a range on it can use the action index.
	statement, args := `SELECT id, data FROM audit_events WHERE (?=0 OR id<?) AND (?='' OR (action>=? AND action<?)) ORDER BY id DESC LIMIT ?`, []any{before, before, category, category + ".", category + "/", limit + 1}
	if fleet != "" {
		statement, args = `SELECT id, data FROM audit_events WHERE user_id=? AND (?=0 OR id<?) AND (?='' OR (action>=? AND action<?)) ORDER BY id DESC LIMIT ?`, append([]any{fleet}, args...)
	}
	rows, err := g.db.QueryContext(r.Context(), statement, args...)
	if err != nil {
		http.Error(w, "could not read audit log", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	events := []AuditEvent{}
	for rows.Next() {
		var id int64
		var data []byte
		var e AuditEvent
		if err := rows.Scan(&id, &data); err != nil || json.Unmarshal(data, &e) != nil {
			http.Error(w, "could not read audit log", http.StatusInternalServerError)
			return
		}
		e.ID = id
		events = append(events, e)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read audit log", http.StatusInternalServerError)
		return
	}
	var next int64
	if len(events) > limit {
		events = events[:limit]
		next = events[limit-1].ID
	}
	if p.Role == "superuser" {
		g.mu.Lock()
		for i := range events {
			events[i].Fleet = g.users[events[i].UserID].Name
		}
		g.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "next": next})
}

// clearAudit deletes events, optionally only those before a time. Account
// owners clear their fleet; the operator clears every fleet. Each affected
// fleet keeps one record of the clearing, written in the same transaction, so
// history cannot disappear without a trace.
func (g *Gateway) clearAudit(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	cutoff, detail := "", "removed all events"
	if value := r.URL.Query().Get("before"); value != "" {
		before, err := time.Parse(time.RFC3339, value)
		if err != nil {
			http.Error(w, "before must be an RFC 3339 time", http.StatusBadRequest)
			return
		}
		cutoff = before.UTC().Format(time.RFC3339Nano)
		detail = "removed events before " + before.UTC().Format("2006-01-02 15:04 MST")
	}
	fleet := p.UserID
	if p.Role == "superuser" {
		fleet = ""
	}
	// Compare instants, not RFC 3339 strings: trimmed fractional seconds make
	// lexical order wrong within the cutoff second. One DELETE ... RETURNING
	// pass finds the affected fleets and removes their events.
	const scope = `(?='' OR user_id=?) AND (?='' OR julianday(json_extract(CAST(data AS TEXT),'$.at')) < julianday(?))`
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "could not clear audit log", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()
	fleets := []string{}
	var removed int64
	rows, err := tx.Query(`DELETE FROM audit_events WHERE `+scope+` RETURNING user_id`, fleet, fleet, cutoff, cutoff)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			removed++
			if !seen[id] {
				seen[id] = true
				fleets = append(fleets, id)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
	}
	if err == nil && removed > 0 {
		events := []AuditEvent{}
		for _, id := range fleets {
			events = append(events, g.auditEventLocked(r, id, "audit.clear", "", "", fmt.Sprintf("%s (%d)", detail, removed)))
		}
		err = g.writeAudit(tx, events)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, "could not clear audit log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"removed": removed})
}

func (g *Gateway) auditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/audit", g.fleetAdmin(g.listAudit))
	mux.HandleFunc("DELETE /v1/audit", g.fleetAdmin(g.clearAudit))
}

func osName(os string) string {
	switch os {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return os
}

// signInEvent describes a browser sign-in attempt. These requests carry no
// session yet, so the attempted login is the actor. Unknown logins belong to
// the operator's history. Caller holds g.mu.
func signInEvent(r *http.Request, username, userID string, ok bool) AuditEvent {
	e := AuditEvent{At: time.Now().UTC(), UserID: userID, Actor: clip(username, 64), Via: "browser", Action: "session.signin", SourceIP: sourceIP(r)}
	if !ok {
		e.Action = auditSignInFailed
	}
	if e.UserID == "" {
		e.UserID = legacyUser
	}
	return e
}

// Deployments persist in the update store, not SQLite, so their record follows
// the action and a failed write is logged rather than undoing the rollout.
func (g *Gateway) recordUpdateAudit(r *http.Request, action string, d *update.Deployment) {
	target, id, detail := "", "", "no newer release"
	if d != nil {
		target, id, detail = d.Manifest.Version, d.ID, d.Source
	}
	g.mu.Lock()
	err := g.commit(change{audit: []AuditEvent{g.auditEventLocked(r, legacyUser, action, target, id, detail)}})
	g.mu.Unlock()
	if err != nil {
		slog.Warn("record update audit event", "action", action, "error", err)
	}
}

// actionVerb names a boolean state change for audit actions.
func actionVerb(enabled bool, on, off string) string {
	if enabled {
		return on
	}
	return off
}
