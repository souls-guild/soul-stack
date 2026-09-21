// Package shellgate is the Keeper-side console gate over the Errand path
// (ADR-0074 amendment 2026-07-27 / NIM-197).
//
// ADR-0074(a) justified a separate `soul.console` right by saying an Errand "can
// be checked against a module allow-list before it runs". That is true of the
// *module*, not of the command line it carries: `core.cmd.shell` and
// `core.exec.run` take an arbitrary command line and run it as the Soul daemon's
// user, typically root. So `errand.run` alone reached an arbitrary shell — the
// privilege inversion ADR-0074 declares unacceptable.
//
// This package holds the one decision every Errand choke-point now makes: when
// the requested module is verb-shell ([coremanifest.IsVerbShell]), `errand.run`
// stays NECESSARY but stops being SUFFICIENT — `soul.console` is required too,
// with the same selector the site already resolves for `errand.run`. A non-shell
// module is untouched: an ordinary Errand must not start demanding a console
// right.
//
// # Deprecation window
//
// Requiring a second right breaks existing grants, and on the Cadence path it
// would break them by the clock rather than in front of an operator. So the gate
// ships in two steps ([Mode]): `warn` records what enforcement WOULD have denied
// and lets the call through; `enforce` denies. The window is a whole minor
// release, and the inventory ([rbac.Enforcer.ShellErrandLegacyRoles]) names the
// roles that will break before it closes.
//
// The mode is a keeper.yml key, deliberately NOT a SettingsStore key: it is a
// security gate, and admission rule (j.2) of ADR-0073 keeps those out of a
// fail-soft overlay that degrades toward the more permissive value.
package shellgate

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// Mode is the gate's enforcement stage.
type Mode string

const (
	// ModeWarn is the deprecation window: a missing `soul.console` is recorded
	// (metric + WARN log) and the call proceeds. Default while the window is open.
	ModeWarn Mode = "warn"

	// ModeEnforce denies a verb-shell Errand from an operator without
	// `soul.console`. The target state, default from the next minor.
	ModeEnforce Mode = "enforce"
)

// Modes lists the valid modes in schema order (config validation, docs).
var Modes = []string{string(ModeWarn), string(ModeEnforce)}

// ParseMode validates a raw config value. Empty → [ModeWarn] (the window is the
// default while it is open).
func ParseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case "":
		return ModeWarn, nil
	case ModeWarn:
		return ModeWarn, nil
	case ModeEnforce:
		return ModeEnforce, nil
	default:
		return "", fmt.Errorf("shellgate: mode must be one of %v; got %q", Modes, raw)
	}
}

// Surfaces — the closed enum of choke-points, used as the `surface` metric label
// and in the WARN log. Closed and small on purpose: the operator's question
// during the window is "which entry point will break", and the answer must not
// require an aid/role/module label (ADR-024 §2.2 cardinality + the RBAC-metrics
// invariant that labels never carry identity).
const (
	// SurfaceREST — POST /v1/souls/{sid}/exec.
	SurfaceREST = "rest"
	// SurfaceMCP — the MCP tool `keeper.soul.errand.run`.
	SurfaceMCP = "mcp"
	// SurfaceVoyage — POST /v1/voyages with kind=command.
	SurfaceVoyage = "voyage"
	// SurfaceCadence — Cadence recipe create/update with kind=command.
	SurfaceCadence = "cadence"
	// SurfaceCadenceSpawn — the background spawn of a due Cadence. The only
	// surface with no operator waiting on the answer, which is why the window
	// exists at all.
	SurfaceCadenceSpawn = "cadence_spawn"
)

// ErrConsoleRequired is returned by [Gate.Authorize] in [ModeEnforce] when the
// operator holds `errand.run` but not `soul.console` for a verb-shell module.
// Call sites map it to their own transport shape (403 problem / MCP forbidden /
// a recorded Cadence skip); the sentinel keeps that mapping explicit rather than
// leaking the underlying RBAC error.
var ErrConsoleRequired = errors.New("verb-shell module additionally requires soul.console")

// Gate carries the enforcement stage, the metric and the logger. It does NOT
// hold an RBAC checker: every choke-point resolves its own selector for
// `errand.run` (a single host, a resolved SID set, or bare) and passes the same
// one for `soul.console` — centralising the check here would have to guess which.
//
// A nil *Gate is usable and resolves to the window ([ModeWarn]) with the
// decision counted as `unconfigured`, so a handler built without a gate shows up
// in the metric instead of silently deciding either way.
type Gate struct {
	mode    Mode
	metrics *Metrics
	logger  *slog.Logger
}

// New constructs a gate. metrics and logger are nil-safe.
func New(mode Mode, metrics *Metrics, logger *slog.Logger) *Gate {
	if mode == "" {
		mode = ModeWarn
	}
	return &Gate{mode: mode, metrics: metrics, logger: logger}
}

// Mode reports the gate's current stage ([ModeWarn] on a nil gate).
func (g *Gate) Mode() Mode {
	if g == nil {
		return ModeWarn
	}
	return g.mode
}

// Required reports whether reaching module needs `soul.console` on top of
// `errand.run`. A thin alias over the single source of the verb-shell set so
// call sites read as policy rather than as a string comparison.
func Required(module string) bool { return coremanifest.IsVerbShell(module) }

// Authorize evaluates the gate for one Errand invocation.
//
// module — the full address `<ns>.<name>.<state>` the caller is about to
// dispatch. Not verb-shell → nil immediately, and check is never called: an
// ordinary Errand is not narrowed by this gate.
//
// check — the caller's scope-aware `soul.console` probe, with the SAME selector
// it resolved for `errand.run`. nil error = the operator holds it.
//
// Returns nil when the call may proceed. In [ModeEnforce] a failing check
// returns [ErrConsoleRequired]; in [ModeWarn] it returns nil after recording the
// would-be denial.
func (g *Gate) Authorize(surface, module string, check func() error) error {
	if !Required(module) {
		return nil
	}
	if check == nil {
		// No probe supplied — nothing was verified. Counted rather than assumed:
		// a call site that forgets the check must be visible, and during the
		// window the safe reading is the same as an unconfigured gate.
		g.observe(surface, resultUnconfigured)
		g.warn(surface, module, "console check not wired")
		return nil
	}
	if err := check(); err == nil {
		g.observe(surface, resultAllow)
		return nil
	}
	if g.Mode() == ModeEnforce {
		g.observe(surface, resultDeny)
		return ErrConsoleRequired
	}
	g.observe(surface, resultWouldDeny)
	g.warn(surface, module, "allowed by the deprecation window; enforcement will deny this")
	return nil
}

func (g *Gate) observe(surface, result string) {
	if g == nil {
		return
	}
	g.metrics.Observe(surface, result)
}

// warn logs one gate decision. No aid and no command line: the identity of the
// caller belongs in the audit trail, and the WARN line exists to tell an operator
// WHICH surface and WHICH module will stop working.
func (g *Gate) warn(surface, module, detail string) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Warn("shellgate: verb-shell Errand without soul.console",
		slog.String("surface", surface),
		slog.String("module", module),
		slog.String("mode", string(g.Mode())),
		slog.String("detail", detail))
}
