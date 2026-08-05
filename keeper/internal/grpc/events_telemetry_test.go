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
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
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

// telemetryFakeDB answers the three reads the resolve makes, keeping MEMBERSHIP
// and LABELS as separate facts — a fake that served both from one field could
// not fail the way production did (NIM-248: delivery matched incarnation names
// against `souls.coven`, which migration 099 had emptied).
type telemetryFakeDB struct {
	// soulCoven — tags attached to THIS host (`souls.coven[]`).
	soulCoven []string
	soulErr   error // SelectBySID → e.g. soul.ErrSoulNotFound
	// memberRows — the incarnations this host is BOUND to, as the
	// `incarnation_membership` join returns them ({name, service,
	// service_version, specBytes}, ordered by name).
	memberRows [][]any
	// incarnationCoven — extra tags those incarnations carry; the host inherits
	// them along with each incarnation's name (ADR-080).
	incarnationCoven []string
	// covenNamedRows — incarnations whose NAME merely matches one of the host's
	// tags. Nothing in production may read these: the field exists so that a
	// revert to the pre-NIM-248 `FROM incarnation WHERE name = ANY(<covens>)`
	// lookup shows up as a red test instead of shipping as a silent no-op.
	covenNamedRows [][]any
	// hitCovenNamed records that the legacy lookup above was issued at all —
	// asserted false by the resolve tests, so the regression is caught by its
	// shape and not only by its effect.
	hitCovenNamed bool
}

// memberNames — the names of the incarnations the host is bound to, which are
// inherited labels in their own right.
func (f *telemetryFakeDB) memberNames() []string {
	out := make([]string, 0, len(f.memberRows))
	for _, r := range f.memberRows {
		name, _ := r[0].(string)
		out = append(out, name)
	}
	return out
}

func (f *telemetryFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *telemetryFakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	// Must precede any `incarnation_membership` route: the inherited-labels
	// statement reads that table too, and only the marker tells them apart.
	case strings.Contains(sql, soul.InheritedLabelsQueryMarker):
		return oracleValRow{vals: []any{
			append(f.memberNames(), f.incarnationCoven...),
			[]byte("[]"),
		}}
	case strings.Contains(sql, "soulprint_facts"):
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
	switch {
	case strings.Contains(sql, "FROM incarnation_membership"):
		return &oracleStaticRows{rows: f.memberRows}, nil
	case strings.Contains(sql, "FROM incarnation"):
		f.hitCovenNamed = true
		return &oracleStaticRows{rows: f.covenNamedRows}, nil
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

// TestResolveForSID_MergesManifestAndServiceVars — end-to-end resolve chain:
// membership → incarnation → ServiceRef(git from the registry, ref=ServiceVersion)
// → load → manifest `telemetry:` merged with the service's own vars from
// `vars/00-base.yaml`.
//
// The host carries NO tag of its own: being bound to the incarnation is the
// whole reason it is owed a config (NIM-248).
func TestResolveForSID_MergesManifestAndServiceVars(t *testing.T) {
	tmp := t.TempDir()
	varsDir := filepath.Join(tmp, "vars")
	if err := os.MkdirAll(varsDir, 0o755); err != nil {
		t.Fatalf("mkdir vars: %v", err)
	}
	// The service's own interval (collectors untouched — taken from the manifest).
	if err := os.WriteFile(filepath.Join(varsDir, "00-base.yaml"), []byte("telemetry_interval: 90s\n"), 0o644); err != nil {
		t.Fatalf("write vars/00-base.yaml: %v", err)
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
		memberRows: [][]any{{"web-app", "web", "v2.0.0", []string(nil), []byte(nil)}},
	}
	src := NewTelemetrySource(db, resolver, loader, servicevars.NewResolver(discardLogger(t)), discardLogger(t))

	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ResolveForSID: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil, want an effective config")
	}
	if cfg.GetIntervalSec() != 90 {
		t.Errorf("interval_sec = %d, want 90 (the service's own var)", cfg.GetIntervalSec())
	}
	if len(cfg.GetCollectors()) != 1 || cfg.GetCollectors()[0] != "cpu" {
		t.Errorf("collectors = %v, want [cpu] (manifest)", cfg.GetCollectors())
	}
	// ServiceVersion override reached the loader, git — from the registry.
	if loader.gotRef.Ref != "v2.0.0" || loader.gotRef.Git != "file:///repo" || loader.gotRef.Name != "web" {
		t.Errorf("loader ref = %+v, want {web, file:///repo, v2.0.0}", loader.gotRef)
	}
	if db.hitCovenNamed {
		t.Error("delivery looked incarnations up by coven name — the relation is the only source of membership (NIM-248)")
	}
}

// TestResolveForSID_SoulNotFound — host not in the registry → (nil,nil) (broadcast skipped).
func TestResolveForSID_SoulNotFound(t *testing.T) {
	db := &telemetryFakeDB{soulErr: soul.ErrSoulNotFound}
	src := NewTelemetrySource(db, telemetryFakeResolver{}, &telemetryFakeLoader{}, servicevars.NewResolver(discardLogger(t)), discardLogger(t))
	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil || cfg != nil {
		t.Fatalf("ResolveForSID = (%v, %v), want (nil, nil)", cfg, err)
	}
}

// TestResolveForSID_NoMembershipNoConfig — a host bound to nothing gets no
// config, and the Soul keeps its soul-local cadence.
func TestResolveForSID_NoMembershipNoConfig(t *testing.T) {
	db := &telemetryFakeDB{memberRows: nil}
	src := NewTelemetrySource(db, telemetryFakeResolver{}, &telemetryFakeLoader{}, servicevars.NewResolver(discardLogger(t)), discardLogger(t))
	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil || cfg != nil {
		t.Fatalf("ResolveForSID = (%v, %v), want (nil, nil)", cfg, err)
	}
}

// TestResolveForSID_CovenTagIsNotMembership — a host merely TAGGED with a string
// that spells an incarnation's name is not a member of it and is owed nothing,
// even though an incarnation by that name exists and would have been served by
// the pre-NIM-248 lookup (the fake answers that lookup from covenNamedRows, so a
// revert turns this red rather than shipping a host someone else's config).
func TestResolveForSID_CovenTagIsNotMembership(t *testing.T) {
	loader := &telemetryFakeLoader{art: &artifact.ServiceArtifact{
		LocalDir: t.TempDir(),
		Manifest: &config.ServiceManifest{Name: "web", Telemetry: &config.TelemetryConfig{Interval: telStrPtr("45s")}},
	}}
	db := &telemetryFakeDB{
		soulCoven:      []string{"web-app"},
		memberRows:     nil,
		covenNamedRows: [][]any{{"web-app", "web", "v2.0.0", []string(nil), []byte(nil)}},
	}
	src := NewTelemetrySource(db, telemetryFakeResolver{ref: artifact.ServiceRef{Name: "web", Git: "file:///repo"}, ok: true},
		loader, servicevars.NewResolver(discardLogger(t)), discardLogger(t))

	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ResolveForSID: %v", err)
	}
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil (a coven tag is not a membership)", cfg)
	}
	if db.hitCovenNamed {
		t.Error("delivery looked incarnations up by coven name — the relation is the only source of membership (NIM-248)")
	}
}

// TestResolveForSID_InheritsIncarnationCovenIntoServiceVars — the other half of
// the NIM-248 split, restored over the mechanism that replaced the deleted
// hard-wired `coven/<label>.yaml` overlay (ADR-0082). Membership decides WHICH
// service config the host is owed; the incarnation's own labels decide which
// overlays of that config apply, and a `vars/_stack.yaml` foreach step is how a
// service asks for them. The tag lives on the incarnation and on no host.
func TestResolveForSID_InheritsIncarnationCovenIntoServiceVars(t *testing.T) {
	tmp := t.TempDir()
	varsDir := filepath.Join(tmp, "vars")
	if err := os.MkdirAll(filepath.Join(varsDir, "coven"), 0o755); err != nil {
		t.Fatalf("mkdir vars/coven: %v", err)
	}
	if err := os.WriteFile(filepath.Join(varsDir, "coven", "cache.yaml"),
		[]byte("telemetry_interval: 15s\n"), 0o644); err != nil {
		t.Fatalf("write coven overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(varsDir, "_stack.yaml"), []byte(`stack:
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    optional: true
`), 0o644); err != nil {
		t.Fatalf("write _stack.yaml: %v", err)
	}

	manifest := &config.ServiceManifest{
		Name:      "web",
		Telemetry: &config.TelemetryConfig{Interval: telStrPtr("45s"), Collectors: []string{"cpu"}},
	}
	loader := &telemetryFakeLoader{art: &artifact.ServiceArtifact{LocalDir: tmp, Manifest: manifest}}
	db := &telemetryFakeDB{
		// The tag is on the INCARNATION row; the host carries none of its own.
		memberRows: [][]any{{"web-app", "web", "v2.0.0", []string{"cache"}, []byte(nil)}},
	}
	src := NewTelemetrySource(db, telemetryFakeResolver{ref: artifact.ServiceRef{Name: "web", Git: "file:///repo"}, ok: true},
		loader, servicevars.NewResolver(discardLogger(t)), discardLogger(t))

	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ResolveForSID: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil, want an effective config")
	}
	if cfg.GetIntervalSec() != 15 {
		t.Errorf("interval_sec = %d, want 15 — the incarnation's tag must reach its members' service vars", cfg.GetIntervalSec())
	}
}

// TestResolveForSID_PassesIncarnationTraitsToTheStack — the rule
// `scenario/state.go` writes down as hard: covens AND traits must reach the
// resolver on EVERY path, or the same `vars/_stack.yaml` answers differently
// depending on which one asked. The asymmetry is why this is a test and not a
// convention: a missing key aborts loudly (`no such key: traits`), and on this
// path the loudness is then swallowed — `broadcastTelemetryConfig` treats a
// resolve failure as a warning and the Soul silently keeps its local cadence.
func TestResolveForSID_PassesIncarnationTraitsToTheStack(t *testing.T) {
	tmp := t.TempDir()
	varsDir := filepath.Join(tmp, "vars")
	if err := os.MkdirAll(varsDir, 0o755); err != nil {
		t.Fatalf("mkdir vars: %v", err)
	}
	if err := os.WriteFile(filepath.Join(varsDir, "00-base.yaml"), []byte("telemetry_interval: 45s\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(varsDir, "10-prd.yaml"), []byte("telemetry_interval: 15s\n"), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(varsDir, "_stack.yaml"), []byte(`stack:
  - file: 00-base.yaml
  - file: 10-prd.yaml
    when: "incarnation.traits.env == 'prd'"
`), 0o644); err != nil {
		t.Fatalf("write _stack.yaml: %v", err)
	}

	manifest := &config.ServiceManifest{
		Name:      "web",
		Telemetry: &config.TelemetryConfig{Interval: telStrPtr("90s"), Collectors: []string{"cpu"}},
	}
	loader := &telemetryFakeLoader{art: &artifact.ServiceArtifact{LocalDir: tmp, Manifest: manifest}}
	db := &telemetryFakeDB{
		memberRows: [][]any{{"web-app", "web", "v2.0.0", []string(nil), []byte(`{"env":"prd"}`)}},
	}
	src := NewTelemetrySource(db, telemetryFakeResolver{ref: artifact.ServiceRef{Name: "web", Git: "file:///repo"}, ok: true},
		loader, servicevars.NewResolver(discardLogger(t)), discardLogger(t))

	cfg, err := src.ResolveForSID(context.Background(), "host-a.example.com")
	if err != nil {
		t.Fatalf("ResolveForSID: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil, want an effective config")
	}
	if cfg.GetIntervalSec() != 15 {
		t.Errorf("interval_sec = %d, want 15 — the incarnation's traits must reach the stack step", cfg.GetIntervalSec())
	}
}

// TestIncarnationForSID — v1 policy for picking one incarnation out of a host's
// memberships: ≥2 → the first by name, 1 → that one, 0 → (nil,nil). Determinism
// of "the first" in prod comes from ORDER BY i.name in
// selectIncarnationsForSIDSQL; the fake returns rows in insertion order, so the
// multi-membership case is already sorted by name (as live PG would return it) —
// the fake itself does not sort.
func TestIncarnationForSID(t *testing.T) {
	cases := []struct {
		name     string
		rows     [][]any
		wantName string // "" → expect nil
	}{
		{
			name: "≥2 memberships → the first by name",
			rows: [][]any{
				{"alpha", "web", "v1.0.0", []string{"cache"}, []byte(nil)},
				{"beta", "db", "v2.0.0", []string(nil), []byte(nil)},
			},
			wantName: "alpha",
		},
		{
			name:     "exactly 1 membership → it",
			rows:     [][]any{{"solo", "web", "v1.0.0", []string(nil), []byte(nil)}},
			wantName: "solo",
		},
		{
			name:     "no membership → nil",
			rows:     nil,
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &telemetryFakeDB{memberRows: tc.rows}
			s := &telemetrySource{db: db, logger: discardLogger(t)}
			inc, err := s.incarnationForSID(context.Background(), "host-a.example.com")
			if err != nil {
				t.Fatalf("incarnationForSID: %v", err)
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
