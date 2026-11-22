package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SyncState is the per-collection bookkeeping that drives full vs incremental syncs.
type SyncState struct {
	Resource Resource
	// BackfillComplete is true once a sync has paged through the entire history without error.
	BackfillComplete    bool
	LastFullSyncAt      *time.Time
	LastFullSyncRecords *int64
	// LastSuccessAt is when the last successful sync (of either kind) started. Incremental
	// syncs re-read from this point minus the lookback window, so downtime never leaves a gap.
	LastSuccessAt *time.Time
	LastError     string
	LastErrorAt   *time.Time
}

func (db *DB) SyncState(ctx context.Context, res Resource) (SyncState, error) {
	st := SyncState{Resource: res}
	var full, success, errAt sql.NullTime
	var records sql.NullInt64
	var lastErr sql.NullString
	err := db.sql.QueryRowContext(ctx, `
		SELECT backfill_complete, last_full_sync_at, last_full_sync_records, last_success_at, last_error, last_error_at
		FROM whoop_sync_state WHERE resource = $1`, string(res),
	).Scan(&st.BackfillComplete, &full, &records, &success, &lastErr, &errAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.LastFullSyncAt, st.LastSuccessAt, st.LastErrorAt = timePtr(full), timePtr(success), timePtr(errAt)
	if records.Valid {
		st.LastFullSyncRecords = &records.Int64
	}
	st.LastError = lastErr.String
	return st, nil
}

// RecordSyncSuccess marks a collection synced as of startedAt. A full sync also marks the
// history as completely downloaded.
func (db *DB) RecordSyncSuccess(ctx context.Context, res Resource, startedAt time.Time, full bool, records int) error {
	var err error
	if full {
		_, err = db.sql.ExecContext(ctx, `
			INSERT INTO whoop_sync_state (resource, backfill_complete, last_full_sync_at, last_full_sync_records, last_success_at)
			VALUES ($1, true, $2, $3, $2)
			ON CONFLICT (resource) DO UPDATE SET
				backfill_complete = true, last_full_sync_at = $2, last_full_sync_records = $3,
				last_success_at = $2, last_error = NULL, last_error_at = NULL`,
			string(res), startedAt, records)
	} else {
		_, err = db.sql.ExecContext(ctx, `
			INSERT INTO whoop_sync_state (resource, last_success_at) VALUES ($1, $2)
			ON CONFLICT (resource) DO UPDATE SET last_success_at = $2, last_error = NULL, last_error_at = NULL`,
			string(res), startedAt)
	}
	return err
}

func (db *DB) RecordSyncError(ctx context.Context, res Resource, at time.Time, msg string) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO whoop_sync_state (resource, last_error, last_error_at) VALUES ($1, $2, $3)
		ON CONFLICT (resource) DO UPDATE SET last_error = $2, last_error_at = $3`,
		string(res), msg, at)
	return err
}

// SyncRun is one invocation of the syncer.
type SyncRun struct {
	ID         int64      `json:"id"`
	Mode       string     `json:"mode"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status"`
	Requests   int64      `json:"requests"`
	Error      string     `json:"error,omitempty"`
}

func (db *DB) StartRun(ctx context.Context, mode string, at time.Time) (int64, error) {
	var id int64
	err := db.sql.QueryRowContext(ctx,
		`INSERT INTO whoop_sync_run (mode, started_at) VALUES ($1, $2) RETURNING id`, mode, at).Scan(&id)
	return id, err
}

func (db *DB) FinishRun(ctx context.Context, id int64, at time.Time, requests int64, runErr error) error {
	status, msg := "ok", sql.NullString{}
	if runErr != nil {
		status, msg = "failed", sql.NullString{String: runErr.Error(), Valid: true}
	}
	_, err := db.sql.ExecContext(ctx,
		`UPDATE whoop_sync_run SET finished_at = $2, status = $3, requests = $4, error = $5 WHERE id = $1`,
		id, at, status, requests, msg)
	return err
}

// LastRun returns the most recent sync run, or nil if there has been none.
func (db *DB) LastRun(ctx context.Context) (*SyncRun, error) {
	var r SyncRun
	var finished sql.NullTime
	var requests sql.NullInt64
	var msg sql.NullString
	err := db.sql.QueryRowContext(ctx, `
		SELECT id, mode, started_at, finished_at, status, requests, error
		FROM whoop_sync_run ORDER BY id DESC LIMIT 1`,
	).Scan(&r.ID, &r.Mode, &r.StartedAt, &finished, &r.Status, &requests, &msg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.FinishedAt, r.Requests, r.Error = timePtr(finished), requests.Int64, msg.String
	return &r, nil
}
