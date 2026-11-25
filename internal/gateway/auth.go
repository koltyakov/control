package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/fleetkey"
	"github.com/koltyakov/control/internal/identity"
)

type APIKey struct {
	UserID    string    `json:"userId"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Revoked   bool      `json:"revoked"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
}
type keyRecord struct {
	APIKey
	Hash string `json:"hash"`
}

type keyTime struct {
	credential string
	last       atomic.Int64 // Milliseconds since epoch; the timestamp of the latest valid bearer lookup.
	persisted  int64        // Accessed under g.mu.
}

func (p principal) trackUse() principal {
	if p.target != nil {
		now := time.Now().UnixMilli()
		for old := p.target.last.Load(); old < now; old = p.target.last.Load() {
			if p.target.last.CompareAndSwap(old, now) {
				break
			}
		}
	}
	return p
}

// Legacy role identifiers used by account recovery and role-based issuance.
const (
	roleSuperuser          = "superuser"
	roleUser               = "user"
	roleOrchestrator       = "orchestrator"
	roleOrchestratorEnroll = "orchestrator-enroll"
	roleCommon             = "common"
	roleDiscovery          = "discovery"
)

func managesFleet(role string) bool { return fleetkey.ForRole(role).Has(fleetkey.ManageFleet) }

// executionRole reports credentials whose client sessions may execute work.
func executionRole(role string) bool {
	return fleetkey.ForRole(role).Has(fleetkey.Run)
}

func enrollmentRole(role string) bool {
	return fleetkey.ForRole(role).Has(fleetkey.Enroll)
}

// The public permission list describes what the gateway actually admits.
// Keep the stored roles for existing credentials and protocol clients.
func keyPermissions(role string) []string {
	return fleetkey.ForRole(role).Names()
}

// Named managers cannot issue a stronger credential or alter the recovery key.
func canManageKey(caller, target string) bool {
	if target == roleUser {
		return caller == roleUser || caller == roleSuperuser
	}
	required := fleetkey.ForRole(target)
	return required != 0 && fleetkey.ForRole(caller).Has(fleetkey.ManageFleet|required)
}

// issuableKeyRole maps a requested fleet key role to the stored role. Owners
// may issue enrollment keys, execution keys, or a key covering both.
func issuableKeyRole(requested string) (string, bool) {
	switch requested {
	case "", roleCommon:
		return roleCommon, true
	case roleOrchestrator, roleOrchestratorEnroll:
		return requested, true
	}
	return "", false
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) role(token string) (role, keyID string) {
	p := g.authenticate(token)
	return p.Role, p.KeyID
}

// authenticate resolves a bearer credential without taking the gateway lock,
// so authentication never waits behind a database commit and a flood of
// invalid credentials cannot contend with connection or relay handling.
func (g *Gateway) authenticate(token string) principal {
	if token == "" {
		return principal{}
	}
	if g.superuser != "" && subtle.ConstantTimeCompare([]byte(token), []byte(g.superuser)) == 1 {
		return principal{Role: roleSuperuser, KeyID: "superuser", UserID: legacyUser, target: g.operatorUse.Load()}.trackUse()
	} else if g.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(g.token)) == 1 {
		return principal{Role: roleCommon, KeyID: "bootstrap", UserID: legacyUser, target: g.bootstrapUse.Load()}.trackUse()
	}
	// Credentials are 256-bit random secrets, so a lookup by their SHA-256
	// digest reveals nothing useful through timing.
	if index := g.credentials.Load(); index != nil {
		if p, ok := (*index)[tokenHash(token)]; ok {
			return p.trackUse()
		}
	}
	return principal{}
}

// authenticateLocked is authenticate for callers already holding g.mu.
func (g *Gateway) authenticateLocked(token string) principal { return g.authenticate(token) }

// reindexCredentialsLocked publishes the usable credentials after a committed
// change to users, keys, or invitations. Caller holds g.mu or is initializing.
func (g *Gateway) reindexCredentialsLocked() {
	index := make(map[string]principal, len(g.keys)+len(g.installations)+2)
	active := make(map[string]*keyTime, len(g.keys)+2)
	target := func(id, role, owner, credential string, grant bool) {
		if credential == "" {
			return
		}
		entry := g.keyTimes[id]
		if entry == nil || entry.credential != credential {
			entry = &keyTime{credential: credential}
		}
		active[id] = entry
		if grant {
			index[credential] = principal{Role: role, KeyID: id, UserID: owner, Username: g.users[owner].Name, target: entry}
		}
	}
	for hash, i := range g.installations {
		if u, ok := g.users[i.UserID]; ok && !u.Disabled && !i.Revoked && i.RedeemedID != "" && i.CredentialHash != "" {
			index[i.CredentialHash] = principal{Role: "common", KeyID: "install:" + i.ID, UserID: i.UserID, Installation: hash}
		}
	}
	// Account and common keys take precedence, as they did in the linear scan.
	for id, key := range g.keys {
		u, ok := g.users[key.UserID]
		target(id, key.Role, key.UserID, key.Hash, ok && !u.Disabled && !key.Revoked)
	}
	if g.token != "" {
		target("bootstrap", roleCommon, legacyUser, tokenHash(g.token), false)
		g.bootstrapUse.Store(active["bootstrap"])
	}
	if g.superuser != "" {
		target("superuser", roleSuperuser, legacyUser, tokenHash(g.superuser), false)
		g.operatorUse.Store(active["superuser"])
	}
	g.keyTimes = active
	g.credentials.Store(&index)
}

func bearer(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return value
}

func (g *Gateway) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := g.requestPrincipal(r).Role
		if role == "" {
			g.counters.authFailures.Add(1)
		}
		if role != "superuser" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// Recheck the caller while holding g.mu: revocation or sign-out may have
// committed after middleware admitted the request but before its mutation.
func (g *Gateway) keyManagerLocked(r *http.Request) (principal, bool) {
	p := g.requestPrincipal(r)
	_, browser := r.Context().Value(consoleContextKey{}).(consoleContext)
	return p, managesFleet(p.Role) && (!browser || g.consolePolicySessionLiveLocked(r))
}

func (g *Gateway) hasActiveAccountKeyLocked(user string) bool {
	for _, k := range g.keys {
		if k.UserID == user && k.Role == roleUser && !k.Revoked {
			return true
		}
	}
	return false
}

func (g *Gateway) issueFleetKey(w http.ResponseWriter, r *http.Request, name, role string) {
	token, err := newCredential()
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	g.mu.Lock()
	p, ok := g.keyManagerLocked(r)
	_, browser := r.Context().Value(consoleContextKey{}).(consoleContext)
	if !ok || !canManageKey(p.Role, role) || (role == roleUser && (p.Role != roleUser || !browser)) {
		g.mu.Unlock()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if role == roleUser && g.hasActiveAccountKeyLocked(p.UserID) {
		g.mu.Unlock()
		http.Error(w, "an active fleet account key already exists", http.StatusConflict)
		return
	}
	key := APIKey{ID: identity.NewID(), UserID: p.UserID, Name: name, Role: role, CreatedAt: time.Now().UTC()}
	detail := ""
	if role != roleCommon {
		detail = "role " + role
	}
	g.keys[key.ID] = keyRecord{APIKey: key, Hash: tokenHash(token)}
	err = g.commit(change{keys: []string{key.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "key.create", key.Name, key.ID, detail)}})
	if err != nil {
		delete(g.keys, key.ID)
	}
	g.mu.Unlock()
	if err != nil {
		http.Error(w, "could not persist key", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	consoleJSON(w, map[string]any{"key": key, "token": token})
}

func (g *Gateway) removeRevokedKey(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, live := g.keyManagerLocked(r)
	if !live {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	key, ok := g.keys[r.PathValue("id")]
	if !ok || key.UserID != p.UserID {
		http.NotFound(w, r)
		return
	}
	if !canManageKey(p.Role, key.Role) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !key.Revoked {
		http.Error(w, "revoke the key before removing it", http.StatusConflict)
		return
	}
	delete(g.keys, key.ID)
	if err := g.commit(change{keys: []string{key.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "key.remove", key.Name, key.ID, "")}}); err != nil {
		g.keys[key.ID] = key
		http.Error(w, "could not remove revoked key", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// A reactivated key can match existing access-lock allowances again. Only
// the password-authenticated console, which manages those allowances, can
// restore the original credential.
func (g *Gateway) reactivateKey(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.consolePolicySessionLiveLocked(r) {
		http.Error(w, "session expired; sign in again", http.StatusUnauthorized)
		return
	}
	p := g.requestPrincipal(r)
	key, ok := g.keys[r.PathValue("id")]
	if !ok || key.UserID != p.UserID {
		http.NotFound(w, r)
		return
	}
	if !key.Revoked || (key.Role == roleUser && g.hasActiveAccountKeyLocked(p.UserID)) {
		http.Error(w, "this key or another fleet account key is already active", http.StatusConflict)
		return
	}
	old := key
	key.Revoked = false
	g.keys[key.ID] = key
	if err := g.commit(change{keys: []string{key.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "key.reactivate", key.Name, key.ID, "")}}); err != nil {
		g.keys[key.ID] = old
		http.Error(w, "could not reactivate key", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/auth", g.auth(func(w http.ResponseWriter, r *http.Request) {
		p := g.requestPrincipal(r)
		w.Header().Set("Content-Type", "application/json")
		capabilities := []string{}
		switch {
		case p.Role == roleSuperuser:
			capabilities = []string{"updates.manage", "users.manage", "keys.manage", "installations.manage", "machines.manage"}
		case managesFleet(p.Role):
			capabilities = []string{"keys.manage", "installations.manage", "machines.manage"}
		}
		// Management keys learn the browser username their console password applies to.
		username := ""
		switch p.Role {
		case "superuser":
			username = "@operator"
		case "user":
			username = p.Username
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"role": p.Role, "userId": p.UserID, "username": username, "capabilities": capabilities, "nodeHealth": true, "clientSessions": true, "clientOwners": true, "peerChannels": true, "delegation": true, "largePackets": true, "nodeBytes": true, "machineStatePush": true, "gatewayHeartbeat": true, "telemetryLiveness": true, "relayDrain": true, "gatewayProtocol": g.options.Software.GatewayProtocol, "trickleICE": true, "lanDiscovery": true, "lanView": true, "gatewayProxy": true, "managedGatewayProxy": true, "machineLabels": true, "fleetVaultCheck": true, "orchestratorKeys": true, "orchestratorEnrollKeys": true, "stunServers": g.options.STUNServers})
	}))
	create := func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if !decodeStrict(w, r, 4096, &q) || q.Name == "" || len(q.Name) > 128 {
			http.Error(w, "name is required, maximum 128 bytes", http.StatusBadRequest)
			return
		}
		role, ok := issuableKeyRole(q.Role)
		if !ok {
			http.Error(w, `role must be "common", "orchestrator", or "orchestrator-enroll"`, http.StatusBadRequest)
			return
		}
		g.issueFleetKey(w, r, q.Name, role)
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		p := g.requestPrincipal(r)
		g.mu.Lock()
		type listedKey struct {
			APIKey
			Permissions       []string `json:"permissions"`
			GatewayConfigured bool     `json:"gatewayConfigured,omitempty"`
		}
		keys := []listedKey{}
		if p.UserID == legacyUser {
			if g.superuser != "" {
				keys = append(keys, listedKey{APIKey: operatorAPIKey(), Permissions: keyPermissions(roleSuperuser), GatewayConfigured: true})
			}
			if g.token != "" {
				keys = append(keys, listedKey{APIKey: APIKey{ID: "bootstrap", Name: "Gateway bootstrap key", Role: roleCommon, UserID: legacyUser}, Permissions: keyPermissions(roleCommon), GatewayConfigured: true})
			}
		}
		for _, key := range g.keys {
			if key.UserID == p.UserID {
				keys = append(keys, listedKey{APIKey: key.APIKey, Permissions: keyPermissions(key.Role)})
			}
		}
		for i := range keys {
			keys[i].LastUsed = g.keyLastUsed(keys[i].ID)
		}
		g.mu.Unlock()
		sort.SliceStable(keys, func(i, j int) bool { return keys[i].CreatedAt.Before(keys[j].CreatedAt) })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(keys)
	}
	revoke := func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		p, live := g.keyManagerLocked(r)
		if !live {
			g.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		key, ok := g.keys[r.PathValue("id")]
		if !ok || key.UserID != p.UserID {
			g.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if !canManageKey(p.Role, key.Role) {
			g.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		old := key
		key.Revoked = true
		g.keys[key.ID] = key
		err := g.commit(change{keys: []string{key.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "key.revoke", key.Name, key.ID, "")}})
		if err != nil {
			g.keys[key.ID] = old
		}
		var peers []*connection
		if err == nil {
			for _, peer := range g.peers {
				if peer.keyID == key.ID {
					peers = append(peers, peer)
				}
			}
		}
		g.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist revocation", 500)
			return
		}
		for _, peer := range peers {
			_ = peer.ws.CloseNow()
		}
		w.WriteHeader(http.StatusNoContent)
	}
	rename := func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var q struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&q) != nil || q.Name == "" || len(q.Name) > 128 {
			http.Error(w, "name is required, maximum 128 bytes", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		p, live := g.keyManagerLocked(r)
		if !live {
			g.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		key, ok := g.keys[r.PathValue("id")]
		if !ok || key.UserID != p.UserID {
			g.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if !canManageKey(p.Role, key.Role) {
			g.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		old := key
		key.Name = q.Name
		g.keys[key.ID] = key
		err := g.commit(change{keys: []string{key.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "key.rename", key.Name, key.ID, "was "+old.Name)}})
		if err != nil {
			g.keys[key.ID] = old
		}
		g.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist key name", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(key.APIKey)
	}
	for _, prefix := range []string{"/v1/admin/keys", "/v1/fleet/keys"} {
		guard := g.fleetAdmin
		if prefix == "/v1/admin/keys" {
			guard = g.admin
		}
		mux.HandleFunc("POST "+prefix, guard(create))
		// A distinct route makes an older gateway reject this request rather
		// than ignoring permissions and issuing a common enrollment key.
		mux.HandleFunc("POST "+prefix+"/issue", guard(func(w http.ResponseWriter, r *http.Request) {
			var q struct {
				Name        string   `json:"name"`
				Permissions []string `json:"permissions"`
			}
			if !decodeStrict(w, r, 4096, &q) || q.Name == "" || len(q.Name) > 128 {
				http.Error(w, "supply a name of 1 to 128 bytes and a permissions list", http.StatusBadRequest)
				return
			}
			role, ok := fleetkey.NamedRole(q.Permissions)
			if !ok {
				http.Error(w, "include discover and optionally enroll, run, or manage-fleet, once each", http.StatusBadRequest)
				return
			}
			g.issueFleetKey(w, r, q.Name, role)
		}))
		mux.HandleFunc("GET "+prefix, guard(list))
		mux.HandleFunc("PATCH "+prefix+"/{id}", guard(rename))
		mux.HandleFunc("DELETE "+prefix+"/{id}", guard(revoke))
		mux.HandleFunc("DELETE "+prefix+"/{id}/record", guard(g.removeRevokedKey))
	}
}

func operatorAPIKey() APIKey {
	return APIKey{ID: superuserKeyID, Name: "Gateway operator key", Role: roleSuperuser, UserID: legacyUser}
}
