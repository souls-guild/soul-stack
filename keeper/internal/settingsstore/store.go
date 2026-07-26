package settingsstore

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// DefaultRefreshInterval — TTL period of the fallback re-read (ADR-0073(g)).
// Redis pub/sub has no persistence, so a lost invalidation is picked up by the
// next poll; pub/sub only shortens the typical propagation to milliseconds.
// Same value as serviceregistry.DefaultRefreshInterval — the two share a
// channel and there is no reason for them to drift.
const DefaultRefreshInterval = 10 * time.Second

// EnvConfigSource is the break-glass switch of ADR-0073(h): with
// `KEEPER_CONFIG_SOURCE=file` the instance ignores the overlay entirely and runs
// off `keeper.yml`. An environment variable, because it must work before the
// config is parsed and without a live Postgres — the escape hatch for a VALID
// but harmful value that the write-gate cannot catch by construction.
const (
	EnvConfigSource      = "KEEPER_CONFIG_SOURCE"
	ConfigSourceFileOnly = "file"
)

// Querier is the narrow read surface the store needs from the pgx pool.
// Declared locally (structural typing) so the package stays decoupled from
// serviceregistry, which owns the rest of `keeper_settings`.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// OverlayApplier is the config-side hook run after a refresh that actually
// changed something. Implemented by *config.Store[config.KeeperConfig]; an
// interface so the store can be tested without a real config file.
type OverlayApplier interface {
	RefreshOverlay(ctx context.Context) config.ReloadResult
}

// InvalidationSource — subscription surface for cluster-wide invalidation.
// Satisfied by the same `service:invalidate` adapter the service registry uses
// (ADR-0073(g): the channel is reused, no new plumbing).
type InvalidationSource interface {
	Watch(ctx context.Context, onInvalidate func()) error
}

// snapshot is an immutable parsed view of the `cfg_*` rows. Published whole or
// not at all.
type snapshot struct {
	entries []config.OverlayEntry
	values  map[string]any
}

// Store owns the overlay snapshot: it reads the `cfg_*` rows, validates them
// through the field-registry and serves them to `shared/config` as an
// [config.OverlaySource].
//
// Degradation (ADR-0073(h)): a failed read NEVER resets the snapshot — the last
// good overlay stays in effect. Falling back to the file mid-flight would
// silently RAISE a limit an operator had lowered.
type Store struct {
	db       Querier
	applier  OverlayApplier
	interval time.Duration
	logger   *slog.Logger

	cur atomic.Pointer[snapshot]
}

// New builds an empty store (no rows read yet). Deliberately never fatal: the
// caller does the first [Store.Refresh] and treats its error as a warning —
// Postgres being down at startup means the instance comes up on the pure file
// base, unlike serviceregistry.NewHolder, which IS fatal because a broken
// provisioning policy is a security gate and a runtime tunable is not.
//
// db nil → the store stays empty forever (no overlay); interval <= 0 →
// [DefaultRefreshInterval]; logger nil → slog.Default().
func New(db Querier, applier OverlayApplier, interval time.Duration, logger *slog.Logger) *Store {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{db: db, applier: applier, interval: interval, logger: logger}
	s.cur.Store(&snapshot{values: map[string]any{}})
	return s
}

// Overlay implements [config.OverlaySource]: the current entries, straight from
// the in-memory snapshot (no DB access — this runs on the reload path).
func (s *Store) Overlay() []config.OverlayEntry {
	snap := s.cur.Load()
	out := make([]config.OverlayEntry, len(snap.entries))
	copy(out, snap.entries)
	return out
}

// Values returns the current overrides keyed by `keeper_settings` key — the
// read endpoint's evidence for `source: pg`.
func (s *Store) Values() map[string]any {
	snap := s.cur.Load()
	out := make(map[string]any, len(snap.values))
	for k, v := range snap.values {
		out[k] = v
	}
	return out
}

// Refresh re-reads the `cfg_*` rows and republishes the snapshot, then asks the
// config store to re-merge. All-or-nothing: one unparsable or out-of-range row
// rejects the WHOLE snapshot (a partial merge would leave the cluster in a
// state nobody asked for), and the previous snapshot survives.
func (s *Store) Refresh(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	snap, err := s.load(ctx)
	if err != nil {
		return err
	}
	s.cur.Store(snap)
	if s.applier != nil {
		res := s.applier.RefreshOverlay(ctx)
		if !res.Swapped && len(res.Diagnostics) > 0 {
			// The merged config failed validation: the overlay snapshot is
			// published but not in effect, and the previous config stays
			// current (config.Store keeps last-good on its own side).
			return fmt.Errorf("settingsstore: merged config rejected: %s", firstErrorMessage(res))
		}
	}
	return nil
}

// refresh is the best-effort variant for background goroutines: a failure is a
// warning, never a reset.
func (s *Store) refresh(ctx context.Context) {
	if err := s.Refresh(ctx); err != nil {
		s.logger.Warn("settingsstore: overlay refresh failed, keeping the last good overlay",
			slog.Any("error", err))
	}
}

// Run is the TTL fallback poll until ctx is cancelled. Blocking — the caller
// runs it in a goroutine.
func (s *Store) Run(ctx context.Context) {
	if s.db == nil {
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

// WatchInvalidations re-reads the overlay on every cluster invalidation signal.
// Blocking — the caller runs it in a goroutine. The subscription is shared with
// the service registry, so most wake-ups are unrelated; the idempotence guard in
// config.Store.RefreshOverlay turns those into a comparison.
//
// A subscription error is logged and does NOT crash the daemon — the overlay
// keeps updating through the TTL poll.
func (s *Store) WatchInvalidations(ctx context.Context, src InvalidationSource) {
	if s.db == nil || src == nil {
		return
	}
	err := src.Watch(ctx, func() { s.refresh(ctx) })
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("settingsstore: invalidation subscription ended with error, falling back to TTL-poll",
			slog.Any("error", err))
	}
}

const selectOverlaySQL = `SELECT key, value FROM keeper_settings WHERE key LIKE 'cfg\_%' ORDER BY key`

// load reads and validates every `cfg_*` row. Unknown keys (a leftover from a
// downgrade, or a key admitted by a newer version) are skipped with a warning
// rather than failing the snapshot: refusing to start on a key this binary
// simply does not know yet would make a rolling upgrade impossible.
func (s *Store) load(ctx context.Context) (*snapshot, error) {
	rows, err := s.db.Query(ctx, selectOverlaySQL)
	if err != nil {
		return nil, fmt.Errorf("settingsstore: query overlay rows: %w", err)
	}
	defer rows.Close()

	values := map[string]any{}
	byPath := map[string]any{}
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("settingsstore: scan overlay row: %w", err)
		}
		f, ok := Lookup(key)
		if !ok {
			s.logger.Warn("settingsstore: unknown overlay key ignored",
				slog.String("key", key))
			continue
		}
		v, err := f.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("settingsstore: %w", err)
		}
		values[key] = v
		byPath[f.YAMLPath] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settingsstore: iterate overlay rows: %w", err)
	}

	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	entries := make([]config.OverlayEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, config.OverlayEntry{Path: p, Value: byPath[p]})
	}
	return &snapshot{entries: entries, values: values}, nil
}

func firstErrorMessage(res config.ReloadResult) string {
	for _, d := range res.Diagnostics {
		if d.Level == diag.LevelError {
			return d.Message
		}
	}
	return "unknown validation error"
}
