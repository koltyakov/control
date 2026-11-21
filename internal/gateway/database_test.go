package gateway

import (
	"testing"
)

func TestDatabasePragmasApplyToReplacedConnections(t *testing.T) {
	g, err := New(t.TempDir(), "pragma-test-common-key-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	// Without idle connections every query opens a fresh driver connection.
	g.db.SetMaxIdleConns(0)
	for range 2 {
		var foreignKeys, busy, synchronous int
		var journal string
		if err := g.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := g.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := g.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := g.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busy != 5000 || synchronous != 2 || journal != "wal" {
			t.Fatalf("connection pragmas: foreign_keys=%d busy_timeout=%d synchronous=%d journal_mode=%s", foreignKeys, busy, synchronous, journal)
		}
	}
}
