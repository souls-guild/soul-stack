# ADR-013. Bootstrap the first Archon

- **Context.** The Keeper's RBAC is `default_policy: deny` with no exceptions ([rbac.md](../keeper/rbac.md)). On the first cluster initialization the `operators` registry is empty; without a special mechanism any API/MCP call fails with 403, and it is impossible to issue either a second operator or the first machine-identity. Open Q #1 in [Open questions](../architecture.md#open-questions). In parallel: the name of the "first operator" entity is not fixed in [naming-rules.md](../naming-rules.md) — a propose-and-wait is needed. The name `Bootstrap` is already taken by [ADR-012(f)](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) for Soul onboarding — the operator needs a different one.
- **Decision.**

  **(a) Entity name — Archon.** Greek for "supreme ruler, highest official", it fits the Soul Stack mythic palette (Keeper / Souls / Destiny / Soulprint / Essence / SoulSeed / Coven / Reaper). Semantically precise: "supreme cluster administrator", not "creator" (the Keeper does not create souls, it manages them). Fixed in [naming-rules.md → Domain Entities](../naming-rules.md#domain-entities). The identifier is **AID** (Archon ID), a lowercase ASCII string of the form `archon-alice` / `archon-ops-01` / `alice@corp.com` (regex and shape — [ADR-014(c)](0014-operator-identity.md#adr-014-operator-identity-model-archon)). The AID is free of conflicts with known standards (not OID/ASN.1, not DID/W3C, not PID/GID/unix). **AID safety does not rely on the `archon-` prefix** (removed by the 2026-05-29 amendment, see [ADR-014(c)](0014-operator-identity.md#adr-014-operator-identity-model-archon)), **but on a strict charset** `[a-z0-9._@-]` starting with an alphanumeric: no `/`/`\` (the AID ends up in bootstrap-token file names — no path traversal), ASCII-lowercase only (no unicode look-alikes and no case ambiguity), no control chars/quotes (no injection into logs/JWT/SQL). The prefix was removed so the AID can directly hold an external identity name from LDAP/Keycloak.

  **(b) Mechanism to issue the first credential — a dedicated command `keeper init`.** A one-time administrative subcommand of the `keeper` binary itself:

  ```
  keeper init --archon=<aid> --config=/etc/keeper/keeper.yml [--credential-out=/etc/keeper/archon-credential.json]
  ```

  - The command checks that the `operators` registry in Postgres is empty (via a PG advisory lock, see (e) below).
  - Creates the first Archon with the given AID and attaches the `cluster-admin` role to it ([ADR-014](0014-operator-identity.md#adr-014-operator-identity-model-archon)).
  - Issues a JWT credential (shape — see [ADR-014](0014-operator-identity.md#adr-014-operator-identity-model-archon)) and puts it in a file with `mode 0400` (by analogy with SoulSeed on the Soul host — see [`soul/onboarding.md`](../soul/onboarding.md)).
  - On a repeated call against an already-initialized DB, it refuses with the message "cluster already initialized; archon <aid> exists since <ts>".

  The command is an **administrative subcommand of the `keeper` binary for self-initialization**, not "keeper in client mode". This explicitly does NOT contradict [ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) (the ban on client subcommands): the actual work is done by the locally installed Keeper binary over its own state in Postgres, not by "keeper connecting to a remote keeper as a client". No other administrative subcommands (`keeper migrate`, `keeper bootstrap-something-else`) are introduced at this time — every exception is fixed by a separate ADR.

  **(c) Privileges of the first Archon — an explicit `cluster-admin` role with `permissions: ["*"]`.** A record in `operators` + binding of the AID to the `cluster-admin` role. **With [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres):** the binding is a membership row `(cluster-admin, <aid>)` in the PG table `rbac_role_operators` (not an entry in the YAML list `roles[].operators`); the `cluster-admin` role and its `*` permission come from the seed migration (E1). `keeper init` in its advisory-lock transaction (e) writes **only** this membership row — this is the BUG-1 fix ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres) Context: previously init put `roles` in a JWT claim that the enforcer did not read). A regular RBAC model, without special cases and a hardcoded super-role in code. Invariant: **you cannot delete the last operator with an active `*` permission** — an attempt via the API returns 409 "would lock out the cluster". This is protection against accidental / malicious self-lockout.

  **(d) Restart semantics — refuse without `--initialize`.** If at startup the Keeper sees that `operators` in Postgres is empty:
  - **Without the `--initialize` flag** (or without the `KEEPER_INITIALIZE=true` variable) — the Keeper refuses to start with the message `"operators registry is empty; run 'keeper init --archon=<aid>' before starting the cluster"`. Exit code != 0.
  - **With `--initialize`** — the Keeper starts in read-only mode (listeners come up, but all API/MCP calls return 503 "cluster awaiting first archon") until `keeper init` runs.

  This is protection against an accidental re-bootstrap after a catastrophic wipe of Postgres — without an explicit flag, a casual observer will not get an automatically issued admin token from the logs.

  **(e) HA race condition — Postgres advisory lock.** On the first start of N instances simultaneously: `keeper init` takes a PG advisory lock (`pg_advisory_xact_lock(<keeper_bootstrap_lock_id>)`) and under the lock checks `SELECT count(*) FROM operators`. If zero — it issues the Archon; if non-zero — it refuses. The remaining concurrent `keeper init` calls wait for the lock, see a non-empty registry, and refuse, indicating the already-created Archon.

  On a normal start (not `keeper init`) — each instance independently checks that `operators` is non-empty and starts / refuses by rule (d). There is no race here: the check only reads.

  **(f) Audit.** Issuing the first Archon is written to `audit_log` ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — `event_type: operator.created`, `archon_aid: NULL` (the first one has no initiating Archon), with the KID as executor and `payload: { bootstrap_initial: true, ... }`. Do not confuse this with `incarnation.state_history` (per-incarnation snapshots, [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Related OTel events are mandatory.

- **Consequences.**
  - The `keeper` binary gains a subcommand `keeper init` (a dedicated command, not a connection to a remote keeper).
  - `keeper.yml` gains an optional flag `bootstrap.initialize: true` (equivalent to the CLI `--initialize`) for orchestrators that do not control the process with flags.
  - The `operators` table in Postgres is a mandatory registry; the FK fields `created_by_aid`, `changed_by_aid` and similar now work (see [ADR-014](0014-operator-identity.md#adr-014-operator-identity-model-archon)).
  - Open Q #1 is closed.
  - In [`docs/keeper/rbac.md`](../keeper/rbac.md) the "Bootstrap of the first operator ("Creator")" section is updated to the name Archon.
- **Trade-offs.**
  - Operator experience: on the first install you must explicitly run `keeper init` — it is not "deployed a container → landed in the UI". This is a deliberate trade-off: security > convenience.
  - On catastrophic recovery (truncate `operators`) there is no automatic re-bootstrap — the operator has to run `keeper init` again. That is precisely the protection.
  - `keeper init` runs locally on the Keeper host — for a cloud deployment (Helm/K8s) an Init Container or Job will be needed. This is closed by operational practice, not architecture.

**Amendment 2026-06-23: the invariant "exactly one bootstrap Archon" is expressed via `created_via`.**

Under the operator provisioning model ([ADR-058](0058-operator-auth-ldap-oidc.md#adr-058-federated-operator-authentication-archon---ldap--oauth2oidc), field `operators.created_via`, [ADR-014 amendment 2026-06-23](0014-operator-identity.md#adr-014-operator-identity-model-archon)) the data invariant "exactly one first Archon" is reformulated: the partial unique index `operators_first_archon_idx` is moved from `WHERE created_by_aid IS NULL` to **`WHERE created_via='bootstrap'`** (migration 085). The `keeper init` check (e) "registry empty / already initialized" is semantically unchanged — `keeper init` creates a row with `created_via='bootstrap'` under the same advisory lock. Consequence for (f): the first Archon is still written with `created_by_aid: NULL` and `payload.bootstrap_initial: true`, but uniqueness is now guaranteed by `created_via`, not by a NULL `created_by_aid` — this legalizes `created_by_aid=NULL` for federated/system operators (`archon-system`, LDAP/OIDC auto-provision), which previously conflicted with the earlier index.

**Amendment 2026-07-01: the registry-emptiness check ignores system operators.**

As of migration **086** ([ADR-058(d)](0058-operator-auth-ldap-oidc.md#adr-058-federated-operator-authentication-archon---ldap--oauth2oidc)) the system `archon-system` (`created_via='system'`) is seeded into `operators` on any initialized DB — it is an FK anchor for system-initiated inserts, not a real Archon. Because of this a bare `SELECT count(*) FROM operators` on a **clean** DB returned 1, and both checks broke: (e) `keeper init` failed with `ErrAlreadyInitialized` on an empty cluster (a regression), and (d) the restart guard saw "registry non-empty" and silently started the keeper "as initialized" without a single Archon. Invariant refinement: **"registry empty" = there is no operator with `created_via != 'system'`**. Both checks — (e) under the advisory lock in `keeper init` and (d) at startup of `keeper run` — count only NON-system operators (`operator.CountNonSystem`), symmetrically. The meaning of the invariant (protection against re-bootstrap, HA race, self-lockout) does not change; only the fact that the system anchor is not treated as a "real" operator is corrected.
