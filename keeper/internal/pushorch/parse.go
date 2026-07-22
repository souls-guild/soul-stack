// Package pushorch is a multi-host push orchestrator (Variant C, architect
// verdict a58e1fcd141f8a441). Implements `POST /v1/push/apply` / `GET /v1/push/{apply_id}`
// and MCP-tool `keeper.push.apply`:
//
//   - render: synthetic single-task scenario with apply: destiny (destinyIsolated
//     by design — register/state/essence/soulprint.hosts unavailable), run through
//     the same [render.Pipeline] as scenario-runner;
//   - hosts: read-only from souls registry (topology.Resolver.LoadByInventory),
//     without incarnation-spec phase (Role="" for all);
//   - destiny: resolved by `<name>@<ref>` via pushDestinyResolver, which loads
//     the artifact using the same [artifact.DestinyLoader] as scenario-runner;
//   - dispatch: per-host SendApply via [push.SshDispatcher] (push S1+S5).
//
// Run state is stored in `push_runs` table (migration 051), per-apply_id with
// inventory[] and per-host summary in jsonb (separate from apply_runs, which has
// per-(apply_id, sid) pull semantics and cross-keeper barrier).
//
// Async model: HTTP handler receives request, performs Insert(pending) and returns
// 202 with apply_id; orchestrator goroutine executes render+dispatch and sets
// terminal state via MarkTerminal. Orphan runs (Keeper died during execution)
// are picked up by Reaper rule purge_orphan_push_runs.
package pushorch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// reDestinyName is the kebab-case destiny name regex (mirror of
// shared/config/destiny.go reDestinyName). Duplicated here so pushorch does not
// pull config for a single regex string; synchronization via equivalence test.
var reDestinyName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ErrInvalidDestinyRef is returned when the `destiny` field does not match the
// `<name>@<ref>` form. Sentinel for mapping to 422 validation-failed.
var ErrInvalidDestinyRef = errors.New("pushorch: invalid destiny ref (expected <name>@<ref>)")

// ParseDestinyRef parses the `destiny` request string into (name, ref). Form is
// fixed [docs/keeper/operator-api.md → Push endpoints]: exactly one `@` between
// non-empty parts, name is kebab-case (regex reDestinyName), ref is any non-empty
// string (detailed git-ref form validation is backlog, symmetric to
// shared/config/service.go::DependencyRef.Ref).
//
// Whitespace at edges not allowed: operator passed "dirty" ref — validation-failed,
// not silent-trim (predictable comparison with registry).
func ParseDestinyRef(s string) (name, ref string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("%w: empty string", ErrInvalidDestinyRef)
	}
	if strings.TrimSpace(s) != s {
		return "", "", fmt.Errorf("%w: leading/trailing whitespace", ErrInvalidDestinyRef)
	}
	// FIRST `@` is the separator: destiny name by regex reDestinyName does not contain
	// `@`, but git-ref may (branch name like `feature@odd`). Take the first one,
	// the rest (to end of string) is the ref.
	idx := strings.IndexByte(s, '@')
	if idx < 0 {
		return "", "", fmt.Errorf("%w: missing '@' separator in %q", ErrInvalidDestinyRef, s)
	}
	name = s[:idx]
	ref = s[idx+1:]
	if name == "" {
		return "", "", fmt.Errorf("%w: empty name in %q", ErrInvalidDestinyRef, s)
	}
	if ref == "" {
		return "", "", fmt.Errorf("%w: empty ref in %q", ErrInvalidDestinyRef, s)
	}
	if !reDestinyName.MatchString(name) {
		return "", "", fmt.Errorf("%w: name %q must match kebab-case", ErrInvalidDestinyRef, name)
	}
	return name, ref, nil
}
