package gateway

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// schemaColumn is one table column as SQLite reports it in table_info.
type schemaColumn struct {
	Type    string
	NotNull bool
	PK      int
}

// tableColumns maps each user table to its columns by name.
type tableColumns map[string]map[string]schemaColumn

// expectedSchema returns the tables and columns a database at version has,
// built by applying initialSchema and the upgrade steps up to version to a
// private in-memory database, so checks follow schema.sql and schemaUpgrades
// rather than a separate list.
func expectedSchema(ctx context.Context, version int) (tableColumns, error) {
	if version < baseSchemaVersion || version > currentSchemaVersion {
		return nil, fmt.Errorf("unsupported schema %d", version)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	// Each connection to :memory: is a separate database.
	db.SetMaxOpenConns(1)
	for i, step := range append([]string{initialSchema}, schemaUpgrades[:version-baseSchemaVersion]...) {
		if _, err = db.ExecContext(ctx, step); err != nil {
			return nil, fmt.Errorf("build schema %d: %w", baseSchemaVersion+i, err)
		}
	}
	return readSchema(ctx, db)
}

// readSchema reads the columns of every user table in db.
func readSchema(ctx context.Context, db *sql.DB) (tableColumns, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.name, c.name, upper(c.type), c."notnull", c.pk
		FROM sqlite_master AS m, pragma_table_info(m.name) AS c
		WHERE m.type='table' AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tables := tableColumns{}
	for rows.Next() {
		var table, name string
		var column schemaColumn
		if err = rows.Scan(&table, &name, &column.Type, &column.NotNull, &column.PK); err != nil {
			return nil, err
		}
		if tables[table] == nil {
			tables[table] = map[string]schemaColumn{}
		}
		tables[table][name] = column
	}
	return tables, rows.Err()
}

// checkSchema reports tables and columns of version's schema that db lacks
// or declares differently. Additional tables and columns are allowed; the
// gateway does not read them.
func checkSchema(ctx context.Context, db *sql.DB, version int) error {
	want, err := expectedSchema(ctx, version)
	if err != nil {
		return err
	}
	got, err := readSchema(ctx, db)
	if err != nil {
		return err
	}
	var problems []string
	for _, table := range slices.Sorted(maps.Keys(want)) {
		have, ok := got[table]
		if !ok {
			problems = append(problems, "missing table "+table)
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(want[table])) {
			column, ok := have[name]
			switch {
			case !ok:
				problems = append(problems, "missing column "+table+"."+name)
			case column != want[table][name]:
				problems = append(problems, "column "+table+"."+name+" differs")
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("schema %d mismatch: %s", version, clip(strings.Join(problems, ", "), 512))
	}
	return nil
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
