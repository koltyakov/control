package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// fleetVaultSchema stores each fleet's opt-in lock policy: whether workers
// accept execution only from allowed account clients, a temporary pause, and
// the allowances. One row per fleet; a missing row means the default, an
// unlocked fleet. The operator's gateway-wide policy shares this migration.
const fleetVaultSchema = `
CREATE TABLE IF NOT EXISTS fleet_vault (user_id TEXT PRIMARY KEY REFERENCES users(id), data BLOB NOT NULL);
`

// Bounds for the fleet lock API. Durations are whole seconds; an absent or
// zero value means no pause or no expiry.
const (
	maxFleetVaultAllowances = 256
	maxFleetVaultMachines   = 256
	maxFleetVaultSeconds    = 365 * 24 * 60 * 60
	maxFleetVaultHosts      = 1024
	maxFleetVaultIDBytes    = 128
	superuserKeyID          = "superuser"
)

// fleetVaultPolicy is one fleet's lock. Enabled with a future PausedUntil is
// a temporary pause that resumes strict enforcement on its own; times are
// persisted in UTC. Revision orders console edits.
type fleetVaultPolicy struct {
	UserID      string                `json:"userId"`
	Enabled     bool                  `json:"enabled"`
	PausedUntil time.Time             `json:"pausedUntil,omitzero"`
	Revision    int64                 `json:"revision"`
	Allowances  []fleetVaultAllowance `json:"allowances,omitempty"`
}

// fleetVaultAllowance lets account clients matching every present selector
// reach the listed workers. KeyID names an execution-capable credential;
// OwnerID is an attested stable client owner identity, never a host name.
type fleetVaultAllowance struct {
	ID          string    `json:"id"`
	KeyID       string    `json:"keyId,omitempty"`
	OwnerID     string    `json:"ownerId,omitempty"`
	AllMachines bool      `json:"allMachines"`
	MachineIDs  []string  `json:"machineIds,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"`
}

// locked reports whether this fleet's own policy enforces at now.
func (p fleetVaultPolicy) locked(now time.Time) bool {
	return p.Enabled && !now.Before(p.PausedUntil)
}

func (a fleetVaultAllowance) active(now time.Time) bool {
	return a.ExpiresAt.IsZero() || now.Before(a.ExpiresAt)
}

// covers reports whether this one rule matches the identity for every
// machine. Both selectors present means both must match.
func (a fleetVaultAllowance) covers(keyID, ownerID string, machineIDs []string) bool {
	if (a.KeyID == "" && a.OwnerID == "") || len(machineIDs) == 0 {
		return false
	}
	if (a.KeyID != "" && a.KeyID != keyID) || (a.OwnerID != "" && a.OwnerID != ownerID) {
		return false
	}
	if a.AllMachines {
		return true
	}
	for _, id := range machineIDs {
		if !slices.Contains(a.MachineIDs, id) {
			return false
		}
	}
	return true
}

// fleetDecision is the pure allowance evaluation: one active rule must cover
// the key and owner for every machine. It ignores whether the lock is
// enabled; callers combine it with fleetVaultEnforcedLocked. Empty keyID or
// ownerID means that selector is unknown and only rules without it can match.
func fleetDecision(policy fleetVaultPolicy, keyID, ownerID string, machineIDs []string, now time.Time) bool {
	for _, a := range policy.Allowances {
		if a.active(now) && a.covers(keyID, ownerID, machineIDs) {
			return true
		}
	}
	return false
}

// fleetVaultEnforcedLocked reports whether workers of user's fleet must deny
// unallowed issuers now: the fleet's lock is strict, or the operator's
// gateway-wide lock is active. Caller holds g.mu.
func (g *Gateway) fleetVaultEnforcedLocked(user string, now time.Time) bool {
	return g.vaultPolicies[user].locked(now) || g.globalVault.active(now)
}

// fleetVaultConfiguredLocked reports a configured lock, paused or not. It
// closes registration of non-supporting workers and bearer console password
// resets. Caller holds g.mu.
func (g *Gateway) fleetVaultConfiguredLocked(user string) bool {
	return g.vaultPolicies[user].Enabled || g.globalVault.Enabled
}

// anyVaultConfiguredLocked reports whether any fleet or the gateway has a
// configured lock. The operator's browser login can recover every fleet, so
// its password follows this. Caller holds g.mu.
func (g *Gateway) anyVaultConfiguredLocked() bool {
	if g.globalVault.Enabled {
		return true
	}
	for _, p := range g.vaultPolicies {
		if p.Enabled {
			return true
		}
	}
	return false
}

// vaultExecutionKeyLocked reports whether keyID is a current, unrevoked
// execution-capable credential of user's fleet: an account or orchestrator
// key, or the superuser key for the legacy operator fleet. Common,
// installation, and worker credentials never qualify. Caller holds g.mu.
func (g *Gateway) vaultExecutionKeyLocked(user, keyID string) bool {
	if keyID == superuserKeyID {
		return user == legacyUser && g.superuser != ""
	}
	k, ok := g.keys[keyID]
	if !ok || k.Revoked || k.UserID != user || k.Hash == "" || (k.Role != roleUser && k.Role != roleOrchestrator) {
		return false
	}
	u, ok := g.users[user]
	return ok && !u.Disabled
}

// vaultClientOwnerLocked reports whether id is a durable stable client owner
// identity of user's fleet. Caller holds g.mu.
func (g *Gateway) vaultClientOwnerLocked(user, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if err := g.loadIdentityLocked(id); err != nil {
		return false, err
	}
	return g.owners[id] == user && g.clientOwners[id] && g.clientTransports[id] == "", nil
}

// vaultBearerAllowedLocked gates direct gateway-managed machine operations
// for a bearer credential. A bearer request has no client owner proof, so
// only a rule naming its key without an owner selector admits it while the
// lock is enforced. Caller holds g.mu.
func (g *Gateway) vaultBearerAllowedLocked(p principal, machineIDs []string, now time.Time) bool {
	if !g.fleetVaultEnforcedLocked(p.UserID, now) {
		return true
	}
	return g.vaultExecutionKeyLocked(p.UserID, p.KeyID) && fleetDecision(g.vaultPolicies[p.UserID], p.KeyID, "", machineIDs, now)
}

// fleetVaultBearerDenied is the refusal for a bearer machine mutation.
const fleetVaultBearerDenied = "the fleet lock does not allow this credential to change these machines; use the web console"

// vaultBearerDeniedLocked reports whether an enforced lock refuses a bearer
// request that affects every listed machine. Handlers call it inside the
// lock that admits their mutation; a request admitted before a rule changes
// may finish. Personal console sessions are the administrative recovery path
// and are never refused. Caller holds g.mu.
func (g *Gateway) vaultBearerDeniedLocked(r *http.Request, p principal, machineIDs ...string) bool {
	if _, browser := r.Context().Value(consoleContextKey{}).(consoleContext); browser {
		return false
	}
	return !g.vaultBearerAllowedLocked(p, machineIDs, time.Now())
}

// vaultPasswordResetBlockedLocked closes bearer console password changes that
// could take over a browser login able to change a configured lock, even a
// paused one. The operator's login can recover every fleet, so its reset
// closes while any lock is configured. Its first provisioning closes only for
// the operator's own fleet or gateway lock, so a tenant cannot prevent
// global recovery. Caller holds g.mu.
func (g *Gateway) vaultPasswordResetBlockedLocked(record consolePassword, provisioned bool) bool {
	if record.Role == roleSuperuser {
		if !provisioned {
			return g.fleetVaultConfiguredLocked(legacyUser)
		}
		return g.anyVaultConfiguredLocked()
	}
	return g.fleetVaultConfiguredLocked(record.UserID)
}

// vaultRegistrationAllowedLocked rejects a worker that cannot ask the
// gateway for access decisions while its fleet has a configured lock,
// including a paused one. Caller holds g.mu.
func (g *Gateway) vaultRegistrationAllowedLocked(user string, fleetVaultCheck bool) bool {
	return fleetVaultCheck || !g.fleetVaultConfiguredLocked(user)
}

func (g *Gateway) fleetVaultRoutes(mux *http.ServeMux) {
	// Browser cookie only: these exist solely below /console/api, behind the
	// session, same-origin, and CSRF guard. No bearer route changes a lock.
	browser := func(next http.HandlerFunc) http.Handler {
		return g.consoleHeaders(g.consoleGuard(http.HandlerFunc(g.fleetAdmin(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(consoleContextKey{}).(consoleContext); !ok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next(w, r)
		}))))
	}
	mux.Handle("GET /console/api/fleet/vault", browser(g.getFleetVault))
	mux.Handle("PUT /console/api/fleet/vault", browser(g.setFleetVault))
	mux.Handle("PUT /console/api/fleet/vault/profiles/{id}", browser(g.labelClientProfile))
	mux.Handle("POST /console/api/fleet/vault/allowances", browser(g.addFleetVaultAllowance))
	mux.Handle("DELETE /console/api/fleet/vault/allowances/{id}", browser(g.removeFleetVaultAllowance))
	mux.HandleFunc("POST "+model.FleetVaultCheckPath, g.auth(g.fleetVaultCheckHTTP))
}

type fleetVaultChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"` // Only execution credentials have roles.
}

type fleetVaultAllowanceView struct {
	ID          string    `json:"id"`
	KeyID       string    `json:"keyId,omitempty"`
	OwnerID     string    `json:"ownerId,omitempty"`
	AllMachines bool      `json:"allMachines"`
	MachineIDs  []string  `json:"machineIds"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"`
}

// fleetVaultView is the console projection of a fleet's lock.
type fleetVaultView struct {
	Enabled           bool                      `json:"enabled"`
	PausedUntil       time.Time                 `json:"pausedUntil,omitzero"`
	Revision          int64                     `json:"revision"`
	GlobalEnabled     bool                      `json:"globalEnabled"`
	GlobalPausedUntil time.Time                 `json:"globalPausedUntil,omitzero"`
	Allowances        []fleetVaultAllowanceView `json:"allowances"`
	Keys              []fleetVaultChoice        `json:"keys"`
	Hosts             []clientProfileChoice     `json:"hosts"`
	Machines          []fleetVaultChoice        `json:"machines"`
}

// fleetVaultViewLocked projects user's policy and the choices its console
// may select. Caller holds g.mu.
func (g *Gateway) fleetVaultViewLocked(user string, now time.Time) (fleetVaultView, error) {
	p := g.vaultPolicies[user]
	v := fleetVaultView{Enabled: p.Enabled, Revision: p.Revision, GlobalEnabled: g.globalVault.Enabled, Allowances: []fleetVaultAllowanceView{}, Keys: []fleetVaultChoice{}, Machines: []fleetVaultChoice{}}
	if p.Enabled && now.Before(p.PausedUntil) {
		v.PausedUntil = p.PausedUntil.UTC()
	}
	if g.globalVault.Enabled && now.Before(g.globalVault.PausedUntil) {
		v.GlobalPausedUntil = g.globalVault.PausedUntil.UTC()
	}
	for _, a := range p.Allowances {
		if !a.active(now) {
			continue
		}
		v.Allowances = append(v.Allowances, fleetVaultAllowanceView{ID: a.ID, KeyID: a.KeyID, OwnerID: a.OwnerID, AllMachines: a.AllMachines, MachineIDs: append([]string{}, a.MachineIDs...), ExpiresAt: a.ExpiresAt})
	}
	if user == legacyUser && g.superuser != "" {
		v.Keys = append(v.Keys, fleetVaultChoice{ID: superuserKeyID, Name: "operator key", Role: roleSuperuser})
	}
	for id, k := range g.keys {
		if g.vaultExecutionKeyLocked(user, id) {
			v.Keys = append(v.Keys, fleetVaultChoice{ID: id, Name: k.Name, Role: k.Role})
		}
	}
	for _, n := range g.nodes {
		if n.UserID == user {
			v.Machines = append(v.Machines, fleetVaultChoice{ID: n.ID, Name: n.Name})
		}
	}
	profiles, err := g.clientProfilesLocked(user)
	if err != nil {
		return fleetVaultView{}, err
	}
	v.Hosts = profiles
	for _, list := range [][]fleetVaultChoice{v.Keys, v.Machines} {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Name != list[j].Name {
				return nodeNameLess(list[i].Name, list[j].Name)
			}
			return list[i].ID < list[j].ID
		})
	}
	return v, nil
}

// fleetVaultResultLocked builds the response view after a successful read or
// write. Caller holds g.mu.
func (g *Gateway) fleetVaultResultLocked(user string, status int) (fleetVaultView, int, string) {
	v, err := g.fleetVaultViewLocked(user, time.Now())
	if err != nil {
		return fleetVaultView{}, http.StatusInternalServerError, "could not read fleet lock"
	}
	return v, status, ""
}

// respondFleetVault runs fn under g.mu and encodes its view only after
// releasing the lock, so a slow browser never holds it while the network
// blocks. A non-empty message is an error response.
func (g *Gateway) respondFleetVault(w http.ResponseWriter, fn func() (fleetVaultView, int, string)) {
	g.mu.Lock()
	v, status, message := fn()
	g.mu.Unlock()
	if message != "" {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *Gateway) getFleetVault(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	g.respondFleetVault(w, func() (fleetVaultView, int, string) {
		return g.fleetVaultResultLocked(p.UserID, http.StatusOK)
	})
}

// decodeStrict reads exactly one bounded JSON object with known fields.
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	return d.Decode(dst) == nil && d.Decode(new(any)) == io.EOF
}

// validVaultSeconds accepts an absent or zero duration as none, or a whole
// number of seconds within the bound.
func validVaultSeconds(seconds *int64) bool {
	return seconds == nil || (*seconds >= 0 && *seconds <= maxFleetVaultSeconds)
}

// nextFleetVaultLocked copies user's policy for editing, dropping expired
// allowances. Caller holds g.mu.
func (g *Gateway) nextFleetVaultLocked(user string, now time.Time) (fleetVaultPolicy, fleetVaultPolicy, bool) {
	previous, existed := g.vaultPolicies[user]
	next := previous
	next.UserID = user
	next.Allowances = nil
	for _, a := range previous.Allowances {
		if a.active(now) {
			a.MachineIDs = slices.Clone(a.MachineIDs)
			next.Allowances = append(next.Allowances, a)
		}
	}
	if !next.Enabled || !now.Before(next.PausedUntil) {
		next.PausedUntil = time.Time{}
	}
	next.Revision = previous.Revision + 1
	return previous, next, existed
}

// commitFleetVaultLocked makes next and its audit event durable together,
// restoring the previous policy on failure. Caller holds g.mu.
func (g *Gateway) commitFleetVaultLocked(previous, next fleetVaultPolicy, existed bool, event AuditEvent) error {
	g.vaultPolicies[next.UserID] = next
	if err := g.commit(change{vaultPolicies: []string{next.UserID}, audit: []AuditEvent{event}}); err != nil {
		if existed {
			g.vaultPolicies[next.UserID] = previous
		} else {
			delete(g.vaultPolicies, next.UserID)
		}
		return err
	}
	return nil
}

const fleetVaultConflict = "fleet lock changed; reload before editing"

func (g *Gateway) setFleetVault(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Revision     *int64 `json:"revision"`
		Enabled      *bool  `json:"enabled"`
		PauseSeconds *int64 `json:"pauseSeconds"`
	}
	if !decodeStrict(w, r, 4096, &q) || q.Revision == nil || q.Enabled == nil || !validVaultSeconds(q.PauseSeconds) || (!*q.Enabled && q.PauseSeconds != nil && *q.PauseSeconds != 0) {
		http.Error(w, "supply revision, enabled, and an optional pause of up to 365 days in whole seconds for an enabled lock", http.StatusBadRequest)
		return
	}
	p := g.requestPrincipal(r)
	g.respondFleetVault(w, func() (fleetVaultView, int, string) {
		now := time.Now().UTC()
		if !g.consolePolicySessionLiveLocked(r) {
			return fleetVaultView{}, http.StatusUnauthorized, "session expired; sign in again"
		}
		if *q.Revision != g.vaultPolicies[p.UserID].Revision {
			return fleetVaultView{}, http.StatusConflict, fleetVaultConflict
		}
		if *q.Enabled {
			var old []string
			for _, n := range g.nodes {
				if n.UserID == p.UserID && !n.FleetVaultCheck {
					old = append(old, n.Name)
				}
			}
			if len(old) != 0 {
				sort.Strings(old)
				if len(old) > 5 {
					old = append(old[:5], fmt.Sprintf("and %d more", len(old)-5))
				}
				return fleetVaultView{}, http.StatusConflict, "upgrade these registered workers to a version that enforces the fleet lock before enabling it: " + strings.Join(old, ", ")
			}
		}
		previous, next, existed := g.nextFleetVaultLocked(p.UserID, now)
		next.Enabled, next.PausedUntil = *q.Enabled, time.Time{}
		action, detail := "vault.unlock", ""
		if next.Enabled {
			action = "vault.lock"
			if q.PauseSeconds != nil && *q.PauseSeconds > 0 {
				next.PausedUntil = now.Add(time.Duration(*q.PauseSeconds) * time.Second)
				action, detail = "vault.pause", "strict again at "+next.PausedUntil.Format(time.RFC3339)
			}
		}
		if err := g.commitFleetVaultLocked(previous, next, existed, g.auditEventLocked(r, "", action, "fleet lock", "", detail)); err != nil {
			return fleetVaultView{}, http.StatusInternalServerError, "could not persist fleet lock"
		}
		return g.fleetVaultResultLocked(p.UserID, http.StatusOK)
	})
}

func (g *Gateway) addFleetVaultAllowance(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Revision    *int64   `json:"revision"`
		KeyID       string   `json:"keyId"`
		OwnerID     string   `json:"ownerId"`
		AllMachines bool     `json:"allMachines"`
		MachineIDs  []string `json:"machineIds"`
		TTLSeconds  *int64   `json:"ttlSeconds"`
	}
	if !decodeStrict(w, r, 64<<10, &q) || q.Revision == nil || !validVaultSeconds(q.TTLSeconds) {
		http.Error(w, "supply revision, selectors, machines, and an optional lifetime of up to 365 days in whole seconds", http.StatusBadRequest)
		return
	}
	if (q.KeyID == "" && q.OwnerID == "") || len(q.KeyID) > maxFleetVaultIDBytes || len(q.OwnerID) > maxFleetVaultIDBytes {
		http.Error(w, "select an execution credential, a client profile, or both", http.StatusBadRequest)
		return
	}
	if q.AllMachines == (len(q.MachineIDs) != 0) || len(q.MachineIDs) > maxFleetVaultMachines {
		http.Error(w, fmt.Sprintf("select all machines or list 1..%d machines", maxFleetVaultMachines), http.StatusBadRequest)
		return
	}
	machines := slices.Clone(q.MachineIDs)
	slices.Sort(machines)
	if len(slices.Compact(machines)) != len(q.MachineIDs) {
		http.Error(w, "machines must not repeat", http.StatusBadRequest)
		return
	}
	p := g.requestPrincipal(r)
	g.respondFleetVault(w, func() (fleetVaultView, int, string) {
		now := time.Now().UTC()
		if !g.consolePolicySessionLiveLocked(r) {
			return fleetVaultView{}, http.StatusUnauthorized, "session expired; sign in again"
		}
		if *q.Revision != g.vaultPolicies[p.UserID].Revision {
			return fleetVaultView{}, http.StatusConflict, fleetVaultConflict
		}
		if q.KeyID != "" && !g.vaultExecutionKeyLocked(p.UserID, q.KeyID) {
			return fleetVaultView{}, http.StatusBadRequest, "key is not a current execution-capable key of this fleet"
		}
		if q.OwnerID != "" {
			owner, err := g.vaultClientOwnerLocked(p.UserID, q.OwnerID)
			if err != nil {
				return fleetVaultView{}, http.StatusInternalServerError, "could not verify client profile"
			}
			if !owner {
				return fleetVaultView{}, http.StatusBadRequest, "profile is not a client owner identity of this fleet"
			}
		}
		for _, id := range machines {
			if n, ok := g.nodes[id]; !ok || n.UserID != p.UserID {
				return fleetVaultView{}, http.StatusBadRequest, "machine is not registered in this fleet"
			}
		}
		previous, next, existed := g.nextFleetVaultLocked(p.UserID, now)
		if len(next.Allowances) >= maxFleetVaultAllowances {
			return fleetVaultView{}, http.StatusConflict, "allowance limit reached; remove one first"
		}
		a := fleetVaultAllowance{ID: identity.NewID(), KeyID: q.KeyID, OwnerID: q.OwnerID, AllMachines: q.AllMachines, MachineIDs: machines, CreatedAt: now}
		if q.TTLSeconds != nil && *q.TTLSeconds > 0 {
			a.ExpiresAt = now.Add(time.Duration(*q.TTLSeconds) * time.Second)
		}
		next.Allowances = append(next.Allowances, a)
		if err := g.commitFleetVaultLocked(previous, next, existed, g.auditEventLocked(r, "", "vault.allow", "fleet lock", a.ID, allowanceDetail(a))); err != nil {
			return fleetVaultView{}, http.StatusInternalServerError, "could not persist allowance"
		}
		return g.fleetVaultResultLocked(p.UserID, http.StatusCreated)
	})
}

func (g *Gateway) removeFleetVaultAllowance(w http.ResponseWriter, r *http.Request) {
	// Like other console deletions, the precondition may travel in the query;
	// a JSON body {"revision":N} is accepted too.
	var q struct {
		Revision *int64 `json:"revision"`
	}
	if value := r.URL.Query().Get("revision"); value != "" {
		revision, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			http.Error(w, "revision must be an integer", http.StatusBadRequest)
			return
		}
		q.Revision = &revision
	} else if !decodeStrict(w, r, 1024, &q) {
		q.Revision = nil
	}
	if q.Revision == nil {
		http.Error(w, "supply the fleet lock revision", http.StatusBadRequest)
		return
	}
	p := g.requestPrincipal(r)
	g.respondFleetVault(w, func() (fleetVaultView, int, string) {
		now := time.Now().UTC()
		if !g.consolePolicySessionLiveLocked(r) {
			return fleetVaultView{}, http.StatusUnauthorized, "session expired; sign in again"
		}
		if *q.Revision != g.vaultPolicies[p.UserID].Revision {
			return fleetVaultView{}, http.StatusConflict, fleetVaultConflict
		}
		previous, next, existed := g.nextFleetVaultLocked(p.UserID, now)
		index := slices.IndexFunc(next.Allowances, func(a fleetVaultAllowance) bool { return a.ID == r.PathValue("id") })
		if index < 0 {
			return fleetVaultView{}, http.StatusNotFound, "allowance not found"
		}
		removed := next.Allowances[index]
		next.Allowances = slices.Delete(next.Allowances, index, index+1)
		if err := g.commitFleetVaultLocked(previous, next, existed, g.auditEventLocked(r, "", "vault.revoke", "fleet lock", removed.ID, allowanceDetail(removed))); err != nil {
			return fleetVaultView{}, http.StatusInternalServerError, "could not persist allowance removal"
		}
		return g.fleetVaultResultLocked(p.UserID, http.StatusOK)
	})
}

// allowanceDetail summarizes a rule for the audit log. Identifiers are not
// secrets; machine lists are summarized by count.
func allowanceDetail(a fleetVaultAllowance) string {
	parts := []string{}
	if a.KeyID != "" {
		parts = append(parts, "key "+a.KeyID)
	}
	if a.OwnerID != "" {
		parts = append(parts, "profile "+a.OwnerID)
	}
	if a.AllMachines {
		parts = append(parts, "all machines")
	} else {
		parts = append(parts, fmt.Sprintf("%d machines", len(a.MachineIDs)))
	}
	if !a.ExpiresAt.IsZero() {
		parts = append(parts, "until "+a.ExpiresAt.Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

var errVaultProvenance = errors.New("issuer provenance does not match current state")

// resolveVaultIssuerLocked derives the stable key and owner for a check. An
// issuer is the account client's authenticated transport identity: its owner
// comes from the durable transport binding and its key only from a live
// session. Otherwise KeyID and OwnerID are receiver-stored provenance from an
// earlier decision; both are revalidated against current state and never
// issue or extend a grant. A foreign or non-client identity, an unknown owner,
// or a revoked or discovery-only key yields errVaultProvenance. Caller holds
// g.mu.
func (g *Gateway) resolveVaultIssuerLocked(fleet string, q model.FleetVaultCheck) (keyID, ownerID string, err error) {
	if q.Issuer != "" {
		if err := g.loadIdentityLocked(q.Issuer); err != nil {
			return "", "", err
		}
		if g.owners[q.Issuer] != fleet {
			return "", "", errVaultProvenance
		}
		switch {
		case g.clientTransports[q.Issuer] != "":
			ownerID = g.clientTransports[q.Issuer]
		case g.clientOwners[q.Issuer]:
			ownerID = q.Issuer
		default:
			// Machines and other identities are never account-client issuers.
			return "", "", errVaultProvenance
		}
		if c := g.peers[q.Issuer]; c != nil && c.client && c.userID == fleet {
			keyID = c.keyID
		}
	} else {
		if q.OwnerID != "" {
			owner, err := g.vaultClientOwnerLocked(fleet, q.OwnerID)
			if err != nil {
				return "", "", err
			}
			if !owner {
				return "", "", errVaultProvenance
			}
			ownerID = q.OwnerID
		}
		keyID = q.KeyID
	}
	if keyID != "" && !g.vaultExecutionKeyLocked(fleet, keyID) {
		return "", "", errVaultProvenance
	}
	return keyID, ownerID, nil
}

// fleetVaultCheckLocked decides one worker request. While the lock is
// enforced, it allows only a current execution key together with a proven
// stable owner, both covered by one allowance for every machine: a durable
// owner alone never admits a disconnected issuer, a revoked key, or a
// discovery-only client, even under a host-only rule. An unenforced lock
// allows unknown issuers but still reports a resolvable identity. Caller holds
// g.mu.
func (g *Gateway) fleetVaultCheckLocked(fleet string, q model.FleetVaultCheck, now time.Time) (model.FleetVaultDecision, error) {
	keyID, ownerID, err := g.resolveVaultIssuerLocked(fleet, q)
	if err != nil && !errors.Is(err, errVaultProvenance) {
		return model.FleetVaultDecision{}, err
	}
	result := model.FleetVaultDecision{KeyID: keyID, OwnerID: ownerID}
	if !g.fleetVaultEnforcedLocked(fleet, now) {
		result.Allowed = true
		return result, nil
	}
	if err != nil || keyID == "" || ownerID == "" {
		return result, nil
	}
	for _, id := range q.MachineIDs {
		if n, ok := g.nodes[id]; !ok || n.UserID != fleet {
			return result, nil
		}
	}
	result.Allowed = fleetDecision(g.vaultPolicies[fleet], keyID, ownerID, q.MachineIDs, now)
	return result, nil
}

func (g *Gateway) fleetVaultCheckHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := g.requestPrincipal(r)
	var q model.FleetVaultCheck
	if !decodeStrict(w, r, 64<<10, &q) || len(q.Issuer) > maxFleetVaultIDBytes || len(q.KeyID) > maxFleetVaultIDBytes || len(q.OwnerID) > maxFleetVaultIDBytes || len(q.MachineIDs) > maxFleetVaultMachines {
		http.Error(w, fmt.Sprintf("supply an issuer or stored key and owner, and 1..%d machineIds", maxFleetVaultMachines), http.StatusBadRequest)
		return
	}
	for _, id := range q.MachineIDs {
		if len(id) > maxFleetVaultIDBytes {
			http.Error(w, "invalid machine ID", http.StatusBadRequest)
			return
		}
	}
	q = q.Normalize()
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer g.trimIdentityCache()
	g.mu.Lock()
	result, err := g.fleetVaultCheckLocked(p.UserID, q, time.Now())
	g.mu.Unlock()
	if err != nil {
		http.Error(w, "could not verify issuer", http.StatusInternalServerError)
		return
	}
	consoleJSON(w, result)
}
