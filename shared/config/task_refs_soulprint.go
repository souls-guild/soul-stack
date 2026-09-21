package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Static check of `soulprint.<...>` references inside scenario CEL predicates
// (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`/`loop.when:`). Mirrors
// validateTaskRefs for register.* — but schema-driven, not cross-task.
//
// What we check:
//
//  1. soulprint_naked — a bare `soulprint.<x>` without `.self`/`.hosts`/`.where(`
//     is a canonical-form error ([docs/soul/soulprint.md]: "a bare soulprint.<path>
//     without .self is a soul-lint validation error").
//  2. soulprint_unknown_path — `soulprint.self.<unknown>` (a typo like
//     `soulprint.self.os.familly`). Checked against [soulprintSelfTopLevel] +
//     [soulprintSelfSubPaths] (typed schema ADR-018 + registry projections).
//     A dynamic tail after an array/scalar segment is not validated
//     (interfaces[i].ipv4, .sid.startsWith(...), etc.) — the linter is deliberately
//     coarse, catching only typos in known prefixes.
//
// Token extraction is done by [extractSoulprintRefs] — textual: CEL string literal
// contents are stripped with the same celStringLiteral as for register. This is
// critical for predicates of the form `soulprint.hosts.where("role == 'primary'")`:
// the string CEL inside `.where(...)` is nested CEL that the interpreter parses
// separately (see shared/cel/hosts.go), and for the shallow static check we do NOT
// want to descend into it (there `role`/`covens` are __host fields, not soulprint.self.*).
//
// Dynamic access `soulprint["self"]["x"]` is not covered by the form (no dotted
// notation) — a safe skip, symmetric to the register case.

// reSoulprintCELRef extracts the first/second segment from `soulprint.<top>(.<sub>)?`.
// Left boundary is start-of-string or a non-id/dot char; `soulprint` must be the root
// identifier (`foo.soulprint.x` doesn't match, nor does `mysoulprint.x`). Group 2 is
// the top segment (`self`/`hosts`/`os`/`familly`/…); group 3 is sub (optional), used
// when top == "self" for the two-segment check. A `(` after the third segment
// deliberately doesn't match (a method call on a scalar like `.startsWith(...)` is a
// valid pattern).
var reSoulprintCELRef = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])soulprint\.([a-z][a-z0-9_]*)(?:\.([a-z][a-z0-9_]*))?`,
)

// soulprintRef — an extracted `soulprint.<top>(.<sub>)?` reference for the static check.
type soulprintRef struct {
	top string // "self" / "hosts" / "<typo>"
	sub string // empty if there was no second segment
}

// extractSoulprintRefs returns the sorted set of unique (top,sub) pairs for
// `soulprint.<top>(.<sub>)?` in a CEL string. String-literal contents are stripped
// before extraction: the nested CEL argument `.where("role == 'x'")` is validated
// inside shared/cel.rewriteHostsWhere, and here it's just data. Sorting makes
// diagnostics deterministic.
func extractSoulprintRefs(expr string) []soulprintRef {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	seen := map[soulprintRef]struct{}{}
	for _, m := range reSoulprintCELRef.FindAllStringSubmatch(stripped, -1) {
		ref := soulprintRef{top: m[2], sub: m[3]}
		// If top is a scalar (sid/hostname/covens/role), the segment after the dot
		// is a method/index/dynamic access; ignore sub so that `.sid.startsWith(...)`
		// and `.covens.exists(...)` aren't flagged.
		if soulprintScalarTopLevel[ref.top] {
			ref.sub = ""
		}
		// Special hosts/where accessors (`soulprint.hosts.where(...)`,
		// `soulprint.where(...)`): sub is not checked, that validation is done by
		// shared/cel.rewriteHostsWhere in the render phase.
		if ref.top == "hosts" || ref.top == "where" {
			ref.sub = ""
		}
		seen[ref] = struct{}{}
	}
	out := make([]soulprintRef, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].top != out[j].top {
			return out[i].top < out[j].top
		}
		return out[i].sub < out[j].sub
	})
	return out
}

// checkSoulprintRefs checks soulprint references in one CEL predicate (one of:
// `when`/`changed_when`/`failed_when`/`where`/`retry.until`/`loop.when`).
//
// A bool literal/null (force-shortcut changed_when:/failed_when:) is not a CEL string
// and is skipped. The diagnostic is at the value node's position (the exact offset
// within the string is not extracted, symmetric to checkPredicateRefs).
func checkSoulprintRefs(kind string, value ast.Node, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok {
		return nil
	}
	rt := sn.GetToken()
	line, col := 0, 0
	if rt != nil {
		line, col = rt.Position.Line, rt.Position.Column
	}
	var out []diag.Diagnostic
	for _, ref := range extractSoulprintRefs(sn.Value) {
		// 1. Canonical form: a bare `soulprint.<X>` is forbidden unless X is a
		// scenario accessor (`hosts`/`where`) or `self`.
		switch ref.top {
		case "self":
			// Allowed form, sub is checked below.
		case "hosts", "where":
			// scenario-only accessors (orchestration.md §4.1); the semantic check
			// (destiny isolation) is done in shared/cel, here we only verify this
			// is a known top segment.
			continue
		default:
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "soulprint_naked_reference",
				Message: fmt.Sprintf(
					"%s references soulprint.%s — bare soulprint.<path> without .self is not allowed (canonical form: soulprint.self.<path>)",
					kind, ref.top,
				),
				Hint:     "soulprint.self.<path> for current host facts; soulprint.hosts / soulprint.hosts.where(...) for scenario-only host listing (see docs/soul/soulprint.md)",
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
			continue
		}

		// 2. Second segment `soulprint.self.<sub>`: must be in the whitelist of
		// top-level SoulprintFacts fields (ADR-018). Empty sub is the form
		// `soulprint.self` without a dotted descent (e.g. `has(...)`), allowed.
		if ref.sub == "" {
			continue
		}
		if !soulprintSelfTopLevel[ref.sub] {
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "soulprint_unknown_path",
				Message: fmt.Sprintf(
					"%s references soulprint.self.%s — unknown field in SoulprintFacts (ADR-018)",
					kind, ref.sub,
				),
				Hint:     soulprintSelfTopHint(),
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
		}
	}
	return out
}

// soulprintSelfTopHint — a deterministic hint listing the valid top segments under
// soulprint.self.*. Built from soulprintSelfTopLevel once via lazy init.
var soulprintSelfTopHintCache string

func soulprintSelfTopHint() string {
	if soulprintSelfTopHintCache != "" {
		return soulprintSelfTopHintCache
	}
	keys := make([]string, 0, len(soulprintSelfTopLevel))
	for k := range soulprintSelfTopLevel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	soulprintSelfTopHintCache = "known top-level fields under soulprint.self.* (ADR-018): " + joinComma(keys)
	return soulprintSelfTopHintCache
}

// checkSoulprintSubPath — an extended check under `soulprint.self.<msg>.<field>`,
// currently not called (see extractSoulprintRefs grouping). Reserved for a slice that
// wants to catch `os.familly` (a second-level typo). For now the linter flags only the
// first segment; the second (a nested-message field) is deferred: it requires handling
// method-call/index forms and the positions of mixed tails `network.interfaces[i].ipv4`.
// Enabled by a separate slice on PM request.
//
// Why this placeholder: to explicitly record where the extension lives, so a future
// developer doesn't duplicate the extraction logic.
func checkSoulprintSubPath(_ string, _ string) bool { return true }

// joinComma — deterministic join of ["a","b","c"] → "a, b, c" without dependencies.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
