package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"whooper/internal/whoop"
)

// UpdateToken implements whoop.TokenStore. The row lock (SELECT … FOR UPDATE) serializes
// refreshes across the web server and scheduled sync processes.
func (db *DB) UpdateToken(ctx context.Context, fn func(current *whoop.Token) (*whoop.Token, error)) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cur *whoop.Token
	var t whoop.Token
	err = tx.QueryRowContext(ctx,
		`SELECT access_token, refresh_token, expires_at, scope FROM whoop_token WHERE id = 1 FOR UPDATE`,
	).Scan(&t.AccessToken, &t.RefreshToken, &t.ExpiresAt, &t.Scope)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		cur = &t
	}

	next, err := fn(cur)
	if err != nil {
		return err
	}
	if next != nil {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO whoop_token (id, access_token, refresh_token, expires_at, scope, updated_at)
			VALUES (1, $1, $2, $3, $4, now())
			ON CONFLICT (id) DO UPDATE SET
				access_token = EXCLUDED.access_token, refresh_token = EXCLUDED.refresh_token,
				expires_at = EXCLUDED.expires_at, scope = EXCLUDED.scope, updated_at = now()`,
			next.AccessToken, next.RefreshToken, next.ExpiresAt, next.Scope)
		if err != nil {
			return fmt.Errorf("saving token: %w", err)
		}
	}
	return tx.Commit()
}

// SaveToken replaces the stored token pair.
func (db *DB) SaveToken(ctx context.Context, t *whoop.Token) error {
	return db.UpdateToken(ctx, func(*whoop.Token) (*whoop.Token, error) { return t, nil })
}

// TokenInfo describes the stored token without exposing it.
type TokenInfo struct {
	Connected   bool       `json:"connected"`
	ExpiresAt   *time.Time `json:"access_token_expires_at,omitempty"`
	RefreshedAt *time.Time `json:"refreshed_at,omitempty"`
	Scope       string     `json:"scope,omitempty"`
}

func (db *DB) TokenInfo(ctx context.Context) (TokenInfo, error) {
	var ti TokenInfo
	var exp, upd time.Time
	err := db.sql.QueryRowContext(ctx, `SELECT expires_at, updated_at, scope FROM whoop_token WHERE id = 1`).
		Scan(&exp, &upd, &ti.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return ti, nil
	}
	if err != nil {
		return ti, err
	}
	ti.Connected, ti.ExpiresAt, ti.RefreshedAt = true, &exp, &upd
	return ti, nil
}

// CreateOAuthState records a state value for an authorization attempt.
func (db *DB) CreateOAuthState(ctx context.Context, state string) error {
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM whoop_oauth_state WHERE created_at < now() - interval '1 hour'`); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, `INSERT INTO whoop_oauth_state (state) VALUES ($1)`, state)
	return err
}

// ConsumeOAuthState reports whether state was issued in the last 15 minutes, and spends it.
func (db *DB) ConsumeOAuthState(ctx context.Context, state string) (bool, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM whoop_oauth_state WHERE state = $1 AND created_at > now() - interval '15 minutes'`, state)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UpsertProfile stores the basic profile and returns its WHOOP user id.
func (db *DB) UpsertProfile(ctx context.Context, raw json.RawMessage, at time.Time) (int64, error) {
	var p struct {
		UserID int64 `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.UserID == 0 {
		return 0, fmt.Errorf("profile response has no user_id: %s", snippet(raw))
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO whoop_profile (user_id, raw, fetched_at) VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (user_id) DO UPDATE SET raw = EXCLUDED.raw, fetched_at = EXCLUDED.fetched_at`,
		p.UserID, string(raw), at)
	return p.UserID, err
}

// ProfileUserID returns the WHOOP user this database holds data for, if any.
func (db *DB) ProfileUserID(ctx context.Context) (int64, bool, error) {
	var id int64
	err := db.sql.QueryRowContext(ctx, `SELECT user_id FROM whoop_profile ORDER BY fetched_at DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// RecordBodyMeasurement stores the measurement if it differs from the last one stored.
func (db *DB) RecordBodyMeasurement(ctx context.Context, raw json.RawMessage, at time.Time) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO whoop_body_measurement (raw, fetched_at)
		SELECT $1::jsonb, $2
		WHERE NOT EXISTS (
			SELECT 1 FROM (SELECT raw FROM whoop_body_measurement ORDER BY id DESC LIMIT 1) latest
			WHERE latest.raw = $1::jsonb)`,
		string(raw), at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
