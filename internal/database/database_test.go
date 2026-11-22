package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"whooper/internal/testpg"
	"whooper/internal/whoop"
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

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dsn := pg.NewDatabase(t)
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dsn
}

func TestOpenAppliesSchemaIdempotentlyAndConcurrently(t *testing.T) {
	_, dsn := openTestDB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			db, err := Open(context.Background(), dsn)
			if err == nil {
				db.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertAllApplied(t, db)
}

func assertAllApplied(t *testing.T, db *DB) {
	t.Helper()
	migrations, err := db.MigrationStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations found")
	}
	for _, m := range migrations {
		if m.State != goose.StateApplied {
			t.Errorf("%s is %s", m.Source.Path, m.State)
		}
	}
}

// A database created before goose was introduced (schema applied directly on every start)
// must be adopted as-is, without losing data.
func TestMigrationsAdoptPreGooseDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := pg.NewDatabase(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	initial, err := migrationFiles.ReadFile("migrations/00001_initial_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	upSQL, _, found := strings.Cut(string(initial), "-- +goose Down")
	if !found {
		t.Fatal("initial migration has no Down section")
	}
	if _, err := raw.ExecContext(ctx, upSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO whoop_token (access_token, refresh_token, expires_at) VALUES ('a', 'r', now())`); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("adopting a pre-goose database: %v", err)
	}
	defer db.Close()
	assertAllApplied(t, db)
	if info, _ := db.TokenInfo(ctx); !info.Connected {
		t.Fatal("existing data was lost")
	}
}

func TestMigrateDownAndUpAgain(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for {
		v, err := db.MigrationVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if v == 0 {
			break
		}
		if _, err := db.MigrateDown(ctx); err != nil {
			t.Fatalf("rolling back version %d: %v", v, err)
		}
	}
	var exists bool
	if err := db.sql.QueryRow(`SELECT to_regclass('whoop_cycle') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("whoop_cycle still exists after rolling back every migration")
	}
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	assertAllApplied(t, db)
}

func TestHealth(t *testing.T) {
	db, _ := openTestDB(t)
	if got := db.Health(context.Background())["status"]; got != "up" {
		t.Fatalf("status = %q", got)
	}
	db.Close()
	if got := db.Health(context.Background())["status"]; got != "down" {
		t.Fatalf("status after close = %q, want down (and no crash)", got)
	}
}

func TestUpsertRecordsKeepsRawJSONAndUpdates(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	t1 := time.Now().Truncate(time.Microsecond)
	v1 := json.RawMessage(`{"id":7,"start":"2025-01-01T03:00:00.000Z","end":null,"updated_at":"2025-01-01T03:00:00.000Z","score_state":"PENDING_SCORE","score":null}`)
	v2 := json.RawMessage(`{"id":7,"start":"2025-01-01T03:00:00.000Z","end":"2025-01-02T02:00:00.000Z","updated_at":"2025-01-02T05:00:00.000Z","score_state":"SCORED","score":{"strain":12.3}}`)
	if err := db.UpsertRecords(ctx, Cycles, []json.RawMessage{v1}, t1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRecords(ctx, Cycles, []json.RawMessage{v2}, t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	var strain float64
	var state string
	var first, last time.Time
	err := db.sql.QueryRow(`SELECT count(*) OVER (), (raw->'score'->>'strain')::float8, score_state, first_seen_at, last_seen_at FROM whoop_cycle`).
		Scan(&n, &strain, &state, &first, &last)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || strain != 12.3 || state != "SCORED" || !first.Equal(t1) || !last.Equal(t1.Add(time.Hour)) {
		t.Fatalf("n=%d strain=%v state=%s first=%v last=%v", n, strain, state, first, last)
	}
}

func TestUpsertRecordsRejectsMalformedRecord(t *testing.T) {
	db, _ := openTestDB(t)
	good := json.RawMessage(`{"id":"00000000-0000-4000-8000-000000000001","start":"2025-01-01T03:00:00.000Z"}`)
	bad := json.RawMessage(`{"start":"2025-01-01T03:00:00.000Z"}`)
	if err := db.UpsertRecords(context.Background(), Workouts, []json.RawMessage{good, bad}, time.Now()); err == nil {
		t.Fatal("expected an error for a record without an id")
	}
	if n, _ := db.LiveCount(context.Background(), Workouts); n != 0 {
		t.Fatalf("page was partially stored: %d rows", n)
	}
}

func TestMarkUnseenDeletedAndResurrect(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	rec := func(id string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","start":"2025-01-01T03:00:00.000Z","sport_name":"running"}`)
	}
	a, b := "00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b"
	run1 := time.Now().Truncate(time.Microsecond)
	if err := db.UpsertRecords(ctx, Workouts, []json.RawMessage{rec(a), rec(b)}, run1); err != nil {
		t.Fatal(err)
	}
	run2 := run1.Add(time.Hour)
	if err := db.UpsertRecords(ctx, Workouts, []json.RawMessage{rec(a)}, run2); err != nil {
		t.Fatal(err)
	}
	n, err := db.MarkUnseenDeleted(ctx, Workouts, run2)
	if err != nil || n != 1 {
		t.Fatalf("marked %d (err %v), want 1", n, err)
	}
	stats, _ := db.ResourceStats(ctx, Workouts)
	if stats.Live != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if err := db.UpsertRecords(ctx, Workouts, []json.RawMessage{rec(b)}, run2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if stats, _ := db.ResourceStats(ctx, Workouts); stats.Live != 2 || stats.Deleted != 0 {
		t.Fatalf("record seen again was not restored: %+v", stats)
	}
}

func TestTokenRoundTripAndInfo(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if info, _ := db.TokenInfo(ctx); info.Connected {
		t.Fatal("fresh database reports a connected token")
	}
	exp := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := db.SaveToken(ctx, &whoop.Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp, Scope: "offline"}); err != nil {
		t.Fatal(err)
	}
	var got *whoop.Token
	err := db.UpdateToken(ctx, func(cur *whoop.Token) (*whoop.Token, error) { got = cur; return nil, nil })
	if err != nil || got == nil || got.RefreshToken != "r" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v err %v", got, err)
	}
	if info, _ := db.TokenInfo(ctx); !info.Connected || info.Scope != "offline" {
		t.Fatalf("info = %+v", info)
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateOAuthState(ctx, "abcd1234"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.ConsumeOAuthState(ctx, "abcd1234"); !ok {
		t.Fatal("state not accepted")
	}
	if ok, _ := db.ConsumeOAuthState(ctx, "abcd1234"); ok {
		t.Fatal("state accepted twice")
	}
	if ok, _ := db.ConsumeOAuthState(ctx, "never-issued"); ok {
		t.Fatal("unknown state accepted")
	}
}

func TestBodyMeasurementOnlyStoresChanges(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	m1 := json.RawMessage(`{"height_meter":1.8,"weight_kilogram":80,"max_heart_rate":190}`)
	m1reordered := json.RawMessage(`{"max_heart_rate":190,"weight_kilogram":80,"height_meter":1.8}`)
	m2 := json.RawMessage(`{"height_meter":1.8,"weight_kilogram":79,"max_heart_rate":190}`)
	for i, c := range []struct {
		raw  json.RawMessage
		want bool
	}{{m1, true}, {m1reordered, false}, {m2, true}, {m2, false}} {
		got, err := db.RecordBodyMeasurement(ctx, c.raw, time.Now())
		if err != nil || got != c.want {
			t.Fatalf("step %d: stored=%v err=%v, want %v", i, got, err, c.want)
		}
	}
}

func TestTryLockExcludesSecondHolder(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	unlock, ok, err := db.TryLock(ctx, SyncLock)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := db.TryLock(ctx, SyncLock); ok {
		t.Fatal("second holder got the lock")
	}
	unlock()
	unlock2, ok, err := db.TryLock(ctx, SyncLock)
	if err != nil || !ok {
		t.Fatalf("lock after unlock: ok=%v err=%v", ok, err)
	}
	unlock2()
}
