//go:build integration

// The window and freeze store against a real Postgres with every migration
// applied (NIM-1072, migration 133): column lists and scans agree, the
// incarnation in the address is part of the key, the before/after pair of an
// update is the real transition, rows cascade with their incarnation, and the
// per-incarnation cap and the foreign key surface as their own errors.

package maintenance

import (
	"context"
	"errors"
	"log"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	if err != nil {
		if integrationenv.RequireDocker() {
			log.Fatalf("maintenance integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("maintenance integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool
	return m.Run()
}

func seedIncarnations(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `TRUNCATE TABLE incarnation CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	for _, id := range ids {
		inc := &incarnation.Incarnation{
			ID: id, Service: "redis", ServiceVersion: "v1.0.0", StateSchemaVersion: 1,
			State: map[string]any{}, Status: incarnation.StatusReady,
		}
		if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
			t.Fatalf("seed incarnation %s: %v", id, err)
		}
	}
}

func TestIntegration_WindowRoundTrip(t *testing.T) {
	seedIncarnations(t, "redis-prod", "redis-stage")
	ctx := context.Background()

	w, err := InsertWindow(ctx, integrationPool, "redis-prod", WindowSpec{
		Days: []string{"thu", "mon"}, StartTime: "02:00", DurationMinutes: 180, Timezone: "Europe/Berlin", Comment: "nightly",
	})
	if err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	if !ValidID(w.ID) || w.IncarnationID != "redis-prod" || !reflect.DeepEqual(w.Days, []string{"mon", "thu"}) ||
		w.StartTime != "02:00" || w.DurationMinutes != 180 || w.Timezone != "Europe/Berlin" || w.Comment != "nightly" {
		t.Fatalf("inserted window = %+v", w)
	}

	list, err := ListWindows(ctx, integrationPool, "redis-prod")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], w) {
		t.Fatalf("ListWindows = %+v, %v; want exactly the inserted row", list, err)
	}
	if other, _ := ListWindows(ctx, integrationPool, "redis-stage"); len(other) != 0 {
		t.Errorf("another incarnation lists %d windows, want 0", len(other))
	}

	// The incarnation is part of the address: the right id under the wrong
	// incarnation is not found, and nothing changes.
	if _, _, err := UpdateWindow(ctx, integrationPool, "redis-stage", w.ID, WindowSpec{
		Days: []string{"sun"}, StartTime: "03:00", DurationMinutes: 60, Timezone: "UTC",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateWindow under another incarnation = %v, want ErrNotFound", err)
	}
	if _, err := DeleteWindow(ctx, integrationPool, "redis-stage", w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteWindow under another incarnation = %v, want ErrNotFound", err)
	}

	before, after, err := UpdateWindow(ctx, integrationPool, "redis-prod", w.ID, WindowSpec{
		Days: []string{"sun"}, StartTime: "03:00", DurationMinutes: 60, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("UpdateWindow: %v", err)
	}
	if !reflect.DeepEqual(before, w) {
		t.Errorf("before = %+v, want the row as it was: %+v", before, w)
	}
	if after.ID != w.ID || !reflect.DeepEqual(after.Days, []string{"sun"}) || after.StartTime != "03:00" ||
		after.Timezone != "UTC" || after.Comment != "" || !after.CreatedAt.Equal(w.CreatedAt) {
		t.Errorf("after = %+v", after)
	}

	gone, err := DeleteWindow(ctx, integrationPool, "redis-prod", w.ID)
	if err != nil || !reflect.DeepEqual(gone, after) {
		t.Errorf("DeleteWindow = %+v, %v; want the row as it was", gone, err)
	}
	if _, err := DeleteWindow(ctx, integrationPool, "redis-prod", w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteWindow = %v, want ErrNotFound", err)
	}
}

func TestIntegration_FreezeRoundTrip(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	msk := time.FixedZone("MSK", 3*3600)
	start := time.Date(2026, 12, 10, 0, 0, 0, 0, msk)

	f, err := InsertFreeze(ctx, integrationPool, "redis-prod", FreezeSpec{StartsAt: start, EndsAt: start.Add(48 * time.Hour), Comment: "year end"})
	if err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	if !f.StartsAt.Equal(start) || f.StartsAt.Location() != time.UTC || !f.EndsAt.Equal(start.Add(48*time.Hour)) {
		t.Errorf("inserted freeze = %+v, want the same instants read back in UTC", f)
	}

	before, after, err := UpdateFreeze(ctx, integrationPool, "redis-prod", f.ID, FreezeSpec{StartsAt: start, EndsAt: start.Add(time.Hour)})
	if err != nil || !reflect.DeepEqual(before, f) || !after.EndsAt.Equal(start.Add(time.Hour)) || after.Comment != "" {
		t.Errorf("UpdateFreeze = %+v → %+v, %v", before, after, err)
	}

	list, err := ListFreezes(ctx, integrationPool, "redis-prod")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], after) {
		t.Errorf("ListFreezes = %+v, %v", list, err)
	}
	if _, err := DeleteFreeze(ctx, integrationPool, "redis-prod", f.ID); err != nil {
		t.Errorf("DeleteFreeze: %v", err)
	}
	if _, err := DeleteFreeze(ctx, integrationPool, "redis-prod", f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteFreeze = %v, want ErrNotFound", err)
	}
}

func TestIntegration_RowsCascadeWithTheIncarnation(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	if _, err := InsertWindow(ctx, integrationPool, "redis-prod", WindowSpec{
		Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	start := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	if _, err := InsertFreeze(ctx, integrationPool, "redis-prod", FreezeSpec{StartsAt: start, EndsAt: start.Add(time.Hour)}); err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM incarnation WHERE id = 'redis-prod'`); err != nil {
		t.Fatalf("delete incarnation: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM incarnation_maintenance_windows) + (SELECT count(*) FROM incarnation_freezes)`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d window/freeze rows outlived their incarnation, want 0", n)
	}
}

func TestIntegration_CapAndForeignKey(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	spec := WindowSpec{Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC"}
	for i := 0; i < MaxWindowsPerIncarnation; i++ {
		if _, err := InsertWindow(ctx, integrationPool, "redis-prod", spec); err != nil {
			t.Fatalf("InsertWindow #%d: %v", i+1, err)
		}
	}
	if _, err := InsertWindow(ctx, integrationPool, "redis-prod", spec); !errors.Is(err, ErrLimitReached) {
		t.Errorf("InsertWindow past the cap = %v, want ErrLimitReached", err)
	}
	if _, err := InsertWindow(ctx, integrationPool, "redis-missing", spec); !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("InsertWindow for a missing incarnation = %v, want ErrIncarnationNotFound", err)
	}
	start := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	if _, err := InsertFreeze(ctx, integrationPool, "redis-missing", FreezeSpec{StartsAt: start, EndsAt: start.Add(time.Hour)}); !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("InsertFreeze for a missing incarnation = %v, want ErrIncarnationNotFound", err)
	}
}

// TestIntegration_SchemaBacksTheValidation writes around the Go validation:
// the CHECKs refuse what Normalize refuses, so a row that reaches the table by
// another path cannot break the calculator.
func TestIntegration_SchemaBacksTheValidation(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	bad := map[string]string{
		"unknown day":   `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{monday}', '02:00', 60, 'UTC')`,
		"no days":       `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{}', '02:00', 60, 'UTC')`,
		"hour 24":       `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{mon}', '24:00', 60, 'UTC')`,
		"over a day":    `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{mon}', '02:00', 1441, 'UTC')`,
		"empty freeze":  `INSERT INTO incarnation_freezes (incarnation_id, starts_at, ends_at) VALUES ('redis-prod', '2026-12-10T00:00:00Z', '2026-12-10T00:00:00Z')`,
		"long comment":  `INSERT INTO incarnation_freezes (incarnation_id, starts_at, ends_at, comment) VALUES ('redis-prod', '2026-12-10T00:00:00Z', '2026-12-11T00:00:00Z', repeat('x', 501))`,
		"empty zone":    `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{mon}', '02:00', 60, '')`,
		"zero duration": `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{mon}', '02:00', 0, 'UTC')`,
	}
	for name, sql := range bad {
		if _, err := integrationPool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: the table accepted a row the validation refuses", name)
		}
	}
}

func TestIntegration_LoadCompilesTheStoredRows(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	if _, err := InsertWindow(ctx, integrationPool, "redis-prod", WindowSpec{
		Days: []string{"sat"}, StartTime: "23:00", DurationMinutes: 240, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("InsertWindow: %v", err)
	}
	freezeStart := time.Date(2026, 11, 7, 22, 0, 0, 0, time.UTC)
	if _, err := InsertFreeze(ctx, integrationPool, "redis-prod", FreezeSpec{StartsAt: freezeStart, EndsAt: freezeStart.Add(3 * time.Hour)}); err != nil {
		t.Fatalf("InsertFreeze: %v", err)
	}
	c, err := Load(ctx, integrationPool, "redis-prod", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Text(freezeStart); got != "sat 23:00 for 4h (UTC)" {
		t.Errorf("Text = %q", got)
	}
	// Saturday 2026-11-07's slot is frozen until 01:00 Sunday, then open.
	if got := c.NextStart(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("NextStart = %s, want 2026-11-08T01:00:00Z", got)
	}
}

func TestIntegration_OnceWindowRoundTrip(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	start := time.Date(2026, 12, 10, 1, 0, 0, 0, time.FixedZone("MSK", 3*3600))

	w, err := InsertWindow(ctx, integrationPool, "redis-prod", WindowSpec{
		Kind: KindOnce, StartsAt: start, EndsAt: start.Add(3 * time.Hour), Comment: "hotfix",
	})
	if err != nil {
		t.Fatalf("InsertWindow once: %v", err)
	}
	if w.Kind != KindOnce || !w.StartsAt.Equal(start) || w.StartsAt.Location() != time.UTC ||
		!w.EndsAt.Equal(start.Add(3*time.Hour)) || w.Days != nil || w.StartTime != "" || w.Timezone != "" || w.DurationMinutes != 0 {
		t.Fatalf("inserted once window = %+v, want its bounds in UTC and no weekly field", w)
	}
	list, err := ListWindows(ctx, integrationPool, "redis-prod")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], w) {
		t.Fatalf("ListWindows = %+v, %v; want exactly the inserted row", list, err)
	}

	// A PUT replaces the kind too: once -> weekly nulls the bounds, and back.
	before, after, err := UpdateWindow(ctx, integrationPool, "redis-prod", w.ID, WindowSpec{
		Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC",
	})
	if err != nil || !reflect.DeepEqual(before, w) || after.Kind != KindWeekly || !after.StartsAt.IsZero() || !after.EndsAt.IsZero() {
		t.Fatalf("UpdateWindow once -> weekly = %+v -> %+v, %v", before, after, err)
	}
	_, back, err := UpdateWindow(ctx, integrationPool, "redis-prod", w.ID, WindowSpec{
		Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour),
	})
	if err != nil || back.Kind != KindOnce || back.Days != nil || back.Timezone != "" || !back.EndsAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("UpdateWindow weekly -> once = %+v, %v", back, err)
	}

	c, err := Load(ctx, integrationPool, "redis-prod", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OpenAt(start.Add(-time.Minute)) || !c.OpenAt(start) || !c.NextStart(start.Add(-time.Hour)).Equal(start) {
		t.Errorf("the stored once window does not gate the start: closed before starts_at, open from it")
	}
}

// TestIntegration_KindColumnsCheck writes around Normalize: each kind carries
// exactly its own columns.
func TestIntegration_KindColumnsCheck(t *testing.T) {
	seedIncarnations(t, "redis-prod")
	ctx := context.Background()
	bad := map[string]string{
		"once with days":     `INSERT INTO incarnation_maintenance_windows (incarnation_id, kind, days, starts_at, ends_at) VALUES ('redis-prod', 'once', '{mon}', '2026-12-10T00:00:00Z', '2026-12-10T01:00:00Z')`,
		"once without end":   `INSERT INTO incarnation_maintenance_windows (incarnation_id, kind, starts_at) VALUES ('redis-prod', 'once', '2026-12-10T00:00:00Z')`,
		"once empty":         `INSERT INTO incarnation_maintenance_windows (incarnation_id, kind, starts_at, ends_at) VALUES ('redis-prod', 'once', '2026-12-10T00:00:00Z', '2026-12-10T00:00:00Z')`,
		"weekly with bounds": `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone, starts_at, ends_at) VALUES ('redis-prod', '{mon}', '02:00', 60, 'UTC', '2026-12-10T00:00:00Z', '2026-12-10T01:00:00Z')`,
		"weekly without tz":  `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes) VALUES ('redis-prod', '{mon}', '02:00', 60)`,
		"unknown kind":       `INSERT INTO incarnation_maintenance_windows (incarnation_id, kind, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', 'daily', '{mon}', '02:00', 60, 'UTC')`,
	}
	for name, sql := range bad {
		if _, err := integrationPool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: the table accepted a row the validation refuses", name)
		}
	}
	if _, err := integrationPool.Exec(ctx, `INSERT INTO incarnation_maintenance_windows (incarnation_id, days, start_time, duration_minutes, timezone) VALUES ('redis-prod', '{mon}', '02:00', 60, 'UTC')`); err != nil {
		t.Errorf("a row without kind (every row before migration 136) is refused: %v", err)
	}
}

// TestIntegration_DeleteExpiredOnlyPastTheRetention is the guard of the
// cleanup: a row goes only once its end is more than the retention in the past
// by the database clock, a weekly window never, and the batch is bounded.
func TestIntegration_DeleteExpiredOnlyPastTheRetention(t *testing.T) {
	seedIncarnations(t, "redis-prod", "redis-stage")
	ctx := context.Background()
	var now time.Time
	if err := integrationPool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("now: %v", err)
	}
	const retention = 72 * time.Hour
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	onceEnding := func(inc, comment string, end time.Time) {
		t.Helper()
		if _, err := InsertWindow(ctx, integrationPool, inc, WindowSpec{
			Kind: KindOnce, StartsAt: end.Add(-time.Hour), EndsAt: end, Comment: comment,
		}); err != nil {
			t.Fatalf("InsertWindow %s: %v", comment, err)
		}
	}
	onceEnding("redis-prod", "long gone", ago(retention+time.Hour))
	onceEnding("redis-stage", "gone", ago(retention+time.Minute))
	onceEnding("redis-prod", "ended, kept", ago(retention-time.Minute))
	onceEnding("redis-prod", "just ended", ago(time.Minute))
	onceEnding("redis-prod", "future", now.Add(time.Hour))
	if _, err := InsertWindow(ctx, integrationPool, "redis-prod", WindowSpec{
		Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC", Comment: "weekly",
	}); err != nil {
		t.Fatalf("InsertWindow weekly: %v", err)
	}

	first, err := DeleteExpiredWindows(ctx, integrationPool, retention, 1)
	if err != nil || len(first) != 1 || first[0].Comment != "long gone" || first[0].Kind != KindOnce {
		t.Fatalf("first batch of 1 = %+v, %v; want the longest-ended window", first, err)
	}
	rest, err := DeleteExpiredWindows(ctx, integrationPool, retention, 100)
	if err != nil || len(rest) != 1 || rest[0].Comment != "gone" || rest[0].IncarnationID != "redis-stage" {
		t.Fatalf("second batch = %+v, %v; want only the other window past the retention", rest, err)
	}
	if again, err := DeleteExpiredWindows(ctx, integrationPool, retention, 100); err != nil || len(again) != 0 {
		t.Fatalf("third batch = %+v, %v; want nothing left to delete", again, err)
	}
	left, _ := ListWindows(ctx, integrationPool, "redis-prod")
	var kept []string
	for _, w := range left {
		kept = append(kept, w.Comment)
	}
	if want := []string{"ended, kept", "just ended", "future", "weekly"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("windows left = %v, want %v", kept, want)
	}

	freezeEnding := func(comment string, end time.Time) {
		t.Helper()
		if _, err := InsertFreeze(ctx, integrationPool, "redis-prod", FreezeSpec{StartsAt: end.Add(-time.Hour), EndsAt: end, Comment: comment}); err != nil {
			t.Fatalf("InsertFreeze %s: %v", comment, err)
		}
	}
	freezeEnding("gone", ago(retention+time.Minute))
	freezeEnding("kept", ago(retention-time.Minute))
	freezeEnding("active", now.Add(time.Hour))
	gone, err := DeleteExpiredFreezes(ctx, integrationPool, retention, 100)
	if err != nil || len(gone) != 1 || gone[0].Comment != "gone" {
		t.Fatalf("DeleteExpiredFreezes = %+v, %v; want only the freeze past the retention", gone, err)
	}
	if freezes, _ := ListFreezes(ctx, integrationPool, "redis-prod"); len(freezes) != 2 {
		t.Errorf("freezes left = %+v, want the two inside the retention", freezes)
	}

	if _, err := DeleteExpiredWindows(ctx, integrationPool, 0, 100); err == nil {
		t.Error("DeleteExpiredWindows with retention 0 ran; 0 means never and the caller skips it")
	}
}
