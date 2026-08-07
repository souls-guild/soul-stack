package schema

import "fmt"

// Deprecation of an input parameter — the migration path that makes param-level
// strictness safe (ADR-0076 deprecation policy).
//
// Strictness and deprecation are two halves of one rule. An engine that rejects an
// unknown parameter must never let a contract shrink without notice, or every
// tightening of a module schema would break definitions that were valid yesterday. So
// a parameter is never deleted outright: it is first marked deprecated, keeps being
// honored for the whole declared window while the author sees a warning, and only
// then leaves the schema — at which point it becomes `unknown_param` at both gates
// (the static check in `shared/config` and the Soul-side check before Apply).

// DeprecationMinMinors — how many MINOR releases a deprecated parameter must keep
// working before it may be removed (ADR-0076 deprecation policy). Marked in X.Y.0 →
// removable no earlier than X.(Y+2).0, which is exactly the half-open window
// `{min: X.Y.0, max: X.(Y+2).0}` an author may declare in `compat:` (ADR-0076(c)):
// the guarantee an author reads off the policy and the window they can write down are
// the same interval, not two rules to reconcile.
//
// Two rather than one because an author who upgrades one minor at a time needs at
// least one release where the old parameter and its replacement BOTH work — with a
// one-minor window there is no version they can stand on while migrating.
const DeprecationMinMinors = 2

// Deprecated is the deprecation notice on one input parameter (ADR-0076 deprecation
// policy).
//
// RemovedIn is EXCLUSIVE: the first engine release that no longer honors the
// parameter, so it reads exactly like a compat window's upper bound and a parameter
// deprecated in `0.4.0` with `removed_in: 0.6.0` is live across the whole
// `0.4.x`–`0.5.x` band. Both bounds are required — a deprecation with no end date is a
// warning that never resolves, and an author cannot plan against it.
type Deprecated struct {
	// Since — the release that marked the parameter deprecated (inclusive).
	Since string `json:"since,omitempty"`
	// RemovedIn — the first release that no longer honors it (EXCLUSIVE).
	RemovedIn string `json:"removed_in,omitempty"`
	// Use — the replacement parameter in the SAME state; empty when the parameter
	// is going away with no successor.
	Use string `json:"use,omitempty"`
}

// Notice renders the author-facing deprecation text used by every surface that
// reports one (the static check, the Soul-side log, the module catalog), so an author
// reads the same sentence wherever it reaches them.
func (d *Deprecated) Notice(param string) string {
	if d == nil {
		return ""
	}
	s := fmt.Sprintf("param %q is deprecated since %s and stops working in %s", param, d.Since, d.RemovedIn)
	if d.Use != "" {
		s += fmt.Sprintf("; use %q instead", d.Use)
	}
	return s
}

// meetsDeprecationWindow reports whether removed is far enough above since. A major
// bump always qualifies: a new major IS the declared breaking-change boundary, so it
// is not held to the minor count.
func meetsDeprecationWindow(since, removed version) bool {
	if removed.major > since.major {
		return true
	}
	if removed.major < since.major {
		return false
	}
	return removed.minor >= since.minor+DeprecationMinMinors
}
