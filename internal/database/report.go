package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ResourceStats summarizes what is stored for one collection.
type ResourceStats struct {
	Resource  Resource   `json:"resource"`
	Live      int64      `json:"records"`
	Deleted   int64      `json:"deleted_in_whoop"`
	NotScored int64      `json:"not_scored"`
	Earliest  *time.Time `json:"earliest,omitempty"`
	Latest    *time.Time `json:"latest,omitempty"`
}

func (db *DB) ResourceStats(ctx context.Context, res Resource) (ResourceStats, error) {
	t, err := tableFor(res)
	if err != nil {
		return ResourceStats{}, err
	}
	from, when := t.name+" r", "r.start_at"
	if res == Recoveries { // recoveries have no start of their own; date them by their cycle
		from, when = "whoop_recovery r LEFT JOIN whoop_cycle c ON c.id = r.cycle_id", "c.start_at"
	}
	q := fmt.Sprintf(`
		SELECT
			count(*) FILTER (WHERE r.deleted_at IS NULL),
			count(*) FILTER (WHERE r.deleted_at IS NOT NULL),
			count(*) FILTER (WHERE r.deleted_at IS NULL AND r.score_state IS DISTINCT FROM 'SCORED'),
			min(%[2]s) FILTER (WHERE r.deleted_at IS NULL),
			max(%[2]s) FILTER (WHERE r.deleted_at IS NULL)
		FROM %[1]s`, from, when)
	s := ResourceStats{Resource: res}
	var earliest, latest sql.NullTime
	if err := db.sql.QueryRowContext(ctx, q).Scan(&s.Live, &s.Deleted, &s.NotScored, &earliest, &latest); err != nil {
		return s, err
	}
	s.Earliest, s.Latest = timePtr(earliest), timePtr(latest)
	return s, nil
}

// Integrity counts cross-references between collections that point at records we don't have.
// After a complete download these should all be zero (except CompletedCyclesWithoutRecovery,
// which is normal for nights WHOOP couldn't score).
type Integrity struct {
	RecoveriesMissingSleep         int64 `json:"recoveries_missing_sleep"`
	RecoveriesMissingCycle         int64 `json:"recoveries_missing_cycle"`
	SleepsMissingCycle             int64 `json:"sleeps_missing_cycle"`
	CompletedCyclesWithoutRecovery int64 `json:"completed_cycles_without_recovery"`
}

func (db *DB) Integrity(ctx context.Context) (Integrity, error) {
	var i Integrity
	err := db.sql.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM whoop_recovery r
			 WHERE r.deleted_at IS NULL AND r.sleep_id IS NOT NULL
			   AND NOT EXISTS (SELECT 1 FROM whoop_sleep s WHERE s.id = r.sleep_id AND s.deleted_at IS NULL)),
			(SELECT count(*) FROM whoop_recovery r
			 WHERE r.deleted_at IS NULL
			   AND NOT EXISTS (SELECT 1 FROM whoop_cycle c WHERE c.id = r.cycle_id AND c.deleted_at IS NULL)),
			(SELECT count(*) FROM whoop_sleep s
			 WHERE s.deleted_at IS NULL AND s.cycle_id IS NOT NULL
			   AND NOT EXISTS (SELECT 1 FROM whoop_cycle c WHERE c.id = s.cycle_id AND c.deleted_at IS NULL)),
			(SELECT count(*) FROM whoop_cycle c
			 WHERE c.deleted_at IS NULL AND c.end_at IS NOT NULL
			   AND NOT EXISTS (SELECT 1 FROM whoop_recovery r WHERE r.cycle_id = c.id AND r.deleted_at IS NULL))`,
	).Scan(&i.RecoveriesMissingSleep, &i.RecoveriesMissingCycle, &i.SleepsMissingCycle, &i.CompletedCyclesWithoutRecovery)
	return i, err
}

// Gap is a stretch of time between one cycle's end and the next cycle's start.
type Gap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// CycleGaps finds breaks longer than min in the chain of cycles (each cycle normally starts
// where the previous one ended). It returns the total count and the most recent `limit` gaps.
func (db *DB) CycleGaps(ctx context.Context, min time.Duration, limit int) (int64, []Gap, error) {
	rows, err := db.sql.QueryContext(ctx, `
		WITH ordered AS (
			SELECT start_at, lag(end_at) OVER (ORDER BY start_at) AS prev_end
			FROM whoop_cycle WHERE deleted_at IS NULL
		), gaps AS (
			SELECT prev_end, start_at FROM ordered
			WHERE prev_end IS NOT NULL AND start_at - prev_end > make_interval(secs => $1)
		)
		SELECT prev_end, start_at, count(*) OVER () FROM gaps ORDER BY start_at DESC LIMIT $2`,
		min.Seconds(), limit)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var total int64
	var gaps []Gap
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.From, &g.To, &total); err != nil {
			return 0, nil, err
		}
		gaps = append(gaps, g)
	}
	return total, gaps, rows.Err()
}
