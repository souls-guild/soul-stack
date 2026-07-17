package grpc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

func telStrPtr(s string) *string { return &s }

// --- broadcastTelemetryConfig (fake source + fakeBidiStream from sigil_broadcast_test.go) ---

type fakeTelemetrySource struct {
	cfg *keeperv1.TelemetryConfig
	err error
}

func (f *fakeTelemetrySource) ResolveForSID(context.Context, string) (*keeperv1.TelemetryConfig, error) {
	return f.cfg, f.err
}

func newTelemetryBroadcastHandler(t *testing.T, src TelemetrySource) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:          &fakeSeedDB{},
		AuditWriter:     nopAudit{},
		KID:             "kid-test",
		TelemetrySource: src,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

func TestBroadcastTelemetryConfig_SendsConfig(t *testing.T) {
	cfg := &keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 60, Collectors: []string{"cpu", "mem"}}
	h := newTelemetryBroadcastHandler(t, &fakeTelemetrySource{cfg: cfg})
	stream := &fakeBidiStream{}

	h.broadcastTelemetryConfig(context.Background(), stream, "sid", "sess")

	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (one TelemetryConfig)", len(stream.sent))
	}
	got := stream.sent[0].GetTelemetryConfig()
	if got == nil {
		t.Fatalf("payload = %T, want TelemetryConfig", stream.sent[0].GetPayload())
	}
	if got.GetIntervalSec() != 60 || !got.GetEnabled() || len(got.GetCollectors()) != 2 {
		t.Errorf("config mismatch: %+v", got)
	}
}

func TestBroadcastTelemetryConfig_NilSourceNoOp(t *testing.T) {
	h := newTelemetryBroadcastHandler(t, nil)
	stream := &fakeBidiStream{}
	h.broadcastTelemetryConfig(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 (TelemetrySource off → no-op)", len(stream.sent))
	}
}

// TestBroadcastTelemetryConfig_NilConfigSkips — (nil,nil) = "no config" (host
// without an incarnation) → NO send (Soul stays on its soul-local cadence), unlike
// snapshot-broadcasts (an empty ReplaceAll is sent regardless).
func TestBroadcastTelemetryConfig_NilConfigSkips(t *testing.T) {
	h := newTelemetryBroadcastHandler(t, &fakeTelemetrySource{cfg: nil})
	stream := &fakeBidiStream{}
	h.broadcastTelemetryConfig(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 ((nil,nil) → skip)", len(stream.sent))
	}
}

func TestBroadcastTelemetryConfig_ErrorSkips(t *testing.T) {
	h := newTelemetryBroadcastHandler(t, &fakeTelemetrySource{err: context.DeadlineExceeded})
	stream := &fakeBidiStream{}
	h.broadcastTelemetryConfig(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 (resolve error → skip, stream stays alive)", len(stream.sent))
	}
}

func TestBroadcastTelemetryConfig_SendFailNoPanic(t *testing.T) {
	cfg := &keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 30}
	h := newTelemetryBroadcastHandler(t, &fakeTelemetrySource{cfg: cfg})
	stream := &fakeBidiStream{failAt: 1}
	// The single Send fails → the method must not panic or bubble the error up.
	h.broadcastTelemetryConfig(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (one Send attempt, which failed)", len(stream.sent))
	}
}

// --- telemetrySource.ResolveForSID (fake DB precedent from events_oracle_test.go) ---

type telemetryFakeDB struct {
	soulCoven []string
	soulErr   error   // SelectBySID → e.g. soul.ErrSoulNotFound
	incRows   [][]any // {name, service, service_version, specBytes}
}

func (f *telemetryFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *telemetryFakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "soulprint_facts") {
		// SelectSoulprint: facts NULL → ErrSoulprintNotReceived (osFamily "").
		return oracleValRow{vals: []any{"host-a.example.com", nil, nil, nil}}
	}
	// selectBySIDSQL (soul.SelectBySID): 11 columns.
	if f.soulErr != nil {
		return oracleErrRow{err: f.soulErr}
	}
	return oracleValRow{vals: []any{
		"host-a.example.com", "agent", "connected", f.soulCoven,
		nil, time.Now(), nil, nil, nil, nil, nil,
	}}
}

func (f *telemetryFakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM incarnation") {
		return &oracleStaticRows{rows: f.incRows}, nil
	}
	return &oracleEmptyRows{}, nil
}

type telemetryFakeResolver struct {
	ref artifact.ServiceRef
	ok  bool
}

func (f telemetryFakeResolver) Resolve(string) (artifact.ServiceRef, bool) { return f.ref, f.ok }

type telemetryFakeLoader struct {
	art    *artifact.ServiceArtifact
	err    error
	gotRef artifact.ServiceRef
}

func (f *telemetryFakeLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	f.gotRef = ref
	return f.art, f.err
}

// TestResolveForSID_MergesManifestAndEssence — end-to-end resolve chain: soul
// covens → incarnation by covens → ServiceRef(git from the registry, ref=ServiceVersion)
// → load → manifest `telemetry:` merged with the essence-override from `_default.yaml`.
func TestResolveForSID_MergesManifestAndEssence(t *testing.T) {
	tmp := t.TempDir()
	essDir := filepath.Join(tmp, "essence")
	if err := os.MkdirAll(essDir, 0o755); err != nil {
		t.Fatalf("mkdir essence: %v", err)
	}
	// essence override of the interval (collectors untouched — taken from the manifest).
	if err := os.WriteFile(filepath.Join(essDir, "_default.yaml"), []byte("telemetry_interval: 90s\n"), 0o644); err != nil {
		t.Fatalf("write _default.yaml: %v", err)
	}

	manifest := &config.ServiceManifest{
		Name: "web",
		Telemetry: &config.TelemetryConfig{
			Interval:   telStrPtr("45s"),
			Collectors: []string{"cpu"},
		},
	}
	loader := &telemetryFakeLoader{art: &artifact.ServiceArtifact{LocalDir: tmp, Manifest: manifest}}
	resolver := telemetryFakeResolver{ref: artifact.ServiceRef{Name: "web", Git: "file:///repo"}, ok: true}
	db := &telemetryFakeDB{
		soulCoven: []string{"web-app"},
		incRows:   [][]any{{"web-app", "web", "v2.0.0", []byte(`{}`)}},
	}
	src := NewTelemetrySource(db, resolver, loader, essence.NewResolver(discardLogger(t)), discardLogger(t))

	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ResolveForSID: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil, want an effective config")
	}
	if cfg.GetIntervalSec() != 90 {
		t.Errorf("interval_sec = %d, want 90 (essence override)", cfg.GetIntervalSec())
	}
	if len(cfg.GetCollectors()) != 1 || cfg.GetCollectors()[0] != "cpu" {
		t.Errorf("collectors = %v, want [cpu] (manifest)", cfg.GetCollectors())
	}
	// ServiceVersion override reached the loader, git — from the registry.
	if loader.gotRef.Ref != "v2.0.0" || loader.gotRef.Git != "file:///repo" || loader.gotRef.Name != "web" {
		t.Errorf("loader ref = %+v, want {web, file:///repo, v2.0.0}", loader.gotRef)
	}
}

// TestResolveForSID_SoulNotFound — host not in the registry → (nil,nil) (broadcast skipped).
func TestResolveForSID_SoulNotFound(t *testing.T) {
	db := &telemetryFakeDB{soulErr: soul.ErrSoulNotFound}
	src := NewTelemetrySource(db, telemetryFakeResolver{}, &telemetryFakeLoader{}, essence.NewResolver(discardLogger(t)), discardLogger(t))
	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil || cfg != nil {
		t.Fatalf("ResolveForSID = (%v, %v), want (nil, nil)", cfg, err)
	}
}

// TestResolveForSID_NoIncarnation — covens exist, but there is no incarnation → (nil,nil).
func TestResolveForSID_NoIncarnation(t *testing.T) {
	db := &telemetryFakeDB{soulCoven: []string{"web-app"}, incRows: nil}
	src := NewTelemetrySource(db, telemetryFakeResolver{}, &telemetryFakeLoader{}, essence.NewResolver(discardLogger(t)), discardLogger(t))
	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil || cfg != nil {
		t.Fatalf("ResolveForSID = (%v, %v), want (nil, nil)", cfg, err)
	}
}

// TestIncarnationForCovens — v1 policy for selecting an incarnation by covens: ≥2 matches →
// the first, 1 match → that one, 0 → (nil,nil). Determinism of "the first" in prod is set by
// ORDER BY name in selectIncarnationByCovensSQL; the fake returns rows in insertion
// order, so multi-match test data is already sorted by name (as live-PG would
// return it) — the fake itself does not sort.
func TestIncarnationForCovens(t *testing.T) {
	cases := []struct {
		name     string
		rows     [][]any
		wantName string // "" → expect nil
	}{
		{
			name: "≥2 matches → the first by name",
			rows: [][]any{
				{"alpha", "web", "v1.0.0", []byte(`{}`)},
				{"beta", "db", "v2.0.0", []byte(`{}`)},
			},
			wantName: "alpha",
		},
		{
			name:     "exactly 1 match → it",
			rows:     [][]any{{"solo", "web", "v1.0.0", []byte(`{}`)}},
			wantName: "solo",
		},
		{
			name:     "0 matches → nil",
			rows:     nil,
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &telemetryFakeDB{soulCoven: []string{"web-app"}, incRows: tc.rows}
			s := &telemetrySource{db: db, logger: discardLogger(t)}
			inc, err := s.incarnationForCovens(context.Background(), []string{"web-app"})
			if err != nil {
				t.Fatalf("incarnationForCovens: %v", err)
			}
			if tc.wantName == "" {
				if inc != nil {
					t.Fatalf("inc = %+v, want nil", inc)
				}
				return
			}
			if inc == nil || inc.Name != tc.wantName {
				t.Fatalf("inc = %+v, want name=%q", inc, tc.wantName)
			}
		})
	}
}
