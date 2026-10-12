package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// loadMigrations returns the embedded migrations in file name order; migration i brings the schema
// to PRAGMA user_version i+1.
func loadMigrations() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	stmts := make([]string, 0, len(entries))
	for _, e := range entries {
		b, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, string(b))
	}
	return stmts, nil
}

// migrate applies the migrations the database has not seen yet in one transaction.
func migrate(ctx context.Context, db *sql.DB, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(stmts) {
		return fmt.Errorf("database schema version %d is newer than the supported version %d", version, len(stmts))
	}
	if version == len(stmts) {
		return nil
	}
	for i := version; i < len(stmts); i++ {
		if _, err := tx.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(stmts))); err != nil {
		return fmt.Errorf("write schema version: %w", err)
	}
	return tx.Commit()
}
