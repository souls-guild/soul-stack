package incarnation

import "github.com/souls-guild/soul-stack/shared/config"

// EngineCompat — the engine contract a state snapshot was produced under
// (ADR-0076(l), column `engine_compat` on incarnation and state_history,
// migration 103). A definition is rendered by keeper and applied by soul-agent,
// both versioned independently; state alone recorded WHAT was applied and never
// what applied it.
//
// A record, not a rule. Every compatibility gate lives elsewhere — the declared
// window on the render path (NIM-159) and the per-host capability set before
// dispatch (NIM-161) — and nothing may gate on this struct.
type EngineCompat struct {
	// KeeperVersion — raw build version of the instance that rendered the run,
	// verbatim (the release core is what gets compared, but the operator needs
	// the string their binary reports).
	KeeperVersion string `json:"keeper_version,omitempty"`

	// KeeperWindow — the EFFECTIVE window at render time: the intersection over
	// the service manifest and every destiny the run actually resolved
	// (ADR-0076(b)). nil when no entity declared one — unbounded, nothing to
	// record.
	KeeperWindow *config.VersionWindow `json:"keeper_window,omitempty"`

	// WindowEnforced — whether the window was really compared against this build:
	// true iff a window was declared AND the build carried a comparable version.
	// False therefore covers both "nothing declared" (read it with a nil
	// KeeperWindow) and the version-less build of ADR-0076(e), which renders
	// unenforced by design and must not later look like it was checked.
	WindowEnforced bool `json:"window_enforced"`

	// SoulCapabilities — union over the run's hosts of the capability set the
	// rendered plan required from them (ADR-0076(i)), sorted. Per-host
	// attribution is deliberately not kept: at incarnation granularity the
	// question is what the state needed from the estate, and the per-host
	// answer is the apply_runs rows of the same run.
	SoulCapabilities []string `json:"soul_capabilities,omitempty"`
}

// IsZero reports whether the stamp carries nothing worth writing. A run that
// rendered always carries at least a keeper version, so this only fires on
// paths with no engine facts at all (unit wiring), where writing an empty
// object would replace a real earlier stamp with noise.
func (e *EngineCompat) IsZero() bool {
	return e == nil ||
		(e.KeeperVersion == "" && e.KeeperWindow == nil && !e.WindowEnforced && len(e.SoulCapabilities) == 0)
}
