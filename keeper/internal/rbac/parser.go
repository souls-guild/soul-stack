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
)

// ParsePermission parses a permission string into a Permission. Invalid
// forms return an error stating the reason.
//
// Accepted forms:
//
//	"*"                                         → full wildcard.
//	"incarnation.create"                        → resource+action, no scope.
//	"incarnation.*"                             → resource+wildcard-action.
//	"incarnation.create on coven=foo"           → +scope.
//	"incarnation.* on coven in (a,b) AND host matches web-*" → +boolean scope.
//
// The scope is a boolean predicate over coven/service/incarnation/host/trait
// (NIM-128, see scope_ast.go); the removed regex/soulprint/state dimensions
// are rejected as unknown dimensions (fail-closed).
//
// Rejections:
//   - Empty string.
//   - Three or more segments (`keeper.incarnation.create` is an MCP tool, not a permission).
//   - Wildcard in resource (`*.create`).
//   - Malformed scope predicate.
//   - Unknown permission in the [AllowedPermissions] catalog — checked here so
//     that loading `keeper.yml` fails before runtime (PM-decision M0.6b #7).
func ParsePermission(raw string) (Permission, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Permission{}, fmt.Errorf("permission: empty string")
	}

	// Optional `on <scope>`. The separator is exactly ` on ` (one space
	// on each side); rbac.md is pinned to this form.
	var head, tail string
	if idx := strings.Index(s, " on "); idx >= 0 {
		head = strings.TrimSpace(s[:idx])
		tail = strings.TrimSpace(s[idx+len(" on "):])
		if tail == "" {
			return Permission{}, fmt.Errorf("permission %q: empty scope after \" on \"", raw)
		}
	} else {
		head = s
	}

	// Full or scoped wildcard: `*` (cluster-admin) or `* on <scope>` (NIM-128
	// amendment — a "scoped super-admin": all actions, bounded to <scope>). A
	// bare `*` is unrestricted; a scoped `*` is enforced against the request
	// context by [Permission.Matches] like any other scope.
	if head == "*" {
		p := Permission{IsWildcard: true}
		if tail != "" {
			expr, err := ParseScopeExpr(tail)
			if err != nil {
				return Permission{}, fmt.Errorf("permission %q: %w", raw, err)
			}
			p.Scope = expr
		}
		return p, nil
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
		return Permission{}, fmt.Errorf("permission %q: unknown_permission (resource.action not in catalog rbac.md -> §Permissions Catalog)", raw)
	}

	// A DEPRECATED alias is canonicalized to the new name (scope is kept):
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
		expr, err := ParseScopeExpr(tail)
		if err != nil {
			return Permission{}, fmt.Errorf("permission %q: %w", raw, err)
		}
		p.Scope = expr
	}

	return p, nil
}

// ParseDefaultScope parses a role's default_scope string (ADR-047 S1) into a
// boolean scope predicate (NIM-128), same grammar as the per-permission
// `on <scope>`. An empty string → nil (dimension NOT introduced, role has no
// scope restriction). An invalid form → error (snapshot load fails, same as
// for a broken permission).
func ParseDefaultScope(raw string) (*ScopeExpr, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	return ParseScopeExpr(s)
}
