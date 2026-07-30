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
	"sync/atomic"
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
	mu   sync.Mutex
	rows [][2]string
	err  error
}

func (d *overlayDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	rows := make([][2]string, len(d.rows))
	copy(rows, d.rows)
	return &overlayRows{rows: rows}, nil
}

// set upserts one `cfg_*` row — what a PUT on the other node commits.
func (d *overlayDB) set(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.rows {
		if d.rows[i][0] == key {
			d.rows[i][1] = value
			return
		}
	}
	d.rows = append(d.rows, [2]string{key, value})
}

// fakeInvalidations stands in for the shared `service:invalidate` channel: it
// keeps the subscriber callback so the test can deliver a cluster event.
type fakeInvalidations struct {
	mu sync.Mutex
	cb func()
}

func (f *fakeInvalidations) Watch(ctx context.Context, onInvalidate func()) error {
	f.mu.Lock()
	f.cb = onInvalidate
	f.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// publish delivers one invalidation, as the mutating node's publish would.
func (f *fakeInvalidations) publish(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		cb := f.cb
		f.mu.Unlock()
		if cb != nil {
			cb()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nobody subscribed to the invalidation channel")
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

// ★ The cluster property, on two instances: node A commits a row, node B is
// told over the shared invalidation channel and re-merges — no SIGHUP, no
// restart, no file edit on B. Node B here is a SEPARATE config.Store over its
// own copy of keeper.yml, exactly as a second VM would be.
func TestSettingsOverlay_ReachesTheOtherNodeWithoutRestart(t *testing.T) {
	db := &overlayDB{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Node A — the one taking the write.
	storeA, _ := keeperFixtureStore(t)
	nodeA := &daemon{store: storeA, logger: discardLogger(), cfg: storeA.Get()}
	if err := nodeA.initSettingsStore(ctx, db); err != nil {
		t.Fatalf("node A initSettingsStore: %v", err)
	}

	// Node B — its own file, its own store, subscribed to the shared channel.
	storeB, _ := keeperFixtureStore(t)
	nodeB := &daemon{store: storeB, logger: discardLogger(), cfg: storeB.Get()}
	if err := nodeB.initSettingsStore(ctx, db); err != nil {
		t.Fatalf("node B initSettingsStore: %v", err)
	}
	bus := &fakeInvalidations{}
	go nodeB.settings.WatchInvalidations(ctx, bus)

	// Keys the golden keeper.yml does NOT set — the file wins where it speaks
	// (ADR-0073(b), amended), so a cluster value can only be observed on a key
	// the local file leaves alone.
	db.set("cfg_cadence_scheduler_poll_idle", "5m")
	db.set("cfg_toll_threshold", "0.42")
	if err := nodeA.settings.Refresh(ctx); err != nil {
		t.Fatalf("node A refresh: %v", err)
	}
	if got := storeA.Get().CadenceScheduler.ResolvedPollIdle(); got != 5*time.Minute {
		t.Fatalf("node A poll_idle = %v, want 5m", got)
	}

	bus.publish(t)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cfg := storeB.Get()
		if cfg.CadenceScheduler.ResolvedPollIdle() == 5*time.Minute &&
			cfg.Toll != nil && cfg.Toll.Threshold == 0.42 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg := storeB.Get()
	t.Fatalf("node B never picked the overlay up: poll_idle=%v toll=%+v",
		cfg.CadenceScheduler.ResolvedPollIdle(), cfg.Toll)
}

// ★ The other half of the same rule, end to end in the daemon: this instance's
// keeper.yml sets `reaper.interval: 1h`, so a cluster override of the same key
// is stored, reported — and NOT applied here (ADR-0073(b), amended).
func TestSettingsOverlay_LocalFileWinsOverCluster(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}

	if err := d.initSettingsStore(context.Background(), &overlayDB{rows: [][2]string{
		{"cfg_reaper_interval", "2h"},
		{"cfg_toll_threshold", "0.42"},
	}}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	if got := store.Get().Reaper.ResolvedInterval(); got != time.Hour {
		t.Errorf("reaper.interval = %v, want the local 1h from keeper.yml", got)
	}
	// The override is still in the snapshot — the catalog has to be able to say
	// "the cluster asks for 2h, this host runs 1h".
	if got := d.settings.Values()["cfg_reaper_interval"]; got != "2h" {
		t.Errorf("cluster value = %v, want 2h to remain visible", got)
	}
	// A key the file leaves alone takes the cluster value on the same instance.
	if cfg := store.Get(); cfg.Toll == nil || cfg.Toll.Threshold != 0.42 {
		t.Errorf("toll.threshold not applied from the cluster: %+v", cfg.Toll)
	}
}

// A wake-up that changes nothing must not reconfigure anything: the shared
// channel also carries service-registry events and the TTL poll fires every 10s,
// so without the idempotence guard every consumer would be reconfigured (and an
// audit event written) on a heartbeat (ADR-0073(g)).
func TestSettingsOverlay_UnrelatedInvalidationIsANoOp(t *testing.T) {
	db := &overlayDB{rows: [][2]string{{"cfg_reaper_interval", "2h"}}}
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}
	if err := d.initSettingsStore(context.Background(), db); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	var reloads atomic.Int64
	store.OnReload(func(_, _ *config.KeeperConfig) { reloads.Add(1) })

	for i := 0; i < 3; i++ {
		if err := d.settings.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if n := reloads.Load(); n != 0 {
		t.Errorf("%d config swaps on unchanged overlay, want 0", n)
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

// ★ The console plane switched off from Postgres, with nothing edited on the
// box (NIM-292). This is the whole point of admitting `cfg_console_enabled`:
// "this cluster carries no consoles" becomes one row rather than an edit to
// keeper.yml on every Keeper VM.
//
// The providers are the ones daemon.go hands to the router and the MCP handler,
// so what this exercises is the production read path, not a re-implementation
// of it.
func TestSettingsOverlay_ConsolePlaneSwitchedOffFromPostgres(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}
	planeEnabled := d.consolePlaneEnabledProvider()

	if !planeEnabled() {
		t.Fatal("the plane is off before any overlay row — absent must mean on")
	}

	if err := d.initSettingsStore(context.Background(), &overlayDB{
		rows: [][2]string{{"cfg_console_enabled", "false"}},
	}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	if planeEnabled() {
		t.Fatal("cfg_console_enabled=false did not reach the plane provider — the switch does not work from Postgres")
	}
}

// The envelope travels the same way, and through the same providers the Hub and
// the recorder hold. Without the live apply path these keys could not have been
// admitted at all (ADR-0073(j.5)).
func TestSettingsOverlay_ConsoleEnvelopeReadsLiveValues(t *testing.T) {
	store, _ := keeperFixtureStore(t)
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}
	limits := d.consoleLimitsProvider()
	recording := d.consoleRecorderConfigProvider()

	if got := limits().MaxSessionsPerAID; got != 0 {
		t.Fatalf("pre-overlay per-Archon ceiling = %d, want 0 so the package default applies", got)
	}

	if err := d.initSettingsStore(context.Background(), &overlayDB{rows: [][2]string{
		{"cfg_console_max_sessions_per_archon", "3"},
		{"cfg_console_max_sessions_global", "7"},
		{"cfg_console_idle_timeout", "5m"},
		{"cfg_console_recording_max_session_bytes", "2097152"},
	}}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	got := limits()
	if got.MaxSessionsPerAID != 3 || got.MaxSessionsGlobal != 7 || got.IdleTimeout != 5*time.Minute {
		t.Errorf("post-overlay limits = %+v, want per-Archon 3, global 7, idle 5m", got)
	}
	if b := recording().MaxBytes; b != 2097152 {
		t.Errorf("post-overlay recording cap = %d, want 2097152", b)
	}
}

// The escape hatch that makes the admission survivable: a value pinned in the
// instance's own keeper.yml outranks the cluster row (ADR-0073(b)). A cluster
// that must never carry consoles writes `enabled: false` in the FILE, and no
// `setting.update` can switch it back on.
func TestSettingsOverlay_ConsoleFilePinOutranksPostgres(t *testing.T) {
	store, _ := keeperFixtureStoreWith(t, "\nconsole:\n  enabled: false\n")
	d := &daemon{store: store, logger: discardLogger(), cfg: store.Get()}
	planeEnabled := d.consolePlaneEnabledProvider()

	if planeEnabled() {
		t.Fatal("the file pin did not switch the plane off")
	}

	if err := d.initSettingsStore(context.Background(), &overlayDB{
		rows: [][2]string{{"cfg_console_enabled", "true"}},
	}); err != nil {
		t.Fatalf("initSettingsStore: %v", err)
	}

	if planeEnabled() {
		t.Fatal("a Postgres row switched the console plane back on over a keeper.yml that forbids it — the file must win (ADR-0073(b))")
	}
}
