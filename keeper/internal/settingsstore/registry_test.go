package settingsstore

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// staticSource is a [config.OverlaySource] over a fixed entry set.
type staticSource []config.OverlayEntry

func (s staticSource) Overlay() []config.OverlayEntry { return s }

// keeperFixture copies the golden keeper.yml into a temp file. It DOES set
// several admitted keys (reaper.*, logging.level) — which is what makes it the
// right fixture for the "the local file wins" guard and the wrong one for a
// delivery test.
func keeperFixture(t *testing.T) string {
	return copyFixture(t, "../../../examples/keeper/keeper.yml")
}

// floorFixture is a keeper.yml reduced to the bootstrap floor: no admitted key
// is set locally, so every cluster value can actually reach its consumer.
func floorFixture(t *testing.T) string {
	return copyFixture(t, "testdata/bootstrap-floor.yml")
}

func copyFixture(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(src))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dst
}

func loadFixtureStore(t *testing.T) *config.Store[config.KeeperConfig] {
	return loadStore(t, keeperFixture(t))
}

func loadFloorStore(t *testing.T) *config.Store[config.KeeperConfig] {
	return loadStore(t, floorFixture(t))
}

func loadStore(t *testing.T, path string) *config.Store[config.KeeperConfig] {
	t.Helper()
	store, diags, err := config.LoadKeeperStore(path, config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load fixture %s: err=%v diags=%v", path, err, diags)
	}
	return store
}

// ADR-0073(e): the registry is the contract — keys must be unique, carry the
// reserved prefix and be storable at all under the migration-035 CHECK.
func TestRegistry_KeyContract(t *testing.T) {
	seen := map[string]bool{}
	paths := map[string]bool{}
	for _, f := range Fields() {
		if seen[f.Key] {
			t.Errorf("duplicate key %q", f.Key)
		}
		seen[f.Key] = true

		if len(f.Key) <= len(KeyPrefix) || f.Key[:len(KeyPrefix)] != KeyPrefix {
			t.Errorf("key %q lacks the reserved %q prefix", f.Key, KeyPrefix)
		}
		if !KeyFormat.MatchString(f.Key) {
			t.Errorf("key %q violates the keeper_settings CHECK", f.Key)
		}
		if paths[f.YAMLPath] {
			t.Errorf("duplicate yaml path %q", f.YAMLPath)
		}
		paths[f.YAMLPath] = true

		switch f.Kind {
		case KindFloat, KindInt:
			if f.Min >= f.Max {
				t.Errorf("key %q: empty range [%v, %v]", f.Key, f.Min, f.Max)
			}
		case KindDuration:
			if f.MinDur <= 0 || f.MinDur >= f.MaxDur {
				t.Errorf("key %q: empty duration range [%v, %v]", f.Key, f.MinDur, f.MaxDur)
			}
		case KindBool:
			// A boolean is in-domain by construction; bounds would be noise.
		case KindString:
			def, isStr := f.Default.(string)
			if !isStr {
				t.Errorf("key %q: string field with a %T default", f.Key, f.Default)
			} else if len(f.Allowed) > 0 && !slices.Contains(f.Allowed, def) {
				t.Errorf("key %q: default %q is outside its own enum %v", f.Key, def, f.Allowed)
			}
		default:
			t.Errorf("key %q: unknown kind %q", f.Key, f.Kind)
		}

		if f.Read == nil {
			t.Errorf("key %q: no Read accessor", f.Key)
		}
		if f.Default == nil {
			t.Errorf("key %q: no default", f.Key)
		}
		if f.Description == "" {
			t.Errorf("key %q: no description — the UI renders the form from this catalog", f.Key)
		}
		// A field may legitimately have NO built-in value: "" (the feature is
		// unavailable until set) or 0 (`cloud_init.event_stream_port` falls back
		// to the bootstrap port). Such a default is deliberately outside the
		// writable range — the way back to it is DELETE, not a write. Every
		// other default must round-trip through the field's own gate.
		if !isZeroDefault(f.Default) {
			if _, err := f.Parse(f.Format(f.Default)); err != nil {
				t.Errorf("key %q: the built-in default is outside its own bounds: %v", f.Key, err)
			}
		}
		if _, err := f.Parse(""); err == nil {
			t.Errorf("key %q: an empty value was accepted", f.Key)
		}
	}
}

// ADR-0073(j.2/j.4): admission is closed to the bootstrap floor and to security
// gates. This is what keeps a sensitive value out of the catalog, the audit
// events and the logs — it can never be admitted in the first place, which is
// stronger than masking it on the way out.
func TestRegistry_AdmissionGuard(t *testing.T) {
	forbiddenPrefixes := map[string]string{
		"$.postgres":                            "bootstrap floor — it is the way to reach the store",
		"$.redis":                               "bootstrap floor — the invalidation channel itself",
		"$.vault":                               "bootstrap floor — read before Postgres",
		"$.kid":                                 "per-instance identity",
		"$.listen":                              "per-instance network surface, bound once at startup",
		"$.hot_reload":                          "governs the reload mechanism itself",
		"$.auth":                                "security gate — a fail-soft overlay must not touch authn/authz",
		"$.metrics":                             "security gate — metrics auth",
		"$.otel":                                "require-restart — the exporter is initialised once",
		"$.sigil":                               "signing material",
		"$.rbac":                                "moved to Postgres with its own consumer (ADR-028)",
		"$.plugin_runtime.allowed_capabilities": "security whitelist",
		// logging.level IS admitted (a live apply path, the most common
		// cluster-wide operation); the rest of the block builds the writer and
		// must work before Postgres exists.
		"$.logging.file":     "the writer is built before Postgres",
		"$.logging.format":   "the writer is built before Postgres",
		"$.logging.rotation": "the writer is built before Postgres",
	}
	secretMarkers := []string{"_ref", "password", "secret", "token", "signing_key"}
	// A vault-ref is a POINTER, not a secret: the value in the row is a path,
	// the material never leaves Vault. Each exception is listed by name so a new
	// `*_ref` key cannot slip in unnoticed.
	refExceptions := map[string]bool{"$.cloud_init.tls_ca_ref": true}

	for _, f := range Fields() {
		for prefix, why := range forbiddenPrefixes {
			if f.YAMLPath == prefix || strings.HasPrefix(f.YAMLPath, prefix+".") {
				t.Errorf("key %q overlays %s, which is out of admission: %s", f.Key, f.YAMLPath, why)
			}
		}
		if refExceptions[f.YAMLPath] {
			continue
		}
		for _, marker := range secretMarkers {
			if strings.Contains(f.YAMLPath, marker) {
				t.Errorf("key %q overlays %s, which looks like a secret (%q) — the overlay carries no secrets", f.Key, f.YAMLPath, marker)
			}
		}
	}
}

func isZeroDefault(v any) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case int:
		return t == 0
	default:
		return false
	}
}

// stringProbes — a valid, non-default sample per string key. Kept here rather
// than in the registry: production has no use for it, but a new string key must
// not silently skip the "does the path reach the consumer" guard.
var stringProbes = map[string]string{
	"cfg_logging_level":                 "debug",
	"cfg_cloud_init_bootstrap_endpoint": "keeper.probe.test:8443",
	"cfg_cloud_init_tls_ca_ref":         "vault:secret/keeper/ca#pem",
	"cfg_cloud_init_soul_binary_url":    "https://dist.probe.test/soul",
	"cfg_cloud_init_soul_binary_ca":     "https://dist.probe.test/ca.pem",
	"cfg_cloud_init_soul_version":       "v9.9.9",
}

// Every registered yaml path must actually land in KeeperConfig: the merged
// config validates cleanly AND the value arrives where the consumer reads it.
// This is what catches a typo'd path, which would otherwise fail silently as an
// unknown key at reload time.
func TestRegistry_PathsResolveInKeeperConfig(t *testing.T) {
	for _, f := range Fields() {
		t.Run(f.Key, func(t *testing.T) {
			store := loadFloorStore(t)
			want := probeValue(t, f, f.Read(store.Get()))
			store.SetOverlaySource(staticSource{{Path: f.YAMLPath, Value: want}})

			res := store.RefreshOverlay(context.Background())
			if !res.Swapped {
				t.Fatalf("overlay at %s rejected: %+v", f.YAMLPath, res.Diagnostics)
			}
			if got := f.Read(store.Get()); got != want {
				t.Errorf("%s: effective value = %v, want %v (path does not reach the consumer)", f.YAMLPath, got, want)
			}
		})
	}
}

// probeValue returns an in-bounds value DIFFERENT from the current one, so
// "applied" is distinguishable from "resolved to the default". The step is
// deliberately minimal: a big jump would break a cross-field invariant
// (poll_floor <= poll_ceiling <= poll_idle) and test the wrong thing.
func probeValue(t *testing.T, f Field, cur any) any {
	t.Helper()
	var raw string
	switch f.Kind {
	case KindFloat:
		v, ok := cur.(float64)
		if !ok {
			t.Fatalf("%s: Read returned %T, want float64", f.Key, cur)
		}
		raw = f.Format(v * 1.5)
	case KindInt:
		v, ok := cur.(int)
		if !ok {
			t.Fatalf("%s: Read returned %T, want int", f.Key, cur)
		}
		raw = f.Format(v + 1)
	case KindDuration:
		v, ok := cur.(string)
		if !ok {
			t.Fatalf("%s: Read returned %T, want string", f.Key, cur)
		}
		d, err := config.ParseDuration(v)
		if err != nil {
			t.Fatalf("%s: Read returned %q, which is not a duration: %v", f.Key, v, err)
		}
		raw = formatDuration(d + time.Second)
	case KindBool:
		v, ok := cur.(bool)
		if !ok {
			t.Fatalf("%s: Read returned %T, want bool", f.Key, cur)
		}
		raw = f.Format(!v)
	case KindString:
		sample, ok := stringProbes[f.Key]
		if !ok {
			t.Fatalf("%s: no probe sample — add one to stringProbes", f.Key)
		}
		raw = sample
	}
	probe, err := f.Parse(raw)
	if err != nil {
		t.Fatalf("%s: probe %q is out of the field's own bounds: %v", f.Key, raw, err)
	}
	if probe == cur {
		t.Fatalf("%s: probe equals the current value %v — the test would pass on a dead path", f.Key, cur)
	}
	return probe
}

// ★ ADR-0073(b), amended: a key this instance's keeper.yml sets explicitly keeps
// its LOCAL value — the cluster override applies only where the file is silent.
// The golden fixture sets reaper.* and logging.level, which is exactly the case
// this guard needs.
func TestRegistry_LocalFileWinsOverCluster(t *testing.T) {
	shadowed := map[string]any{
		"cfg_reaper_interval":   "1h",
		"cfg_reaper_batch_size": 500,
		"cfg_logging_level":     "info",
	}
	probes := map[string]any{
		"cfg_reaper_interval":   "2h",
		"cfg_reaper_batch_size": 999,
		"cfg_logging_level":     "debug",
	}

	for key, local := range shadowed {
		t.Run(key, func(t *testing.T) {
			f := mustLookup(t, key)
			store := loadFixtureStore(t)
			if got := f.Read(store.Get()); got != local {
				t.Fatalf("precondition: the fixture should set %s to %v, got %v", f.YAMLPath, local, got)
			}

			store.SetOverlaySource(staticSource{{Path: f.YAMLPath, Value: probes[key]}})
			store.RefreshOverlay(context.Background())

			if got := f.Read(store.Get()); got != local {
				t.Errorf("%s = %v, want the local %v — the cluster value overrode the file", f.YAMLPath, got, local)
			}
		})
	}

	// The same override on an instance whose file stays silent DOES apply — the
	// rule is "the file wins", not "the cluster is ignored".
	f := mustLookup(t, "cfg_reaper_interval")
	store := loadFloorStore(t)
	store.SetOverlaySource(staticSource{{Path: f.YAMLPath, Value: "2h"}})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay rejected on the floor fixture: %+v", res.Diagnostics)
	}
	if got := f.Read(store.Get()); got != "2h" {
		t.Errorf("%s = %v on a file that does not set it, want the cluster 2h", f.YAMLPath, got)
	}
}

func TestField_ParseAndBounds(t *testing.T) {
	thr := mustLookup(t, "cfg_toll_threshold")
	if v, err := thr.Parse("0.5"); err != nil || v != 0.5 {
		t.Errorf("Parse(0.5) = (%v, %v)", v, err)
	}
	// A type validator alone would accept every one of these.
	for _, bad := range []string{"", "abc", "-1", "0", "1.5", "1e9"} {
		if _, err := thr.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a range/type rejection", bad)
		}
	}
	if got := thr.Bounds(); got != "(0, 1]" {
		t.Errorf("Bounds() = %q, want (0, 1]", got)
	}

	burst := mustLookup(t, "cfg_tempo_voyage_create_burst")
	if v, err := burst.Parse("20"); err != nil || v != 20 {
		t.Errorf("Parse(20) = (%v, %v)", v, err)
	}
	for _, bad := range []string{"0", "-5", "1.5", "100000"} {
		if _, err := burst.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a rejection", bad)
		}
	}

	interval := mustLookup(t, "cfg_reaper_interval")
	// Normalisation: what the operator types and what the file spells agree.
	if v, err := interval.Parse("120m"); err != nil || v != "2h" {
		t.Errorf("Parse(120m) = (%v, %v), want 2h", v, err)
	}
	if v, err := interval.Parse("7d"); err == nil {
		t.Errorf("Parse(7d) = %v, want a rejection above the 24h ceiling", v)
	}
	// The range bound is the point: a type validator is happy with 1ms.
	for _, bad := range []string{"", "1ms", "5s", "soon", "-1h"} {
		if _, err := interval.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a rejection", bad)
		}
	}
	if got := interval.Bounds(); got != "[1m, 24h]" {
		t.Errorf("Bounds() = %q, want [1m, 24h]", got)
	}

	dry := mustLookup(t, "cfg_reaper_dry_run")
	if v, err := dry.Parse("true"); err != nil || v != true {
		t.Errorf("Parse(true) = (%v, %v)", v, err)
	}
	for _, bad := range []string{"", "yes", "1.5", "maybe"} {
		if _, err := dry.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a rejection", bad)
		}
	}
	if got := dry.Bounds(); got != "true | false" {
		t.Errorf("Bounds() = %q, want true | false", got)
	}
}

// The write-gate rejects a value that is in-bounds per field but breaks a
// cross-field invariant. Without this the row would be written, every reader
// would then reject the WHOLE overlay (all-or-nothing) and the cluster would sit
// on its last-good snapshot with nobody having been told (ADR-0073(h/i)).
func TestValidateOverlay_RejectsCrossFieldViolation(t *testing.T) {
	store := loadFloorStore(t)

	floor := mustLookup(t, "cfg_cadence_scheduler_poll_floor")
	// 10m is inside [30s, 1h] for the field itself, but above the 60s ceiling.
	v, err := floor.Parse("10m")
	if err != nil {
		t.Fatalf("the probe must pass the per-field gate: %v", err)
	}
	diags := store.ValidateOverlay([]config.OverlayEntry{{Path: floor.YAMLPath, Value: v}})
	if !diag.HasErrors(diags) {
		t.Fatalf("poll_floor=10m accepted with poll_ceiling=1m; diags=%+v", diags)
	}

	// The same field within the corridor is accepted, so the guard is not just
	// rejecting everything.
	ok, err := floor.Parse("45s")
	if err != nil {
		t.Fatalf("parse 45s: %v", err)
	}
	if diags := store.ValidateOverlay([]config.OverlayEntry{{Path: floor.YAMLPath, Value: ok}}); diag.HasErrors(diags) {
		t.Errorf("poll_floor=45s rejected: %+v", diags)
	}
}

func TestLookup_UnknownKeyIsNotAdmitted(t *testing.T) {
	for _, k := range []string{"cfg_nope", "default_destiny_source", "provisioning_allowed_methods", ""} {
		if _, ok := Lookup(k); ok {
			t.Errorf("Lookup(%q) admitted a key outside the registry", k)
		}
	}
}

func mustLookup(t *testing.T, key string) Field {
	t.Helper()
	f, ok := Lookup(key)
	if !ok {
		t.Fatalf("%s missing from the registry", key)
	}
	return f
}

// ★ The write-gate has to judge the candidate as the CLUSTER will see it, not
// only as this node will. With the file winning (ADR-0073(b), amended), a key
// this node's keeper.yml sets is skipped by the local merge — so a value that
// breaks a cross-field invariant would validate cleanly here and then be
// rejected as a whole by every node whose file stays silent, freezing them on
// their last-good overlay.
func TestValidateOverlay_JudgesTheCandidateForNodesWhoseFileIsSilent(t *testing.T) {
	// This node pins poll_ceiling locally; poll_idle is left to its default (2m).
	path := copyFixture(t, "testdata/bootstrap-floor.yml")
	appendYAML(t, path, "\ncadence_scheduler:\n  poll_ceiling: 1m\n")
	store := loadStore(t, path)

	ceiling := mustLookup(t, "cfg_cadence_scheduler_poll_ceiling")
	bad, err := ceiling.Parse("50m") // in-bounds per field, above the 2m poll_idle
	if err != nil {
		t.Fatalf("the probe must pass the per-field gate: %v", err)
	}

	// The local merge skips the key (the file sets it), so this node would never
	// notice — but a node whose file is silent would apply 50m and reject the
	// whole overlay.
	diags := store.ValidateOverlay([]config.OverlayEntry{{Path: ceiling.YAMLPath, Value: bad}})
	if !diag.HasErrors(diags) {
		t.Errorf("a value this node ignores was accepted without asking what it does elsewhere")
	}

	// A value that is fine everywhere still goes through, so the guard is not
	// simply refusing every shadowed key.
	ok, err := ceiling.Parse("90s")
	if err != nil {
		t.Fatalf("parse 90s: %v", err)
	}
	if diags := store.ValidateOverlay([]config.OverlayEntry{{Path: ceiling.YAMLPath, Value: ok}}); diag.HasErrors(diags) {
		t.Errorf("poll_ceiling=90s rejected: %+v", diags)
	}
}

func appendYAML(t *testing.T, path, extra string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte(extra)...), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
