package gateway

import (
	"fmt"
	"testing"
)

func TestIdentityCacheEvictionPreservesLedgerAndNames(t *testing.T) {
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	var ids []string
	for i := range maxIdentityCache + 10 {
		id := fmt.Sprintf("%016x%048x", i, i)
		g.owners[id] = legacyUser
		g.clientOwners[id] = true
		ids = append(ids, id)
	}
	if err = g.commit(change{identities: ids, clients: ids}); err != nil {
		t.Fatal(err)
	}
	g.trimIdentityCacheLocked()
	if len(g.clientOwners) > maxIdentityCache {
		t.Fatal("unbounded cache")
	}
	var evicted string
	for _, id := range ids {
		if !g.clientOwners[id] {
			evicted = id
			break
		}
	}
	if evicted == "" {
		t.Fatal("no eviction")
	}
	if ok, err := g.storedClientName(legacyUser, "cli-"+evicted[:16], ""); err != nil || !ok {
		t.Fatal("lost name reservation", err)
	}
	if ok, err := g.storedClientName("another-user", "cli-"+evicted[:16], ""); err != nil || ok {
		t.Fatal("name crossed fleets", err)
	}
	if err = g.loadIdentityLocked(evicted); err != nil {
		t.Fatal(err)
	}
	if g.owners[evicted] != legacyUser || !g.clientOwners[evicted] {
		t.Fatal("lost immutable identity")
	}
	var count int
	if err = g.db.QueryRow(`SELECT count(*) FROM client_identities`).Scan(&count); err != nil || count != len(ids) {
		t.Fatal("eviction changed ledger", count, err)
	}
}
