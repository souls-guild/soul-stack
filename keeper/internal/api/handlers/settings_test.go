package handlers

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/config"
)

// setPool — fake ServicePool recording every write that reached Postgres.
type setPool struct {
	writes  atomic.Int64
	deletes atomic.Int64
	key     string
	value   string
	delErr  error
}

func (p *setPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM keeper_settings") {
		if p.delErr != nil {
			return pgconn.CommandTag{}, p.delErr
		}
		p.deletes.Add(1)
		if len(args) > 0 {
			p.key, _ = args[0].(string)
		}
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *setPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.writes.Add(1)
	if len(args) >= 2 {
		p.key, _ = args[0].(string)
		p.value, _ = args[1].(string)
	}
	return setScanRow{}
}

func (p *setPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("setPool.Query unused")
}

type setScanRow struct{}

func (setScanRow) Scan(dest ...any) error {
	for _, d := range dest {
		if tp, ok := d.(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

// fakeSettingsConfig — a config reader over an inline keeper.yml.
type fakeSettingsConfig struct {
	cfg *config.KeeperConfig
	doc *config.Document
}

func (f fakeSettingsConfig) Get() *config.KeeperConfig  { return f.cfg }
func (f fakeSettingsConfig) Document() *config.Document { return f.doc }

// fakeOverlay — the SettingsStore surface: which keys PG overrides + a refresh
// counter.
type fakeOverlay struct {
	values   map[string]any
	refreshN atomic.Int64
	err      error
}

func (f *fakeOverlay) Values() map[string]any { return f.values }

func (f *fakeOverlay) Refresh(context.Context) error {
	f.refreshN.Add(1)
	return f.err
}

// settingsFixture builds a handler over the given keeper.yml source and
// overlay.
func settingsFixture(t *testing.T, yml string, overlay map[string]any) (*SettingsHandler, *setPool, *fakeOverlay) {
	t.Helper()
	// Schema diagnostics are irrelevant here (the fixture deliberately omits the
	// required blocks): the handler reads a parsed snapshot and the document, it
	// does not validate.
	cfg, doc, _, err := config.LoadKeeperFromBytes("keeper.yml", []byte(yml), config.ValidateOptions{})
	if err != nil || doc == nil {
		t.Fatalf("load config fixture: err=%v doc=%v", err, doc)
	}
	pool := &setPool{}
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ov := &fakeOverlay{values: overlay}
	return NewSettingsHandler(fakeSettingsConfig{cfg: cfg, doc: doc}, ov, svc, nil), pool, ov
}

// A minimal but valid keeper.yml with an explicit toll block and no tempo block
// — enough to tell "the operator wrote it down" from "the default showed
// through".
const settingsYML = `kid: keeper-1
toll:
  threshold: 0.9
`

func settingsClaims(aid string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: aid} }

func findSetting(views []SettingView, key string) (SettingView, bool) {
	for _, v := range views {
		if v.Key == key {
			return v, true
		}
	}
	return SettingView{}, false
}

// ADR-0073(f): the catalog answers `source` from evidence — an override in
// Postgres, an explicit key in the file, or neither.
func TestSettingsList_ReportsSourcePerKey(t *testing.T) {
	h, _, _ := settingsFixture(t, settingsYML, map[string]any{"cfg_tempo_voyage_create_rate": 3.0})
	views := h.ListTyped()

	rate, ok := findSetting(views, "cfg_tempo_voyage_create_rate")
	if !ok {
		t.Fatal("rate missing from the catalog")
	}
	if rate.Source != SettingSourcePG {
		t.Errorf("rate source = %q, want pg", rate.Source)
	}

	thr, ok := findSetting(views, "cfg_toll_threshold")
	if !ok {
		t.Fatal("threshold missing from the catalog")
	}
	if thr.Source != SettingSourceFile {
		t.Errorf("threshold source = %q, want file", thr.Source)
	}
	if thr.Value != 0.9 {
		t.Errorf("threshold value = %v, want the file value 0.9", thr.Value)
	}

	burst, ok := findSetting(views, "cfg_tempo_voyage_create_burst")
	if !ok {
		t.Fatal("burst missing from the catalog")
	}
	if burst.Source != SettingSourceDefault {
		t.Errorf("burst source = %q, want default", burst.Source)
	}
	if burst.Bounds == "" || burst.Type == "" || burst.Default == nil {
		t.Errorf("catalog entry is not self-describing: %+v", burst)
	}
}

func TestSettingsPut_WritesAndRefreshes(t *testing.T) {
	h, pool, ov := settingsFixture(t, settingsYML, map[string]any{"cfg_toll_threshold": 0.4})

	reply, err := h.PutTyped(context.Background(), settingsClaims("archon-alice"), "cfg_toll_threshold", "0.5")
	if err != nil {
		t.Fatalf("PutTyped: %v", err)
	}
	if pool.writes.Load() != 1 {
		t.Errorf("writes = %d, want 1", pool.writes.Load())
	}
	if pool.key != "cfg_toll_threshold" || pool.value != "0.5" {
		t.Errorf("wrote (%q, %q)", pool.key, pool.value)
	}
	// The publisher self-filters its own invalidation, so the writing node has
	// to re-read the overlay itself.
	if ov.refreshN.Load() != 1 {
		t.Errorf("local refresh count = %d, want 1", ov.refreshN.Load())
	}
	p := reply.AuditPayload()
	if p["key"] != "cfg_toll_threshold" || p["value"] != 0.5 || p["previous"] != 0.4 {
		t.Errorf("audit payload = %+v", p)
	}
}

// ★ The write-gate: a value outside the bounds is a 422 and NOTHING reaches
// Postgres (ADR-0073(i), fail-closed).
func TestSettingsPut_OutOfRangeIsRejectedBeforeWrite(t *testing.T) {
	for _, bad := range []string{"-1", "0", "5", "abc", ""} {
		h, pool, ov := settingsFixture(t, settingsYML, nil)
		_, err := h.PutTyped(context.Background(), settingsClaims("archon-alice"), "cfg_toll_threshold", bad)
		if err == nil {
			t.Fatalf("value %q accepted, want a rejection", bad)
		}
		d, ok := AsProblemDetails(err)
		if !ok || d.Status != 422 {
			t.Errorf("value %q: status = %v, want 422", bad, d)
		}
		if pool.writes.Load() != 0 {
			t.Errorf("value %q: keeper_settings was written despite the rejection", bad)
		}
		if ov.refreshN.Load() != 0 {
			t.Errorf("value %q: overlay refreshed despite the rejection", bad)
		}
	}
}

// Admission is enumerated: a key outside the field-registry does not exist.
func TestSettingsPut_UnknownKeyIs404(t *testing.T) {
	h, pool, _ := settingsFixture(t, settingsYML, nil)
	for _, key := range []string{"cfg_nope", "default_destiny_source", "toll_threshold"} {
		_, err := h.PutTyped(context.Background(), settingsClaims("archon-alice"), key, "0.5")
		d, ok := AsProblemDetails(err)
		if !ok || d.Status != 404 {
			t.Errorf("key %q: status = %v, want 404", key, d)
		}
	}
	if pool.writes.Load() != 0 {
		t.Errorf("an unregistered key reached keeper_settings")
	}
}

// DELETE is the clean revert of ADR-0073(f): the row goes, the file value comes
// back, and the previous override is recorded in the audit trail.
func TestSettingsDelete_DropsOverride(t *testing.T) {
	h, pool, ov := settingsFixture(t, settingsYML, map[string]any{"cfg_toll_threshold": 0.4})

	reply, err := h.DeleteTyped(context.Background(), "cfg_toll_threshold")
	if err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
	if pool.deletes.Load() != 1 || pool.key != "cfg_toll_threshold" {
		t.Errorf("deletes = %d, key = %q", pool.deletes.Load(), pool.key)
	}
	if ov.refreshN.Load() != 1 {
		t.Errorf("local refresh count = %d, want 1", ov.refreshN.Load())
	}
	p := reply.AuditPayload()
	if p["key"] != "cfg_toll_threshold" || p["previous"] != 0.4 {
		t.Errorf("audit payload = %+v", p)
	}
	if _, ok := p["value"]; ok {
		t.Errorf("delete payload carries a value: %+v", p)
	}
}

func TestSettingsDelete_UnknownKeyIs404(t *testing.T) {
	h, pool, _ := settingsFixture(t, settingsYML, nil)
	_, err := h.DeleteTyped(context.Background(), "cfg_nope")
	d, ok := AsProblemDetails(err)
	if !ok || d.Status != 404 {
		t.Errorf("status = %v, want 404", d)
	}
	if pool.deletes.Load() != 0 {
		t.Errorf("a delete reached keeper_settings for an unregistered key")
	}
}
