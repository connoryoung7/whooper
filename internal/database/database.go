// Package database stores the WHOOP mirror in Postgres.
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// SyncLock is the advisory lock key held for a whole sync run so scheduled runs never
// overlap. The value is arbitrary but fixed so every process agrees on it.
const SyncLock int64 = 7_305_942_002

type DB struct {
	sql *sql.DB
}

// Open connects to Postgres and applies any pending migrations.
func Open(ctx context.Context, dsn string) (*DB, error) {
	db, err := Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	results, err := db.MigrateUp(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("applying migrations: %w", err)
	}
	for _, r := range results {
		log.Printf("applied migration %s (%s)", r.Source.Path, r.Duration.Round(time.Millisecond))
	}
	return db, nil
}

// Connect connects to Postgres without touching the schema (see Open).
func Connect(ctx context.Context, dsn string) (*DB, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	db := &DB{sql: sqlDB}

	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return db, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// Health pings the database and reports pool statistics.
func (db *DB) Health(ctx context.Context) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stats := map[string]string{}
	if err := db.sql.PingContext(ctx); err != nil {
		stats["status"] = "down"
		stats["error"] = err.Error()
		return stats
	}
	s := db.sql.Stats()
	stats["status"] = "up"
	stats["open_connections"] = strconv.Itoa(s.OpenConnections)
	stats["in_use"] = strconv.Itoa(s.InUse)
	stats["idle"] = strconv.Itoa(s.Idle)
	return stats
}

// TryLock takes a session-level advisory lock without waiting. ok is false if another
// process holds it. Call unlock when done.
func (db *DB) TryLock(ctx context.Context, key int64) (unlock func(), ok bool, err error) {
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil || !ok {
		conn.Close()
		return nil, false, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			// Couldn't unlock: discard the connection so its session, and the lock, end.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		conn.Close()
	}, true, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
