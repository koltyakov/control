package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/crypto/argon2"
)

//go:embed consoleweb/*
var consoleFiles embed.FS

const consoleCookie = "control_console"

func (g *Gateway) consoleCookieName(r *http.Request) string {
	origin, _ := g.consoleOrigin(r)
	if strings.HasPrefix(origin, "https:") {
		return "__Host-" + consoleCookie
	}
	return consoleCookie
}

type consolePassword struct {
	UserID, Role, Revision string
	Salt, Hash             []byte
}

type consoleSession struct {
	Username, Revision, CSRF string
	Created, Seen            time.Time
	saved                    time.Time
}

type consoleAttempt struct {
	Count int
	Until time.Time
}

type consoleState struct {
	// Passwords and sessions use g.mu and the gateway transaction discipline.
	passwords map[string]consolePassword
	mu        sync.Mutex
	sessions  map[string]consoleSession
	attempts  map[string]consoleAttempt
	verify    chan struct{}
}

type consoleContext struct {
	principal
	Username, Revision string
}

type consoleContextKey struct{}

func newConsoleState() *consoleState {
	return &consoleState{passwords: map[string]consolePassword{}, sessions: map[string]consoleSession{}, attempts: map[string]consoleAttempt{}, verify: make(chan struct{}, 2)}
}

// Only the explicitly registered console adapter supplies this context. Bearer
// APIs never accept session cookies, including peer and execution endpoints.
func (g *Gateway) requestPrincipal(r *http.Request) principal {
	if c, ok := r.Context().Value(consoleContextKey{}).(consoleContext); ok {
		return c.principal
	}
	return g.authenticate(bearer(r))
}

func consoleJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func passwordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
}

func passwordMatches(password string, record consolePassword) bool {
	return subtle.ConstantTimeCompare(passwordHash(password, record.Salt), record.Hash) == 1
}

func consoleSecret() string {
	var bytes [32]byte
	// crypto/rand.Read cannot return an error on supported Go versions.
	_, _ = rand.Read(bytes[:])
	return base64.RawURLEncoding.EncodeToString(bytes[:])
}

func (g *Gateway) consoleRoutes(mux *http.ServeMux) {
	mux.Handle("PUT /v1/console/password", g.consoleHeaders(http.HandlerFunc(g.fleetAdmin(g.consolePasswordHTTP))))
	static, _ := fs.Sub(consoleFiles, "consoleweb")
	files := http.FileServerFS(static)
	mux.Handle("GET /console/static/", g.consoleHeaders(http.StripPrefix("/console/static/", files)))
	page := g.consoleHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/console", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/console" {
			if _, _, ok := g.consoleSession(r); !ok {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}
		data, _ := consoleFiles.ReadFile("consoleweb/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}))
	mux.Handle("GET /{$}", page)
	mux.Handle("GET /login", page)
	mux.Handle("GET /console", page)
	mux.Handle("POST /console/api/login", g.consoleHeaders(http.HandlerFunc(g.consoleLogin)))
	mux.Handle("GET /console/api/session", g.consoleHeaders(http.HandlerFunc(g.consoleWhoami)))
	mux.Handle("POST /console/api/logout", g.consoleHeaders(g.consoleGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		cookie, _ := r.Cookie(g.consoleCookieName(r))
		key := tokenHash(cookie.Value)
		old := g.console.sessions[key]
		delete(g.console.sessions, key)
		err := g.commit(change{sessions: []string{key}, audit: []AuditEvent{g.auditEventLocked(r, "", "session.signout", "", "", "")}})
		if err != nil {
			g.console.sessions[key] = old
		}
		g.mu.Unlock()
		if err != nil {
			http.Error(w, "could not persist sign-out; try again", http.StatusInternalServerError)
			return
		}
		g.consoleSetCookie(w, r, "", -1)
		w.WriteHeader(http.StatusNoContent)
	}))))
	// Deliberate management-only allowlist. No peer connect, RPC, binary upload,
	// artifact, node-state, or general-purpose forwarding endpoint is exposed.
	for _, route := range []string{
		"GET status", "GET nodes", "PUT console/password",
		"GET fleet/keys", "POST fleet/keys", "PATCH fleet/keys/{id}", "DELETE fleet/keys/{id}",
		"GET fleet/installations", "POST fleet/installations", "DELETE fleet/installations", "DELETE fleet/installations/{id}",
		"PATCH fleet/nodes/{id}", "DELETE fleet/nodes/{id}", "POST fleet/nodes/{id}/wake",
		"POST fleet/nodes/{id}/startup",
		"GET admin/users", "POST admin/users", "DELETE admin/users/{id}",
		"GET admin/updates", "POST admin/updates/check", "GET admin/metrics", "GET audit", "DELETE audit",
	} {
		method, path, _ := strings.Cut(route, " ")
		mux.Handle(method+" /console/api/"+path, g.consoleHeaders(g.consoleGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			request := r.Clone(r.Context())
			request.URL.Path = "/v1/" + strings.TrimPrefix(r.URL.Path, "/console/api/")
			request.URL.RawPath = ""
			request.Header.Del("Authorization")
			mux.ServeHTTP(w, request)
		}))))
	}
}

func (g *Gateway) consoleHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if _, ok := g.consoleOrigin(r); !ok {
			http.Error(w, "console requires HTTPS; configure CONTROL_PUBLIC_URL behind a trusted proxy", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PublicURL is operator configuration, not an untrusted forwarded header.
// Plain HTTP is accepted only for direct loopback development.
func (g *Gateway) consoleOrigin(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	peer := net.ParseIP(host)
	loopback := err == nil && peer != nil && peer.IsLoopback()
	if g.options.PublicURL != "" {
		u, err := url.Parse(g.options.PublicURL)
		if err != nil || u.Host != r.Host || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		if u.Scheme == "https" && (r.TLS != nil || loopback) {
			return "https://" + u.Host, true
		}
		if u.Scheme != "http" {
			return "", false
		}
	}
	if r.TLS != nil {
		return "https://" + r.Host, true
	}
	u, parseErr := url.Parse("http://" + r.Host)
	if !loopback || parseErr != nil {
		return "", false
	}
	name := u.Hostname()
	ip := net.ParseIP(name)
	if name != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", false
	}
	return "http://" + r.Host, true
}

func (g *Gateway) consoleSameOrigin(r *http.Request) bool {
	origin, ok := g.consoleOrigin(r)
	return ok && r.Header.Get("Origin") == origin
}

func (g *Gateway) consoleSetCookie(w http.ResponseWriter, r *http.Request, value string, age int) {
	origin, _ := g.consoleOrigin(r)
	http.SetCookie(w, &http.Cookie{Name: g.consoleCookieName(r), Value: value, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(origin, "https:"), SameSite: http.SameSiteStrictMode, MaxAge: age})
}

func (g *Gateway) consoleSession(r *http.Request) (consoleContext, consoleSession, bool) {
	cookie, err := r.Cookie(g.consoleCookieName(r))
	if err != nil || len(cookie.Value) > 128 {
		return consoleContext{}, consoleSession{}, false
	}
	key, now := tokenHash(cookie.Value), time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.console.sessions[key]
	if !ok || !g.validConsoleSession(s, now) {
		return consoleContext{}, s, false
	}
	s.Seen = now
	g.console.sessions[key] = s
	// Checkpoint activity at most once a minute, not on every console poll.
	// A failed checkpoint never advances the durable idle deadline.
	if now.Sub(s.saved) >= time.Minute {
		if err := g.commit(change{sessions: []string{key}}); err == nil {
			s.saved = now
			g.console.sessions[key] = s
		}
	}
	p := g.console.passwords[s.Username]
	return consoleContext{principal: principal{Role: p.Role, UserID: p.UserID}, Username: s.Username, Revision: s.Revision}, s, true
}

func (g *Gateway) consoleGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, session, ok := g.consoleSession(r)
		if !ok {
			http.Error(w, "session expired; sign in again", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && (!g.consoleSameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) != 1) {
			http.Error(w, "invalid request origin or CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), consoleContextKey{}, c)))
	})
}

func (g *Gateway) consoleWhoami(w http.ResponseWriter, r *http.Request) {
	c, s, ok := g.consoleSession(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	consoleJSON(w, map[string]any{"username": c.Username, "role": c.Role, "userId": c.UserID, "csrf": s.CSRF})
}

// Admission state is bounded and independent of the gateway mutation lock.
func (g *Gateway) consoleLoginAllowed(r *http.Request, username string) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	g.console.mu.Lock()
	defer g.console.mu.Unlock()
	for key, attempt := range g.console.attempts {
		if now.After(attempt.Until) {
			delete(g.console.attempts, key)
		}
	}
	keys := []string{"global", "ip:" + host, "user:" + username}
	limits := []int{120, 60, 10}
	if len(g.console.attempts) > 4090 {
		return false
	}
	for i, key := range keys {
		if g.console.attempts[key].Count >= limits[i] {
			return false
		}
	}
	for _, key := range keys {
		a := g.console.attempts[key]
		if a.Count == 0 {
			a.Until = now.Add(15 * time.Minute)
		}
		a.Count++
		g.console.attempts[key] = a
	}
	return true
}

func (g *Gateway) consoleVerifier(w http.ResponseWriter) bool {
	select {
	case g.console.verify <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "password verification busy; try again shortly", http.StatusTooManyRequests)
		return false
	}
}

func (g *Gateway) consoleLogin(w http.ResponseWriter, r *http.Request) {
	if !g.consoleSameOrigin(r) {
		http.Error(w, "invalid request origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var q struct{ Username, Password string }
	if json.NewDecoder(r.Body).Decode(&q) != nil || len(q.Username) > 64 || len(q.Password) > 1024 {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	if !g.consoleLoginAllowed(r, q.Username) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "too many login attempts; try again later", http.StatusTooManyRequests)
		return
	}
	if !g.consoleVerifier(w) {
		return
	}
	defer func() { <-g.console.verify }()
	g.mu.Lock()
	p, exists := g.console.passwords[q.Username]
	u := g.users[p.UserID]
	g.mu.Unlock()
	if !exists {
		p = consolePassword{Salt: make([]byte, 16), Hash: make([]byte, 32)}
	}
	match := passwordMatches(q.Password, p)
	if !match || !exists || u.Disabled || u.ID == "" || (p.Role == "superuser" && g.superuser == "") {
		g.counters.authFailures.Add(1)
		g.mu.Lock()
		// A failed write must not change the response an attacker observes.
		_ = g.commit(change{audit: []AuditEvent{signInEvent(r, q.Username, p.UserID, false)}})
		g.mu.Unlock()
		http.Error(w, "incorrect username or password", http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.console.passwords[q.Username].Revision != p.Revision || g.users[p.UserID].Disabled {
		http.Error(w, "account changed; sign in again", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	secret := consoleSecret()
	s := consoleSession{Username: q.Username, Revision: p.Revision, CSRF: consoleSecret(), Created: now, Seen: now, saved: now}
	removed := map[string]consoleSession{}
	remove := func(key string) {
		if session, ok := g.console.sessions[key]; ok {
			removed[key] = session
			delete(g.console.sessions, key)
		}
	}
	for key, session := range g.console.sessions {
		if !g.validConsoleSession(session, now) {
			remove(key)
		}
	}
	if cookie, err := r.Cookie(g.consoleCookieName(r)); err == nil {
		remove(tokenHash(cookie.Value))
	}
	defer func() {
		for key, session := range removed {
			g.console.sessions[key] = session
		}
	}()
	if len(g.console.sessions) >= 4096 {
		http.Error(w, "session limit reached", http.StatusTooManyRequests)
		return
	}
	key := tokenHash(secret)
	g.console.sessions[key] = s
	changed := []string{key}
	for hash := range removed {
		changed = append(changed, hash)
	}
	if err := g.commit(change{sessions: changed, audit: []AuditEvent{signInEvent(r, q.Username, p.UserID, true)}}); err != nil {
		delete(g.console.sessions, key)
		http.Error(w, "could not persist sign-in", http.StatusInternalServerError)
		return
	}
	clear(removed)
	g.consoleSetCookie(w, r, secret, 12*60*60)
	consoleJSON(w, map[string]any{"username": q.Username, "role": p.Role, "userId": p.UserID, "csrf": s.CSRF})
}

func (g *Gateway) consolePasswordHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var q struct{ Username, Password, CurrentPassword string }
	if json.NewDecoder(r.Body).Decode(&q) != nil || len(q.Password) < 12 || len(q.Password) > 1024 || len(q.CurrentPassword) > 1024 {
		http.Error(w, "password must contain 12..1024 bytes", http.StatusBadRequest)
		return
	}
	if !g.consoleVerifier(w) {
		return
	}
	defer func() { <-g.console.verify }()
	p := g.requestPrincipal(r)
	g.mu.Lock()
	username := g.users[p.UserID].Name
	if p.Role == "superuser" {
		username = "@operator"
	}
	if q.Username != "" {
		if p.Role != "superuser" && q.Username != username {
			g.mu.Unlock()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		username = q.Username
	}
	record := consolePassword{UserID: p.UserID, Role: p.Role}
	if p.Role == "superuser" && username != "@operator" {
		record = consolePassword{Role: "user"}
		for _, user := range g.users {
			if user.Name == username && !user.Disabled && user.ID != legacyUser {
				record.UserID = user.ID
			}
		}
	}
	old, existed := g.console.passwords[username]
	g.mu.Unlock()
	if record.UserID == "" || username == "legacy" {
		http.NotFound(w, r)
		return
	}
	c, browser := r.Context().Value(consoleContextKey{}).(consoleContext)
	if browser && c.Username == username && !g.consoleLoginAllowed(r, username) {
		http.Error(w, "too many password attempts; try again later", http.StatusTooManyRequests)
		return
	}
	if browser && c.Username == username && (!passwordMatches(q.CurrentPassword, old) || c.Revision != old.Revision) {
		http.Error(w, "current password is incorrect", http.StatusForbidden)
		return
	}
	record.Salt = make([]byte, 16)
	_, _ = rand.Read(record.Salt)
	record.Hash, record.Revision = passwordHash(q.Password, record.Salt), identity.NewID()
	g.mu.Lock()
	defer g.mu.Unlock()
	// Do not let concurrent changes or account revocation overwrite a reset.
	if g.console.passwords[username].Revision != old.Revision || g.users[record.UserID].Disabled || (browser && (g.console.passwords[c.Username].Revision != c.Revision || g.users[c.UserID].Disabled)) {
		http.Error(w, "account changed; sign in again before retrying", http.StatusConflict)
		return
	}
	g.console.passwords[username] = record
	if err := g.commit(change{passwords: []string{username}, audit: []AuditEvent{g.auditEventLocked(r, record.UserID, "account.password", username, "", "")}}); err != nil {
		if existed {
			g.console.passwords[username] = old
		} else {
			delete(g.console.passwords, username)
		}
		http.Error(w, "could not persist password", http.StatusInternalServerError)
		return
	}
	for key, session := range g.console.sessions {
		if session.Username == username {
			delete(g.console.sessions, key)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
