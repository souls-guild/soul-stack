//go:build integration

// L1 integration for the config-overlay read (NIM-313).
//
// WHY, given this package owns exactly ONE statement. The statement is
//
//	SELECT key, value FROM keeper_settings WHERE key LIKE 'cfg\_%' ORDER BY key
//
// and the predicate is the whole contract. `keeper_settings` is a SHARED table:
// serviceregistry owns the ADR-029 well-known keys in it (`default_destiny_source`
// and friends), this package owns the `cfg_*` namespace, and the LIKE is the only
// thing keeping them apart. The escaped underscore matters too — unescaped, `_`
// is LIKE's single-character wildcard.
//
// `store_test.go` has a test that looks like it covers this,
// TestStore_ReadsOnlyPrefixedKeys, but it is
//
//	strings.Contains(db.sql, `key LIKE 'cfg\_%'`)
//
// — an assertion about the source text, not about behaviour. The fake returns
// whatever rows the test handed it regardless of the query, so the predicate is
// never evaluated by anything. It cannot answer whether a foreign key is
// excluded, whether a `cfg_*` key is included, or whether Postgres reads that
// backslash as an escape at all.
//
// The failure mode is quiet and bad in one direction: if the predicate stops
// matching, the overlay comes back EMPTY, Refresh reports success, and every
// limit an operator lowered through the API silently reverts to the keeper.yml
// default. Nothing logs, because an empty overlay is a legitimate state.
package settingsstore

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("settingsstore integration: setup failed (docker required): %v", err)
		}
		log.Printf("settingsstore integration: skipping, docker unavailable: %v", err)
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

func clearSettings(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `DELETE FROM keeper_settings`); err != nil {
		t.Fatalf("DELETE FROM keeper_settings: %v", err)
	}
}

func put(t *testing.T, key, value string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO keeper_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("put(%s=%s): %v", key, value, err)
	}
}

func newIntegrationStore() *Store {
	return New(integrationPool, nil, 0, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// newLoggingStore returns a store whose warnings are captured. The log is the
// ONLY place a row that passed the SQL predicate but failed [Lookup] becomes
// visible — see the escape argument in TestIntegration_Overlay_ReadsOnlyCfgNamespace.
func newLoggingStore() (*Store, *bytes.Buffer) {
	var buf bytes.Buffer
	return New(integrationPool, nil, 0, slog.New(slog.NewJSONHandler(&buf, nil))), &buf
}

// TestIntegration_Overlay_ReadsOnlyCfgNamespace — the predicate, evaluated by
// Postgres instead of asserted as a substring.
//
// The neighbours in the table are real: `default_destiny_source` is the ADR-029
// well-known key serviceregistry owns, and `cfg` / `config_toll_threshold` are
// near-misses on the prefix that the LIKE has to reject.
//
// The ESCAPE needs a separate observable, and it is subtle enough to spell out.
// Drop the backslash and `cfg_%` becomes "cfg, any single character, anything",
// which admits `cfgx_toll_threshold`. That row then fails [Lookup] and is SKIPPED
// as an unknown key — so Values() still holds exactly the two real keys, and a
// count assertion sees nothing wrong. The only trace is the warning `load` emits
// on the way past, which is why this test reads the log: a broken escape is a
// predicate that quietly stops meaning what it says, not one that breaks.
func TestIntegration_Overlay_ReadsOnlyCfgNamespace(t *testing.T) {
	clearSettings(t)
	ctx := context.Background()

	// Ours.
	put(t, "cfg_toll_threshold", "0.5")
	put(t, "cfg_toll_window_size", "5m")
	// Somebody else's, in the same table.
	put(t, "default_destiny_source", "git@example.com:destiny.git")
	// Near-misses on the prefix. The key format CHECK is `^[a-z][a-z0-9_]*$`, so
	// these are all storable.
	put(t, "cfgx_toll_threshold", "0.9")
	put(t, "cfg", "1")
	put(t, "config_toll_threshold", "0.9")

	s, logBuf := newLoggingStore()
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// `cfgx_toll_threshold` must never have been fetched at all. If it reached
	// Go, `load` logged it as an unknown key — which means the underscore stopped
	// being escaped and the namespace boundary is now a wildcard.
	if strings.Contains(logBuf.String(), "cfgx_toll_threshold") {
		t.Errorf("cfgx_toll_threshold reached the Go layer and was skipped as unknown — "+
			"the underscore in LIKE 'cfg\\_%%' is no longer escaped, so the prefix matches "+
			"any character in that position. Log: %s", logBuf.String())
	}

	values := s.Values()
	if len(values) != 2 {
		t.Fatalf("Values() = %#v, want exactly the two cfg_ keys", values)
	}
	if values["cfg_toll_threshold"] != 0.5 {
		t.Errorf("cfg_toll_threshold = %#v, want 0.5", values["cfg_toll_threshold"])
	}
	if _, ok := values["cfg_toll_window_size"]; !ok {
		t.Error("cfg_toll_window_size is missing from the overlay")
	}
	for _, foreign := range []string{"default_destiny_source", "cfgx_toll_threshold", "cfg", "config_toll_threshold"} {
		if _, ok := values[foreign]; ok {
			t.Errorf("%q leaked into the overlay — the LIKE predicate is not scoped to the cfg_ namespace", foreign)
		}
	}

	// Overlay() is what shared/config merges, keyed by YAML path rather than by
	// row key. Two rows in, two entries out, sorted.
	entries := s.Overlay()
	if len(entries) != 2 {
		t.Fatalf("Overlay() = %+v, want 2 entries", entries)
	}
	if entries[0].Path >= entries[1].Path {
		t.Errorf("Overlay() paths are not sorted: %q then %q", entries[0].Path, entries[1].Path)
	}
}

// TestIntegration_Overlay_EmptyTableIsEmptyOverlay — the boundary between "no
// overrides" and "the query stopped matching" is invisible from the outside, so
// the empty case is worth stating: it must be reached only when there is
// genuinely nothing in the namespace.
func TestIntegration_Overlay_EmptyTableIsEmptyOverlay(t *testing.T) {
	clearSettings(t)
	ctx := context.Background()

	// A table that is not empty, but has nothing of ours in it.
	put(t, "default_destiny_source", "git@example.com:destiny.git")

	s := newIntegrationStore()
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := s.Values(); len(got) != 0 {
		t.Errorf("Values() = %#v, want empty", got)
	}
	if got := s.Overlay(); len(got) != 0 {
		t.Errorf("Overlay() = %+v, want empty", got)
	}
}

// TestIntegration_Overlay_BrokenRowKeepsLastGood — ADR-0073(h) all-or-nothing,
// against a row that is actually in Postgres.
//
// The distinction from the fake version of this test: the bad value arrives
// through the column as TEXT, the way a value written by an older binary or by
// hand would. A partial merge here would leave the cluster running a
// configuration nobody asked for, so the previous snapshot has to survive whole.
func TestIntegration_Overlay_BrokenRowKeepsLastGood(t *testing.T) {
	clearSettings(t)
	ctx := context.Background()

	put(t, "cfg_toll_threshold", "0.5")
	s := newIntegrationStore()
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// A second key whose stored text cannot be parsed into its type. The write
	// gate would have refused it; the read path must refuse it too, since a
	// value can reach the column by other means.
	put(t, "cfg_toll_window_size", "not-a-number")
	if err := s.Refresh(ctx); err == nil {
		t.Fatal("Refresh with an unparsable row: err = nil, want a failure")
	}

	// The whole previous snapshot is intact — not partially updated, and not
	// reset to empty.
	values := s.Values()
	if len(values) != 1 {
		t.Fatalf("Values() = %#v, want the one key from the last good snapshot", values)
	}
	if values["cfg_toll_threshold"] != 0.5 {
		t.Errorf("cfg_toll_threshold = %#v, want the last good 0.5", values["cfg_toll_threshold"])
	}
}

// TestIntegration_Overlay_UnknownKeyIsSkippedNotFatal — a `cfg_*` row this binary
// does not know about must be ignored with a warning, not rejected.
//
// This is the rolling-upgrade case: during a deploy, a key written by the newer
// version sits in a table the older version is still reading. Refusing the
// snapshot would take the old instances' overlay down mid-upgrade — and since the
// row is in the namespace, the LIKE lets it through, so the skip has to happen in
// Go. Both halves of that only line up against a real query.
func TestIntegration_Overlay_UnknownKeyIsSkippedNotFatal(t *testing.T) {
	clearSettings(t)
	ctx := context.Background()

	put(t, "cfg_toll_threshold", "0.5")
	put(t, "cfg_from_a_newer_keeper", "whatever")

	s := newIntegrationStore()
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh with an unknown cfg_ key: %v — a rolling upgrade would take the overlay down", err)
	}
	values := s.Values()
	if len(values) != 1 {
		t.Fatalf("Values() = %#v, want only the known key", values)
	}
	if values["cfg_toll_threshold"] != 0.5 {
		t.Errorf("cfg_toll_threshold = %#v, want 0.5", values["cfg_toll_threshold"])
	}
}

// TestIntegration_Overlay_EveryDeclaredFieldSurvivesTheColumn — the registry
// declares each overlay field with a Kind and bounds; the column stores TEXT. For
// every field the binary claims to support, a representative value must make the
// round trip and come back as the type shared/config will merge.
//
// Driven off Fields() rather than a hand-written list, so adding a field to the
// registry without a working TEXT representation fails here. That is the drift
// this catches: the registry, the parse gate and the column are three separate
// places, and only the column is real.
// representativeText picks a value the field must accept.
//
// First choice is the field's own declared default, rendered through its own
// formatter — the most realistic value there is. Some fields cannot use it: the
// `cloud_init.*` group spells "no built-in default" as the zero value (Default:
// 0 on a port whose Min is 1), so the default is deliberately outside the
// accepted range. For those, a value is derived from the declared bounds, which
// keeps every field covered instead of skipping the awkward ones.
func representativeText(t *testing.T, f Field) string {
	t.Helper()
	if f.Default != nil {
		if raw := f.Format(f.Default); raw != "" {
			if _, err := f.Parse(raw); err == nil {
				return raw
			}
		}
	}
	switch f.Kind {
	case KindInt:
		v := int(f.Min)
		if f.MinExclusive {
			v++
		}
		if v < 1 {
			v = 1
		}
		return strconv.Itoa(v)
	case KindFloat:
		v := f.Min
		if f.MinExclusive {
			v = (f.Min + f.Max) / 2
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case KindDuration:
		d := f.MinDur
		if d <= 0 {
			d = time.Second
		}
		return d.String()
	case KindBool:
		return "true"
	case KindString:
		if len(f.Allowed) > 0 {
			return f.Allowed[0]
		}
		return "representative-value"
	}
	t.Fatalf("%s: unknown kind %q — no representative value can be derived", f.Key, f.Kind)
	return ""
}

func TestIntegration_Overlay_EveryDeclaredFieldSurvivesTheColumn(t *testing.T) {
	ctx := context.Background()

	for _, f := range Fields() {
		t.Run(f.Key, func(t *testing.T) {
			clearSettings(t)

			raw := representativeText(t, f)
			want, err := f.Parse(raw)
			if err != nil {
				t.Fatalf("%s: Parse(%q): %v", f.Key, raw, err)
			}

			put(t, f.Key, raw)
			s := newIntegrationStore()
			if err := s.Refresh(ctx); err != nil {
				t.Fatalf("%s: Refresh with %q: %v", f.Key, raw, err)
			}

			got, ok := s.Values()[f.Key]
			if !ok {
				t.Fatalf("%s did not appear in the overlay — it is declared but the query does not reach it", f.Key)
			}
			if got != want {
				t.Errorf("%s = %#v (%T), want %#v (%T)", f.Key, got, got, want, want)
			}

			entries := s.Overlay()
			if len(entries) != 1 {
				t.Fatalf("%s: Overlay() = %+v, want one entry", f.Key, entries)
			}
			if entries[0].Path != f.YAMLPath {
				t.Errorf("%s: overlay path = %q, want %q", f.Key, entries[0].Path, f.YAMLPath)
			}
		})
	}
}
