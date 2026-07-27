package plugin

// Deprecation of a manifest input param — the migration path that makes
// param-level strictness safe (ADR-0076 deprecation policy).
//
// Strictness and deprecation are two halves of one rule. An engine that rejects
// an unknown param must never let a contract shrink without notice, or every
// tightening of a manifest would break definitions that were valid yesterday.
// So a param is never deleted outright: it is first marked `deprecated:`, keeps
// being honored for the whole declared window while the author sees a warning,
// and only then leaves the manifest — at which point it becomes `unknown_param`
// at both gates (the static check in shared/config and the Soul-side check
// before Apply).

import (
	"fmt"
	"regexp"

	"github.com/Masterminds/semver/v3"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// DeprecationMinMinors — how many MINOR releases a deprecated param must keep
// working before it may be removed (ADR-0076 deprecation policy). Marked in
// X.Y.0 → removable no earlier than X.(Y+2).0, which is exactly the half-open
// window `{min: X.Y.0, max: X.(Y+2).0}` an author may declare in `compat:`
// (ADR-0076(c)): the guarantee an author reads off the policy and the window
// they can write down are the same interval, not two rules to reconcile.
//
// Two rather than one because an author who upgrades one minor at a time needs
// at least one release where the old param and its replacement BOTH work — with
// a one-minor window there is no version they can stand on while migrating.
const DeprecationMinMinors = 2

// reDeprecationVersion — the accepted form of a deprecation bound: plain
// MAJOR.MINOR.PATCH, the same grammar as a `compat:` bound
// (config.reCompatVersion, ADR-0076(c)). Duplicated rather than shared because
// shared/config imports this package, not the other way round.
var reDeprecationVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// DeprecatedDef — the deprecation notice on one manifest input param
// (ADR-0076 deprecation policy).
//
// RemovedIn is EXCLUSIVE: the first engine release that no longer honors the
// param, so it reads exactly like [VersionWindow.Max] and a param deprecated in
// `0.4.0` with `removed_in: 0.6.0` is live across the whole `0.4.x`–`0.5.x`
// band. Both bounds are required — a deprecation with no end date is a warning
// that never resolves, and an author cannot plan against it.
type DeprecatedDef struct {
	// Since — the release that marked the param deprecated (inclusive).
	Since string `yaml:"since,omitempty"`
	// RemovedIn — the first release that no longer honors it (EXCLUSIVE).
	RemovedIn string `yaml:"removed_in,omitempty"`
	// Use — the replacement param in the SAME state; empty when the param is
	// going away with no successor.
	Use string `yaml:"use,omitempty"`
}

// Notice renders the author-facing deprecation text used by every surface that
// reports one (the static check, the Soul-side log, the module catalog), so an
// author reads the same sentence wherever it reaches them.
func (d *DeprecatedDef) Notice(param string) string {
	if d == nil {
		return ""
	}
	s := fmt.Sprintf("param %q is deprecated since %s and stops working in %s", param, d.Since, d.RemovedIn)
	if d.Use != "" {
		s += fmt.Sprintf("; use %q instead", d.Use)
	}
	return s
}

// validateDeprecated — schema validation of an input param's `deprecated:`
// block: both bounds present and well-formed, and a window no shorter than the
// policy minimum. Called from validateInputParam; a nil block is valid (the
// param is simply not deprecated).
func validateDeprecated(root *ast.MappingNode, path, name string, d *DeprecatedDef) []diag.Diagnostic {
	if d == nil {
		return nil
	}
	var out []diag.Diagnostic
	for _, b := range []struct{ key, value string }{{"since", d.Since}, {"removed_in", d.RemovedIn}} {
		switch {
		case b.value == "":
			out = append(out, atPath(root, path+".deprecated."+b.key, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "deprecated_bound_missing",
				Message: fmt.Sprintf("input parameter %q deprecated: has no %s", name, b.key),
				Hint:    "both since (inclusive) and removed_in (EXCLUSIVE) are required - an open-ended deprecation cannot be planned against",
			}))
		case !reDeprecationVersion.MatchString(b.value):
			out = append(out, atPath(root, path+".deprecated."+b.key, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "deprecated_version_invalid",
				Message: fmt.Sprintf("input parameter %q deprecated.%s=%q is not a plain MAJOR.MINOR.PATCH version", name, b.key, b.value),
				Hint:    "use 0.6.0 - no v prefix, no pre-release suffix, no >=/< operators (ADR-0076)",
			}))
		}
	}
	if diag.HasErrors(out) {
		return out
	}

	since, serr := semver.StrictNewVersion(d.Since)
	removed, rerr := semver.StrictNewVersion(d.RemovedIn)
	if serr != nil || rerr != nil {
		return out
	}
	if !meetsDeprecationWindow(since, removed) {
		out = append(out, atPath(root, path+".deprecated.removed_in", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code: "deprecation_window_too_short",
			Message: fmt.Sprintf(
				"input parameter %q is deprecated in %s but removed in %s - the policy grants at least %d minor releases",
				name, d.Since, d.RemovedIn, DeprecationMinMinors),
			Hint: fmt.Sprintf("set removed_in to %d.%d.0 or later, or bump the major (ADR-0076 deprecation policy)",
				since.Major(), since.Minor()+DeprecationMinMinors),
		}))
	}
	return out
}

// meetsDeprecationWindow reports whether removed is far enough above since. A
// major bump always qualifies: a new major IS the declared breaking-change
// boundary, so it is not held to the minor count.
func meetsDeprecationWindow(since, removed *semver.Version) bool {
	if removed.Major() > since.Major() {
		return true
	}
	if removed.Major() < since.Major() {
		return false
	}
	return removed.Minor() >= since.Minor()+DeprecationMinMinors
}
