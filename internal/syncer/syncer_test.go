package syncer

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"whooper/internal/database"
	"whooper/internal/testpg"
	"whooper/internal/whoop"
	"whooper/internal/whoop/whooptest"
)

var pg *testpg.Server

func TestMain(m *testing.M) {
	var err error
	pg, err = testpg.Start(context.Background())
	if err != nil {
		log.Fatalf("could not start postgres container: %v", err)
	}
	code := m.Run()
	pg.Close()
	os.Exit(code)
}

type env struct {
	db   *database.DB
	fake *whooptest.Server
	sync *Syncer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, pg.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fake := whooptest.New()
	t.Cleanup(fake.Close)
	if err := db.SaveToken(ctx, fake.Issue()); err != nil {
		t.Fatal(err)
	}
	api := fake.Client(&whoop.TokenSource{OAuth: fake.OAuth(), Store: db})
	return &env{db: db, fake: fake, sync: &Syncer{DB: db, API: api, Lookback: 14 * 24 * time.Hour, Logf: t.Logf}}
}

func (e *env) run(t *testing.T, full bool) {
	t.Helper()
	if err := e.sync.Run(context.Background(), full); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func (e *env) report(t *testing.T) *Report {
	t.Helper()
	r, err := BuildReport(context.Background(), e.db, DefaultStatusOptions())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) live(t *testing.T, res database.Resource) int64 {
	t.Helper()
	n, err := e.db.LiveCount(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFirstRunDownloadsWholeHistory(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(400, 120) // > 1 year, many pages per collection
	e.run(t, false)

	for res, want := range map[database.Resource]int64{
		database.Cycles: 400, database.Sleeps: 400, database.Recoveries: 400, database.Workouts: 120,
	} {
		if got := e.live(t, res); got != want {
			t.Errorf("%s: stored %d, want %d", res, got, want)
		}
	}
	if q := e.fake.Queries(whooptest.CyclePath)[0]; q.Has("start") {
		t.Errorf("first sync should request the whole history, got start=%s", q.Get("start"))
	}

	r := e.report(t)
	if !r.Complete {
		t.Fatalf("report incomplete after a full download: %v", r.Problems)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", r.Warnings)
	}
	var buf bytes.Buffer
	r.WriteText(&buf)
	if !strings.Contains(buf.String(), "COMPLETE") {
		t.Errorf("text report:\n%s", buf.String())
	}
}

func TestLaterRunsAreIncrementalFromLastSuccess(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(60, 10)
	e.run(t, false)
	st, err := e.db.SyncState(context.Background(), database.Cycles)
	if err != nil {
		t.Fatal(err)
	}

	e.fake.AddDay()
	before := len(e.fake.Queries(whooptest.CyclePath))
	e.run(t, false)

	if got := e.live(t, database.Cycles); got != 61 {
		t.Fatalf("cycles = %d, want 61", got)
	}
	qs := e.fake.Queries(whooptest.CyclePath)[before:]
	want := whoop.FormatTime(st.LastSuccessAt.Add(-14 * 24 * time.Hour))
	if len(qs) != 1 || qs[0].Get("start") != want {
		t.Fatalf("incremental queries = %v, want one with start=%s", qs, want)
	}
	if !e.report(t).Complete {
		t.Fatal("report incomplete after incremental sync")
	}
}

func TestFullSyncMarksRecordsDeletedInWhoop(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(30, 10)
	e.run(t, false)

	gone := e.fake.Keys(whooptest.WorkoutPath)[5]
	e.fake.Remove(whooptest.WorkoutPath, gone)
	e.run(t, false) // incremental: can't tell deletions apart from records outside the window
	if got := e.live(t, database.Workouts); got != 10 {
		t.Fatalf("incremental sync changed live workouts to %d", got)
	}
	e.run(t, true)
	stats, err := e.db.ResourceStats(context.Background(), database.Workouts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Live != 9 || stats.Deleted != 1 {
		t.Fatalf("after full sync: %+v, want 9 live / 1 deleted", stats)
	}
}

func TestReportFlagsMissingReferencedRecords(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(30, 0)
	e.fake.Remove(whooptest.SleepPath, e.fake.Keys(whooptest.SleepPath)[3])
	e.run(t, false)

	r := e.report(t)
	if r.Complete || r.Integrity.RecoveriesMissingSleep != 1 {
		t.Fatalf("complete=%v integrity=%+v problems=%v", r.Complete, r.Integrity, r.Problems)
	}
}

func TestReportWarnsAboutCycleGapsWithoutFailing(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(30, 0)
	e.fake.RemoveDay(10) // a day with no cycle/sleep/recovery at all, e.g. strap off
	e.run(t, false)

	r := e.report(t)
	if !r.Complete {
		t.Fatalf("a strap-off gap should not make the history incomplete: %v", r.Problems)
	}
	if r.CycleGapCount != 1 || len(r.Warnings) != 1 {
		t.Fatalf("gaps=%d warnings=%v", r.CycleGapCount, r.Warnings)
	}
}

func TestExpiredTokenIsRefreshedAndRotationPersisted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stale := e.fake.Issue()
	stale.ExpiresAt = time.Now().Add(-time.Hour)
	if err := e.db.SaveToken(ctx, stale); err != nil {
		t.Fatal(err)
	}
	e.fake.Seed(5, 0)
	e.run(t, false)

	if e.fake.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", e.fake.Refreshes())
	}
	var stored *whoop.Token
	e.db.UpdateToken(ctx, func(cur *whoop.Token) (*whoop.Token, error) { stored = cur; return nil, nil })
	if stored.RefreshToken == stale.RefreshToken {
		t.Fatal("rotated refresh token was not saved")
	}
}

func TestNotConnected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db, err := database.Open(ctx, pg.NewDatabase(t)) // no token stored
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Syncer{DB: db, API: e.fake.Client(&whoop.TokenSource{OAuth: e.fake.OAuth(), Store: db}), Logf: t.Logf}
	if err := s.Run(ctx, false); !errors.Is(err, whoop.ErrNotAuthorized) {
		t.Fatalf("err = %v, want ErrNotAuthorized", err)
	}
	r, err := BuildReport(ctx, db, DefaultStatusOptions())
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete || !strings.Contains(strings.Join(r.Problems, "\n"), "no WHOOP account connected") {
		t.Fatalf("problems = %v", r.Problems)
	}
}

func TestOverlappingRunIsSkipped(t *testing.T) {
	e := newEnv(t)
	unlock, ok, err := e.db.TryLock(context.Background(), database.SyncLock)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer unlock()
	if err := e.sync.Run(context.Background(), false); !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
}

func TestStaleSyncsMakeReportIncomplete(t *testing.T) {
	e := newEnv(t)
	e.fake.Seed(5, 0)
	e.run(t, false)
	r, err := BuildReport(context.Background(), e.db, StatusOptions{MaxSyncAge: time.Nanosecond, MaxFullSyncAge: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete || len(r.Problems) != 2*len(database.Resources) {
		t.Fatalf("problems = %v", r.Problems)
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "10m", time.Hour: "1h", 90 * time.Minute: "1h30m", 72 * time.Hour: "3d",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%s) = %q, want %q", d, got, want)
		}
	}
}
