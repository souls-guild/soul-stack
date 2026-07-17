package mcp

import (
	"encoding/json"
)

// toolDeclaration — MCP-spec tool-object form returned by `tools/list`.
// Fields per docs/keeper/mcp-tools.md → § Tool declaration format.
// inputSchema/outputSchema are JSON Schema draft 2020-12, stored as
// json.RawMessage so static schemas stay constants instead of being
// re-marshaled on every list.
type toolDeclaration struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// toolStatus — implementation marker for a tool in the current slice.
// Used only by the tool handler to decide whether to call the real
// service or return a `not implemented` error (M0.7.a stubs for 13
// incarnation/soul/push/cloud tools).
type toolStatus int

const (
	toolStatusImplemented toolStatus = iota
	toolStatusStub
)

// toolEntry — catalog entry: declaration for tools/list plus the
// implementation flag for tools/call dispatch.
type toolEntry struct {
	decl   toolDeclaration
	status toolStatus
}

// catalogManifest — static tool manifest (current count enforced by
// TestCatalog_TotalCount so this comment can't go stale).
//
// Declarations stay 1:1 with docs/keeper/mcp-tools.md input/output
// schemas; mcp-tools.md is the source of truth on divergence.
//
// `description` is a short (1-2 sentence) explanation for the LLM agent of
// what the tool does. Full semantics, async shape, and error codes live in
// operator-api.md (cross-linked from mcp-tools.md).
var catalogManifest = []toolEntry{
	// --- Operator (3) — implemented in M0.7.a ---
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.create",
			Description:  "Creates a new Archon (Archon) in the operators registry and issues a JWT for it. The JWT is returned once - the client must save the token. Permission: operator.create.",
			InputSchema:  schemaOperatorCreateInput,
			OutputSchema: schemaOperatorCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.revoke",
			Description:  "Revokes an Archon (sets revoked_at=NOW). Active JWTs keep working until exp. Permission: operator.revoke. Fails with code=would-lock-out-cluster if target is the only active cluster-admin.",
			InputSchema:  schemaOperatorRevokeInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.operator.issue-token",
			Description:  "Issues a new JWT for an existing active Archon with current roles from keeper.yml. Permission: operator.issue-token.",
			InputSchema:  schemaOperatorIssueTokenInput,
			OutputSchema: schemaOperatorIssueTokenOutput,
		},
	},

	// --- Role (6) — RBAC CRUD, implemented in Slice 2b ---
	//
	// 1:1 with permission (Variant A): keeper.role.<action> ↔ role.<action>.
	// Business logic (builtin boundary, self-lockout) lives in rbac.Service;
	// tool is transport only. All tools dispatch only when RBACRoles is set
	// (optional HandlerDeps field); otherwise returns "role management not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.create",
			Description:  "Creates an RBAC role with a set of permissions. Permission: role.create. Fails with code=role-already-exists if name is taken, and validation-failed on a malformed name/permission.",
			InputSchema:  schemaRoleCreateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.delete",
			Description:  "Deletes an RBAC role (cascades permissions + membership). Permission: role.delete. Fails with code=role-builtin for a builtin role and would-lock-out-cluster if removal leaves the cluster without an admin.",
			InputSchema:  schemaRoleDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.list",
			Description:  "Lists RBAC roles with expanded permissions and assigned Archons (AID). Permission: role.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaRoleListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.update",
			Description:  "Replaces the role's set of permissions (replace semantics). Permission: role.update. Fails with code=role-builtin for a builtin role and would-lock-out-cluster when removing the last `*`.",
			InputSchema:  schemaRoleUpdateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.grant-operator",
			Description:  "Binds an Archon (AID) to a role. Idempotent. Permission: role.grant-operator. Fails with code=not-found if the role or AID doesn't exist.",
			InputSchema:  schemaRoleGrantOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.role.revoke-operator",
			Description:  "Removes an Archon's (AID) binding from a role. Permission: role.revoke-operator. Fails with code=would-lock-out-cluster if the last admin with `*` is being removed.",
			InputSchema:  schemaRoleRevokeOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Synod (8) — ADR-049 ---
	//
	// Archon groups (Archon → Synod → Roles). 1:1 keeper.synod.<action> ↔
	// permission synod.<action>. Business logic (builtin boundary,
	// least-privilege subset on add-operator/grant-role, self-lockout on
	// delete/remove-operator/revoke-role) lives in rbac.Service; tool is
	// transport only. Dispatches only when RBACRoles is set.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.create",
			Description:  "Creates a Synod group (bundles roles for a set of archons). Permission: synod.create. Fails with code=synod-already-exists if name is taken, and validation-failed on a malformed name.",
			InputSchema:  schemaSynodCreateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.update",
			Description:  "Changes ONLY the Synod group's description (name (PK) immutable). Permission: synod.update. builtin ALLOWED (description is cosmetic, no subset/self-lockout). Fails with code=synod-not-found if the group doesn't exist.",
			InputSchema:  schemaSynodUpdateInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.delete",
			Description:  "Deletes a Synod group (cascades membership + bundle). Permission: synod.delete. Fails with code=synod-builtin for a builtin group and would-lock-out-cluster if the group's disappearance leaves the cluster without an admin with `*`.",
			InputSchema:  schemaSynodDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.list",
			Description:  "Lists Synod groups with expanded roles (bundle) and members (AID). Permission: synod.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaSynodListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.add-operator",
			Description:  "Adds an archon (AID) to a Synod group. Idempotent. Permission: synod.add-operator. Under the least-privilege subset: fails with code=forbidden if the initiator doesn't hold the group's bundle rights. Fails with code=not-found for a nonexistent group/AID.",
			InputSchema:  schemaSynodAddOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.remove-operator",
			Description:  "Removes an archon (AID) from a Synod group. Permission: synod.remove-operator. Fails with code=would-lock-out-cluster if removal orphans the last admin with `*`.",
			InputSchema:  schemaSynodRemoveOperatorInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.grant-role",
			Description:  "Adds a role to a Synod group's bundle (grants it to all members). Idempotent. Permission: synod.grant-role. Under the least-privilege subset: fails with code=forbidden if the initiator doesn't hold the role's rights. Fails with code=not-found for a nonexistent group/role.",
			InputSchema:  schemaSynodGrantRoleInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.synod.revoke-role",
			Description:  "Removes a role from a Synod group's bundle (from all members). Permission: synod.revoke-role. Fails with code=would-lock-out-cluster if removal orphans the last admin with `*`.",
			InputSchema:  schemaSynodRevokeRoleInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Incarnation (11) ---
	//
	// All 11 tools (create/run/get/list/history/unlock/rerun-last/upgrade/
	// destroy/check-drift/traits-set) implemented: dispatch branches wired,
	// bodies at parity with REST IncarnationHandler. destroy wired in S-D4
	// (DELETE /v1/incarnations/{name}). check-drift wired in ADR-031 Slice B
	// (Scry on-demand pilot). traits-set — ADR-060 amend R1 (PUT
	// /v1/incarnations/{name}/traits, relocated from per-soul).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.create",
			Description:  "Creates a new Incarnation: runs the 'create' scenario of the given Service. Async operation - returns _apply_id. Permission: incarnation.create.",
			InputSchema:  schemaIncarnationCreateInput,
			OutputSchema: schemaApplyIDOutputWithIncarnation,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.run",
			Description:  "Runs an arbitrary scenario against an existing Incarnation. Async operation - returns _apply_id. Permission: incarnation.run.",
			InputSchema:  schemaIncarnationRunInput,
			OutputSchema: schemaIncarnationRunOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.get",
			Description:  "Reads spec + state + status of an Incarnation by name. Permission: incarnation.get.",
			InputSchema:  schemaIncarnationGetInput,
			OutputSchema: schemaIncarnationGetOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.list",
			Description:  "Lists Incarnations with filtering by service/status and pagination. Permission: incarnation.list.",
			InputSchema:  schemaIncarnationListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.history",
			Description:  "Returns the state_history log for an Incarnation with pagination. Used to poll async operations (_apply_id appears in history after a successful commit). Permission: incarnation.history.",
			InputSchema:  schemaIncarnationHistoryInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.unlock",
			Description:  "Clears the error_locked state on an Incarnation. Permission: incarnation.unlock.",
			InputSchema:  schemaIncarnationUnlockInput,
			OutputSchema: schemaIncarnationUnlockOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.rerun-last",
			Description:  "Clears error_locked and, in the same action, reruns the LAST failed incarnation scenario (bootstrap 'create'/... or day-2 add_user/...) with the saved input of the failed run. Only from error_locked. Async operation - returns _apply_id + scenario. Permission: incarnation.rerun-last.",
			InputSchema:  schemaIncarnationRerunLastInput,
			OutputSchema: schemaIncarnationRerunLastOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.upgrade",
			Description:  "Moves an Incarnation to a new state_schema_version + changes service_version. Async operation - returns _apply_id. Permission: incarnation.upgrade.",
			InputSchema:  schemaIncarnationUpgradeInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.destroy",
			Description:  "Tears down an Incarnation. allow_destroy=false - destroy via the 'destroy' teardown scenario; allow_destroy=true - teardown-free destroy (force). Async operation - returns _apply_id. Permission: incarnation.destroy.",
			InputSchema:  schemaIncarnationDestroyInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.check-drift",
			Description:  "Scry drift check (ADR-031): Keeper renders the 'converge' scenario and sends ApplyRequest{dry_run:true} to all hosts (Soul calls mod.Plan instead of mod.Apply), collects per-host per-task changed and returns a DriftReport. Sync. input - optional override of converge parameters; auto-from-state by naming convention. Permission: incarnation.check-drift. Fails with code=validation-failed if converge is missing from the service snapshot or drift-input doesn't resolve.",
			InputSchema:  schemaIncarnationCheckDriftInput,
			OutputSchema: schemaIncarnationCheckDriftOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.incarnation.traits-set",
			Description:  "Wholesale REPLACES the incarnation's operator-set trait labels (incarnation.traits jsonb - source of truth, ADR-060) and projects them onto member hosts' souls.traits. 'traits' - full key->(scalar|list of scalars) set; empty/omitted = clear labels. Replaces per-soul keeper.soul.traits-assign (deprecated). Permission: incarnation.traits-set (scope incarnation/coven/service by name). Fails with code=validation-failed on a malformed key / nested value; not-found if the incarnation doesn't exist.",
			InputSchema:  schemaIncarnationTraitsSetInput,
			OutputSchema: schemaIncarnationTraitsSetOutput,
		},
	},

	// --- Soul (6) — create + issue-token + coven-assign + traits-assign +
	// ssh-target.update implemented (parity with REST POST /v1/souls +
	// issue-token + coven + traits + ssh-target); list stays a stub (awaits M2). ---
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.create",
			Description:  "Registers a Soul in the souls registry (status: pending) and, for transport=agent, issues the first bootstrap token (souls-row + token atomically). The token is returned once - the client must save it. Permission: soul.create.",
			InputSchema:  schemaSoulCreateInput,
			OutputSchema: schemaSoulCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.issue-token",
			Description:  "Reissues a bootstrap token for an existing Soul (transport=agent). The token is returned once. force=true expires the active token and issues a new one. Permission: soul.issue-token. Fails with code=bootstrap-token-active if an active token exists and force isn't passed.",
			InputSchema:  schemaSoulIssueTokenInput,
			OutputSchema: schemaSoulIssueTokenOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.coven-assign",
			Description:  "Bulk adds (mode=append) / removes (mode=remove) ONE Coven label, or REPLACES (mode=replace) the set of Coven labels on hosts under the selector (all/sids/coven/incarnation/status) ∩ operator coven-scope. append/remove requires a 'label' field, replace requires 'labels[]' (may be empty = remove all). Coven is a cold PG label. Permission: soul.coven-assign. dry_run=true returns matched without UPDATE. Fails with code=validation-failed on an empty selector or on a label/any label of the set outside the operator's coven-scope.",
			InputSchema:  schemaSoulCovenAssignInput,
			OutputSchema: schemaSoulCovenAssignOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.traits-assign",
			Description:  "DEPRECATED (ADR-060): use keeper.incarnation.traits-set (incarnation.traits - source of truth, projected onto souls.traits). Per-soul write gets overwritten by the projection. Bulk-assigns operator-set trait labels (souls.traits jsonb) on hosts under the selector (all/sids/coven/incarnation/status) ∩ operator coven-scope. mode=merge (default) set/overwrite keys from 'traits' (keep the rest); mode=replace replace the whole map ('traits', empty = clear); mode=remove delete keys from 'keys'. A trait value is a scalar (string/number/bool) or list of scalars (nested objects/arrays are forbidden). Permission: soul.traits-assign. dry_run=true returns matched without UPDATE. trait key is NOT a scope dimension: least-privilege is held by coven-scope (target hosts subseteq scope). Fails with code=validation-failed on an empty selector / malformed key / nested value.",
			InputSchema:  schemaSoulTraitsAssignInput,
			OutputSchema: schemaSoulTraitsAssignOutput,
		},
	},
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.soul.list",
			Description:  "Lists Souls with filtering by coven/status/transport and pagination. Permission: soul.list.",
			InputSchema:  schemaSoulListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.ssh-target.update",
			Description:  "Updates per-host SSH credentials for the push flow (souls.ssh_target jsonb, ADR-032 amendment 2026-05-26, S7-1): ssh_port/ssh_user/soul_path. Source-of-truth for PGFallbackTargetResolver; keeper.yml::push.targets[] - legacy fallback under the push.allow_legacy_push_targets flag. Permission: soul.ssh-target-update; selector host=<sid>. Fails with code=not-found if the SID isn't in the souls registry.",
			InputSchema:  schemaSoulSshTargetUpdateInput,
			OutputSchema: schemaSoulSshTargetUpdateOutput,
		},
	},

	// --- Plugin (3) — Sigil allow-list, implemented in S4b ---
	//
	// 1:1 with permission (keeper.plugin.<action> ↔ plugin.<action>) and
	// REST POST/GET/DELETE /v1/plugins/sigils*. Business logic (cache-slot
	// read, signing, registry CRUD) lives in sigil.Service; tool is
	// transport only. All three dispatch only when SigilSvc is set (optional
	// HandlerDeps field); otherwise returns "sigil is not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.allow",
			Description:  "Allows a plugin (namespace, name) under an operator-asserted ref label in the plugin_sigils allow-list: Keeper reads the current binary from the single-slot cache, computes sha256, signs it and inserts the record. Permission: plugin.allow. Fails with code=plugin-not-in-cache if the plugin isn't in the cache, and sigil-already-active if an active allowance for (ns, name, ref) already exists.",
			InputSchema:  schemaPluginAllowInput,
			OutputSchema: schemaPluginAllowOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.revoke",
			Description:  "Revokes an active allowance (namespace, name, ref) from the plugin_sigils allow-list (the binary stops passing Sigil verification). Permission: plugin.revoke. Fails with code=sigil-not-found if no active record exists.",
			InputSchema:  schemaPluginRevokeInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.plugin.list",
			Description:  "Lists active plugin_sigils allow-list entries (without signature/manifest), newest first. Permission: plugin.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaPluginListOutput,
		},
	},

	// --- Sigil-key (4) — Sigil signing-key rotation, implemented in R3-S7 ---
	//
	// keeper.sigil.key.<verb> ↔ permission sigil.key-<verb> ↔ REST
	// /v1/sigil/keys*. Business logic (key-gen + Vault write + registry CRUD
	// + publish anchors-changed) lives in sigil.KeyService; tool is
	// transport only. All four dispatch only when SigilKeySvc is set
	// (optional HandlerDeps field); otherwise returns "sigil is not
	// configured". SECURITY: the private key is NEVER included in output.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.introduce",
			Description:  "Introduces a new Sigil signing trust-anchor key: Keeper generates an ed25519 pair, writes the private key to Vault KV and inserts the public part (SPKI) into the sigil_signing_keys registry as active. Returns key_id + pubkey_pem (NOT the private key). make_primary=true makes the key primary (new Sigils are signed with it). Permission: sigil.key-introduce. After introduction the cluster reloads the anchor set (anchors-changed). Fails with code=sigil-key-concurrent-change on a primary race.",
			InputSchema:  schemaSigilKeyIntroduceInput,
			OutputSchema: schemaSigilKeyIntroduceOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.list",
			Description:  "Lists active Sigil signing trust-anchor keys (primary first). Without vault_ref. Permission: sigil.key-list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaSigilKeyListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.set-primary",
			Description:  "Makes an active key primary (new Sigils are signed with it after cluster reload). Permission: sigil.key-set-primary. Fails with code=sigil-key-not-found if the key doesn't exist, and sigil-key-concurrent-change on a primary race or if the key is retired.",
			InputSchema:  schemaSigilKeyIDInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.sigil.key.retire",
			Description:  "Retires a signing trust-anchor key from the set (Soul forgets it on the next SigilTrustAnchors). Permission: sigil.key-retire. Fails with code=sigil-key-not-found (no active record), sigil-key-last-active (last active) and sigil-key-primary (primary directly - set-primary another key first).",
			InputSchema:  schemaSigilKeyIDInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Service (4) — Service registry, implemented in S3 ---
	//
	// 1:1 with permission (keeper.service.<action> ↔ service.<action>) and
	// REST POST/GET/PATCH/DELETE /v1/services*. Business logic (name/git/ref/
	// refresh validation, invalidate hook) lives in serviceregistry.Service;
	// tool is transport only. All four dispatch only when ServiceSvc is set
	// (optional HandlerDeps field); otherwise returns "service registry is
	// not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.register",
			Description:  "Registers a Service in the service_registry (git source of the service repo + ref + opt. auto-refresh). Permission: service.register. Fails with code=service-already-exists if name is taken, validation-failed on a malformed name/git/ref/refresh, and not-found if the creator's AID is missing from the operators registry.",
			InputSchema:  schemaServiceRegisterInput,
			OutputSchema: schemaServiceView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.update",
			Description:  "Replaces the mutable fields of a Service record (git/ref/refresh, replace semantics); name - the key, unchanged. Permission: service.update. Fails with code=not-found if the record doesn't exist, and validation-failed on a malformed git/ref/refresh.",
			InputSchema:  schemaServiceUpdateInput,
			OutputSchema: schemaServiceView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.list",
			Description:  "Lists registered Services (sort name ASC). Permission: service.list.",
			InputSchema:  schemaEmptyObject,
			OutputSchema: schemaServiceListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.service.deregister",
			Description:  "Deletes a Service record from service_registry by name. Permission: service.deregister. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaServiceDeregisterInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Augur (6) — Omen/Rite registries, implemented (ADR-025) ---
	//
	// 4-segment tool-name keeper.augur.<resource>.<action> ↔ 2-segment
	// permission <resource>.<action> (omen.create / rite.list / …). Business
	// logic (name/source_type/auth_ref validation, XOR subject, allow shape,
	// token fields) lives in augur.Service; tool is transport only. All six
	// dispatch only when AugurSvc is set (optional HandlerDeps field);
	// otherwise returns "augur registry is not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.create",
			Description:  "Creates an Omen in the omens registry (external Augur system: vault / prometheus / elk + endpoint + auth_ref - vault-ref to the master credential, not the secret itself). Permission: omen.create. Fails with code=omen-already-exists if name is taken, validation-failed on a malformed name/source_type/endpoint/auth_ref.",
			InputSchema:  schemaOmenCreateInput,
			OutputSchema: schemaOmenView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.list",
			Description:  "Lists registry Omens (sort created_at DESC, name ASC; opt. offset/limit). Permission: omen.list.",
			InputSchema:  schemaOmenListInput,
			OutputSchema: schemaOmenListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.omen.delete",
			Description:  "Deletes an Omen by name; cascades to remove related Rites (ON DELETE CASCADE). Permission: omen.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaOmenDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.create",
			Description:  "Creates a Rite (grant) in the rites registry: subject (coven XOR sid) x omen -> allow-list + delegate + opt. token_ttl/token_num_uses (vault-delegate only). Permission: rite.create. Fails with code=not-found if the Omen doesn't exist, and validation-failed on an XOR violation / malformed allow / token fields.",
			InputSchema:  schemaRiteCreateInput,
			OutputSchema: schemaRiteView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.list",
			Description:  "Lists Rites for a single Omen (omen filter required; sort created_at DESC, id ASC). Permission: rite.list.",
			InputSchema:  schemaRiteListInput,
			OutputSchema: schemaRiteListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.augur.rite.delete",
			Description:  "Deletes a Rite by surrogate id. Permission: rite.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaRiteDeleteInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Oracle (6) — Vigil/Decree registries, implemented (ADR-030 beacons) ---
	//
	// 4-segment tool-name keeper.oracle.<resource>.<action> ↔ 2-segment
	// permission <resource>.<action> (vigil.create / decree.list / …).
	// Business logic (name/interval/check/subject validation for Vigil;
	// name/on_beacon/incarnation/scenario/subject/where-CEL for Decree) lives
	// in oracle.Service; tool is transport only. All six dispatch only when
	// OracleSvc is set (optional HandlerDeps field); otherwise returns
	// "oracle registry is not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.create",
			Description:  "Creates a Vigil in the vigils registry (Soul-side beacon check: check - core.beacon address + interval + subject coven XOR sid). Read-only by construction (observes, doesn't mutate the host). Permission: vigil.create. Fails with code=vigil-already-exists if name is taken, validation-failed on a malformed name/interval/check/subject.",
			InputSchema:  schemaVigilCreateInput,
			OutputSchema: schemaVigilView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.list",
			Description:  "Lists registry Vigils (sort created_at DESC, name ASC; opt. offset/limit). Permission: vigil.list.",
			InputSchema:  schemaOraclePaginatedInput,
			OutputSchema: schemaVigilListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.vigil.delete",
			Description:  "Deletes a Vigil by name (stops being distributed to hosts in VigilSnapshot; Decrees are NOT cascaded). Permission: vigil.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaOracleNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.create",
			Description:  "Creates a Decree (reactor rule) in the decrees registry: on_beacon (Vigil) x subject (coven XOR sid) x incarnation_name -> action_scenario (named, whitelist) + opt. where-CEL predicate over event.data + cooldown. Default-deny. Permission: decree.create. Fails with code=decree-already-exists if name is taken, validation-failed on a malformed name/on_beacon/incarnation_name/action_scenario/subject/where-CEL/cooldown.",
			InputSchema:  schemaDecreeCreateInput,
			OutputSchema: schemaDecreeView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.list",
			Description:  "Lists registry Decrees (sort created_at DESC, name ASC; opt. offset/limit). Permission: decree.list.",
			InputSchema:  schemaOraclePaginatedInput,
			OutputSchema: schemaDecreeListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.oracle.decree.delete",
			Description:  "Deletes a Decree by name; cascades to clear cooldown state (oracle_fires, ON DELETE CASCADE). Permission: decree.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaOracleNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Errand (4) — pull-ad-hoc exec ADR-033, slice E4 + cancel slice E5 ---
	//
	// 1:1 with REST POST /v1/souls/{sid}/exec + GET
	// /v1/errands{,/{errand_id}} + DELETE /v1/errands/{errand_id} and
	// permission (errand.run — selector host=<sid>; errand.list —
	// NoSelector; errand.cancel — NoSelector). Business logic (validate,
	// INSERT, send/wait, mask+cap, async escalation, cancel signal, audit)
	// lives in errand.Dispatcher/Store; tool is transport only. Dispatches
	// only when ErrandDispatcher/ErrandStore are set (optional HandlerDeps
	// fields); otherwise returns "errand orchestrator is not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.soul.errand.run",
			Description:  "Runs a single module on a Soul over the mTLS EventStream (pull-ad-hoc exec, ADR-033). Returns a sync result (terminal status) or async=true with status=running if the server-cap is exceeded - then poll keeper.errand.get. A module whitelist and stdout/stderr cap (64 KiB) are applied by the Soul-side errand-runner. Permission: errand.run; selector host=<sid>. Fails with code=not-found if the Soul isn't connected to the cluster; validation-failed on an empty sid/module and timeout_seconds outside [1,300].",
			InputSchema:  schemaErrandRunInput,
			OutputSchema: schemaErrandRunOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.list",
			Description:  "Lists Errands with sid/status/started_after filters and pagination (sort started_at DESC). Read-only. Permission: errand.list.",
			InputSchema:  schemaErrandListInput,
			OutputSchema: schemaErrandListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.get",
			Description:  "Reads the current state of an Errand by ULID. For a running row returns status=running without stdout/exit_code (poll). Permission: errand.list. Fails with code=not-found if errand_id doesn't exist.",
			InputSchema:  schemaErrandGetInput,
			OutputSchema: schemaErrandRow,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.errand.cancel",
			Description:  "Cancels an in-flight Errand (slice E5, ADR-033). Best-effort: Keeper sends CancelErrand to the Soul, the Soul-side errandrunner cancels ctx -> returns ErrandResult{CANCELLED}. 204-equivalent on success. Permission: errand.cancel. Fails with code=not-found (errand_id doesn't exist / Soul not connected) or code=errand-not-cancellable (Errand already in a terminal status).",
			InputSchema:  schemaErrandCancelInput,
			OutputSchema: schemaErrandCancelOutput,
		},
	},

	// --- Voyage (4) — unified batch run (ADR-043, S5). Dispatches only when
	// Voyage deps are set (VoyageDB + resolvers); otherwise returns "voyage
	// orchestrator is not configured". RBAC-by-kind (scenario→incarnation.run
	// / command→errand.run) is done by the handler itself.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.start",
			Description:  "Creates a Voyage - a unified batch run (ADR-043). kind=scenario: apply a named scenario to a set of INCARNATIONS (target incarnations[] union service/coven; per-incarnation state-commit, B1). kind=command: run a whitelisted module on a set of HOSTS (target sids/coven/where, AND-merge; state isn't touched). Batch (Leg) = N units (batch_size); on_failure abort|continue; schedule_at -> delayed start. Async: 202 + voyage_id; progress - polling keeper.voyage.get. RBAC-by-kind (security-critical): scenario->incarnation.run, command->errand.run.",
			InputSchema:  schemaVoyageStartInput,
			OutputSchema: schemaVoyageStartOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.get",
			Description:  "Reads a Voyage snapshot by ULID (detail + summary). Permission: incarnation.history. Fails with code=not-found if voyage_id doesn't exist.",
			InputSchema:  schemaVoyageGetInput,
			OutputSchema: schemaVoyageView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.list",
			Description:  "Lists Voyage runs with kind/status filters (multi-value) and pagination (sort created_at DESC). Permission: incarnation.history.",
			InputSchema:  schemaVoyageListInput,
			OutputSchema: schemaVoyageListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.voyage.cancel",
			Description:  "Cancels a Voyage (pending/scheduled -> cancelled, ADR-043 S5). Running-abort - post-MVP (code=errand-not-cancellable). RBAC-by-kind same as start. Fails with code=not-found if voyage_id doesn't exist, or code=errand-not-cancellable if the Voyage is running/terminal.",
			InputSchema:  schemaVoyageGetInput,
			OutputSchema: schemaVoyageCancelOutput,
		},
	},

	// --- Push (2) — keeper.push.apply implemented (Variant C orchestrator,
	// docs/keeper/push.md); keeper.push.cleanup stays a stub (separate slice).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push.apply",
			Description:  "Push run of Destiny over SSH to a host inventory (Variant C orchestrator). Async operation - returns _apply_id. Permission: push.apply.",
			InputSchema:  schemaPushApplyInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},
	{
		status: toolStatusStub,
		decl: toolDeclaration{
			Name:         "keeper.push.cleanup",
			Description:  "Cleans up /var/lib/soul-stack/ on inventory hosts. Async operation - returns _apply_id. Permission: push.cleanup.",
			InputSchema:  schemaPushCleanupInput,
			OutputSchema: schemaApplyIDOutput,
		},
	},

	// --- Cloud Provider (4) — operator-facing CRUD for providers registry (ADR-017) ---
	//
	// keeper.provider.<verb> ↔ permission provider.<verb> ↔ REST
	// POST/GET/DELETE /v1/providers*. Business logic lives in
	// provider.Service; tool is transport only. Dispatches only when
	// ProviderSvc is set; otherwise "provider registry is not configured".
	// NO update (Provider is immutable). credentials_ref is returned as a
	// vault path; the secret is never resolved.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.provider.create",
			Description:  "Creates a Cloud Provider in the providers registry (ADR-017). credentials_ref - a vault reference (vault:<path>), the secret itself is NOT resolved. Permission: provider.create. 409 - name taken.",
			InputSchema:  schemaProviderCreateInput,
			OutputSchema: schemaProviderCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.provider.read",
			Description:  "Reads a single Cloud Provider by name. Permission: provider.read. credentials_ref - a path, the secret isn't resolved. code=not-found if the record doesn't exist.",
			InputSchema:  schemaProviderByNameInput,
			OutputSchema: schemaProviderCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.provider.list",
			Description:  "Lists Cloud Providers (sort created_at DESC). Permission: provider.read.",
			InputSchema:  schemaProviderListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.provider.delete",
			Description:  "Deletes a Cloud Provider. Permission: provider.delete. code=not-found if the record doesn't exist; code=provider-has-profiles if Profiles reference it (FK RESTRICT).",
			InputSchema:  schemaProviderByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Cloud Profile (4) — operator-facing CRUD for profiles registry (ADR-017) ---
	//
	// keeper.profile.<verb> ↔ permission profile.<verb> ↔ REST
	// POST/GET/DELETE /v1/profiles*. NO update. Param VALUES are never
	// written to audit (keys only).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.profile.create",
			Description:  "Creates a Cloud Profile (VM spec on top of a Provider) in the profiles registry (ADR-017). Permission: profile.create. 409 - name taken; validation-failed - provider doesn't exist.",
			InputSchema:  schemaProfileCreateInput,
			OutputSchema: schemaProfileCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.profile.read",
			Description:  "Reads a single Cloud Profile by name. Permission: profile.read. code=not-found if the record doesn't exist.",
			InputSchema:  schemaProfileByNameInput,
			OutputSchema: schemaProfileCreateOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.profile.list",
			Description:  "Lists Cloud Profiles (sort created_at DESC) with an opt. provider filter. Permission: profile.read.",
			InputSchema:  schemaProfileListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.profile.delete",
			Description:  "Deletes a Cloud Profile. Permission: profile.delete. code=not-found if the record doesn't exist.",
			InputSchema:  schemaProfileByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},

	// --- Push-Provider (5) — registry CRUD, implemented in S7-2 ---
	//
	// keeper.push-provider.<verb> ↔ permission push-provider.<verb> ↔ REST
	// POST/GET/PUT/DELETE /v1/push-providers*. Business logic (sensitive
	// params validated as vault-refs, Redis invalidate-publish) lives in
	// pushprovider.Service; tool is transport only. All five dispatch only
	// when PushProviderSvc is set (optional HandlerDeps field); otherwise
	// returns internal-error "push-provider registry is not configured"
	// (same pattern as ServiceSvc/AugurSvc).
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.create",
			Description:  "Creates a Push-Provider in the push_providers registry (per-provider env-payload params for the SSH push-flow plugin, ADR-032 amendment 2026-05-26, S7-2). Sensitive params (secret_id/token/password/private_key) MUST be vault-refs (vault:<path>). After commit, a cluster-wide invalidate via Redis pub/sub push-providers:changed -> SshDispatcher re-spawns the plugin on the nearest RPC. Permission: push-provider.create.",
			InputSchema:  schemaPushProviderCreateInput,
			OutputSchema: schemaPushProviderView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.update",
			Description:  "Replaces a Push-Provider's params (replace semantics; name - the key, unchanged). Same sensitive invariant. Permission: push-provider.update. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaPushProviderUpdateInput,
			OutputSchema: schemaPushProviderView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.delete",
			Description:  "Deletes a Push-Provider record. Permission: push-provider.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaPushProviderByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.list",
			Description:  "Lists Push-Providers (sort updated_at DESC). Permission: push-provider.list.",
			InputSchema:  schemaPushProviderListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.push-provider.read",
			Description:  "Reads a single Push-Provider record by name. Permission: push-provider.read.",
			InputSchema:  schemaPushProviderByNameInput,
			OutputSchema: schemaPushProviderView,
		},
	},

	// --- Herald (5) — notification-channel registry CRUD (ADR-052, S4) ---
	//
	// keeper.herald.<verb> ↔ permission herald.<verb> ↔ REST
	// POST/GET/PUT/DELETE /v1/heralds*. Business logic (config/secret_ref
	// validation + SSRF guard + dispatcher-cache invalidation) lives in
	// herald.Service; tool is transport only. All five dispatch only when
	// HeraldSvc is set (optional HandlerDeps field); otherwise returns
	// internal-error "herald registry is not configured".
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.create",
			Description:  "Creates a Herald notification-delivery channel (ADR-052; webhook in MVP: config.url + opt. headers; SSRF perimeter https-only + deny private IPs armed by default, lifted via config.http_allowed/allow_private). secret_ref (opt.) - a vault-ref to the signing token (webhook signature X-SoulStack-Signature: sha256=<hex>). Permission: herald.create. Fails with code=herald-already-exists if name is taken, validation-failed on a malformed name/type/config/secret_ref.",
			InputSchema:  schemaHeraldCreateInput,
			OutputSchema: schemaHeraldView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.update",
			Description:  "Replaces a Herald channel's mutable fields (replace semantics; name - the key, unchanged). Same SSRF invariant as create. Permission: herald.update. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaHeraldUpdateInput,
			OutputSchema: schemaHeraldView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.delete",
			Description:  "Deletes a Herald channel; cascades to tear down related Tiding subscriptions (ON DELETE CASCADE). Permission: herald.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaHeraldByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.list",
			Description:  "Lists Herald channels (sort updated_at DESC, name ASC; opt. offset/limit). Permission: herald.list.",
			InputSchema:  schemaHeraldListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.herald.read",
			Description:  "Reads a single Herald channel by name. Permission: herald.read.",
			InputSchema:  schemaHeraldByNameInput,
			OutputSchema: schemaHeraldView,
		},
	},

	// --- Tiding (5) — subscription-rule registry CRUD (ADR-052, S4) ---
	//
	// keeper.tiding.<verb> ↔ permission tiding.<verb> ↔ REST
	// POST/GET/PUT/DELETE /v1/tidings*. event_types is an area-glob within
	// the run scope; herald is an FK to an existing Herald. Same HeraldSvc /
	// nil-guard as herald-tools.
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.create",
			Description:  "Creates a Tiding subscription rule (ADR-052): which event_types (area-glob scenario_run.* within run scope: scenario_run/command_run/voyage/cadence + incarnation.drift_checked) to react to -> which Herald to deliver via. Filters only_failures/only_changes, opt. incarnation/cadence selectors. Permission: tiding.create. Fails with code=tiding-already-exists (name taken), not-found (herald doesn't exist), validation-failed (malformed name/event_types).",
			InputSchema:  schemaTidingCreateInput,
			OutputSchema: schemaTidingView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.update",
			Description:  "Replaces a Tiding rule's mutable fields (replace semantics; name - the key). Permission: tiding.update. Fails with code=not-found if the rule doesn't exist or the FK herald doesn't exist.",
			InputSchema:  schemaTidingUpdateInput,
			OutputSchema: schemaTidingView,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.delete",
			Description:  "Deletes a Tiding rule by name. Permission: tiding.delete. Fails with code=not-found if the record doesn't exist.",
			InputSchema:  schemaTidingByNameInput,
			OutputSchema: schemaEmptyObject,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.list",
			Description:  "Lists Tiding rules (sort updated_at DESC, name ASC; opt. offset/limit). By default hides one-off (ephemeral) rules; include_ephemeral=true returns all. Permission: tiding.list.",
			InputSchema:  schemaTidingListInput,
			OutputSchema: schemaPaginatedListOutput,
		},
	},
	{
		status: toolStatusImplemented,
		decl: toolDeclaration{
			Name:         "keeper.tiding.read",
			Description:  "Reads a single Tiding rule by name. Permission: tiding.read.",
			InputSchema:  schemaTidingByNameInput,
			OutputSchema: schemaTidingView,
		},
	},
}

// toolByName — linear catalog lookup. Returns ok=false if name is not
// registered.
func toolByName(name string) (toolEntry, bool) {
	for _, e := range catalogManifest {
		if e.decl.Name == name {
			return e, true
		}
	}
	return toolEntry{}, false
}

// listAllTools — ordered snapshot of all tool declarations for
// `tools/list`. Order follows catalogManifest declaration order (operator
// → role → incarnation → soul → plugin → service → augur → oracle → push →
// cloud), matching docs/keeper/mcp-tools.md.
func listAllTools() []toolDeclaration {
	out := make([]toolDeclaration, len(catalogManifest))
	for i, e := range catalogManifest {
		out[i] = e.decl
	}
	return out
}

// --- JSON Schema literals for tool declarations ---
//
// Each schema is JSON Schema draft 2020-12, additionalProperties=false,
// with required fields listed explicitly. Stored as json.RawMessage so
// the package can serve them in tools/list without re-marshaling per request.

var (
	schemaEmptyObject = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{}}`)

	schemaApplyIDOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["_apply_id"],
"properties":{
"_apply_id":{"type":"string","description":"ULID of the run."}}}`)

	schemaApplyIDOutputWithIncarnation = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["incarnation"],
"properties":{
"_apply_id":{"type":"string","description":"ULID of the scenario create run. Missing if lifecycle.auto_create=false - the incarnation was created in ready without a run."},
"incarnation":{"type":"string"}}}`)

	schemaPaginatedListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000},
"total":{"type":"integer","minimum":0}}}`)

	schemaOperatorCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","display_name"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$","description":"AID of the new Archon."},
"display_name":{"type":"string","description":"Human-readable name."}}}`)

	schemaOperatorCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","display_name","created_at","created_by_aid","jwt","expires_at"],
"properties":{
"aid":{"type":"string"},
"display_name":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"jwt":{"type":"string","description":"Issued once; the client must save it."},
"expires_at":{"type":"string","format":"date-time"}}}`)

	schemaOperatorRevokeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"},
"reason":{"type":"string","description":"Free-form text for the audit trail."}}}`)

	schemaOperatorIssueTokenInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid"],
"properties":{
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaOperatorIssueTokenOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["aid","jwt","expires_at"],
"properties":{
"aid":{"type":"string"},
"jwt":{"type":"string"},
"expires_at":{"type":"string","format":"date-time"}}}`)

	schemaRoleCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","permissions"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Role name (kebab-case)."},
"description":{"type":"string","description":"Human-readable role description."},
"permissions":{"type":"array","items":{"type":"string"},"description":"Permission strings '<resource>.<action>' (+ optional ' on <selector>')."},
"default_scope":{"type":["string","null"],"description":"ADR-047 S1: scope selector (per-perm selector syntax, e.g. 'coven=prod,stage'), inherited by all of the role's permissions without their own selector. null/omitted = role without a scope restriction (bare-perms unrestricted)."}}}`)

	schemaRoleDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaRoleUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","permissions"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"permissions":{"type":"array","items":{"type":"string"},"description":"New set of permissions (replace semantics)."},
"default_scope":{"type":["string","null"],"description":"ADR-047 S1: replace the role's default_scope (null clears the scope). Key ABSENT -> scope is left untouched (PATCH semantics)."}}}`)

	schemaRoleGrantOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["role","aid"],
"properties":{
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaRoleRevokeOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["role","aid"],
"properties":{
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaRoleListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["roles"],
"properties":{
"roles":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","description","builtin","permissions","operators"],
"properties":{
"name":{"type":"string"},
"description":{"type":"string"},
"builtin":{"type":"boolean"},
"permissions":{"type":"array","items":{"type":"string"}},
"operators":{"type":"array","items":{"type":"string"}},
"default_scope":{"type":"string","description":"ADR-047 S1: the role's default_scope (empty = role without scope)."}}}}}}`)

	schemaSynodCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Synod group name (kebab-case)."},
"description":{"type":"string","description":"Human-readable group description."}}}`)

	schemaSynodUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","description"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Synod group name (kebab-case). PK immutable - addresses the group, doesn't change."},
"description":{"type":"string","minLength":1,"maxLength":1024,"description":"New group description (replaces the old one; ONLY this changes)."}}}`)

	schemaSynodDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodAddOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","aid"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaSynodRemoveOperatorInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","aid"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"aid":{"type":"string","pattern":"^[a-z0-9][a-z0-9._@-]{1,127}$"}}}`)

	schemaSynodGrantRoleInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","role"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodRevokeRoleInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synod","role"],
"properties":{
"synod":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"role":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaSynodListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["synods"],
"properties":{
"synods":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","description","builtin","roles","operators"],
"properties":{
"name":{"type":"string"},
"description":{"type":"string"},
"builtin":{"type":"boolean"},
"roles":{"type":"array","items":{"type":"string"}},
"operators":{"type":"array","items":{"type":"string"}}}}}}}`)

	schemaIncarnationCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","service"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
"service":{"type":"string"},
"covens":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Declared env-Coven labels for the incarnation (ADR-008 amendment a). Affect RBAC create-scope: an operator with scoped-permission incarnation.create on coven=X can only create an incarnation with covens within their scope."},
"input":{"type":"object"},
"create_scenario":{"type":"string","pattern":"^[a-z][a-z0-9_]*$","description":"Name of the starting scenario (multiple create-scenario mechanism). Empty: if the service offers create scenarios -> choice is required (validation-failed with their list); if the service has no create scenarios -> bare incarnation (ready without a run). Non-empty must be part of the service's create set (scenario with create: true), otherwise validation-failed; saved into incarnation.created_scenario (NULL for bare; rerun-last uses it on the create path)."},
"traits":{"type":"object","additionalProperties":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"},{"type":"array","items":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"}]}}]},"propertyNames":{"pattern":"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$"},"description":"Operator-set trait labels for the incarnation (ADR-060 amend R1) key->(scalar|list of scalars). Extracted into incarnation.traits (source of truth) and projected onto member hosts' souls.traits."}}}`)

	schemaIncarnationRunInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","scenario"],
"properties":{
"name":{"type":"string"},
"scenario":{"type":"string"},
"input":{"type":"object"}}}`)

	schemaIncarnationRunOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["_apply_id","incarnation","scenario"],
"properties":{"_apply_id":{"type":"string"},"incarnation":{"type":"string"},"scenario":{"type":"string"}}}`)

	schemaIncarnationGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string"}}}`)

	schemaIncarnationGetOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"description":"See operator-api.md -> IncarnationGetReply."}`)

	schemaIncarnationListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"service":{"type":"string"},
"status":{"type":"string","enum":["provisioning","ready","applying","error_locked","migration_failed","drift","destroying"]},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaIncarnationHistoryInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaIncarnationUnlockInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","reason"],
"properties":{
"name":{"type":"string"},
"reason":{"type":"string","minLength":1,"maxLength":500}}}`)

	schemaIncarnationRerunLastInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","reason"],
"properties":{
"name":{"type":"string"},
"reason":{"type":"string","minLength":1,"maxLength":500,"description":"Free-form operator confirmation text; written to audit incarnation.rerun_last."}}}`)

	schemaIncarnationRerunLastOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["incarnation","scenario"],
"properties":{
"_apply_id":{"type":"string","description":"ULID of the rerun."},
"incarnation":{"type":"string"},
"scenario":{"type":"string","description":"Name of the rerun scenario (the last one that failed: bootstrap 'create'/... or day-2 add_user/...)."}}}`)

	schemaIncarnationUnlockOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","previous_status","status","unlocked_by_aid","unlocked_at"],
"properties":{
"name":{"type":"string"},
"previous_status":{"type":"string"},
"status":{"type":"string"},
"unlocked_by_aid":{"type":"string"},
"unlocked_at":{"type":"string","format":"date-time"}}}`)

	schemaIncarnationUpgradeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","to_version"],
"properties":{
"name":{"type":"string"},
"to_version":{"type":"string"}}}`)

	schemaIncarnationDestroyInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","allow_destroy"],
"properties":{
"name":{"type":"string"},
"allow_destroy":{"type":"boolean"}}}`)

	// check-drift: input override is optional (auto-from-state by naming
	// convention). Override param names/types are defined by the service's
	// converge schema, so this stays a free-form map (additionalProperties allowed).
	schemaIncarnationCheckDriftInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","description":"Incarnation name."},
"input":{"type":"object","description":"Override of converge parameters; overrides auto-from-state by naming convention."}}}`)

	// DriftReport (ADR-031 Slice B): per-host aggregate of task results + summary.
	// Schema mirrors scenario.DriftReport (Go type in keeper/internal/scenario/checkdrift.go).
	schemaIncarnationCheckDriftOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["checked_at","incarnation","scenario_ref","hosts","summary"],
"properties":{
"checked_at":{"type":"string","format":"date-time"},
"incarnation":{"type":"string"},
"scenario_ref":{"type":"string","description":"Name of the Scry scenario - 'converge'."},
"hosts":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["sid","status","tasks"],
"properties":{
"sid":{"type":"string"},
"status":{"type":"string","enum":["clean","drifted","unsupported","failed"]},
"tasks":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["idx","module","changed"],
"properties":{
"idx":{"type":"integer","minimum":0},
"module":{"type":"string"},
"action":{"type":"string"},
"changed":{"type":"boolean"},
"message":{"type":"string"}}}}}}},
"summary":{"type":"object","additionalProperties":false,
"required":["hosts_drifted","hosts_clean","hosts_unsupported","hosts_failed"],
"properties":{
"hosts_drifted":{"type":"integer","minimum":0},
"hosts_clean":{"type":"integer","minimum":0},
"hosts_unsupported":{"type":"integer","minimum":0},
"hosts_failed":{"type":"integer","minimum":0}}}}}`)

	// traits-set: wholesale replacement of incarnation.traits (ADR-060).
	// 'traits' is the full key→(scalar|list of scalars) set; empty/omitted
	// clears it. Value shape mirrors soul.traits-assign (no nesting).
	schemaIncarnationTraitsSetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Incarnation name."},
"traits":{"type":"object","additionalProperties":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"},{"type":"array","items":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"}]}}]},"propertyNames":{"pattern":"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$"},"description":"Full set of operator-set trait labels key->(scalar|list of scalars). Empty/omitted = clear labels. Wholesale replaces incarnation.traits."}}}`)

	schemaIncarnationTraitsSetOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["incarnation","keys"],
"properties":{
"incarnation":{"type":"string"},
"keys":{"type":"array","items":{"type":"string"},"description":"Final set of trait keys after the replacement (sorted). Values are not echoed (secret hygiene)."}}}`)

	schemaSoulCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","transport"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$","description":"SID (= host FQDN)."},
"transport":{"type":"string","enum":["agent","ssh"],"description":"agent - a bootstrap token is issued; ssh - no token."},
"covens":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Stable Coven labels of the host."},
"note":{"type":"string"}}}`)

	schemaSoulCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","transport","status","covens","registered_at","created_by_aid"],
"properties":{
"sid":{"type":"string"},
"transport":{"type":"string"},
"status":{"type":"string"},
"covens":{"type":"array","items":{"type":"string"}},
"registered_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"bootstrap_token":{"type":"string","description":"Only for transport=agent; issued once."},
"expires_at":{"type":"string","format":"date-time","description":"Only for transport=agent."}}}`)

	schemaSoulIssueTokenInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"force":{"type":"boolean","description":"Expire the active token and issue a new one."}}}`)

	schemaSoulIssueTokenOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","bootstrap_token","expires_at"],
"properties":{
"sid":{"type":"string"},
"bootstrap_token":{"type":"string","description":"Issued once; the client must save it."},
"expires_at":{"type":"string","format":"date-time"}}}`)

	// selector is a subset of the soul.* targeting vocabulary (all/sids/coven/
	// incarnation/status), mirroring REST POST /v1/souls/coven. A free-form
	// CEL predicate is deliberately unsupported (it would break provable
	// scope checks). At least one criterion is required (all=true or one of
	// sids/coven/incarnation/status) — enforced at runtime in the service
	// layer (ErrBulkEmptySelector → validation-failed).
	//
	// `label` ↔ `labels` is XOR by mode: append/remove → label, replace →
	// labels[]. JSON Schema can't express the XOR via if/then on mode
	// (client validator implementation varies), so the real check happens in
	// handler/MCP (422 on violation).
	schemaSoulCovenAssignInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["mode","selector"],
"properties":{
"mode":{"type":"string","enum":["append","remove","replace"],"description":"append - add the label; remove - clear it; replace - replace the whole set of Coven labels."},
"label":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$","description":"Coven label to assign for append/remove (for append it must be within the operator's coven-scope). Use labels for replace."},
"labels":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"Set of Coven labels for mode=replace (may be empty = clear all). Each label must be within the operator's coven-scope."},
"selector":{"type":"object","additionalProperties":false,"description":"Host target; intersected with the operator's coven-scope. At least one criterion is required.","properties":{
"all":{"type":"boolean","description":"The whole registry (intersect scope). No host filter."},
"sids":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},"description":"Explicit list of SIDs."},
"coven":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$","description":"Hosts that ALREADY have this label."},
"incarnation":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Hosts of this incarnation (incarnation name as the root Coven label, ADR-008)."},
"status":{"type":"string","enum":["pending","connected","disconnected","revoked","expired","destroyed"],"description":"Filter by status."}}},
"dry_run":{"type":"boolean","description":"true - return matched under selector intersect scope without UPDATE."}}}`)

	schemaSoulCovenAssignOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["mode","matched","changed","status","dry_run"],
"properties":{
"mode":{"type":"string"},
"label":{"type":"string","description":"The label applied for append/remove."},
"labels":{"type":"array","items":{"type":"string"},"description":"The set of labels applied for replace."},
"matched":{"type":"integer","minimum":0,"description":"Hosts under selector intersect scope."},
"changed":{"type":"integer","minimum":0,"description":"Rows actually changed (0 for dry_run)."},
"status":{"type":"string","enum":["completed","partial"]},
"dry_run":{"type":"boolean"}}}`)

	// trait-value is scalar (string/number/bool) or list of scalars (depth ≤
	// 1, ADR-060). JSON Schema expresses "scalar | list<scalar>" via oneOf;
	// nested objects/arrays are rejected by the domain (ValidTraitValue) —
	// the real check happens in handler/MCP (422). `traits` ↔ `keys` is XOR
	// by mode: merge/replace → traits (map), remove → keys[] (names).
	// selector is the same subset as coven-assign. At least one selector
	// criterion is required (runtime check ErrBulkEmptySelector →
	// validation-failed).
	schemaSoulTraitsAssignInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["selector"],
"properties":{
"mode":{"type":"string","enum":["merge","replace","remove"],"description":"merge (default) - set/overwrite keys; replace - replace the whole traits-map; remove - delete keys from keys."},
"traits":{"type":"object","additionalProperties":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"},{"type":"array","items":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"boolean"}]}}]},"propertyNames":{"pattern":"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$"},"description":"Set of key->value for merge/replace; the value is a scalar or list of scalars. Forbidden for remove."},
"keys":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$"},"description":"List of key names for remove. Forbidden for merge/replace."},
"selector":{"type":"object","additionalProperties":false,"description":"Host target; intersected with the operator's coven-scope. At least one criterion is required.","properties":{
"all":{"type":"boolean","description":"The whole registry (intersect scope). No host filter."},
"sids":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},"description":"Explicit list of SIDs."},
"coven":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$","description":"Hosts that have this Coven label."},
"incarnation":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Hosts of this incarnation (incarnation name as the root Coven label, ADR-008)."},
"status":{"type":"string","enum":["pending","connected","disconnected","revoked","expired","destroyed"],"description":"Filter by status."}}},
"dry_run":{"type":"boolean","description":"true - return matched under selector intersect scope without UPDATE."}}}`)

	schemaSoulTraitsAssignOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["mode","keys","matched","changed","status","dry_run"],
"properties":{
"mode":{"type":"string"},
"keys":{"type":"array","items":{"type":"string"},"description":"Affected trait keys (merge/replace - keys of the set; remove - deleted). Values are not echoed."},
"matched":{"type":"integer","minimum":0,"description":"Hosts under selector intersect scope."},
"changed":{"type":"integer","minimum":0,"description":"Rows actually changed (0 for dry_run)."},
"status":{"type":"string","enum":["completed","partial"]},
"dry_run":{"type":"boolean"}}}`)

	schemaSoulListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"coven":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},
"status":{"type":"string","enum":["pending","connected","disconnected","expired"]},
"transport":{"type":"string","enum":["agent","ssh"]},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaSoulSshTargetUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","ssh_port","ssh_user","soul_path"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"ssh_port":{"type":"integer","minimum":1,"maximum":65535},
"ssh_user":{"type":"string","minLength":1},
"soul_path":{"type":"string","pattern":"^/.+","description":"Absolute Unix path to the soul binary on the host."}}}`)

	schemaSoulSshTargetUpdateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","ssh_target"],
"properties":{
"sid":{"type":"string"},
"ssh_target":{"type":"object","additionalProperties":false,"required":["ssh_port","ssh_user","soul_path"],"properties":{
"ssh_port":{"type":"integer"},
"ssh_user":{"type":"string"},
"soul_path":{"type":"string"}}}}}`)

	schemaPluginAllowInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref"],
"properties":{
"namespace":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Plugin namespace (kebab-case + dots/underscore; no slashes)."},
"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Plugin name."},
"ref":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$","description":"Operator-asserted allowance label (tag-ref shaped like v1.0.0). A branch-ref with a slash isn't supported in MVP."}}}`)

	schemaPluginAllowOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref","sha256"],
"properties":{
"namespace":{"type":"string"},
"name":{"type":"string"},
"ref":{"type":"string"},
"sha256":{"type":"string","description":"SHA-256 (hex) of the allowed binary."}}}`)

	schemaPluginRevokeInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref"],
"properties":{
"namespace":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"},
"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"},
"ref":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"}}}`)

	schemaPluginListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sigils"],
"properties":{
"sigils":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["namespace","name","ref","sha256","allowed_by_aid","allowed_at","revoked_at"],
"properties":{
"namespace":{"type":"string"},
"name":{"type":"string"},
"ref":{"type":"string"},
"sha256":{"type":"string"},
"allowed_by_aid":{"type":"string"},
"allowed_at":{"type":"string","format":"date-time"},
"revoked_at":{"type":["string","null"],"format":"date-time"}}}}}}`)

	schemaSigilKeyIntroduceInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"make_primary":{"type":"boolean","description":"Make the new key primary (new Sigils are signed with it). Defaults to false."}}}`)

	schemaSigilKeyIntroduceOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["key_id","pubkey_pem","is_primary","status","introduced_at"],
"properties":{
"key_id":{"type":"string","pattern":"^[0-9a-f]{64}$","description":"Stable key id: SHA-256(SPKI), hex."},
"pubkey_pem":{"type":"string","description":"Public part (SPKI PEM). The private key is NOT returned in the response."},
"is_primary":{"type":"boolean"},
"status":{"type":"string"},
"introduced_at":{"type":"string","format":"date-time"}}}`)

	schemaSigilKeyIDInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["key_id"],
"properties":{
"key_id":{"type":"string","pattern":"^[0-9a-f]{64}$","description":"Signing key id (SHA-256(SPKI), hex)."}}}`)

	schemaSigilKeyListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["keys"],
"properties":{
"keys":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["key_id","is_primary","status","introduced_at"],
"properties":{
"key_id":{"type":"string"},
"is_primary":{"type":"boolean"},
"status":{"type":"string"},
"introduced_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaServiceRegisterInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Service name (kebab-case)."},
"git":{"type":"string","description":"git source of the service repo (URL; not a secret)."},
"ref":{"type":"string","description":"git ref (tag/branch) - the Service's version (ADR-007)."},
"refresh":{"type":"string","description":"Opt. auto-refresh duration ('5m'); omitted - no auto-refresh."}}}`)

	schemaServiceUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$","description":"Service name (record key, unchanged)."},
"git":{"type":"string","description":"New git source (replace semantics)."},
"ref":{"type":"string","description":"New git ref (replace semantics)."},
"refresh":{"type":"string","description":"Opt. auto-refresh duration ('5m')."}}}`)

	schemaServiceDeregisterInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]*$"}}}`)

	schemaServiceView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","git","ref","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"git":{"type":"string"},
"ref":{"type":"string"},
"refresh":{"type":"string"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaServiceListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["services"],
"properties":{
"services":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","git","ref","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"git":{"type":"string"},
"ref":{"type":"string"},
"refresh":{"type":"string"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaOmenCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Omen name (kebab-case)."},
"source_type":{"type":"string","enum":["vault","prometheus","elk"],"description":"External system type."},
"endpoint":{"type":"string","description":"External system URL (not a secret)."},
"auth_ref":{"type":"string","pattern":"^vault:","description":"vault-ref to the master credential (vault:<mount>/<path>); the secret itself isn't transmitted."}}}`)

	schemaOmenListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaOmenDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaOmenView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref","created_at"],
"properties":{
"name":{"type":"string"},
"source_type":{"type":"string","enum":["vault","prometheus","elk"]},
"endpoint":{"type":"string"},
"auth_ref":{"type":"string"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaOmenListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omens","total"],
"properties":{
"total":{"type":"integer"},
"omens":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","source_type","endpoint","auth_ref","created_at"],
"properties":{
"name":{"type":"string"},
"source_type":{"type":"string","enum":["vault","prometheus","elk"]},
"endpoint":{"type":"string"},
"auth_ref":{"type":"string"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}}}}`)

	// allow is free-form at the JSON Schema level: its shape depends on the
	// Omen's source_type (vault {paths?,policies?} / prometheus {queries} /
	// elk {indices}), which can't be expressed declaratively without a
	// trigger — runtime validation is augur.ValidateAllow (augur.md §4.2).
	schemaRiteCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omen","allow"],
"properties":{
"omen":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Omen this grant belongs to."},
"coven":{"type":"string","description":"Grant subject by Coven label (XOR with sid)."},
"sid":{"type":"string","description":"Grant subject by specific SID (XOR with coven)."},
"allow":{"type":"object","description":"Allow-list; shape depends on the Omen's source_type: vault {paths?,policies?} / prometheus {queries} / elk {indices}."},
"delegate":{"type":"boolean","description":"false - broker (MVP-1); true - delegation (MVP-2)."},
"token_ttl":{"type":"string","description":"TTL of the minted scoped token; vault-delegate only."},
"token_num_uses":{"type":"integer","minimum":0,"description":"Usage limit of the token; vault-delegate only."}}}`)

	schemaRiteListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["omen"],
"properties":{
"omen":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaRiteDeleteInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["id"],
"properties":{
"id":{"type":"integer","minimum":1}}}`)

	schemaRiteView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["id","omen","allow","delegate","created_at"],
"properties":{
"id":{"type":"integer"},
"omen":{"type":"string"},
"coven":{"type":"string"},
"sid":{"type":"string"},
"allow":{"type":"object"},
"delegate":{"type":"boolean"},
"token_ttl":{"type":"string"},
"token_num_uses":{"type":"integer"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaRiteListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["rites"],
"properties":{
"rites":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["id","omen","allow","delegate","created_at"],
"properties":{
"id":{"type":"integer"},
"omen":{"type":"string"},
"coven":{"type":"string"},
"sid":{"type":"string"},
"allow":{"type":"object"},
"delegate":{"type":"boolean"},
"token_ttl":{"type":"string"},
"token_num_uses":{"type":"integer"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}}}}`)

	// --- Oracle (Vigil / Decree, ADR-030 beacons) ---
	//
	// params (Vigil) / action_input (Decree) are free-form at the JSON
	// Schema level: shape depends on check / scenario and can't be expressed
	// declaratively without a trigger (typed payload deferred, ADR-030);
	// runtime validation is in the service layer.
	schemaOraclePaginatedInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaOracleNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaVigilCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","interval","check"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Vigil name (kebab-case)."},
"coven":{"type":"array","items":{"type":"string"},"description":"Subject coven labels (XOR with sid): the Vigil is distributed to all Souls with any of these labels."},
"sid":{"type":"string","description":"Subject - one specific SID (XOR with coven)."},
"interval":{"type":"string","description":"Check frequency (duration convention, e.g. '30s')."},
"check":{"type":"string","description":"core-beacon address (e.g. 'core.beacon.file_changed')."},
"params":{"type":"object","description":"Check parameters; shape depends on check (path / service-name / threshold)."},
"enabled":{"type":"boolean","description":"Whether the check is active. Defaults to true."}}}`)

	schemaVigilView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","interval","check","params","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"interval":{"type":"string"},
"check":{"type":"string"},
"params":{"type":"object"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaVigilListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["vigils","total"],
"properties":{
"total":{"type":"integer"},
"vigils":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","interval","check","params","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"interval":{"type":"string"},
"check":{"type":"string"},
"params":{"type":"object"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaDecreeCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Decree name (kebab-case)."},
"on_beacon":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Name of the Vigil whose Portent the rule reacts to."},
"where":{"type":"string","description":"Opt. CEL predicate over event.data (e.g. 'event.data.severity == \"critical\"'); compile-checked on create."},
"coven":{"type":"array","items":{"type":"string"},"description":"Subject coven labels (XOR with sid): which hosts can trigger the rule."},
"sid":{"type":"string","description":"Subject - one specific SID (XOR with coven)."},
"incarnation_name":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$","description":"Target incarnation for the reaction (ServiceRef is resolved from it; required)."},
"action_scenario":{"type":"string","pattern":"^[a-z][a-z0-9_]*$","description":"Named scenario (whitelist; a raw command is rejected)."},
"action_input":{"type":"object","description":"Scenario input (vault-ref passes through as-is)."},
"cooldown":{"type":"string","description":"Minimum interval between firings per (decree, subject) (duration; omitted -> disabled)."},
"enabled":{"type":"boolean","description":"Whether the rule is active. Defaults to true."}}}`)

	schemaDecreeView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario","action_input","cooldown","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"on_beacon":{"type":"string"},
"where":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"incarnation_name":{"type":"string"},
"action_scenario":{"type":"string"},
"action_input":{"type":"object"},
"cooldown":{"type":"string"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}`)

	schemaDecreeListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["decrees","total"],
"properties":{
"total":{"type":"integer"},
"decrees":{"type":"array","items":{
"type":"object",
"additionalProperties":false,
"required":["name","on_beacon","incarnation_name","action_scenario","action_input","cooldown","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"on_beacon":{"type":"string"},
"where":{"type":"string"},
"coven":{"type":"array","items":{"type":"string"}},
"sid":{"type":"string"},
"incarnation_name":{"type":"string"},
"action_scenario":{"type":"string"},
"action_input":{"type":"object"},
"cooldown":{"type":"string"},
"enabled":{"type":"boolean"},
"created_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"}}}}}}`)

	schemaPushApplyInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["inventory","destiny"],
"properties":{
"inventory":{"type":"array","items":{"type":"string"},"minItems":1},
"destiny":{"type":"string","description":"<name>@<ref>"},
"input":{"type":"object"},
"ssh_provider":{"type":"string"},
"cleanup_stale_versions":{"type":"boolean"}}}`)

	schemaPushCleanupInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["inventory"],
"properties":{
"inventory":{"type":"array","items":{"type":"string"},"minItems":1},
"ssh_provider":{"type":"string"},
"full":{"type":"boolean"}}}`)

	schemaProviderCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","region","credentials_ref"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"type":{"type":"string"},
"region":{"type":"string"},
"credentials_ref":{"type":"string","pattern":"^vault:"}}}`)

	schemaProviderCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","region","credentials_ref","created_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"type":{"type":"string"},
"region":{"type":"string"},
"credentials_ref":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"}}}`)

	schemaProfileCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","provider","params"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"provider":{"type":"string"},
"params":{"type":"object"},
"cloud_init":{"type":"string"}}}`)

	// --- Errand (ADR-033) ---
	//
	// schemaErrandRow — base shape of an errands row (used directly as
	// schemaErrandGetOutput and as an element of schemaErrandListOutput.items).
	schemaErrandRow = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","sid","module","status","started_by_aid","started_at"],
"properties":{
"errand_id":{"type":"string","description":"ULID of the run."},
"sid":{"type":"string","description":"Target Soul (FQDN)."},
"module":{"type":"string","description":"Fully-qualified module name."},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"exit_code":{"type":"integer","description":"Exit code of the verb module (NULL for read-safe non-shell)."},
"stdout":{"type":"string"},
"stderr":{"type":"string"},
"stdout_truncated":{"type":"boolean"},
"stderr_truncated":{"type":"boolean"},
"duration_ms":{"type":"integer"},
"error_message":{"type":"string"},
"output":{"type":"object","description":"Structured output of read-safe modules; absent for shell/exec."},
"started_by_aid":{"type":"string"},
"started_at":{"type":"string","format":"date-time"},
"finished_at":{"type":"string","format":"date-time","description":"Filled only for terminal statuses."}}}`)

	schemaErrandRunInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["sid","module"],
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$","description":"FQDN of the target Soul."},
"module":{"type":"string","description":"Module address core.<class>.<state> or core.cmd.shell / core.exec.run."},
"input":{"type":"object","description":"Module input (shape depends on the module)."},
"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"Server-cap of the overall timeout. Default 30."},
"dry_run":{"type":"boolean","description":"true -> Soul calls mod.Plan instead of mod.Apply (read-safe modules only)."}}}`)

	schemaErrandRunOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","sid","module","status","async"],
"properties":{
"errand_id":{"type":"string"},
"sid":{"type":"string"},
"module":{"type":"string"},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"async":{"type":"boolean","description":"true -> server-cap exceeded, follow up via keeper.errand.get."},
"exit_code":{"type":"integer"},
"stdout":{"type":"string"},
"stderr":{"type":"string"},
"stdout_truncated":{"type":"boolean"},
"stderr_truncated":{"type":"boolean"},
"duration_ms":{"type":"integer"},
"error_message":{"type":"string"},
"output":{"type":"object"}}}`)

	schemaErrandListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"sid":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},
"status":{"type":"string","enum":["running","success","failed","timed_out","cancelled","module_not_allowed"]},
"started_after":{"type":"string","format":"date-time"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaErrandListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array","items":{"type":"object"}},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000},
"total":{"type":"integer","minimum":0}}}`)

	schemaErrandGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id"],
"properties":{
"errand_id":{"type":"string"}}}`)

	// schemaErrandCancelInput / Output — keeper.errand.cancel (ADR-033 slice
	// E5). Input is a single errand_id field (like get); output is an ack
	// object mirroring the REST DELETE 204 equivalent.
	schemaErrandCancelInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id"],
"properties":{
"errand_id":{"type":"string"}}}`)

	schemaErrandCancelOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["errand_id","cancelled"],
"properties":{
"errand_id":{"type":"string"},
"cancelled":{"type":"boolean"}}}`)

	// --- Voyage (ADR-043, S5) — input/output schemas. RBAC-by-kind is in the handler.
	schemaVoyageStartInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["kind","target"],
"properties":{
"kind":{"type":"string","enum":["scenario","command"]},
"scenario_name":{"type":"string","description":"Required for kind=scenario."},
"module":{"type":"string","description":"Required for kind=command (Soul-side whitelist)."},
"input":{"type":"object","description":"Run parameters (NOT logged to audit)."},
"target":{
"type":"object","additionalProperties":false,
"properties":{
"incarnations":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9-]{0,62}$"},"description":"scenario - incarnation names."},
"service":{"type":"string","description":"scenario - incarnation.service filter."},
"sids":{"type":"array","items":{"type":"string","pattern":"^[a-z0-9][a-z0-9.-]{0,253}$"},"description":"command — SID-snapshot."},
"where":{"type":"string","maxLength":4096,"description":"command - CEL predicate (MVP stored without evaluate)."},
"coven":{"type":"array","items":{"type":"string","pattern":"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"},"description":"scenario - env tag (any-of) / command - coven label (AND)."}
}
},
"batch":{"type":"string","examples":["20%"],"description":"Batch size: N units | N% (1-100) of scope. Mutually exclusive with batch_size - mixing -> 422 voyage_batch_spec_conflict. grammar ^(\\d+)%?$."},
"max_failures":{"type":"string","examples":["25%"],"description":"Failure threshold: N absolute | N% of run units. Mutually exclusive with fail_threshold -> 422 voyage_batch_spec_conflict."},
"batch_size":{"type":"integer","minimum":1,"deprecated":true,"description":"DEPRECATED - use batch. Leg size. null -> the whole run is one Leg."},
"concurrency":{"type":"integer","minimum":1,"maximum":500,"description":"0/missing → default 50."},
"dry_run":{"type":"boolean"},
"schedule_at":{"type":"string","format":"date-time","description":"Delayed start -> status scheduled."},
"inter_batch_interval_ms":{"type":"integer","minimum":0,"description":"Pause between Legs (ms)."},
"on_failure":{"type":"string","enum":["abort","continue"]}
}}`)

	schemaVoyageStartOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id","kind","scope_size","status","location"],
"properties":{
"voyage_id":{"type":"string","description":"ULID."},
"kind":{"type":"string","enum":["scenario","command"]},
"scope_size":{"type":"integer","description":"Number of resolved units."},
"status":{"type":"string","enum":["pending","scheduled"]},
"location":{"type":"string","description":"REST path for get/poll (/v1/voyages/<id>)."}}}`)

	schemaVoyageGetInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id"],
"properties":{
"voyage_id":{"type":"string"}}}`)

	schemaVoyageListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"kind":{"type":"string","enum":["scenario","command"]},
"status":{"type":"array","items":{"type":"string","enum":["scheduled","pending","running","succeeded","failed","partial_failed","cancelled"]}},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaVoyageView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":true,
"required":["voyage_id","kind","status","scope_size","total_batches","current_batch_index","started_by_aid","created_at"],
"properties":{
"voyage_id":{"type":"string"},
"kind":{"type":"string"},
"status":{"type":"string"},
"scope_size":{"type":"integer"},
"total_batches":{"type":"integer"},
"current_batch_index":{"type":"integer"},
"started_by_aid":{"type":"string"},
"created_at":{"type":"string","format":"date-time"}}}`)

	schemaVoyageListOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["items","offset","limit","total"],
"properties":{
"items":{"type":"array","items":{"type":"object","additionalProperties":true}},
"offset":{"type":"integer"},
"limit":{"type":"integer"},
"total":{"type":"integer"}}}`)

	schemaVoyageCancelOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["voyage_id","status"],
"properties":{
"voyage_id":{"type":"string"},
"status":{"type":"string","enum":["cancelled"]}}}`)

	schemaProfileCreateOutput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","provider","params","created_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"provider":{"type":"string"},
"params":{"type":"object"},
"cloud_init":{"type":"string"},
"created_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"}}}`)

	// Cloud Provider/Profile — read/delete (by-name) and list (paged) input
	// schemas (ADR-017). name pattern mirrors provider/profile.NamePattern (kebab 1..63).
	schemaProviderByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Cloud-Provider name."}}}`)

	schemaProviderListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0,"description":"Offset from the start of the set."},
"limit":{"type":"integer","minimum":1,"description":"Page size (default 100)."}}}`)

	schemaProfileByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Cloud-Profile name."}}}`)

	schemaProfileListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"provider":{"type":"string","description":"Filter by Provider name (opt.)."},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"description":"Page size (default 100)."}}}`)

	// Push-Provider (S7-2) — input/output schemas. name pattern mirrors
	// pushprovider.NamePattern (`^[a-z][a-z0-9-]{0,62}$` — env-var-name-safe).
	schemaPushProviderCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$","description":"Plugin name (= plugins.ssh_providers[].name)."},
"params":{"type":"object","description":"Opaque per-provider params. Sensitive keys (secret_id/token/password/private_key) MUST be vault-refs (vault:<path>)."}}}`)

	schemaPushProviderUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","params"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$"},
"params":{"type":"object","description":"Full new set of params (replace semantics)."}}}`)

	schemaPushProviderByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{0,62}$"}}}`)

	schemaPushProviderListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"name_pattern":{"type":"string","description":"LIKE-form name filter (e.g. vault%)."},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaPushProviderView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","params","created_at","updated_at","created_by_aid"],
"properties":{
"name":{"type":"string"},
"params":{"type":"object"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":"string"},
"updated_by_aid":{"type":["string","null"]}}}`)

	// Herald/Tiding (ADR-052, S4). 1:1 with REST schemas in openapi.yaml.
	schemaHeraldCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$","description":"Herald channel name (kebab-case)."},
"type":{"type":"string","enum":["webhook"],"description":"Channel type (webhook in MVP)."},
"config":{"type":"object","description":"Per-type config (webhook - { url, opt. headers, opt. http_allowed/allow_private })."},
"secret_ref":{"type":["string","null"],"description":"Opt. vault-ref to the signing token (vault:<mount>/<path>); signs webhooks X-SoulStack-Signature."},
"enabled":{"type":"boolean","description":"Channel enabled (omitted -> true)."}}}`)

	schemaHeraldUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"type":{"type":"string","enum":["webhook"]},
"config":{"type":"object","description":"Full new config (replace semantics)."},
"secret_ref":{"type":["string","null"]},
"enabled":{"type":"boolean"}}}`)

	schemaHeraldByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaHeraldListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaHeraldView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","type","config","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"type":{"type":"string","enum":["webhook"]},
"config":{"type":"object"},
"secret_ref":{"type":["string","null"]},
"enabled":{"type":"boolean"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":["string","null"]}}}`)

	schemaTidingCreateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"herald":{"type":"string","description":"Delivery Herald channel name (FK)."},
"event_types":{"type":"array","items":{"type":"string"},"description":"area-glob scenario_run.* within run scope (scenario_run/command_run/voyage/cadence + incarnation.drift_checked)."},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"],"description":"Opt. subscription selector for a specific task by register-union-id from changed_tasks (ADR-052 Sec l)."},
"enabled":{"type":"boolean","description":"Omitted -> true."}}}`)

	schemaTidingUpdateInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"},
"herald":{"type":"string"},
"event_types":{"type":"array","items":{"type":"string"}},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"],"description":"Opt. subscription selector for a specific task (register-union-id from changed_tasks, ADR-052 Sec l). Replace - absence clears it."},
"enabled":{"type":"boolean"}}}`)

	schemaTidingByNameInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name"],
"properties":{
"name":{"type":"string","pattern":"^[a-z0-9-]{1,63}$"}}}`)

	schemaTidingListInput = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"properties":{
"include_ephemeral":{"type":"boolean","description":"true -> include one-off (ephemeral) rules; defaults to false (hidden)"},
"offset":{"type":"integer","minimum":0},
"limit":{"type":"integer","minimum":1,"maximum":1000}}}`)

	schemaTidingView = json.RawMessage(`{
"$schema":"https://json-schema.org/draft/2020-12/schema",
"type":"object",
"additionalProperties":false,
"required":["name","herald","event_types","only_failures","only_changes","enabled","created_at","updated_at"],
"properties":{
"name":{"type":"string"},
"herald":{"type":"string"},
"event_types":{"type":"array","items":{"type":"string"}},
"only_failures":{"type":"boolean"},
"only_changes":{"type":"boolean"},
"incarnation":{"type":["string","null"]},
"cadence":{"type":["string","null"]},
"task":{"type":["string","null"]},
"enabled":{"type":"boolean"},
"created_at":{"type":"string","format":"date-time"},
"updated_at":{"type":"string","format":"date-time"},
"created_by_aid":{"type":["string","null"]}}}`)
)
