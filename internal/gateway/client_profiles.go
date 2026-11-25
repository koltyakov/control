package gateway

import (
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
)

// Labels and observations describe a stable owner. They are not part of its
// signed identity or its permissions. Pre-upgrade owners have no observation.
const clientProfileSchema = `
CREATE TABLE IF NOT EXISTS client_profiles (
	owner_id TEXT PRIMARY KEY REFERENCES client_identities(id),
	label TEXT NOT NULL DEFAULT '', last_seen_ms INTEGER NOT NULL DEFAULT 0
);`

type clientProfileLabel struct{ id, name string }

type clientProfileChoice struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"` // Optional fleet-owner label; empty for an unnamed profile.
	LastSeen time.Time `json:"lastSeen,omitzero"`
	Online   bool      `json:"online"`
}

func (g *Gateway) noteProfileSeenLocked(id string, when time.Time) {
	if g.profileSeenDirty == nil {
		g.profileSeenDirty = make(map[string]time.Time)
	}
	g.profileSeenDirty[id] = when
}

func (g *Gateway) clientProfilesLocked(user string) ([]clientProfileChoice, error) {
	online := make(map[string]bool)
	for id, n := range g.clients {
		if n.UserID == user {
			if n.ClientOwner != "" {
				id = n.ClientOwner
			}
			online[id] = true
		}
	}
	// Apply the limit after prioritizing connected profiles. A long-running
	// client must not disappear behind a thousand newer, disconnected owners.
	rows, err := g.db.Query(`WITH live(id) AS (SELECT key FROM json_each(?))
		SELECT c.id, coalesce(p.label,''), coalesce(p.last_seen_ms,0)
		FROM client_identities c JOIN identities i ON i.id=c.id LEFT JOIN client_profiles p ON p.owner_id=c.id
		WHERE i.user_id=? AND c.id NOT IN (SELECT id FROM client_transports)
		ORDER BY c.id IN (SELECT id FROM live) DESC, coalesce(p.last_seen_ms,0) DESC, c.id LIMIT ?`, string(model.JSON(online)), user, maxFleetVaultHosts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	profiles := []clientProfileChoice{}
	for rows.Next() {
		var item clientProfileChoice
		var millis int64
		if err := rows.Scan(&item.ID, &item.Name, &millis); err != nil {
			return nil, err
		}
		if millis != 0 {
			item.LastSeen = time.UnixMilli(millis).UTC()
		}
		if seen := g.profileSeenDirty[item.ID]; seen.After(item.LastSeen) {
			item.LastSeen = seen
		}
		item.Online = online[item.ID]
		profiles = append(profiles, item)
	}
	sort.SliceStable(profiles, func(i, j int) bool {
		if profiles[i].Online != profiles[j].Online {
			return profiles[i].Online
		}
		return profiles[i].LastSeen.After(profiles[j].LastSeen)
	})
	return profiles, rows.Err()
}

func (g *Gateway) labelClientProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var q struct {
		Label    *string `json:"label"`
		Previous *string `json:"previousLabel"`
	}
	if !decodeStrict(w, r, 2048, &q) || q.Label == nil || q.Previous == nil {
		http.Error(w, "supply label and previousLabel", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(*q.Label)
	if utf8.RuneCountInString(label) > 64 || strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		http.Error(w, "use at most 64 printable characters for the profile label", http.StatusBadRequest)
		return
	}
	p := g.requestPrincipal(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.consolePolicySessionLiveLocked(r) {
		http.Error(w, "session expired; sign in again", http.StatusUnauthorized)
		return
	}
	ok, err := g.vaultClientOwnerLocked(p.UserID, id)
	if err != nil {
		http.Error(w, "could not verify client profile", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "client profile not found", http.StatusNotFound)
		return
	}
	var current string
	if err := g.db.QueryRow(`SELECT coalesce((SELECT label FROM client_profiles WHERE owner_id=?),'')`, id).Scan(&current); err != nil {
		http.Error(w, "could not read profile label", http.StatusInternalServerError)
		return
	}
	if current != *q.Previous {
		http.Error(w, "profile label changed; reload before editing", http.StatusConflict)
		return
	}
	if label != current {
		event := g.auditEventLocked(r, "", "vault.profile_label", "client profile", id, label)
		if err := g.commit(change{profileLabel: &clientProfileLabel{id, label}, audit: []AuditEvent{event}}); err != nil {
			http.Error(w, "could not save profile label", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
