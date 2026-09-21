package rbac

import "sort"

// ShellErrandLegacyRoles returns the names of roles that grant `errand.run` but
// no `soul.console` at all — the grants that stop reaching `core.cmd.shell` /
// `core.exec.run` once the console gate switches from `warn` to `enforce`
// (ADR-0074 amendment, NIM-197). Sorted, so a caller can compare successive
// snapshots and log only on change.
//
// The inventory exists so the flip is not a surprise: during the deprecation
// window an operator reads this list, adds `soul.console` to the roles that
// legitimately need a shell, and watches the count fall to zero.
//
// Matching is deliberately SCOPE-AGNOSTIC — a wildcard counts, and so does a
// scoped `errand.run on coven=prod`. Two consequences, both intended:
//
//   - A role holding `*` or `soul.*` already covers `soul.console` (the
//     consequence recorded in ADR-0074(c)) and is therefore NOT listed: nothing
//     breaks for it.
//   - A role holding BOTH rights but with a NARROWER console scope (say
//     `errand.run on coven=prod` with `soul.console on coven=staging`) is not
//     listed either, although enforcement will deny it on prod hosts. A static
//     answer there needs scope-subset reasoning; the exact live answer is the
//     `would_deny` result of `keeper_rbac_shell_errand_gate_total`, which is the
//     real check on real targets. This list is the floor, that counter is the
//     truth.
func (e *Enforcer) ShellErrandLegacyRoles() []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, r := range e.roles {
		if r == nil {
			continue
		}
		if grantsAction(r.Permissions, "errand", "run") && !grantsAction(r.Permissions, "soul", "console") {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// grantsAction reports whether any permission in the set covers resource.action
// IGNORING scope: `*`, `<resource>.*` and an exact match all count. Scope is out
// of scope here by construction — see [Enforcer.ShellErrandLegacyRoles].
func grantsAction(perms []Permission, resource, action string) bool {
	for _, p := range perms {
		if p.IsWildcard {
			return true
		}
		if p.Resource != resource {
			continue
		}
		if p.Action == "*" || p.Action == action {
			return true
		}
	}
	return false
}
