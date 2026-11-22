// Package syncer mirrors a WHOOP account's data into Postgres.
//
// Each collection is synced in one of two modes:
//
//   - full: page through the entire history (no start filter), upsert every record, then mark
//     records WHOOP no longer returns as deleted. A collection's first sync is always full, and
//     its history only counts as downloaded once a full pass finishes without error.
//   - incremental: re-read everything that started after (last successful sync − lookback).
//     Because the window is anchored to the last success rather than to now, an outage of any
//     length is caught up on the next run.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"whooper/internal/database"
	"whooper/internal/whoop"
)

// ErrLocked means another sync run holds the lock; the caller should just skip this run.
var ErrLocked = errors.New("another sync is already running")

type Syncer struct {
	DB  *database.DB
	API *whoop.Client
	// Lookback is how far before the last successful sync incremental runs re-read, so
	// records WHOOP scores or edits after the fact are picked up.
	Lookback time.Duration
	Logf     func(format string, args ...any)
}

type collection struct {
	res  database.Resource
	path string
}

// Each collection is fetched before the ones it references (recovery → sleep → cycle), so a
// record created mid-run can't point at something this run already finished fetching.
var collections = []collection{
	{database.Recoveries, "/v2/recovery"},
	{database.Sleeps, "/v2/activity/sleep"},
	{database.Cycles, "/v2/cycle"},
	{database.Workouts, "/v2/activity/workout"},
}

// Run performs one sync pass. forceFull re-downloads every collection's whole history;
// otherwise each collection is synced incrementally once its history has been downloaded.
func (s *Syncer) Run(ctx context.Context, forceFull bool) error {
	unlock, ok, err := s.DB.TryLock(ctx, database.SyncLock)
	if err != nil {
		return fmt.Errorf("taking sync lock: %w", err)
	}
	if !ok {
		return ErrLocked
	}
	defer unlock()

	mode := "auto"
	if forceFull {
		mode = "full"
	}
	started := time.Now()
	runID, err := s.DB.StartRun(ctx, mode, started)
	if err != nil {
		return err
	}
	before := s.API.Requests()
	s.logf("sync started (mode %s)", mode)

	runErr := s.run(ctx, forceFull)

	requests := s.API.Requests() - before
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.DB.FinishRun(finishCtx, runID, time.Now(), requests, runErr); err != nil {
		s.logf("recording sync run: %v", err)
	}
	if runErr != nil {
		s.logf("sync failed after %s (%d API requests): %v", time.Since(started).Round(time.Second), requests, runErr)
	} else {
		s.logf("sync finished in %s (%d API requests)", time.Since(started).Round(time.Second), requests)
	}
	return runErr
}

func (s *Syncer) run(ctx context.Context, forceFull bool) error {
	var errs []error
	if err := s.syncSnapshots(ctx); err != nil {
		if fatal(err) {
			return err
		}
		errs = append(errs, err)
	}
	for _, c := range collections {
		err := s.syncCollection(ctx, c, forceFull)
		if err == nil {
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.res, err))
		if fatal(err) {
			break
		}
	}
	return errors.Join(errs...)
}

// fatal errors would fail every remaining request too, so the run stops early.
func fatal(err error) bool {
	return errors.Is(err, whoop.ErrNotAuthorized) || errors.Is(err, whoop.ErrReauthorize) ||
		errors.Is(err, whoop.ErrRateLimited) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// syncSnapshots stores the profile and current body measurement, which have no history.
func (s *Syncer) syncSnapshots(ctx context.Context) error {
	now := time.Now()
	profile, err := s.API.GetRaw(ctx, "/v2/user/profile/basic")
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	if _, err := s.DB.UpsertProfile(ctx, profile, now); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	body, err := s.API.GetRaw(ctx, "/v2/user/measurement/body")
	if err != nil {
		return fmt.Errorf("body measurement: %w", err)
	}
	if _, err := s.DB.RecordBodyMeasurement(ctx, body, now); err != nil {
		return fmt.Errorf("body measurement: %w", err)
	}
	return nil
}

func (s *Syncer) syncCollection(ctx context.Context, c collection, forceFull bool) error {
	st, err := s.DB.SyncState(ctx, c.res)
	if err != nil {
		return err
	}
	full := forceFull || !st.BackfillComplete || st.LastSuccessAt == nil
	// Postgres stores microseconds; truncating keeps "seen this run" comparisons exact.
	started := time.Now().Truncate(time.Microsecond)
	var since time.Time
	if !full {
		since = st.LastSuccessAt.Add(-s.Lookback)
	}

	seen := 0
	err = s.API.ForEachPage(ctx, c.path, since, func(records []json.RawMessage) error {
		seen += len(records)
		return s.DB.UpsertRecords(ctx, c.res, records, started)
	})
	if err != nil {
		s.recordError(ctx, c.res, err)
		return err
	}

	if !full {
		s.logf("%s: incremental sync since %s: %d records", c.res, since.UTC().Format(time.RFC3339), seen)
		return s.DB.RecordSyncSuccess(ctx, c.res, started, false, seen)
	}
	deleted, err := s.reconcileDeletions(ctx, c.res, started, seen)
	if err != nil {
		s.recordError(ctx, c.res, err)
		return err
	}
	s.logf("%s: full sync: %d records, %d no longer in WHOOP (marked deleted)", c.res, seen, deleted)
	return s.DB.RecordSyncSuccess(ctx, c.res, started, true, seen)
}

// reconcileDeletions marks records the full pass didn't see. An empty response while rows
// are stored looks like an API problem rather than a mass deletion, so it fails instead.
func (s *Syncer) reconcileDeletions(ctx context.Context, res database.Resource, started time.Time, seen int) (int64, error) {
	if seen == 0 {
		live, err := s.DB.LiveCount(ctx, res)
		if err != nil {
			return 0, err
		}
		if live > 0 {
			return 0, fmt.Errorf("WHOOP returned no %s records but %d are stored; not marking them deleted", res, live)
		}
		return 0, nil
	}
	return s.DB.MarkUnseenDeleted(ctx, res, started)
}

func (s *Syncer) recordError(ctx context.Context, res database.Resource, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if dbErr := s.DB.RecordSyncError(ctx, res, time.Now(), err.Error()); dbErr != nil {
		s.logf("%s: recording sync error: %v", res, dbErr)
	}
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}
