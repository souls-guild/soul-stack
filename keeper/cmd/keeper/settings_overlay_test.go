package main

// End-to-end guards of the SettingsStore overlay inside the daemon (ADR-0073),
// without Postgres or Redis: a fake row source stands in for `keeper_settings`,
// and everything downstream is the production wiring — the same config.Store,
// the same OnReload subscription setupToll registers, the same per-request
// Tempo closure the router is given.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/settingsstore"
	"github.com/souls-guild/soul-stack/shared/config"
)

// --- fake keeper_settings ---------------------------------------------

type overlayRows struct {
	rows [][2]string
	i    int
}

func (r *overlayRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *overlayRows) Scan(dest ...any) error {
	k, ok1 := dest[0].(*string)
	v, ok2 := dest[1].(*string)
	if !ok1 || !ok2 {
		return errors.New("overlayRows: unexpected dest")
	}
	*k, *v = r.rows[r.i-1][0], r.rows[r.i-1][1]
	return nil
}

func (r *overlayRows) Err() error                                   { return nil }
func (r *overlayRows) Close()                                       {}
func (r *overlayRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *overlayRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *overlayRows) Values() ([]any, error)                       { return nil, nil }
func (r *overlayRows) RawValues() [][]byte                          { return nil }
func (r *overlayRows) Conn() *pgx.Conn                              { return nil }

type overlayDB struct {
	rows [][2]string
	err  error
}

func (d *overlayDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &overlayRows{rows: d.rows}, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- tests -------------------------------------------------------------

// The pilot's whole point: a value living in Postgres reaches the Toll
// consumer through the same OnReload callback a SIGHUP would use — no restart,
// no file edit on this host.
func TestSettingsOverlay_TollHotApplyWithoutRestart(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	logger := discardLogger()
	d := &daemon{store: store, logger: logger, cfg: store.Get()}

	var (
		mu      sync.Mutex
		applied float64
	)
	// The setupToll subscription, verbatim, plus an observer of what it saw.
	store.OnReload(func(_, newCfg *config.KeeperConfig) {
		d.applyTollReload(newCfg, logger)
		mu.Lock()
		defer mu.Unlock()
		if newCfg != nil && newCfg.Toll != nil {
			applied = newCfg.Toll.Threshold
		}
	})

	if err := d.initSettingsStore(context.Background(), &overlayDB{
		rows: [][2]string{{"cfg_toll_threshold", "0.42"}},
	}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	if got := store.Get().Toll.Threshold; got != 0.42 {
		t.Errorf("effective threshold = %v, want 0.42 from the overlay", got)
	}
	// A restarted instance must come up on the effective config: setup steps
	// below read d.cfg, and Toll takes its initial threshold from there.
	if d.cfg == nil || d.cfg.Toll == nil || d.cfg.Toll.Threshold != 0.42 {
		t.Errorf("startup config not refreshed from the overlay: %+v", d.cfg)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := applied
		mu.Unlock()
		if got == 0.42 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("toll subscriber never saw the overlay threshold")
}

// Tempo reads rate/burst from a fresh snapshot on every request, so the
// overlay reaches it with no subscription at all.
func TestSettingsOverlay_TempoLimitsReadLiveValues(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}

	// The closure handed to the router (daemon.go, TempoVoyageCreateLimits).
	limits := func() (float64, int) {
		return d.store.Get().Tempo.ResolvedVoyageCreate()
	}
	if rate, burst := limits(); rate != config.DefaultTempoVoyageCreateRate || burst != config.DefaultTempoVoyageCreateBurst {
		t.Fatalf("pre-overlay limits = (%v, %d), want the defaults", rate, burst)
	}

	if err := d.initSettingsStore(context.Background(), &overlayDB{rows: [][2]string{
		{"cfg_tempo_voyage_create_burst", "2"},
		{"cfg_tempo_voyage_create_rate", "1"},
	}}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	rate, burst := limits()
	if rate != 1 || burst != 2 {
		t.Errorf("post-overlay limits = (%v, %d), want (1, 2)", rate, burst)
	}
}

// ADR-0073(h): Postgres unreachable at startup is a WARN, not a fatal — the
// instance comes up on the file base. A tunables store must not become a new
// hard dependency for the cluster to start.
func TestSettingsOverlay_StartupWithoutPostgresIsNotFatal(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}

	if err := d.initSettingsStore(context.Background(), &overlayDB{err: errors.New("connection refused")}); err != nil {
		t.Fatalf("startup failed on an unreachable Postgres: %v", err)
	}
	if d.settings == nil {
		t.Fatal("settings store not wired")
	}
	if store.Get() == nil {
		t.Fatal("config snapshot lost")
	}
	if len(d.settings.Overlay()) != 0 {
		t.Errorf("overlay is not empty after a failed load: %+v", d.settings.Overlay())
	}
}

// Break-glass: KEEPER_CONFIG_SOURCE=file leaves the overlay unwired entirely,
// so a valid-but-harmful cluster value cannot reach this instance — and the
// /v1/settings routes stay unmounted with it (settingsOverlayOrNil).
func TestSettingsOverlay_BreakGlassRunsOffFileOnly(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}

	t.Setenv(settingsstore.EnvConfigSource, settingsstore.ConfigSourceFileOnly)
	if err := d.initSettingsStore(context.Background(), &overlayDB{
		rows: [][2]string{{"cfg_toll_threshold", "0.42"}},
	}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}
	if d.settings != nil {
		t.Fatal("overlay wired despite the break-glass switch")
	}
	if settingsOverlayOrNil(d.settings) != nil {
		t.Error("a typed nil leaked into the API deps")
	}
	if cfg := store.Get(); cfg.Toll != nil && cfg.Toll.Threshold == 0.42 {
		t.Error("the overlay value was applied under KEEPER_CONFIG_SOURCE=file")
	}
}
