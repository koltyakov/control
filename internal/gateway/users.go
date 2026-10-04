package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/koltyakov/control/internal/identity"
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Disabled  bool      `json:"disabled"`
}

type principal struct{ Role, KeyID, UserID string }

func newCredential() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return "ctl_" + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func (g *Gateway) fleetAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		if p.Role != "user" && p.Role != "superuser" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) userRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/admin/users", g.admin(g.createUser))
	mux.HandleFunc("GET /v1/admin/users", g.admin(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		users := []User{}
		for _, u := range g.users {
			users = append(users, u)
		}
		g.mu.Unlock()
		sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(users)
	}))
	mux.HandleFunc("DELETE /v1/admin/users/{id}", g.admin(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		u, ok := g.users[r.PathValue("id")]
		if !ok {
			g.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if u.ID == legacyUser {
			g.mu.Unlock()
			http.Error(w, "the legacy operator fleet cannot be disabled", 409)
			return
		}
		old := u
		u.Disabled = true
		g.users[u.ID] = u
		err := g.persist()
		if err != nil {
			g.users[u.ID] = old
		}
		peers := []*connection{}
		for _, p := range g.peers {
			if p.userID == u.ID {
				peers = append(peers, p)
			}
		}
		g.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist user revocation", 500)
			return
		}
		for _, p := range peers {
			_ = p.ws.CloseNow()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (g *Gateway) createUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var q struct {
		Name string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil || !validName.MatchString(q.Name) {
		http.Error(w, "valid user name required", 400)
		return
	}
	token, err := newCredential()
	if err != nil {
		http.Error(w, "key generation failed", 500)
		return
	}
	u := User{ID: identity.NewID(), Name: q.Name, CreatedAt: time.Now().UTC()}
	k := keyRecord{APIKey: APIKey{ID: identity.NewID(), UserID: u.ID, Name: "account", Role: "user", CreatedAt: u.CreatedAt}, Hash: tokenHash(token)}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, existing := range g.users {
		if existing.Name == q.Name {
			http.Error(w, "user name already registered", 409)
			return
		}
	}
	if len(g.users) >= 4096 {
		http.Error(w, "user limit reached", 409)
		return
	}
	g.users[u.ID] = u
	g.keys[k.ID] = k
	if err = g.persist(); err != nil {
		delete(g.users, u.ID)
		delete(g.keys, k.ID)
		http.Error(w, "could not persist user", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"user": u, "token": token})
}
