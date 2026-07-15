// Package rbac provides runtime permission checks for Archons, per
// [docs/keeper/rbac.md].
//
// The [AllowedPermissions] catalog is a closed enum of names, validated by
// [NewEnforcer] when loading `keeper.yml`. An unknown name → fatal error
// (PM-decision M0.6b #7).
//
// Selectors (`on key=v1,v2`) are parsed separately; grammar is
// `<resource>.<action>` + optional `key=values` where key ∈ {service,
// coven, incarnation, host}.
package rbac

import "sort"

// AllowedPermissions — the catalog of permission names from rbac.md →
// §Catalog of permissions. 103 names (sum of the categories below):
//
//   - operator (5): create / revoke / issue-token / list / read;
//   - role (6): create / delete / list / update / grant-operator / revoke-operator;
//   - synod (8): create / update / delete / list / add-operator / remove-operator / grant-role / revoke-role (ADR-049);
//   - incarnation (14): create / rerun-last / run / get / list / history / unlock / upgrade / destroy / check-drift / update-hosts / update (deprecated alias) / traits-set / view-secrets;
//   - soul (6): list / create / issue-token / coven-assign / traits-assign / ssh-target-update;
//   - plugin (3): allow / revoke / list;
//   - sigil (4): key-introduce / key-retire / key-list / key-set-primary;
//   - service (4): register / update / list / deregister;
//   - omen (3): create / list / delete;
//   - rite (3): create / list / delete;
//   - vigil (3): create / list / delete;
//   - decree (3): create / list / delete;
//   - push (3): apply / cleanup / read;
//   - push-provider (5): create / update / delete / list / read (ADR-032 amendment S7-2);
//   - errand (3): run / cancel / list (ADR-033);
//   - choir (5): create / delete / list / add-voice / remove-voice (ADR-044, S-T3);
//   - cadence (6): create / list / update / delete / enable / disable (ADR-046, S4; enable/disable — amendment 2026-06-02);
//   - herald (5): create / read / list / update / delete (ADR-052, S4);
//   - tiding (5): create / read / list / update / delete (ADR-052, S4);
//   - provisioning (2): read / update (ADR-058 Part B — operator-creation-method policy);
//   - audit (1): read;
//   - provider (3): create / read / delete (ADR-017, Cloud CRUD);
//   - profile (3): create / read / delete (ADR-017, Cloud CRUD).
//
// A wildcard `*` in `<action>` (`incarnation.*`) expands at resolve time
// and matches any known `<action>` for that `<resource>`. Wildcard in
// `<resource>` is not supported in the MVP.
//
// Extending the catalog is a normal PR with a matching rbac.md update.
// Names are never removed (operator roles in `keeper.yml` may hold
// historical names; removal would break existing installations).
var AllowedPermissions = map[string]struct{}{
	// operator.*
	"operator.create":      {},
	"operator.revoke":      {},
	"operator.issue-token": {},
	// operator.list / operator.read — read-only access to the Archon
	// registry (`GET /v1/operators`, `GET /v1/operators/{aid}`). Selector —
	// NoSelector (no per-resource scope, same as operator.create/revoke);
	// per-AID scope is a separate future slice once multi-tenant RBAC
	// lands. `operator.read` is split from `operator.list` symmetrically
	// with push.read↔push.apply: reading a single record is conceptually
	// broader than `list`, but in the MVP both are covered by one right —
	// the drift test and rbac.md record its presence in the catalog, the
	// route mounts `operator.list` on both endpoints.
	"operator.list": {},
	"operator.read": {},

	// role.* — RBAC management (roles / permissions / membership) via
	// OpenAPI/MCP (ADR-028(e), rbac.md → §Catalog of permissions → Role).
	"role.create":          {},
	"role.delete":          {},
	"role.list":            {},
	"role.update":          {},
	"role.grant-operator":  {},
	"role.revoke-operator": {},

	// synod.* — Synod group management (ADR-049): an intermediate level
	// Archon → Synod → Roles. 8 permissions. Selector — NoSelector (group
	// management is a cluster-level operation, no coven/host scope, same
	// as role.* / operator.*; ADR-049 does NOT introduce group-scope).
	// grant-role/add-operator are gated by the least-privilege subset,
	// delete/remove-operator/revoke-role by self-lockout (ADR-049(f)).
	// synod.update changes ONLY the description (cosmetic, grants/revokes
	// no rights) — no subset/self-lockout check; name (PK) is immutable.
	"synod.create":          {},
	"synod.update":          {},
	"synod.delete":          {},
	"synod.list":            {},
	"synod.add-operator":    {},
	"synod.remove-operator": {},
	"synod.grant-role":      {},
	"synod.revoke-role":     {},

	// incarnation.*
	"incarnation.create": {},
	// incarnation.rerun-last — restarts the last failed scenario from
	// error_locked (`POST /v1/incarnations/{name}/rerun-last`). A separate
	// right from `incarnation.create`/`incarnation.unlock`: rerun clears
	// error_locked and restarts the last failed scenario in one action,
	// requires a reason. Same scope selector (incarnation/coven/service by
	// path-{name}).
	"incarnation.rerun-last":  {},
	"incarnation.run":         {},
	"incarnation.get":         {},
	"incarnation.list":        {},
	"incarnation.history":     {},
	"incarnation.unlock":      {},
	"incarnation.upgrade":     {},
	"incarnation.destroy":     {},
	"incarnation.check-drift": {},
	// incarnation.update-hosts — changes the declared `spec.hosts[]` of an
	// incarnation record via the Operator API (`PATCH
	// /v1/incarnations/{name}/hosts`, UI Hosts editing): same scope
	// selector incarnation/coven/service as other incarnation mutations
	// (run/unlock/upgrade). Name narrowed from the former
	// `incarnation.update` (PM-decision 2026-06-02) to make room for
	// future update-covens/update-spec — each operation gets its own
	// permission.
	"incarnation.update-hosts": {},
	// incarnation.update — DEPRECATED alias for `incarnation.update-hosts`.
	// Kept in the catalog (closed enum, names are never removed): roles/
	// operators in keeper.yml / DB with the historical name must NOT fail
	// snapshot load. [ParsePermission] canonicalizes it to
	// `incarnation.update-hosts` (see [deprecatedActionAliases]), so such
	// roles keep access to /hosts.
	"incarnation.update": {},
	// incarnation.traits-set — a wholesale replacement of an incarnation's
	// operator-set trait labels (`incarnation.traits` jsonb, ADR-060 amend
	// R1) via `PUT /v1/incarnations/{name}/traits`. incarnation.traits is
	// the source of truth, projected by a sync hook into `souls.traits` of
	// member hosts; moves operator-facing trait management from per-soul
	// (`soul.traits-assign`, deprecated) to per-incarnation. Action is
	// hyphenated (`traits-set`) since the permission grammar is exactly
	// `<resource>.<action>` (pattern: soul.traits-assign /
	// incarnation.update-hosts). Same scope selector
	// incarnation/coven/service by path-{name} as incarnation.update-hosts.
	"incarnation.traits-set": {},
	// incarnation.view-secrets — reveals the plaintext value of an
	// incarnation secret declared in the service's `revealable_secrets`
	// (NIM-74): POST .../secrets/reveal + discovery GET
	// .../secrets/revealable. Strictly more privileged than
	// `incarnation.get` (unmasking, not reading under the mask). Same
	// scope selector incarnation/coven/service by path-{name} as other
	// incarnation mutations. Audited as `incarnation.secret_revealed` (no
	// value logged).
	"incarnation.view-secrets": {},

	// soul.*
	"soul.list":         {},
	"soul.create":       {},
	"soul.issue-token":  {},
	"soul.coven-assign": {},
	// soul.traits-assign — bulk mutation of operator-set trait labels
	// (jsonb column `souls.traits`, ADR-060) across a selector:
	// merge/replace/remove. Action is hyphenated (`traits-assign`) since
	// the permission grammar is exactly `<resource>.<action>` (pattern:
	// soul.coven-assign / soul.ssh-target-update). Same selector as
	// soul.coven-assign (`coven=` / `host=` / bare): bulk trait-assign is
	// gated by the same coven-scope (gate a, target hosts ⊆ scope) —
	// least-privilege isn't weakened. The trait KEY is NOT a scope
	// dimension (unlike a Coven label), so there's no gate (b) on keys.
	"soul.traits-assign": {},
	// soul.ssh-target-update — changes per-host SSH credentials for the
	// push flow (ADR-032 amendment 2026-05-26, S7-1). Action is hyphenated
	// (`ssh-target-update`) since the permission grammar is exactly
	// `<resource>.<action>` (the 3-segment `soul.ssh-target.update` is an
	// MCP tool, not a permission; pattern: `sigil.key-introduce` ↔
	// `keeper.sigil.key.introduce`).
	"soul.ssh-target-update": {},

	// plugin.* — management of Sigil's plugin-integrity allow-list
	// (ADR-026, rbac.md → §Catalog of permissions → Plugin Sigil).
	"plugin.allow":  {},
	"plugin.revoke": {},
	"plugin.list":   {},

	// sigil.* — rotation of Sigil's SIGNING trust-anchor keys (ADR-026(h),
	// R3-S7). A separate resource from plugin.* (that one is about binary
	// allow-listing, this one about the keys that sign them). Action is
	// hyphenated (`key-introduce`) since the permission grammar is exactly
	// `<resource>.<action>` (the 3-segment `sigil.key.introduce` is an MCP
	// tool, not a permission); MCP tool keeper.sigil.key.<verb> ↔
	// permission sigil.key-<verb>.
	"sigil.key-introduce":   {},
	"sigil.key-retire":      {},
	"sigil.key-list":        {},
	"sigil.key-set-primary": {},

	// service.* — management of the `service_registry` Service registry
	// (ADR-028 RBAC-storage pattern, naming-rules.md → service_registry).
	"service.register":   {},
	"service.update":     {},
	"service.list":       {},
	"service.deregister": {},

	// omen.* / rite.* — operator-facing CRUD for the Augur registries
	// (omens / rites, ADR-025, rbac.md §Augur). resource is omen/rite (NOT
	// augur.*); 2-segment permission <resource>.<action> with verbs
	// create/list/delete. The Soul's live-fetch (AugurRequest) is NOT
	// gated by an RBAC permission — it's a machine gRPC request, not an
	// operator action (rbac.md §Augur).
	"omen.create": {},
	"omen.list":   {},
	"omen.delete": {},
	"rite.create": {},
	"rite.list":   {},
	"rite.delete": {},

	// vigil.* / decree.* — operator-facing CRUD for the Oracle registries
	// (vigils / decrees, ADR-030 beacons, rbac.md §Oracle). resource is
	// vigil/decree; 2-segment permission <resource>.<action> with verbs
	// create/list/delete (omen/rite pattern). The Reactor flow (Portent →
	// match Decree → enqueue) is NOT gated by an RBAC permission — it's a
	// machine, Soul-initiated path, not an operator action (security is
	// via Decree's subject binding, ADR-030(b)).
	"vigil.create":  {},
	"vigil.list":    {},
	"vigil.delete":  {},
	"decree.create": {},
	"decree.list":   {},
	"decree.delete": {},

	// push.*
	"push.apply":   {},
	"push.cleanup": {},
	// push.read — reads push-run state (`GET /v1/push/{apply_id}`, Variant
	// C orchestrator). Separate from push.apply: a read operation doesn't
	// need mutate rights (pattern: service.list / role.list — read without
	// audit, sometimes scoped to a dedicated role for observability
	// operators).
	"push.read": {},

	// push-provider.* — CRUD for the Push-Provider registry (per-provider
	// env-payload params for push-flow SSH plugins, ADR-032 amendment
	// 2026-05-26, S7-2). resource is `push-provider` (a single kebab
	// section with a hyphen — the correct form for a two-word area,
	// symmetric with the `ssh-target` precedent in an action). 5
	// permissions: create / update / delete / list / read. Selector —
	// NoSelector in the MVP: CRUD operates on the registry itself (like
	// provider.* / service.* / operator.*); per-name scope is a separate
	// future slice once multi-tenant RBAC lands.
	"push-provider.create": {},
	"push-provider.update": {},
	"push-provider.delete": {},
	"push-provider.list":   {},
	"push-provider.read":   {},

	// errand.* — pull ad-hoc exec of a single module (ADR-033, rbac.md
	// §Errand). Selectors — `host=<sid>` / `coven=<label>` (same as
	// soul.list / soul.issue-token); bare — unrestricted. errand.cancel is
	// slice E5 (the DELETE endpoint isn't implemented yet); the permission
	// is registered forward-only so role configs don't break once the
	// endpoint lands.
	"errand.run":    {},
	"errand.cancel": {},
	"errand.list":   {},

	// choir.* — operator-facing CRUD for host topology within an
	// incarnation (Choir/Voice, ADR-044, S-T3). A Choir belongs to an
	// incarnation, so it uses the same selector as incarnation.* —
	// `incarnation=` / `service=` / `coven=` (resolved by
	// [IncarnationScopeSelector] via path-{name}); bare — unrestricted.
	// resource is `choir`; actions — create / delete / list + add-voice /
	// remove-voice (Voice membership management). Voice actions are
	// hyphenated (`add-voice`/`remove-voice`) since the permission grammar
	// is exactly `<resource>.<action>` (pattern: soul.ssh-target-update /
	// sigil.key-introduce). Mutating CRUD is audited (choir.created /
	// choir.deleted / choir.voice_added / choir.voice_removed).
	"choir.create":       {},
	"choir.delete":       {},
	"choir.list":         {},
	"choir.add-voice":    {},
	"choir.remove-voice": {},

	// cadence.* — operator-facing CRUD for the Cadence-schedule registry
	// (`cadences`, ADR-046 §7). resource is `cadence`; actions — create /
	// list / update / delete + granular enable / disable (pattern:
	// omen/rite/vigil/decree). Selector — NoSelector in the MVP: CRUD
	// operates on the schedule registry itself (like push-provider.* /
	// operator.*); per-name scope is a separate future slice once
	// multi-tenant RBAC lands.
	// TWO-LEVEL guard (security-critical, ADR-046 §7): the `cadence.*`
	// right controls the schedule, but the recipe spawns a Voyage, so on
	// CREATE the creator must also hold the Voyage permission for the
	// recipe's `kind` (`incarnation.run` for scenario / `errand.run` for
	// command, ADR-043 §6) — otherwise Cadence would become a
	// privilege-escalation bypass of RBAC. The check lives inside
	// CadenceHandler.Create (kind is only visible from the body, Voyage
	// parity).
	//
	// cadence.enable / cadence.disable — granular rights to toggle a
	// schedule (`POST /v1/cadences/{id}/enable` / `.../disable`), split
	// from `cadence.update` (PATCH of the recipe), ADR-046 amendment
	// 2026-06-02. BACKCOMPAT: `cadence.update` remains a valid grant for
	// toggling too — roles with the old right don't lose enable/disable.
	// Routes use the OR gate [middleware.RequireAnyPermission]: enable
	// accepts `cadence.enable` OR `cadence.update`, disable accepts
	// `cadence.disable` OR `cadence.update`.
	"cadence.create":  {},
	"cadence.list":    {},
	"cadence.update":  {},
	"cadence.delete":  {},
	"cadence.enable":  {},
	"cadence.disable": {},

	// herald.* / tiding.* — operator-facing CRUD for the run-event
	// notification registries: Herald (delivery channels) / Tiding
	// (subscription rules) (ADR-052, S4). resource is `herald` / `tiding`;
	// actions — create / read / list / update / delete (pattern: omen.* /
	// push-provider.*). Selector — NoSelector: channel/rule management is
	// cluster-level (like role.* / synod.* / omen.*); per-name scope is a
	// separate future slice once multi-tenant RBAC lands. Mutating CRUD is
	// audited (herald.created/updated/deleted + tiding.*).
	"herald.create": {},
	"herald.read":   {},
	"herald.list":   {},
	"herald.update": {},
	"herald.delete": {},
	"tiding.create": {},
	"tiding.read":   {},
	"tiding.list":   {},
	"tiding.update": {},
	"tiding.delete": {},

	// provisioning.* — runtime management of the operator-creation-method
	// policy (`provisioning_allowed_methods` in keeper_settings, ADR-058
	// Part B). resource is `provisioning`; actions — read (`GET
	// /v1/provisioning-policy`) / update (`PUT /v1/provisioning-policy`).
	// Selector — NoSelector: the policy is cluster-level (like operator.*
	// / role.*). update is audited (`provisioning.policy_changed`), read
	// is not.
	"provisioning.read":   {},
	"provisioning.update": {},

	// audit.* — read-only access to `audit_log` (`GET /v1/audit`). Selector
	// — NoSelector in the MVP: filtering by archon_aid is done via a query
	// param, and per-AID/coven scope for the audit trail isn't introduced
	// yet. Reading audit events is NOT itself written to audit (avoids
	// recursion: otherwise every GET /v1/audit would double the table).
	"audit.read": {},

	// provider.* / profile.* — operator-facing CRUD for the Cloud-Provider
	// (`providers`) and Cloud-Profile (`profiles`) registries (ADR-017,
	// docs/keeper/cloud.md). resource is `provider` / `profile`; actions —
	// create / read / delete (push-provider.* pattern, no update:
	// Provider/Profile are immutable — changing params means delete+create,
	// protecting against partial mutation of a live VM spec). Selector —
	// NoSelector in the MVP: CRUD operates on the registry itself (like
	// push-provider.* / service.*); per-name scope is a separate future
	// slice once multi-tenant RBAC lands. Mutating CRUD is audited
	// (provider.created/deleted + profile.created/deleted). `read` gates
	// both list and get (like operator.list↔read: one right for both read
	// routes).
	"provider.create": {},
	"provider.read":   {},
	"provider.delete": {},
	"profile.create":  {},
	"profile.read":    {},
	"profile.delete":  {},
}

// deprecatedActionAliases — DEPRECATED permission names → their canonical
// form. Key/value is the full `<resource>.<action>` form. [ParsePermission]
// canonicalizes the key to the value on snapshot load, so
// [Permission.Matches] stays a plain string comparison and the router only
// mounts the canonical name. Both names remain valid in
// [AllowedPermissions] (closed enum, names are never removed): roles in
// keeper.yml / DB with the old name don't fail load and keep their access.
//
//   - `incarnation.update` → `incarnation.update-hosts` (PM-decision
//     2026-06-02): the old name covered only `PATCH /hosts`, narrowed to
//     make room for future update-covens/update-spec.
var deprecatedActionAliases = map[string]string{
	"incarnation.update": "incarnation.update-hosts",
}

// IsAllowedPermission checks a `<resource>.<action>` string against the
// catalog. For a wildcard permission (`<resource>.*`), it checks that at
// least one `<resource>.*`-name exists in the catalog (i.e. the resource is
// known).
func IsAllowedPermission(resource, action string) bool {
	if action == "*" {
		// `<resource>.*` is valid if the catalog has at least one
		// permission for that resource. A full wildcard `*` (no resource)
		// is validated separately in parsePermission.
		for name := range AllowedPermissions {
			if len(name) > len(resource)+1 &&
				name[:len(resource)] == resource && name[len(resource)] == '.' {
				return true
			}
		}
		return false
	}
	_, ok := AllowedPermissions[resource+"."+action]
	return ok
}

// allowedSelectorKeys — the closed enum of selector keys (rbac.md §
// Selector grammar). Extending it is a separate PR.
//
// `regex` (ADR-047 S2a) — an RE2 pattern over SID/host name, quoted form
// `regex='^web-.*'`. Unlike exact keys, matching is regexp.MatchString
// against the host/sid context ([Permission.Matches]).
//
// `soulprint` (ADR-047 S2b) — a CEL predicate over host facts
// (`soulprint.self.*`, ADR-018), quoted form
// `soulprint='soulprint.self.os.family == "debian"'`. Compilation is
// validated by shared/cel on load; real CEL eval against facts is slices
// S3/S4 ([Permission.Matches] for soulprint fail-closed in S2b: the
// map[string]string context carries no nested facts).
//
// `state` (ADR-047 S2c) — a CEL predicate over incarnation.state, quoted
// form `state='state.redis_version == "8.0"'`. Compilation is validated via
// keeper/internal/statepredicate (migration-sandbox root `state`) on load;
// real CEL eval against state is slice S3b ([Permission.Matches] for state
// fail-closed with no incarnation.state in the context).
//
// `trait` (ADR-047 amendment, ADR-060 item 7 slice 1) — an exact
// key:value match against `incarnation.traits` (operator-set key-value
// labels on an incarnation, jsonb). Form `trait=key:value` (exactly one
// `:`; both halves — [a-zA-Z0-9_.-]+, scalar-only). Unlike the CEL
// dimensions soulprint/state, this is exact equality (like coven), not a
// predicate: the Trait value is fail-closed in [Permission.Matches] (the
// current map[string]string context carries no nested
// incarnation.traits — the real match is done by the incarnation
// list/get resolver, slice 1 item 7). Slice 1 semantics — an OR dimension
// of Purview (an incarnation is visible if its traits[key]==value); AND
// narrowing across multiple pairs is a follow-up multi-key feature.
var allowedSelectorKeys = map[string]struct{}{
	"service":     {},
	"coven":       {},
	"incarnation": {},
	"host":        {},
	"regex":       {},
	"soulprint":   {},
	"state":       {},
	"trait":       {},
}

// IsAllowedSelectorKey checks a selector key against the closed enum.
func IsAllowedSelectorKey(key string) bool {
	_, ok := allowedSelectorKeys[key]
	return ok
}

// SelectorKeys returns a sorted list of allowed selector keys (closed enum
// [allowedSelectorKeys]). The MVP catalog has no per-permission metadata —
// this is a general list of allowed scope keys, applicable to permissions
// that support a selector. Used by the `GET /v1/permissions` catalog
// endpoint (selector_keys in the response is this general list).
func SelectorKeys() []string {
	keys := make([]string, 0, len(allowedSelectorKeys))
	for k := range allowedSelectorKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
