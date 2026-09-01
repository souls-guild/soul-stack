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
// §Catalog of permissions. 120 names (sum of the categories below):
//
//   - operator (5): create / revoke / issue-token / list / read;
//   - role (8): create / create-root / delete / list / list-all / update / grant-operator / revoke-operator;
//   - synod (9): create / update / delete / list / list-all / add-operator / remove-operator / grant-role / revoke-role (ADR-049; list-all — NIM-216);
//   - incarnation (14): create / rerun-last / run / get / list / history / unlock / upgrade / destroy / traits-set / view-secrets / bind-member / unbind-member (NIM-209) / label-set ([ADR-0085]);
//   - soul (8): list / create / issue-token / coven-assign / traits-assign / ssh-target-update / console (ADR-0074) / forget (NIM-386);
//   - plugin (3): allow / revoke / list;
//   - sigil (4): key-introduce / key-retire / key-list / key-set-primary;
//   - service (5): register / update / list / deregister / label-set ([ADR-0085]);
//   - omen (4): create / list / delete / label-set ([ADR-0085]);
//   - rite (3): create / list / delete;
//   - vigil (4): create / list / delete / label-set ([ADR-0085]);
//   - decree (4): create / list / delete / label-set ([ADR-0085]);
//   - push (3): apply / cleanup / read;
//   - push-provider (6): create / update / delete / list / read (ADR-032 amendment S7-2) / label-set ([ADR-0085]);
//   - errand (3): run / cancel / list (ADR-033);
//   - choir (5): create / delete / list / add-voice / remove-voice (ADR-044, S-T3);
//   - cadence (6): create / list / update / delete / enable / disable (ADR-046, S4; enable/disable — amendment 2026-06-02);
//   - herald (6): create / read / list / update / delete (ADR-052, S4) / label-set ([ADR-0085]);
//   - tiding (6): create / read / list / update / delete (ADR-052, S4) / label-set ([ADR-0085]);
//   - setting (3): read / update / delete ([ADR-0073] — the cluster settings store; this
//     line was missing while the total said 117, which is why the sum did not check out);
//   - provisioning (2): read / update (ADR-058 Part B — operator-creation-method policy);
//   - audit (1): read;
//   - provider (4): create / read / delete (ADR-017, Cloud CRUD) / label-set ([ADR-0085]);
//   - profile (4): create / read / delete (ADR-017, Cloud CRUD) / label-set ([ADR-0085]).
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
	// role.create-root — create a role with NO parent, i.e. privilege that
	// tracks nothing (rbac.md → § Root roles, NIM-201). `role.create` alone
	// admits only a DERIVED role, whose ceiling follows the parent; without
	// this right an operator cannot mint a snapshot that outlives the rights
	// it came from. NoSelector, same reasoning as role.list-all.
	"role.create-root": {},
	// role.list-all — see the WHOLE role catalog, not only the roles the
	// caller could grant (rbac.md → § Catalog visibility, NIM-203). A breadth
	// modifier on `role.list`, not a route of its own: `role.list` still gates
	// GET /v1/roles, this decides how much of it comes back. Mounted on no
	// endpoint (the pattern of `operator.read`). Scoping it is meaningless —
	// the grammar has no `role=` dimension — so only an UNRESTRICTED holder
	// gets the full catalog, which the subset check enforces by itself.
	"role.list-all": {},

	// synod.* — Synod group management (ADR-049): an intermediate level
	// Archon → Synod → Roles. 9 permissions. Selector — NoSelector (group
	// management is a cluster-level operation, no coven/host scope, same
	// as role.* / operator.*; ADR-049 does NOT introduce group-scope).
	// grant-role/add-operator are gated by the least-privilege subset,
	// delete/remove-operator/revoke-role by self-lockout (ADR-049(f)).
	// synod.update changes ONLY the description (cosmetic, grants/revokes
	// no rights) — no subset/self-lockout check; name (PK) is immutable.
	"synod.create": {},
	"synod.update": {},
	"synod.delete": {},
	"synod.list":   {},
	// synod.list-all — see the WHOLE group catalog, not only the groups the
	// caller could add someone to (rbac.md → § Synod catalog visibility,
	// NIM-216). The mirror of role.list-all: a breadth modifier on
	// `synod.list`, mounted on no endpoint, meaningless to scope (the grammar
	// has no `synod=` dimension). Without it an auditor holding the full role
	// catalog would still be blind to the groups those roles are bundled into.
	"synod.list-all":        {},
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
	"incarnation.rerun-last": {},
	"incarnation.run":        {},
	"incarnation.get":        {},
	"incarnation.list":       {},
	"incarnation.history":    {},
	"incarnation.unlock":     {},
	"incarnation.upgrade":    {},
	"incarnation.destroy":    {},
	// incarnation.traits-set — a wholesale replacement of an incarnation's
	// operator-set trait labels (`incarnation.traits` jsonb, ADR-060) via
	// `PUT /v1/incarnations/{name}/traits`. The labels describe the
	// incarnation and reach no member host (NIM-281), so this permission and
	// the per-HOST `soul.traits-assign` govern disjoint sets of labels —
	// neither can produce or overwrite the other's. Action is
	// hyphenated (`traits-set`) since the permission grammar is exactly
	// `<resource>.<action>` (pattern: soul.traits-assign). Same scope
	// selector incarnation/coven/service by path-{name} as the other
	// incarnation mutations.
	"incarnation.traits-set": {},
	// incarnation.bind-member / incarnation.unbind-member — the OPERATOR
	// path for incarnation membership (`POST /v1/incarnations/{name}/members`,
	// `DELETE .../members/{sid}`; ADR-008 amendment 2026-07-28, NIM-209).
	// Before them the only bind act was `core.soul.registered` INSIDE a
	// scenario run, so an already-onboarded host could not be put into an
	// incarnation from the outside at all — and a create scenario rolling
	// onto a ready roster was unreachable through the API.
	//
	// Split in two (the choir.add-voice / choir.remove-voice pattern):
	// unbinding is the destructive half — it drops the host out of the roster
	// of every FUTURE run of that incarnation — so it is grantable
	// separately from binding. Reading the roster needs no right of its own:
	// it rides on `incarnation.get`.
	//
	// Scope selector — the incarnation/coven/service triple by path-{name},
	// same as other incarnation mutations. That gate alone is NOT enough:
	// a scope predicate on `incarnation=` is satisfied without ever looking
	// at the host, so a holder of `incarnation.bind-member on
	// incarnation=X` would be able to pull ANY host into X and thereby reach
	// it with `incarnation.run`. The handler therefore applies a SECOND,
	// per-host gate — every target SID must be inside the caller's soul
	// visibility (`soul.list` purview, [soulpurview.InScope]) — all-or-
	// nothing, never a silent trim. See rbac.md § Incarnation membership.
	"incarnation.bind-member":   {},
	"incarnation.unbind-member": {},
	// incarnation.view-secrets — reveals the plaintext value of an
	// incarnation secret declared in the service's `state_schema` as a
	// field with `type: secret` ([ADR-0083] §2; the `revealable_secrets`
	// registry it replaced is gone): POST .../secrets/reveal + discovery
	// GET .../secrets/revealable. Strictly more privileged than
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
	// soul.traits-assign — bulk mutation of the trait labels attached to
	// HOSTS (jsonb column `souls.traits`, ADR-060) across a selector:
	// merge/replace/remove. Action is hyphenated (`traits-assign`) since
	// the permission grammar is exactly `<resource>.<action>` (pattern:
	// soul.coven-assign / soul.ssh-target-update). Same selector as
	// soul.coven-assign (`coven=` / `host=` / bare), and both of its gates:
	// target hosts ⊆ the operator's coven-scope (gate a), plus — for
	// merge/replace — every pair attached ⊆ its own trait-scope (gate b,
	// NIM-281). Gate (b) is what stops a holder from handing a host to a
	// foreign role by stamping its pair: a host-attached trait is the only
	// kind there is and it grants visibility, while `trait.<key>=v` is a
	// scope dimension (NIM-128). `remove` is ungated on the pair, as with
	// coven.
	"soul.traits-assign": {},
	// soul.ssh-target-update — changes per-host SSH credentials for the
	// push flow (ADR-032 amendment 2026-05-26, S7-1). Action is hyphenated
	// (`ssh-target-update`) since the permission grammar is exactly
	// `<resource>.<action>` (the 3-segment `soul.ssh-target.update` is an
	// MCP tool, not a permission; pattern: `sigil.key-introduce` ↔
	// `keeper.sigil.key.introduce`).
	"soul.ssh-target-update": {},
	// soul.console — opens an interactive PTY session on a host (WebSocket
	// `/v1/console`, ADR-0074). STRICTLY MORE PRIVILEGED than `errand.run`,
	// and neither right implies the other: an Errand is one named module call
	// with declared params, capped output and a fixed end, while a console is
	// an unbounded interactive shell running as the Soul daemon's user
	// (typically root) whose commands are not knowable in advance and cannot
	// be checked against a module allow-list. Selectors — `host=<sid>` /
	// `coven=<label>` (same as errand.run); bare — unrestricted. No selector
	// key of its own: scope intersection reuses the existing Purview
	// dimensions (ADR-047 §S4). The right is checked TWICE (rbac.md §Console):
	// NoSelector at the WebSocket upgrade (may this Archon open consoles at
	// all — refusal is 403 before a socket exists) and `host=<sid>` per `open`
	// frame, because the target SID arrives in the frame rather than the URL.
	"soul.console": {},
	// soul.forget — erases a host from the registry (`DELETE /v1/souls/{sid}`,
	// NIM-386): the `souls` row goes, and with it — through the four
	// ON DELETE CASCADE edges — its seeds, its unburnt bootstrap tokens, its
	// incarnation memberships and its Choir Voices. Since seed auth is an
	// ALLOWLIST over `soul_seeds.fingerprint`, losing the row is what makes
	// the host unable to reconnect; the revoke is done first so the count the
	// operator is shown is the number of credentials that were actually live.
	// Selector `host=<sid>` from the path, like soul.issue-token /
	// soul.ssh-target-update — the SID is known before the handler.
	// NOT gated on status: a host may be forgotten in any state, including one
	// with a live stream (which the call tears down) and one this cluster has
	// never heard from. There is no `force` flag on purpose — a single verb
	// cannot quietly degrade from "release the host" to "drop its row".
	"soul.forget": {},

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

	// setting.* — the Keeper runtime settings overlay (`cfg_*` rows of
	// keeper_settings, ADR-0073): read the catalog with effective values
	// (`GET /v1/settings`), override a key (`PUT /v1/settings/{key}`), drop
	// an override back to the file value (`DELETE /v1/settings/{key}`).
	// resource is `setting`; a family of its own rather than `service.*`,
	// even though both live in keeper_settings: editing a cluster-wide
	// tunable is a different privilege from registering a Service, and an
	// operator may be granted it without any other cluster-admin power.
	// Selector — NoSelector: settings are cluster-level (like provisioning.*
	// / role.*). Mutations are audited (`setting.updated` /
	// `setting.deleted`), read is not.
	"setting.read":   {},
	"setting.update": {},
	"setting.delete": {},

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

	// <resource>.label-set — replace the DISPLAY CAPTION of one registry row
	// ([ADR-0085], NIM-728). One name per registry rather than one shared name,
	// because the catalog grammar is `<resource>.<action>` and a role that may
	// caption incarnations has no business captioning heralds; the action spells
	// `<field>-<verb>` after the existing `incarnation.traits-set`, which grants
	// the same kind of thing — a mutable operator-set attribute on a row.
	//
	// Deliberately NOT `<resource>.update`. Half these registries have no update
	// at all (`provider`/`profile`/`omen`/`vigil`/`decree` are immutable by
	// decision, and `incarnation.update` was DELETED by migration 109 / NIM-330),
	// and where an update does exist it REPLACES the whole row — granting a
	// caption edit would have granted a rewrite of a Herald's secret_ref.
	//
	// A caption participates in nothing derived: no Vault path, no RBAC scope, no
	// snapshot directory, no CEL root. So this permission is the narrowest write
	// in the catalog — it can move a screen and nothing else.
	//
	// Audit: `<resource>.label_changed`. Selector: NoSelector for the registry
	// families that are already NoSelector; `incarnation.label-set` carries the
	// same scope as the other incarnation mutations.
	"incarnation.label-set":   {},
	"service.label-set":       {},
	"provider.label-set":      {},
	"profile.label-set":       {},
	"push-provider.label-set": {},
	"omen.label-set":          {},
	"herald.label-set":        {},
	"tiding.label-set":        {},
	"vigil.label-set":         {},
	"decree.label-set":        {},
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

// SelectorKeys returns the sorted list of allowed scope dimensions (NIM-128
// closed enum [scopeDims]: coven/service/incarnation/host/trait; the former
// regex/soulprint/state dimensions were removed). Used by the
// `GET /v1/permissions` catalog endpoint (selector_keys in the response).
func SelectorKeys() []string {
	keys := make([]string, 0, len(scopeDims))
	for k := range scopeDims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
