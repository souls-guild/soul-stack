package scenario

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Engine provenance assembly (ADR-0076(l), NIM-162). What the stamp must get
// right is attribution: the window it records is the one that CONSTRAINED the
// run — the intersection over the entities the run actually resolved — and the
// capability set is what the plan actually required, not a snapshot of one
// Passage.

func window(min, max string) *config.VersionWindow {
	return &config.VersionWindow{Min: min, Max: max}
}

func serviceEntity(w *config.VersionWindow) config.CompatEntity {
	return config.CompatEntity{Kind: config.CompatEntityService, Name: "redis", Ref: "v1.0.0", Window: w}
}

func resolverWith(entities ...config.CompatEntity) *destinyResolver {
	r := &destinyResolver{}
	for _, e := range entities {
		r.recordCompat(e)
	}
	return r
}

// The effective window is the intersection — the narrowest bound wins, whichever
// artifact set it (ADR-0076(b)). A stamp recording only the service's window
// would claim the run was free to use a keeper that one of its destinies had
// already ruled out.
func TestRunProvenance_StampIntersectsResolvedWindows(t *testing.T) {
	p := newRunProvenance("v0.2.0", serviceEntity(window("0.1.0", "0.9.0")), resolverWith(
		config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "redis", Ref: "v2.1.0", Window: window("0.2.0", "0.3.0")},
		config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "vector", Ref: "v1.0.0", Window: window("0.1.5", "")},
	))

	got := p.stamp()
	if got == nil {
		t.Fatal("stamp = nil, want a record")
	}
	if got.KeeperWindow == nil {
		t.Fatal("keeper_window = nil, want the intersection")
	}
	if got.KeeperWindow.Min != "0.2.0" || got.KeeperWindow.Max != "0.3.0" {
		t.Errorf("effective window = %s, want [0.2.0, 0.3.0) (narrowest of service+destinies)", got.KeeperWindow)
	}
	if !got.WindowEnforced {
		t.Error("window_enforced = false, want true (declared window + comparable build)")
	}
	if got.KeeperVersion != "v0.2.0" {
		t.Errorf("keeper_version = %q, want the raw string verbatim", got.KeeperVersion)
	}
}

// A destiny resolved once per Passage contributes its window once — otherwise a
// staged run's stamp would grow a duplicate entry per Passage.
func TestRunProvenance_DedupesRepeatedDestinyResolution(t *testing.T) {
	e := config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "redis", Ref: "v2.1.0", Window: window("0.2.0", "0.3.0")}
	r := resolverWith(e, e, e)
	if n := len(r.compatEntities()); n != 1 {
		t.Fatalf("recorded entities = %d, want 1 (same name+ref)", n)
	}
	// A different ref IS a different contributor — same name, other artifact.
	r.recordCompat(config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "redis", Ref: "v3.0.0", Window: window("0.4.0", "")})
	if n := len(r.compatEntities()); n != 2 {
		t.Fatalf("recorded entities = %d, want 2 (a re-pinned destiny is its own contributor)", n)
	}
}

// A staged run gates per Passage; the stamp is the UNION across them. Recording
// only the last Passage would understate what the state needed from the estate.
func TestRunProvenance_UnionsRequiredCapabilitiesAcrossPasses(t *testing.T) {
	p := newRunProvenance("v0.2.0", serviceEntity(nil), nil)
	p.observeRequired(map[string][]string{
		"host-a": {"module:core.exec", "flow_control"},
		"host-b": {"module:core.exec"},
	})
	p.observeRequired(map[string][]string{
		"host-a": {"module:core.pkg"},
	})

	got := p.stamp()
	want := []string{"flow_control", "module:core.exec", "module:core.pkg"}
	if len(got.SoulCapabilities) != len(want) {
		t.Fatalf("soul_capabilities = %v, want %v", got.SoulCapabilities, want)
	}
	for i, c := range want {
		if got.SoulCapabilities[i] != c {
			t.Fatalf("soul_capabilities = %v, want %v (sorted union)", got.SoulCapabilities, want)
		}
	}
}

// ★ A build with no comparable version renders UNENFORCED by design
// (ADR-0076(e)). The stamp must say so: recording the window without that flag
// would later read as "this run was checked against [min,max)" when it was not.
func TestRunProvenance_VersionlessBuildRecordsUnenforced(t *testing.T) {
	for _, raw := range []string{config.DevVersionSentinel, "abc1234", ""} {
		p := newRunProvenance(raw, serviceEntity(window("0.1.0", "0.3.0")), nil)
		got := p.stamp()
		if got == nil {
			t.Fatalf("version %q: stamp = nil, want the window recorded anyway", raw)
		}
		if got.WindowEnforced {
			t.Errorf("version %q: window_enforced = true, want false (nothing to compare)", raw)
		}
		if got.KeeperWindow == nil {
			t.Errorf("version %q: keeper_window dropped - the declared bound is still a fact", raw)
		}
		if got.KeeperVersion != raw {
			t.Errorf("version %q: keeper_version = %q, want the raw string even when unenforceable", raw, got.KeeperVersion)
		}
	}
}

// No entity declared anything: unbounded, so there is no window to record and
// nothing was enforced. The version and capabilities still are facts.
func TestRunProvenance_UndeclaredWindowRecordsNothingEnforced(t *testing.T) {
	p := newRunProvenance("v0.2.0", serviceEntity(nil), resolverWith(
		config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "redis", Ref: "v2.1.0"},
	))
	got := p.stamp()
	if got.KeeperWindow != nil {
		t.Errorf("keeper_window = %v, want nil (nothing declared)", got.KeeperWindow)
	}
	if got.WindowEnforced {
		t.Error("window_enforced = true with no declared window")
	}
	if got.KeeperVersion != "v0.2.0" {
		t.Errorf("keeper_version = %q, want v0.2.0", got.KeeperVersion)
	}
}

// A path with no engine facts at all writes nothing rather than an empty object:
// the incarnation column is COALESCE'd, so an empty stamp would replace a real
// earlier record with noise.
func TestRunProvenance_EmptyStampIsNil(t *testing.T) {
	if got := newRunProvenance("", serviceEntity(nil), nil).stamp(); got != nil {
		t.Errorf("stamp = %+v, want nil (nothing to record)", got)
	}
	var nilProv *runProvenance
	if got := nilProv.stamp(); got != nil {
		t.Errorf("nil provenance stamp = %+v, want nil", got)
	}
}

type stubSoulVersion struct {
	version string
	err     error
}

func (s stubSoulVersion) ReadSoulVersion(context.Context, string) (string, error) {
	return s.version, s.err
}

// ★ The version read is audit-only (ADR-0076(n)) and must degrade, never fail
// closed: a Redis outage costs the field, not the apply. This is the deliberate
// asymmetry with the capability gate, which reads the SAME Hash and DOES reject.
func TestReadAnnouncedSoulVersion_DegradesQuietly(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		reader SoulVersionReader
		sid    string
		want   string
	}{
		{"announced", stubSoulVersion{version: "v0.1.9"}, "host-a", "v0.1.9"},
		{"never announced", stubSoulVersion{}, "host-a", ""},
		{"redis failure", stubSoulVersion{err: errors.New("dial tcp: connection refused")}, "host-a", ""},
		{"no reader wired", nil, "host-a", ""},
		{"no sid", stubSoulVersion{version: "v0.1.9"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readAnnouncedSoulVersion(ctx, tc.reader, tc.sid, nil); got != tc.want {
				t.Errorf("readAnnouncedSoulVersion = %q, want %q", got, tc.want)
			}
		})
	}
}
