package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/identity"
)

type APIKey struct {
	UserID    string    `json:"userId"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	Revoked   bool      `json:"revoked"`
}
type keyRecord struct {
	APIKey
	Hash string `json:"hash"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) role(token string) (role, keyID string) {
	p := g.authenticate(token)
	return p.Role, p.KeyID
}

func (g *Gateway) authenticate(token string) principal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authenticateLocked(token)
}

func (g *Gateway) authenticateLocked(token string) principal {
	if token == "" {
		return principal{}
	}
	if g.superuser != "" && subtle.ConstantTimeCompare([]byte(token), []byte(g.superuser)) == 1 {
		return principal{Role: "superuser", KeyID: "superuser", UserID: legacyUser}
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(g.token)) == 1 {
		return principal{Role: "common", KeyID: "bootstrap", UserID: legacyUser}
	}
	hash := tokenHash(token)
	for id, key := range g.keys {
		if u, ok := g.users[key.UserID]; ok && !u.Disabled && !key.Revoked && subtle.ConstantTimeCompare([]byte(hash), []byte(key.Hash)) == 1 {
			return principal{Role: key.Role, KeyID: id, UserID: key.UserID}
		}
	}
	for _, i := range g.installations {
		if u, ok := g.users[i.UserID]; ok && !u.Disabled && !i.Revoked && i.RedeemedID != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(i.CredentialHash)) == 1 {
			return principal{Role: "common", KeyID: "install:" + i.ID, UserID: i.UserID}
		}
	}
	return principal{}
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
		role, _ := g.role(bearer(r))
		if role != "superuser" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/auth", g.auth(func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		w.Header().Set("Content-Type", "application/json")
		capabilities := []string{}
		if p.Role == "superuser" {
			capabilities = []string{"updates.manage", "users.manage", "keys.manage", "installations.manage"}
		} else if p.Role == "user" {
			capabilities = []string{"keys.manage", "installations.manage"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"role": p.Role, "userId": p.UserID, "capabilities": capabilities})
	}))
	create := func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var q struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&q) != nil || q.Name == "" || len(q.Name) > 128 {
			http.Error(w, "name is required, maximum 128 bytes", http.StatusBadRequest)
			return
		}
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			http.Error(w, "key generation failed", 500)
			return
		}
		token := "ctl_" + base64.RawURLEncoding.EncodeToString(secret[:])
		key := APIKey{ID: identity.NewID(), UserID: p.UserID, Name: q.Name, Role: "common", CreatedAt: time.Now().UTC()}
		g.mu.Lock()
		g.keys[key.ID] = keyRecord{APIKey: key, Hash: tokenHash(token)}
		err := g.persist()
		if err != nil {
			delete(g.keys, key.ID)
		}
		g.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist key", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "token": token})
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		g.mu.Lock()
		keys := []APIKey{}
		for _, key := range g.keys {
			if key.UserID == p.UserID {
				keys = append(keys, key.APIKey)
			}
		}
		g.mu.Unlock()
		sort.Slice(keys, func(i, j int) bool { return keys[i].CreatedAt.Before(keys[j].CreatedAt) })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(keys)
	}
	revoke := func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		g.mu.Lock()
		key, ok := g.keys[r.PathValue("id")]
		if !ok || key.UserID != p.UserID {
			g.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		old := key
		key.Revoked = true
		g.keys[key.ID] = key
		err := g.persist()
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
	for _, prefix := range []string{"/v1/admin/keys", "/v1/fleet/keys"} {
		guard := g.fleetAdmin
		if prefix == "/v1/admin/keys" {
			guard = g.admin
		}
		mux.HandleFunc("POST "+prefix, guard(create))
		mux.HandleFunc("GET "+prefix, guard(list))
		mux.HandleFunc("DELETE "+prefix+"/{id}", guard(revoke))
	}
}
