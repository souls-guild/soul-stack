package errandrunner

import (
	"strings"
	"testing"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// markerModule is an empty SoulModule with the ErrandReadSafe marker — the
// shape of core.http / core.noop: Apply is cleared for ad-hoc, Plan is not
// declared pure-read.
type markerModule struct{ sdkmodule.BaseModule }

func (markerModule) ErrandReadSafe() {}

// planSafeModule carries ONLY PlanReadSafe — the shape of core.file and the 12
// other modules with a pure-read Plan. Deliberately no ErrandReadSafe: this is
// the type whose Apply must stay unreachable through Errand.
type planSafeModule struct{ sdkmodule.BaseModule }

func (planSafeModule) PlanReadSafe() {}

// bothMarkersModule carries both markers. No core module has this shape today;
// it pins the two paths as independent rather than ordered.
type bothMarkersModule struct{ sdkmodule.BaseModule }

func (bothMarkersModule) PlanReadSafe()   {}
func (bothMarkersModule) ErrandReadSafe() {}

// plainModule has no marker. BaseModule deliberately does NOT implement ErrandReadSafe.
type plainModule struct{ sdkmodule.BaseModule }

func TestIsAllowed_Hardcoded(t *testing.T) {
	t.Parallel()
	cases := []string{"core.cmd.shell", "core.exec.run"}
	for _, full := range cases {
		ok, reason := IsAllowed(full, &plainModule{}, false)
		if !ok {
			t.Errorf("IsAllowed(%q, plain, apply) = (false, %q); want true", full, reason)
		}
	}
}

func TestIsAllowed_Marker(t *testing.T) {
	t.Parallel()
	ok, reason := IsAllowed("core.http.probe", &markerModule{}, false)
	if !ok {
		t.Errorf("IsAllowed(marker, apply) = (false, %q); want true", reason)
	}
}

func TestIsAllowed_RejectPlain(t *testing.T) {
	t.Parallel()
	ok, reason := IsAllowed("core.pkg.installed", &plainModule{}, false)
	if ok {
		t.Fatalf("IsAllowed(plain, apply) = true; want false")
	}
	if !strings.HasPrefix(reason, "errand_module_not_allowed:") {
		t.Errorf("reason = %q; want errand_module_not_allowed prefix", reason)
	}
}

func TestIsAllowed_RejectHardcodedStateMismatch(t *testing.T) {
	t.Parallel()
	// core.cmd.foo isn't in the hardcoded list (only shell is); without a marker — reject.
	ok, _ := IsAllowed("core.cmd.foo", &plainModule{}, false)
	if ok {
		t.Errorf("IsAllowed(core.cmd.foo, apply) = true; want false (only shell hardcoded)")
	}
}

func TestIsAllowed_NilModule(t *testing.T) {
	t.Parallel()
	// nil-mod: defensive reject, the hardcoded list still applies on the Apply path.
	if ok, _ := IsAllowed("core.cmd.shell", nil, false); !ok {
		t.Errorf("IsAllowed(core.cmd.shell, nil, apply) = false; want true (hardcoded before marker-check)")
	}
	if ok, _ := IsAllowed("core.pkg.installed", nil, false); ok {
		t.Errorf("IsAllowed(core.pkg.installed, nil, apply) = true; want false")
	}
	// On the dry-run path a nil module can't be asserted for PlanReadSafe;
	// the hardcoded list grants nothing here either, and the reason is the
	// not-allowed family, NOT a dry-run capability gap.
	for _, full := range []string{"core.cmd.shell", "core.pkg.installed"} {
		ok, reason := IsAllowed(full, nil, true)
		if ok {
			t.Errorf("IsAllowed(%q, nil, dry_run) = true; want false", full)
		}
		if reason == ReasonDryRunUnsupported {
			t.Errorf("IsAllowed(%q, nil, dry_run) reason = %q; want the not-allowed family (a missing module is not a capability gap)", full, reason)
		}
	}
}

// TestIsAllowed_DryRunAdmitsPlanReadSafe is side (a) of the NIM-488 guard: a
// module declaring a pure-read Plan is admitted on the dry-run path even
// though it carries NO ErrandReadSafe. Before NIM-488 this returned false for
// every module in the tree, which made `dry_run: true` terminal everywhere
// (ADR-033 contract row for dry_run).
func TestIsAllowed_DryRunAdmitsPlanReadSafe(t *testing.T) {
	t.Parallel()
	// core.file.present is the address NIM-455 needs; the other two states of
	// the same module resolve to the same type and must behave alike.
	for _, full := range []string{"core.file.present", "core.file.absent", "core.file.rendered"} {
		ok, reason := IsAllowed(full, &planSafeModule{}, true)
		if !ok {
			t.Errorf("IsAllowed(%q, planReadSafe, dry_run) = (false, %q); want true", full, reason)
		}
	}
}

// TestIsAllowed_DryRunRejectsWithoutPlanReadSafe covers the two shapes ADR-033
// names explicitly: verb-shell modules and probe answer
// `errand_dry_run_unsupported` on dry_run. ErrandReadSafe is a statement about
// Apply and must NOT buy passage onto the Plan path.
func TestIsAllowed_DryRunRejectsWithoutPlanReadSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		full string
		mod  sdkmodule.SoulModule
	}{
		{"verb-shell", "core.cmd.shell", &plainModule{}},
		{"verb-exec", "core.exec.run", &plainModule{}},
		{"errand-read-safe only", "core.http.probe", &markerModule{}},
		{"no markers", "core.pkg.installed", &plainModule{}},
	}
	for _, tc := range cases {
		ok, reason := IsAllowed(tc.full, tc.mod, true)
		if ok {
			t.Errorf("%s: IsAllowed(%q, dry_run) = true; want false", tc.name, tc.full)
		}
		if reason != ReasonDryRunUnsupported {
			t.Errorf("%s: reason = %q; want %q", tc.name, reason, ReasonDryRunUnsupported)
		}
	}
}

// TestIsAllowed_PlanReadSafeNeverReachesApply is side (b) of the NIM-488
// guard, and the security-load-bearing half: PlanReadSafe alone must NOT open
// the Apply path. `dry_run: false` and an omitted field are the same wire
// state (proto3 bool defaults to false), so a client cannot reach Apply on
// core.file by dropping the field.
func TestIsAllowed_PlanReadSafeNeverReachesApply(t *testing.T) {
	t.Parallel()
	for _, full := range []string{"core.file.present", "core.file.absent", "core.file.rendered", "core.user.present"} {
		ok, reason := IsAllowed(full, &planSafeModule{}, false)
		if ok {
			t.Fatalf("IsAllowed(%q, planReadSafe, apply) = true; want false — PlanReadSafe must not reach Apply", full)
		}
		if !strings.HasPrefix(reason, "errand_module_not_allowed:") {
			t.Errorf("IsAllowed(%q, planReadSafe, apply) reason = %q; want errand_module_not_allowed prefix", full, reason)
		}
	}
}

// TestIsAllowed_BothMarkersIndependent pins the paths as independent: each
// marker opens its own path and neither implies the other.
func TestIsAllowed_BothMarkersIndependent(t *testing.T) {
	t.Parallel()
	mod := &bothMarkersModule{}
	if ok, reason := IsAllowed("core.demo.present", mod, true); !ok {
		t.Errorf("IsAllowed(both, dry_run) = (false, %q); want true", reason)
	}
	if ok, reason := IsAllowed("core.demo.present", mod, false); !ok {
		t.Errorf("IsAllowed(both, apply) = (false, %q); want true", reason)
	}
}
