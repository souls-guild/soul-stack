package rbac

import (
	"fmt"
	"regexp"
	"strings"
)

// reResourceAction — the `<resource>.<action>` grammar from rbac.md.
// Resource: `[a-z][a-z0-9-]*` (kebab-case); action: `*` or
// `[a-z][a-z0-9-]*`. Hyphens are allowed (`issue-token`), space/underscore are not.
var (
	reResource = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reAction   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reSelKey   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reSelValue = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// maxRegexLen — the upper bound on selector regex-value length (ADR-047 S2a).
// RE2 (Go regexp) isn't subject to catastrophic backtracking, but a length
// cap is cheap insurance against bloated patterns in the snapshot
// (compile cost/memory at load).
const maxRegexLen = 256

// ParsePermission parses a permission string into a Permission. Invalid
// forms return an error stating the reason.
//
// Accepted forms:
//
//	"*"                                    → full wildcard.
//	"incarnation.create"                   → resource+action, no selector.
//	"incarnation.*"                        → resource+wildcard-action.
//	"incarnation.create on service=foo"    → +selector.
//	"incarnation.* on service=foo,bar"     → +selector with multiple values.
//
// Rejections:
//   - Empty string.
//   - Three or more segments (`keeper.incarnation.create` is an MCP tool, not a permission).
//   - Wildcard in resource (`*.create`).
//   - Unknown selector key (rbac.md's closed enum: service/coven/incarnation/host).
//   - Malformed selector (no `=`, empty value, invalid characters).
//   - Unknown permission in the [AllowedPermissions] catalog — checked here so
//     that loading `keeper.yml` fails before runtime (PM-decision M0.6b #7).
func ParsePermission(raw string) (Permission, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Permission{}, fmt.Errorf("permission: empty string")
	}

	// Full wildcard.
	if s == "*" {
		return Permission{IsWildcard: true}, nil
	}

	// Optional `on <selector>`. The separator is exactly ` on ` (one space
	// on each side); rbac.md is pinned to this form.
	var head, tail string
	if idx := strings.Index(s, " on "); idx >= 0 {
		head = strings.TrimSpace(s[:idx])
		tail = strings.TrimSpace(s[idx+len(" on "):])
		if tail == "" {
			return Permission{}, fmt.Errorf("permission %q: empty selector after \" on \"", raw)
		}
	} else {
		head = s
	}

	// Head: `<resource>.<action>`. Exactly one `.` separator.
	dotIdx := strings.IndexByte(head, '.')
	if dotIdx < 0 {
		return Permission{}, fmt.Errorf("permission %q: expected <resource>.<action>, no '.' found", raw)
	}
	if strings.IndexByte(head[dotIdx+1:], '.') >= 0 {
		return Permission{}, fmt.Errorf("permission %q: expected exactly two segments (<resource>.<action>); three-segment names are MCP tools, not permissions", raw)
	}
	resource := head[:dotIdx]
	action := head[dotIdx+1:]

	if resource == "" || action == "" {
		return Permission{}, fmt.Errorf("permission %q: empty resource or action segment", raw)
	}
	if resource == "*" {
		return Permission{}, fmt.Errorf("permission %q: wildcard in <resource> is not supported (use bare '*' for full wildcard)", raw)
	}
	if !reResource.MatchString(resource) {
		return Permission{}, fmt.Errorf("permission %q: resource %q does not match [a-z][a-z0-9-]*", raw, resource)
	}
	if action != "*" && !reAction.MatchString(action) {
		return Permission{}, fmt.Errorf("permission %q: action %q does not match [a-z][a-z0-9-]* or '*'", raw, action)
	}

	if !IsAllowedPermission(resource, action) {
		return Permission{}, fmt.Errorf("permission %q: unknown_permission (resource.action not in catalog rbac.md → §Каталог permissions)", raw)
	}

	// A DEPRECATED alias is canonicalized to the new name (selector is kept):
	// roles with the old name match requests by the canonical resource.action
	// without touching [Permission.Matches]. A wildcard action (`incarnation.*`)
	// is not canonicalized — it already covers the canonical name.
	if action != "*" {
		if canon, ok := deprecatedActionAliases[resource+"."+action]; ok {
			dotIdx := strings.IndexByte(canon, '.')
			resource, action = canon[:dotIdx], canon[dotIdx+1:]
		}
	}

	p := Permission{Resource: resource, Action: action}

	if tail != "" {
		sel, err := parseSelector(tail, raw)
		if err != nil {
			return Permission{}, err
		}
		p.Selector = sel
	}

	return p, nil
}

// ParseDefaultScope parses a role's default_scope string (ADR-047 S1) into a
// selector of the same shape as the per-permission `on <selector>` —
// `key=v1,v2,…`. An empty string → nil (dimension NOT introduced, role has no
// scope restriction). An invalid form → error (snapshot load fails, same as
// for a broken permission).
//
// Reuses [parseSelector] — the syntax and closed key enum
// (service/coven/incarnation/host) are shared with the per-perm selector;
// the grammar must not be duplicated.
func ParseDefaultScope(raw string) (map[string][]string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	return parseSelector(s, "default_scope "+raw)
}

// parseSelector — `key=v1,v2,…`. Exactly one key (multi-key via comma is
// not supported; see the rbac.md grammar). raw is the original permission
// string, used for message context in errors.
func parseSelector(s, raw string) (map[string][]string, error) {
	eqIdx := strings.Index(s, "=")
	if eqIdx < 0 {
		return nil, fmt.Errorf("permission %q: selector %q missing '='", raw, s)
	}
	key := s[:eqIdx]
	values := s[eqIdx+1:]
	if key == "" {
		return nil, fmt.Errorf("permission %q: selector key is empty", raw)
	}
	if !reSelKey.MatchString(key) {
		return nil, fmt.Errorf("permission %q: selector key %q does not match [a-z][a-z0-9-]*", raw, key)
	}
	if !IsAllowedSelectorKey(key) {
		return nil, fmt.Errorf("permission %q: unknown selector key %q (allowed: service|coven|incarnation|host|regex|soulprint|state|trait)", raw, key)
	}
	if values == "" {
		return nil, fmt.Errorf("permission %q: selector value-list is empty", raw)
	}

	// The regex key (ADR-047 S2a) has its own grammar: the value is
	// single-quoted, `regex='^web-.*'`. Quotes are needed so a comma inside
	// the regex (`{1,3}`) isn't interpreted as the `,` value-list separator;
	// regex special characters don't pass reSelValue, so the unquoted form is
	// forbidden. One value per key (multi-regex via `,` would be ambiguous
	// with the regex's own commas).
	if key == "regex" {
		pat, err := parseRegexValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {pat}}, nil
	}

	// The soulprint key (ADR-047 S2b) is a CEL predicate over host facts
	// (`soulprint.self.*`, ADR-018 canonical form), single-quoted:
	// `soulprint='soulprint.self.os.family == "debian"'`. Quotes are needed
	// so spaces/commas/double quotes inside the CEL aren't interpreted as the
	// `,` value-list separator. One predicate per key; union comes from
	// multiple roles/permissions. Compilation is validated by shared/cel at
	// load.
	if key == "soulprint" {
		expr, err := parseSoulprintValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {expr}}, nil
	}

	// The state key (ADR-047 S2c) is a CEL predicate over incarnation.state,
	// single-quoted: `state='state.redis_version == "8.0"'`. Quotes are
	// needed for the same reason as soulprint: spaces/commas/double quotes
	// inside the CEL must not be split by the `,` value-list separator. One
	// predicate per key; union comes from multiple roles/permissions.
	// Compilation is validated via keeper/internal/statepredicate at load
	// (migration-sandbox root `state`).
	if key == "state" {
		expr, err := parseStateValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {expr}}, nil
	}

	// The trait key (ADR-047 amendment, ADR-060 §7 slice 1) is an exact
	// key:value match against incarnation.traits. Form `trait=key:value`:
	// exactly ONE `:` (separates key and value), both halves non-empty and
	// matching [a-zA-Z0-9_.-]+ (scalar-only, like regular exact values).
	// Stored in Selector["trait"] as a single `key:value` string (not split)
	// — the match compares it whole against an incarnation's trait
	// key/value pair (slice 1 §7). One trait per key (multi-key via `,` is a
	// follow-up: AND-narrowing across multiple pairs).
	if key == "trait" {
		pair, err := parseTraitValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {pair}}, nil
	}

	parts := strings.Split(values, ",")
	out := make([]string, 0, len(parts))
	for _, v := range parts {
		// rbac.md: comma-separated values with no spaces; whitespace within
		// the separators is invalid — we don't trim.
		if v == "" {
			return nil, fmt.Errorf("permission %q: empty value in selector %q", raw, s)
		}
		if !reSelValue.MatchString(v) {
			return nil, fmt.Errorf("permission %q: selector value %q does not match [a-zA-Z0-9_.-]+", raw, v)
		}
		out = append(out, v)
	}
	return map[string][]string{key: out}, nil
}

// parseRegexValue extracts an RE2 pattern from the quoted `'…'` form and
// validates it at snapshot load (ADR-047 S2a): an empty/unquoted/over-long/
// non-compiling regex → error (load fails, same as unknown-permission).
// Returns the pattern without quotes.
func parseRegexValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: regex value %q must be single-quoted (regex='^web-.*')", raw, values)
	}
	pat := values[1 : len(values)-1]
	if pat == "" {
		return "", fmt.Errorf("permission %q: regex value is empty", raw)
	}
	if len(pat) > maxRegexLen {
		return "", fmt.Errorf("permission %q: regex too long (%d > %d, length cap)", raw, len(pat), maxRegexLen)
	}
	if _, err := regexp.Compile(pat); err != nil {
		return "", fmt.Errorf("permission %q: regex %q does not compile: %w", raw, pat, err)
	}
	return pat, nil
}

// parseSoulprintValue extracts a CEL predicate from the quoted `'…'` form and
// validates its compilation at snapshot load (ADR-047 S2b): an
// empty/unquoted/over-long/non-compiling predicate, or one using a forbidden
// sandbox root, → error (load fails, same as a broken regex/unknown
// permission). Returns the predicate without quotes. Compilation goes through
// shared/cel (sandbox FlowControl env, see [validateSoulprintExpr]); the CEL
// engine isn't duplicated.
func parseSoulprintValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: soulprint value %q must be single-quoted (soulprint='soulprint.self.os.family == \"debian\"')", raw, values)
	}
	expr := values[1 : len(values)-1]
	if expr == "" {
		return "", fmt.Errorf("permission %q: soulprint predicate is empty", raw)
	}
	if len(expr) > maxSoulprintExprLen {
		return "", fmt.Errorf("permission %q: soulprint predicate too long (%d > %d, length cap)", raw, len(expr), maxSoulprintExprLen)
	}
	if err := validateSoulprintExpr(expr); err != nil {
		return "", fmt.Errorf("permission %q: soulprint predicate %q does not compile: %w", raw, expr, err)
	}
	return expr, nil
}

// parseStateValue extracts a CEL predicate from the quoted `'…'` form and
// validates its compilation at snapshot load (ADR-047 S2c): an
// empty/unquoted/over-long/non-compiling predicate, or one using a forbidden
// sandbox root, → error (load fails, same as a broken soulprint/unknown
// permission). Returns the predicate without quotes. Compilation goes through
// keeper/internal/statepredicate (migration sandbox, root `state`, see
// [validateStateExpr]); the CEL engine isn't duplicated.
func parseStateValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: state value %q must be single-quoted (state='state.redis_version == \"8.0\"')", raw, values)
	}
	expr := values[1 : len(values)-1]
	if expr == "" {
		return "", fmt.Errorf("permission %q: state predicate is empty", raw)
	}
	if len(expr) > maxStateExprLen {
		return "", fmt.Errorf("permission %q: state predicate too long (%d > %d, length cap)", raw, len(expr), maxStateExprLen)
	}
	if err := validateStateExpr(expr); err != nil {
		return "", fmt.Errorf("permission %q: state predicate %q does not compile: %w", raw, expr, err)
	}
	return expr, nil
}

// parseTraitValue validates the `key:value` form of the trait selector at
// snapshot load (ADR-047 amendment, ADR-060 §7 slice 1) and returns it as a
// normalized `key:value` string. Requirements: EXACTLY one `:` (a single
// occurrence — separates key/value), both halves non-empty and matching
// [reSelValue] (scalar-only, the same character class as regular exact
// values). A violation → error (load fails, same as a broken soulprint/
// state). A colon inside the key/value is forbidden: it doesn't pass
// reSelValue, so there's no ambiguity about "which `:` is the separator" —
// exactly one occurrence is allowed.
func parseTraitValue(values, raw string) (string, error) {
	key, value, found := strings.Cut(values, ":")
	if !found {
		return "", fmt.Errorf("permission %q: trait value %q must be key:value (single ':')", raw, values)
	}
	// redundant-defensive: a second `:` would also be caught by reSelValue
	// below (a colon is outside [a-zA-Z0-9_.-]+) — the rejection would happen
	// without this branch too. Kept for a precise message to the operator at
	// keeper.yml load ("exactly one ':'" is clearer than "value doesn't match
	// the class"); diagnosing a broken permission is a system boundary.
	if strings.Contains(value, ":") {
		return "", fmt.Errorf("permission %q: trait value %q must contain exactly one ':' (key:value)", raw, values)
	}
	if key == "" || value == "" {
		return "", fmt.Errorf("permission %q: trait %q has empty key or value", raw, values)
	}
	if !reSelValue.MatchString(key) {
		return "", fmt.Errorf("permission %q: trait key %q does not match [a-zA-Z0-9_.-]+", raw, key)
	}
	if !reSelValue.MatchString(value) {
		return "", fmt.Errorf("permission %q: trait value %q does not match [a-zA-Z0-9_.-]+", raw, value)
	}
	return key + ":" + value, nil
}
