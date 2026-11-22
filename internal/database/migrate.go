package database

import (
	"context"
	"embed"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrationFiles are numbered goose SQL migrations, applied in order and recorded in the
// goose_db_version table. Never edit a migration that has been deployed; add a new one with
// `make migration name=<snake_case_name>`.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

func (db *DB) migrator() (*goose.Provider, error) {
	fsys, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	// The web server and a scheduled sync can start at the same moment. The session lock makes
	// the later one wait (retrying every second, for up to 5 minutes) until the first is done.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return nil, err
	}
	// Don't Close the provider: that would close db.sql.
	return goose.NewProvider(goose.DialectPostgres, db.sql, fsys, goose.WithSessionLocker(locker))
}

// MigrateUp applies every pending migration and returns the ones it applied.
func (db *DB) MigrateUp(ctx context.Context) ([]*goose.MigrationResult, error) {
	p, err := db.migrator()
	if err != nil {
		return nil, err
	}
	return p.Up(ctx)
}

// MigrateDown rolls back the most recently applied migration.
func (db *DB) MigrateDown(ctx context.Context) (*goose.MigrationResult, error) {
	p, err := db.migrator()
	if err != nil {
		return nil, err
	}
	return p.Down(ctx)
}

// MigrationStatus lists every known migration and whether it has been applied.
func (db *DB) MigrationStatus(ctx context.Context) ([]*goose.MigrationStatus, error) {
	p, err := db.migrator()
	if err != nil {
		return nil, err
	}
	return p.Status(ctx)
}

// MigrationVersion is the version of the newest applied migration (0 if none).
func (db *DB) MigrationVersion(ctx context.Context) (int64, error) {
	p, err := db.migrator()
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}
