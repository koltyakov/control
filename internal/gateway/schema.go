package gateway

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// initialSchema creates a new database at baseSchemaVersion. Control v0.14.0
// created and upgraded every database to this same schema.
//
//go:embed schema.sql
var initialSchema string

const (
	// baseSchemaVersion is the oldest schema this executable opens. Earlier
	// databases and pre-SQLite JSON state must be upgraded by v0.14.0 first.
	baseSchemaVersion = 10
	// currentSchemaVersion is the newest schema this executable opens.
	currentSchemaVersion = baseSchemaVersion + len(schemaUpgrades)
)

// schemaUpgrades[i] upgrades schema baseSchemaVersion+i to the next version,
// each in its own transaction. Append a step to change the schema; new
// databases apply the same steps after initialSchema.
var schemaUpgrades = [...]string{}

// errPreBaseState reports state this executable cannot upgrade.
func errPreBaseState(what string) error {
	return fmt.Errorf("%s predates Control v0.14.0; start the gateway once with v0.14.0 to upgrade it, then install this version", what)
}

// createSchema initializes an empty database with the operator fleet.
func (g *Gateway) createSchema(dir string) error {
	for _, name := range []string{"nodes.json", "keys.json", "installations.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return errPreBaseState("gateway state in " + name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(initialSchema); err != nil {
		return err
	}
	operator := User{ID: legacyUser, Name: "legacy", CreatedAt: time.Now().UTC()}
	if _, err = tx.Exec(`INSERT INTO users(id,name,data) VALUES(?,?,?)`, operator.ID, operator.Name, model.JSON(operator)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d;", baseSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// upgradeSchema applies the step from version to version+1.
func (g *Gateway) upgradeSchema(version int) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(schemaUpgrades[version-baseSchemaVersion] + fmt.Sprintf("\nPRAGMA user_version=%d;", version+1)); err != nil {
		return err
	}
	return tx.Commit()
}
