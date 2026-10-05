package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

type installation struct {
	enrollment.Invitation
	CredentialHash string `json:"credentialHash,omitempty"`
}

func (g *Gateway) installationRoutes(mux *http.ServeMux) {
	list := func(w http.ResponseWriter, r *http.Request) {
		p := g.authenticate(bearer(r))
		g.mu.Lock()
		list := []enrollment.Invitation{}
		for _, i := range g.installations {
			if i.UserID == p.UserID {
				list = append(list, i.Invitation)
			}
		}
		g.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
	for _, prefix := range []string{"/v1/admin/installations", "/v1/fleet/installations"} {
		guard := g.fleetAdmin
		if prefix == "/v1/admin/installations" {
			guard = g.admin
		}
		mux.HandleFunc("POST "+prefix, guard(g.createInstallation))
		mux.HandleFunc("GET "+prefix, guard(list))
		mux.HandleFunc("DELETE "+prefix+"/{id}", guard(g.revokeInstallation))
	}
	mux.HandleFunc("DELETE /v1/admin/nodes/{id}", g.admin(g.forgetMachine))
	mux.HandleFunc("DELETE /v1/fleet/nodes/{id}", g.fleetAdmin(g.forgetMachine))
	mux.HandleFunc("GET /install/{ticket}", g.installScript)
	mux.HandleFunc("GET /install/{ticket}/info", g.installInfo)
	mux.HandleFunc("GET /install/{ticket}/binary", g.installBinary)
	mux.HandleFunc("POST /install/{ticket}/redeem", g.redeemInstallation)
}

func (g *Gateway) installationAsset(r *http.Request, osName, arch string) (update.Asset, string, error) {
	if d := g.updates.Current(); d != nil {
		if a, ok := d.Manifest.Asset(osName, arch); ok {
			if err := update.Verify(g.updates.Blob(a.SHA256), a); err == nil {
				return a, d.Manifest.Version, nil
			}
		}
	}
	info := g.options.Software
	if info.OS == osName && info.Arch == arch && update.ValidDigest(info.SHA256) {
		path, err := os.Executable()
		if err != nil {
			return update.Asset{}, "", err
		}
		f, err := os.Open(path)
		if err != nil {
			return update.Asset{}, "", err
		}
		defer func() { _ = f.Close() }()
		stat, err := f.Stat()
		if err != nil {
			return update.Asset{}, "", err
		}
		a := update.Asset{OS: osName, Arch: arch, File: update.AssetName(osName, arch), Size: stat.Size(), SHA256: info.SHA256}
		if err := update.Verify(g.updates.Blob(a.SHA256), a); err != nil {
			if err = g.updates.Upload(f, a); err != nil {
				return a, "", err
			}
		}
		return a, info.Version, nil
	}
	if g.options.ReleaseRepo != "" && g.authenticate(bearer(r)).Role == "superuser" {
		if d, err := g.fetchRelease(r.Context()); err == nil && d != nil {
			if a, ok := d.Manifest.Asset(osName, arch); ok {
				return a, d.Manifest.Version, nil
			}
		}
	}
	return update.Asset{}, "", fmt.Errorf("no %s/%s installer binary available; publish a bundle with make update or configure a release containing that platform", osName, arch)
}

func (g *Gateway) createInstallation(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var q enrollment.Request
	if json.NewDecoder(r.Body).Decode(&q) != nil || !validName.MatchString(q.Name) || (q.OS != "linux" && q.OS != "darwin" && q.OS != "windows") || (q.Arch != "" && q.Arch != "amd64" && q.Arch != "arm64") {
		http.Error(w, "name and supported OS are required; arch may be amd64, arm64, or omitted", 400)
		return
	}
	if q.TTLSeconds == 0 {
		q.TTLSeconds = 900
	}
	if q.TTLSeconds < 60 || q.TTLSeconds > 86400 {
		http.Error(w, "ttlSeconds must be 60..86400", 400)
		return
	}
	base := g.options.PublicURL
	if base == "" {
		base = q.Gateway
	}
	base, err := enrollment.GatewayURL(base)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var asset update.Asset
	var assets []update.Asset
	var version string
	if q.Arch == "" {
		// Pin both architectures from one manifest, never a moving release target.
		deployment := g.updates.Current()
		for _, arch := range []string{"amd64", "arm64"} {
			var candidate update.Asset
			var found bool
			if deployment != nil {
				candidate, found = deployment.Manifest.Asset(q.OS, arch)
			}
			if !found || update.Verify(g.updates.Blob(candidate.SHA256), candidate) != nil {
				http.Error(w, "publish a bundle containing both amd64 and arm64 installers for "+q.OS, http.StatusConflict)
				return
			}
			assets = append(assets, candidate)
		}
		asset, version = assets[0], deployment.Manifest.Version
	} else {
		asset, version, err = g.installationAsset(r, q.OS, q.Arch)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		http.Error(w, "could not create ticket", 500)
		return
	}
	ticket := base64.RawURLEncoding.EncodeToString(secret[:])
	now := time.Now().UTC()
	i := installation{Invitation: enrollment.Invitation{UserID: p.UserID, ID: identity.NewID(), Name: q.Name, Gateway: base, Asset: asset, Assets: assets, Version: version, CreatedAt: now, ExpiresAt: now.Add(time.Duration(q.TTLSeconds) * time.Second)}}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.nodes {
		if n.UserID == p.UserID && n.Name == q.Name {
			i.ReplaceID = n.ID
		}
	}
	count := 0
	for hash, old := range g.installations {
		if old.UserID != p.UserID {
			continue
		}
		if old.RedeemedID == "" && now.After(old.ExpiresAt) {
			delete(g.installations, hash)
			continue
		}
		if old.Name == q.Name && !old.Revoked && old.RedeemedID == "" {
			http.Error(w, "name has an existing invitation", http.StatusConflict)
			return
		}
		if old.Name == q.Name && !old.Revoked && old.RedeemedID != "" {
			i.ReplaceID = old.RedeemedID
		}
		count++
	}
	if count >= 4096 {
		http.Error(w, "installation registry limit reached", http.StatusConflict)
		return
	}
	hash := enrollment.Hash(ticket)
	g.installations[hash] = i
	if err = g.persist(); err != nil {
		delete(g.installations, hash)
		http.Error(w, "could not persist invitation", 500)
		return
	}
	link := base + "/install/" + ticket
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollment.Link{Invitation: i.Invitation, URL: link, Command: enrollment.InstallCommand(link, q.OS == "windows")})
}

func (g *Gateway) pendingInstallation(w http.ResponseWriter, r *http.Request) (installation, bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	ticket := r.PathValue("ticket")
	if len(ticket) != 43 {
		http.NotFound(w, r)
		return installation{}, false
	}
	g.mu.Lock()
	i, ok := g.installations[enrollment.Hash(ticket)]
	u, userOK := g.users[i.UserID]
	g.mu.Unlock()
	if !ok || !userOK || u.Disabled || i.Revoked || i.RedeemedID != "" || !time.Now().Before(i.ExpiresAt) {
		http.Error(w, "invitation expired, revoked, or already redeemed", http.StatusGone)
		return i, false
	}
	return i, true
}

func (g *Gateway) installScript(w http.ResponseWriter, r *http.Request) {
	i, ok := g.pendingInstallation(w, r)
	if !ok {
		return
	}
	script, err := enrollment.Script(i.Invitation, i.Gateway+"/install/"+r.PathValue("ticket"))
	if err != nil {
		http.Error(w, "script generation failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(script))
}
func (g *Gateway) installInfo(w http.ResponseWriter, r *http.Request) {
	i, ok := g.pendingInstallation(w, r)
	if !ok {
		return
	}
	if arch := r.URL.Query().Get("arch"); arch != "" {
		asset, found := i.Select(arch)
		if !found {
			http.Error(w, "unsupported invitation architecture", http.StatusBadRequest)
			return
		}
		i.Asset = asset
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(i.Invitation)
}
func (g *Gateway) installBinary(w http.ResponseWriter, r *http.Request) {
	i, ok := g.pendingInstallation(w, r)
	if !ok {
		return
	}
	arch := r.URL.Query().Get("arch")
	if arch != "" || len(i.Assets) > 0 {
		asset, found := i.Select(arch)
		if !found {
			http.Error(w, "select a supported architecture with ?arch=amd64 or ?arch=arm64", http.StatusBadRequest)
			return
		}
		i.Asset = asset
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, g.updates.Blob(i.Asset.SHA256))
}

func (g *Gateway) redeemInstallation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	ticket := r.PathValue("ticket")
	var q enrollment.Redemption
	if len(ticket) != 43 || json.NewDecoder(r.Body).Decode(&q) != nil || len(q.PublicKey) != ed25519.PublicKeySize || !update.ValidDigest(q.CredentialHash) || !ed25519.Verify(q.PublicKey, enrollment.Message(ticket, q), q.Signature) {
		http.Error(w, "invalid enrollment proof", 400)
		return
	}
	hash, id := enrollment.Hash(ticket), identity.ID(q.PublicKey)
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.installations[hash]
	u, userOK := g.users[i.UserID]
	if !ok || !userOK || u.Disabled || i.Revoked {
		http.Error(w, "invitation unavailable", http.StatusGone)
		return
	}
	if i.RedeemedID != "" {
		if i.RedeemedID != id || i.CredentialHash != q.CredentialHash || (q.Arch != "" && q.Arch != i.Asset.Arch) || (len(i.Assets) > 0 && q.Arch == "") {
			http.Error(w, "invitation already redeemed", http.StatusGone)
			return
		}
		// The same locally persisted identity can recover a lost HTTP response.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !time.Now().Before(i.ExpiresAt) {
		http.Error(w, "invitation expired", http.StatusGone)
		return
	}
	if owner := g.owners[id]; owner != "" && owner != i.UserID {
		http.Error(w, "identity cannot be enrolled", http.StatusConflict)
		return
	}
	if g.machineStates[id].Unregistered || (i.ReplaceID != "" && i.ReplaceID != id) {
		http.Error(w, "invitation requires an eligible local identity", http.StatusConflict)
		return
	}
	for _, n := range g.nodes {
		if n.UserID == i.UserID && n.Name == i.Name && n.ID != id {
			http.Error(w, "name is already registered", http.StatusConflict)
			return
		}
	}
	old := i
	if q.Arch != "" || len(i.Assets) > 0 {
		asset, found := i.Select(q.Arch)
		if !found {
			http.Error(w, "unsupported invitation architecture", http.StatusBadRequest)
			return
		}
		i.Asset = asset
	}
	oldOwner := g.owners[id]
	// A signed redemption replaces this identity's previous installation, even
	// when its name changes. Commit the rename and credential revocation together.
	previousNode, registered := g.nodes[id]
	if registered {
		n := previousNode
		n.Name = i.Name
		g.nodes[id] = n
	}
	previousInstallations := map[string]installation{}
	for h, previous := range g.installations {
		if h != hash && previous.RedeemedID == id && !previous.Revoked {
			previousInstallations[h] = previous
			previous.Revoked = true
			g.installations[h] = previous
		}
	}
	g.owners[id] = i.UserID
	i.RedeemedID, i.CredentialHash = id, q.CredentialHash
	g.installations[hash] = i
	if err := g.persist(); err != nil {
		if registered {
			g.nodes[id] = previousNode
		}
		for h, previous := range previousInstallations {
			g.installations[h] = previous
		}
		if oldOwner == "" {
			delete(g.owners, id)
		}
		g.installations[hash] = old
		http.Error(w, "could not persist enrollment", 500)
		return
	}
	if peer := g.peers[id]; peer != nil {
		_ = peer.ws.CloseNow()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) revokeInstallation(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	g.mu.Lock()
	var hash string
	var old installation
	for h, i := range g.installations {
		if i.UserID == p.UserID && i.ID == r.PathValue("id") {
			hash, old = h, i
			break
		}
	}
	if hash == "" {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	i := old
	i.Revoked = true
	g.installations[hash] = i
	err := g.persist()
	if err != nil {
		g.installations[hash] = old
	}
	peer := g.peers[i.RedeemedID]
	g.mu.Unlock()
	if err != nil {
		http.Error(w, "could not persist revocation", 500)
		return
	}
	if peer != nil {
		_ = peer.ws.CloseNow()
	}
	w.WriteHeader(http.StatusNoContent)
}

// Called under g.mu after checking the registration signature.
func (g *Gateway) allowInstallation(keyID string, n model.Node) bool {
	bound := strings.HasPrefix(keyID, "install:")
	allowed := !bound
	for _, i := range g.installations {
		if i.UserID != n.UserID {
			continue
		}
		if keyID == "install:"+i.ID {
			if i.Revoked || i.RedeemedID != n.ID || i.Name != n.Name || i.Asset.OS != n.OS || i.Asset.Arch != n.Software.Arch {
				return false
			}
			allowed = true
			continue
		}
		if !i.Revoked && i.RedeemedID == n.ID {
			return false
		}
		if i.Name == n.Name && !i.Revoked && i.ReplaceID != n.ID && (i.RedeemedID != "" || time.Now().Before(i.ExpiresAt)) {
			return false
		}
	}
	return allowed
}

func (g *Gateway) forgetMachine(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	g.mu.Lock()
	id := r.PathValue("id")
	n, ok := g.nodes[id]
	if !ok || n.UserID != p.UserID {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	peer := g.peers[id]
	if peer != nil && r.URL.Query().Get("stop") != "true" {
		g.mu.Unlock()
		http.Error(w, "stop the machine before forgetting its registration", http.StatusConflict)
		return
	}
	old := map[string]installation{}
	for hash, i := range g.installations {
		if i.RedeemedID == id {
			old[hash] = i
			i.Revoked = true
			g.installations[hash] = i
		}
	}
	delete(g.nodes, id)
	previous := g.machineStates[id]
	g.machineStates[id] = model.MachineState{Revision: previous.Revision + 1, Disabled: true, Unregistered: true}
	if err := g.persist(); err != nil {
		g.machineStates[id] = previous
		g.nodes[id] = n
		for hash, i := range old {
			g.installations[hash] = i
		}
		g.mu.Unlock()
		http.Error(w, "could not remove registration", 500)
		return
	}
	delete(g.updateStatus, id)
	g.mu.Unlock()
	if peer != nil {
		_ = peer.ws.CloseNow()
	}
	w.WriteHeader(http.StatusNoContent)
}
