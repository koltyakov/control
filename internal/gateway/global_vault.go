package gateway

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// No row means the operator has never configured the gateway-wide lock.
// This table is created by the same migration as the per-fleet policy.
const globalVaultSchema = `
CREATE TABLE IF NOT EXISTS global_vault (
 id INTEGER PRIMARY KEY CHECK(id=1),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 paused_until_ns INTEGER NOT NULL CHECK(paused_until_ns>=0 AND (enabled=1 OR paused_until_ns=0)),
 revision INTEGER NOT NULL CHECK(revision>0)
);`

type globalVaultState struct {
	Enabled     bool      `json:"enabled"`
	PausedUntil time.Time `json:"pausedUntil,omitzero"`
	Revision    int64     `json:"revision"`
}

func (p globalVaultState) active(now time.Time) bool {
	return p.Enabled && !now.Before(p.PausedUntil)
}

func (g *Gateway) loadGlobalVault() error {
	var p globalVaultState
	var expiry int64
	err := g.db.QueryRow("SELECT enabled,paused_until_ns,revision FROM global_vault WHERE id=1").Scan(&p.Enabled, &expiry, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read gateway vault policy: %w", err)
	}
	if expiry != 0 {
		p.PausedUntil = time.Unix(0, expiry).UTC()
	}
	g.globalVault = p
	return nil
}

func (g *Gateway) globalVaultRoutes(mux *http.ServeMux) {
	handler := g.consoleHeaders(g.consoleGuard(http.HandlerFunc(g.admin(g.globalVaultHTTP))))
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		mux.Handle(method+" /console/api/admin/vault", handler)
	}
}

func (g *Gateway) globalVaultHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		g.mu.Lock()
		p := g.globalVault
		g.mu.Unlock()
		consoleJSON(w, p)
		return
	}
	var q struct {
		Enabled      *bool  `json:"enabled"`
		Revision     *int64 `json:"revision"`
		PauseSeconds int64  `json:"pauseSeconds"`
	}
	if !decodeStrict(w, r, 4096, &q) || q.Enabled == nil || q.Revision == nil || *q.Revision < 0 ||
		q.PauseSeconds < 0 || q.PauseSeconds > maxFleetVaultSeconds || (!*q.Enabled && q.PauseSeconds != 0) {
		http.Error(w, "supply revision, enabled, and an optional pause of 1 second to 365 days for an enabled lock", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	p, status, err := g.setGlobalVaultLocked(r, *q.Revision, *q.Enabled, q.PauseSeconds)
	g.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	consoleJSON(w, p)
}

func (g *Gateway) setGlobalVaultLocked(r *http.Request, revision int64, enabled bool, pauseSeconds int64) (globalVaultState, int, error) {
	if !g.consolePolicySessionLiveLocked(r) {
		return globalVaultState{}, http.StatusUnauthorized, errors.New("session expired; sign in again")
	}
	if revision != g.globalVault.Revision {
		return globalVaultState{}, http.StatusConflict, errors.New("gateway lock changed; reload before editing")
	}
	if enabled {
		var incompatible int
		for _, n := range g.nodes {
			if !n.FleetVaultCheck {
				incompatible++
			}
		}
		if incompatible != 0 {
			return globalVaultState{}, http.StatusConflict, fmt.Errorf("registered workers without fleet-lock support: %d; upgrade and reconnect them before enabling the gateway lock", incompatible)
		}
	}
	p := globalVaultState{Enabled: enabled, Revision: revision + 1}
	var expiry int64
	if pauseSeconds != 0 {
		p.PausedUntil = time.Now().UTC().Add(time.Duration(pauseSeconds) * time.Second)
		expiry = p.PausedUntil.UnixNano()
	}
	// Commit the operator action and its audit entry together. Requests can
	// observe the new policy only after the database accepts it.
	tx, err := g.db.BeginTx(r.Context(), nil)
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(`INSERT INTO global_vault(id,enabled,paused_until_ns,revision) VALUES(1,?,?,?)
ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled,paused_until_ns=excluded.paused_until_ns,revision=excluded.revision`, p.Enabled, expiry, p.Revision)
	}
	if err == nil {
		detail := fmt.Sprintf("enabled=%t pauseSeconds=%d", p.Enabled, pauseSeconds)
		err = g.writeAudit(tx, []AuditEvent{g.auditEventLocked(r, legacyUser, "vault.gateway", "gateway", "", detail)})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return globalVaultState{}, http.StatusInternalServerError, errors.New("could not persist gateway lock")
	}
	g.globalVault = p
	return p, http.StatusOK, nil
}
