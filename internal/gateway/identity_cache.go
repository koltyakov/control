package gateway

import (
	"database/sql"
	"errors"
	"strings"
)

const maxIdentityCache = 4096

func (g *Gateway) trimIdentityCache() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trimIdentityCacheLocked()
}

// Called under the mutation lock. Missing cache entries are never interpreted
// as permission to change a durable identity's account or role.
func (g *Gateway) loadIdentityLocked(id string) error {
	if g.owners[id] != "" {
		return nil
	}
	var user, owner string
	var client bool
	err := g.db.QueryRow(`SELECT i.user_id, c.id IS NOT NULL, coalesce(t.owner_id,'') FROM identities i LEFT JOIN client_identities c ON c.id=i.id LEFT JOIN client_transports t ON t.id=i.id WHERE i.id=?`, id).Scan(&user, &client, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	g.owners[id] = user
	if client {
		g.clientOwners[id] = true
	}
	if owner != "" {
		g.clientTransports[id] = owner
	}
	return nil
}

func (g *Gateway) trimIdentityCacheLocked() {
	active := make(map[string]bool, len(g.clients)*2)
	for id, c := range g.clients {
		active[id] = true
		active[c.ClientOwner] = true
	}
	for id := range g.clientTransports {
		if len(g.clientTransports) <= maxIdentityCache {
			break
		}
		if !active[id] {
			delete(g.clientTransports, id)
			delete(g.owners, id)
		}
	}
	for id := range g.clientOwners {
		if len(g.clientOwners) <= maxIdentityCache {
			break
		}
		if !active[id] {
			delete(g.clientOwners, id)
			delete(g.owners, id)
		}
	}
}

func (g *Gateway) storedClientName(user, name, except string) (bool, error) {
	if !strings.HasPrefix(name, "cli-") || len(name) != 20 {
		return false, nil
	}
	var found bool
	err := g.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM client_identities c JOIN identities i ON i.id=c.id WHERE substr(c.id,1,16)=? AND i.user_id=? AND c.id<>?)`, strings.TrimPrefix(name, "cli-"), user, except).Scan(&found)
	return found, err
}
