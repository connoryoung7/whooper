package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Resource names a WHOOP collection.
type Resource string

const (
	Cycles     Resource = "cycle"
	Sleeps     Resource = "sleep"
	Recoveries Resource = "recovery"
	Workouts   Resource = "workout"
)

// Resources lists every collection the mirror stores.
var Resources = []Resource{Cycles, Sleeps, Recoveries, Workouts}

type table struct {
	name   string
	upsert string
	// columns extracts the typed columns (in upsertSQL order) from a raw record.
	columns func(raw json.RawMessage) ([]any, error)
}

// timing holds fields shared by most WHOOP records.
type timing struct {
	Start      *time.Time `json:"start"`
	End        *time.Time `json:"end"`
	UpdatedAt  *time.Time `json:"updated_at"`
	ScoreState *string    `json:"score_state"`
}

var tables = map[Resource]table{
	Cycles: {
		name:   "whoop_cycle",
		upsert: upsertSQL("whoop_cycle", "id", "start_at", "end_at", "updated_at", "score_state"),
		columns: func(raw json.RawMessage) ([]any, error) {
			var r struct {
				ID int64 `json:"id"`
				timing
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, err
			}
			if r.ID == 0 || r.Start == nil {
				return nil, errors.New("missing id or start")
			}
			return []any{r.ID, r.Start, r.End, r.UpdatedAt, r.ScoreState}, nil
		},
	},
	Sleeps: {
		name:   "whoop_sleep",
		upsert: upsertSQL("whoop_sleep", "id", "cycle_id", "nap", "start_at", "end_at", "updated_at", "score_state"),
		columns: func(raw json.RawMessage) ([]any, error) {
			var r struct {
				ID      string `json:"id"`
				CycleID *int64 `json:"cycle_id"`
				Nap     bool   `json:"nap"`
				timing
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, err
			}
			if r.ID == "" || r.Start == nil {
				return nil, errors.New("missing id or start")
			}
			return []any{r.ID, r.CycleID, r.Nap, r.Start, r.End, r.UpdatedAt, r.ScoreState}, nil
		},
	},
	Recoveries: {
		name:   "whoop_recovery",
		upsert: upsertSQL("whoop_recovery", "cycle_id", "sleep_id", "updated_at", "score_state"),
		columns: func(raw json.RawMessage) ([]any, error) {
			var r struct {
				CycleID int64  `json:"cycle_id"`
				SleepID string `json:"sleep_id"`
				timing
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, err
			}
			if r.CycleID == 0 {
				return nil, errors.New("missing cycle_id")
			}
			var sleepID *string
			if r.SleepID != "" {
				sleepID = &r.SleepID
			}
			return []any{r.CycleID, sleepID, r.UpdatedAt, r.ScoreState}, nil
		},
	},
	Workouts: {
		name:   "whoop_workout",
		upsert: upsertSQL("whoop_workout", "id", "sport_name", "start_at", "end_at", "updated_at", "score_state"),
		columns: func(raw json.RawMessage) ([]any, error) {
			var r struct {
				ID        string  `json:"id"`
				SportName *string `json:"sport_name"`
				timing
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, err
			}
			if r.ID == "" || r.Start == nil {
				return nil, errors.New("missing id or start")
			}
			return []any{r.ID, r.SportName, r.Start, r.End, r.UpdatedAt, r.ScoreState}, nil
		},
	},
}

// upsertSQL builds an INSERT … ON CONFLICT for a collection table. cols[0] is the primary key;
// the statement takes len(cols) column values, then raw JSON, then the seen-at time.
func upsertSQL(tbl string, cols ...string) string {
	n := len(cols)
	names := append(append([]string{}, cols...), "raw", "first_seen_at", "last_seen_at")
	var vals []string
	for i := range cols {
		vals = append(vals, fmt.Sprintf("$%d", i+1))
	}
	vals = append(vals, fmt.Sprintf("$%d::jsonb", n+1), fmt.Sprintf("$%d", n+2), fmt.Sprintf("$%d", n+2))
	var set []string
	for _, c := range cols[1:] {
		set = append(set, c+" = EXCLUDED."+c)
	}
	set = append(set, "raw = EXCLUDED.raw", "last_seen_at = EXCLUDED.last_seen_at", "deleted_at = NULL")
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		tbl, strings.Join(names, ", "), strings.Join(vals, ", "), cols[0], strings.Join(set, ", "))
}

func tableFor(res Resource) (table, error) {
	t, ok := tables[res]
	if !ok {
		return table{}, fmt.Errorf("unknown resource %q", res)
	}
	return t, nil
}

// UpsertRecords stores one page of records in a single transaction and stamps them as seen.
// A record that can't be parsed fails the whole page rather than being skipped silently.
func (db *DB) UpsertRecords(ctx context.Context, res Resource, records []json.RawMessage, seenAt time.Time) error {
	t, err := tableFor(res)
	if err != nil || len(records) == 0 {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, t.upsert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, raw := range records {
		args, err := t.columns(raw)
		if err != nil {
			return fmt.Errorf("unexpected %s record (%v): %s", res, err, snippet(raw))
		}
		if _, err := stmt.ExecContext(ctx, append(args, string(raw), seenAt)...); err != nil {
			return fmt.Errorf("storing %s record %s: %w", res, snippet(raw), err)
		}
	}
	return tx.Commit()
}

// MarkUnseenDeleted soft-deletes every live record not seen since seenSince. Call it only
// after a full sync has walked the entire collection.
func (db *DB) MarkUnseenDeleted(ctx context.Context, res Resource, seenSince time.Time) (int64, error) {
	t, err := tableFor(res)
	if err != nil {
		return 0, err
	}
	r, err := db.sql.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET deleted_at = $1 WHERE deleted_at IS NULL AND last_seen_at < $1`, t.name), seenSince)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// LiveCount counts records not marked deleted.
func (db *DB) LiveCount(ctx context.Context, res Resource) (int64, error) {
	t, err := tableFor(res)
	if err != nil {
		return 0, err
	}
	var n int64
	err = db.sql.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE deleted_at IS NULL`, t.name)).Scan(&n)
	return n, err
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
