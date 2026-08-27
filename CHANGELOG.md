# Changelog

Format — [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Artifact versioning — via git ref ([ADR-007](docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field)), there is no separate `version:` field on Service/Destiny/Module.

## [Unreleased]

### Added

- Soul-side `core.http.request` for one explicit POST/PUT/PATCH/DELETE API
  mutation with the existing HTTP guards, a final post-redaction 64 KiB
  diagnostic response cap, echoed-header-value redaction, no redirect replay,
  and `changed=true` on an expected status. `core.http.probe` remains
  GET/HEAD-only and read-only; scenario DSL
  owns retries. The first contract fixture covers Consul Agent register,
  maintenance and deregister on loopback with explicit `allow_http` and
  `allow_private`.

### Changed

- **A secret is a declared state field; the author never writes a Vault path**
  ([ADR-0083](docs/adr/0083-declared-secret-state-fields.md), NIM-698). A
  `state_schema` field carries `type: secret` — on the field for a scalar, or on
  a property inside `items` next to the `key:` that names the collection's
  identity — and Keeper derives the path: `<mount>/<service>/<incarnation>/<state-field>/<key>#<property>`
  for a collection, `<mount>/<service>/<incarnation>/<state-field>#value` for a
  scalar. Every segment is validated against the
  [ADR-064](docs/adr/0064-secret-write-path.md) `^[a-zA-Z0-9_-]+$` grammar and
  fails closed, because `<key>` is operator-influenced data.
- `generate_secret({"length": 32, "charset": "alphanumeric"})` — a new CEL
  function returning an opaque `SecretRequest` marker rather than a value, so no
  plaintext exists at render time. It takes a map because CEL has no keyword
  arguments.
- `core.state.set` — a keeper-side write of a state field. It reads the field,
  mints only the properties that are missing, writes them to their derived paths
  and returns the **effective** state in its register, so a second run keeps the
  first run's password.
- The keeper register is **unioned into every per-host bucket** instead of being
  the empty-bucket fallback, so a Soul-side task can read what the keeper-side
  writer just returned. Duplicate register names were already a load error, so
  the union is unambiguous.
- A secret in a register rides as a **`vault:` reference**, not as plaintext,
  which closes the `apply_task_register` window without adding a purge.
- **Explicit state capture — `core.state.<verb>`**
  ([ADR-0084](docs/adr/0084-explicit-state-capture.md), NIM-699). A scenario
  writes its state where it says so, with a keeper-side step, and the write
  lands **at that step** rather than at an end-of-run commit: a later task reads
  what an earlier one wrote, and a run that dies half-way keeps what it had
  already captured. The address **is** the verb — `core.state.set` /
  `.present` / `.add` / `.append` / `.modify` / `.remove` / `.unset`, the
  [ADR-057](docs/adr/0057-state-changes-crud-verbs.md) set, applied by the same
  engine `state_changes:` used, so a verb cannot mean two things depending on
  which path wrote it. A param the verb does not take (`patch:` on a `set`) is
  an authoring error, not a silent drop.
- **Secret resolution is orthogonal to the verb.** The rule that made ADR-0083
  §4 name its module `present` moves onto the *property*: on `type: secret`
  every verb keeps an existing Vault value and mints only what is absent — so
  `core.state.set` overwrites the field's ordinary content and still does not
  rotate a live credential. `core.state.present` now answers the separate
  question of whether the incoming value reaches the field at all.
- `core.state.present`'s two params are renamed `key:` → `field:` and
  `set:` → `value:` (with the register echo key). Both old names collide with
  the verb grammar: there `key:` addresses an element **inside** a collection,
  while the module spelled the containing field the same way.
- **`keeper`, `herald`, `provider` and `internal` are reserved service names**
  ([ADR-0083](docs/adr/0083-declared-secret-state-fields.md) amendment
  2026-08-26, NIM-706). A service's name becomes the first path segment after
  the KV mount of every secret the platform derives for it, and each of those
  four words already opens a path family the platform writes itself:
  `<mount>/keeper/<name>` for keeper's own runtime secrets, and
  `<mount>/herald/<entity>/<field>` / `<mount>/provider/<name>/credentials` for
  the [ADR-064](docs/adr/0064-secret-write-path.md) write path. The two
  derivations met on one KV entry — and Vault KV v2 **replaces** an entry rather
  than merging into it, so on the `secretwrite` path the second write deleted
  the first one's fields with no error at either end. Refused offline
  (`service_name_reserved`), at REST and MCP registration (**422**, not 409 —
  nothing holds the name), and by the derivation itself, which now returns an
  error rather than emitting a colliding path. The comparison is on the whole
  name: `keeper-notes` is still a perfectly good service. **Breaking** — a
  service already registered under one of the four names stops loading and must
  be renamed. `herald` and `provider` also join the reserved registration
  aliases, which are a separate and wider list.
- **`tls` is reserved as the name of a `state_schema` field that declares a
  secret** (same amendment). Keeper issues an incarnation's certificate and
  private key to `<mount>/<service>/<incarnation>/tls/{cert,key}`, which a
  collection secret on a field named `tls` with element key `cert` reproduces
  exactly — and that path lives *inside* the service's own namespace, out of
  reach of any rule about service names. The element key is state data and
  cannot be constrained, so the fence is on the field name
  (`secret_field_reserved_state_name`). Only a field declaring a secret is
  checked: a plain `tls:` object of ports and cipher lists derives nothing and
  is untouched.
- `keeper/internal/secretwrite`'s domain set is closed and enforced: `Writer.path`
  refuses a domain outside `WriteDomains()`, where it previously took any safe path
  segment. Both call sites already pass `DomainHerald` / `DomainProvider`, so nothing
  at runtime changes — but a domain added later must now join the set, and the set is
  what the reserved-name coupling guard iterates. A guard naming today's two members
  would have stayed green for a third domain, which is the change that reopens the
  collision (NIM-706).
- `certissue` no longer spells its KV mount as the literal `"secret"`. On a
  deployment with a non-default `vault.kv_mount` it wrote an incarnation's TLS
  material outside the configured mount entirely; the mount now comes from
  `keeper.yml`, threaded through both callers.
- The cert rotator reads its config once per tick. `issueMaterial` re-read
  `keeper.yml` to find the KV mount, so a hot-reload landing between the CSR and
  the write signed against one config and wrote against another — the rotated
  certificate would land outside the mount the run started with, and the
  incarnation's ref would point at the old entry (NIM-706).
- **`block:` and `on: keeper` on the same task no longer panic the render.** The
  render loop tests `on: keeper` before it tests `block:`, so such a task went to
  the keeper renderer with no `module:` at all and the nil dereference took the
  process down instead of naming the mistake. Two layers refuse it now — the
  config validator, which is what `soul-lint` reports offline
  (`block_on_keeper_invalid`), and the render itself, fail-closed
  (`ErrUnsupportedDSL`) — and at **both** levels,
  because moving the key down onto the block's children does not work either: a
  block fans its children out over the run's hosts and `keeper` is not one. Keeper
  tasks go flat, in the scenario's own task list. The offline half reads the AST
  of every task, an included file's included, so a service whose body lives behind
  `include:` is covered like an inline one — linted inside its service tree, where
  the include resolves the way the keeper resolves it (NIM-652).

- **A task-level `output:` is refused** with a new `output_unsupported` error, on
  every task kind (`module:` / `apply:` / `include:` / `block:`, a task inside a
  block, a keeper-side task) and in both entities that carry tasks - a scenario
  and a destiny's own `tasks/main.yml` (NIM-334). The key parsed and nothing
  ever resolved it: no consumer materialised a value and no name was compared
  against the destiny's declared top-level `output:`, while
  [`docs/destiny/tasks.md §9`](docs/destiny/tasks.md) documented both halves as
  working. It gets its own code rather than joining `<key>_on_apply_invalid` /
  `<key>_on_block_invalid`: those two say "this key works elsewhere and is lost
  HERE", and this one is unbuilt everywhere. `output_on_block_invalid` is
  withdrawn, and so is `include_modifier_unsupported` for `output:` - its hint
  ("move the modifier onto a module task of the included file") named a task kind
  that now refuses the key as well, so one key yields one diagnostic and the
  surviving one is the true one. The top-level `output:` block in
  `destiny.yml` stays valid; it is a declaration, and the machinery that would
  fill it (task-level fill plus the projection into `register.<applier>.<field>`)
  remains one unbuilt slice, now marked as such everywhere it was documented.
  That sweep includes a worked example in
  [`docs/module/core/noop/README.md`](docs/module/core/noop/README.md) that wrote
  a task-level `output:` on `core.noop.run`; it collects its three probe results
  through `vars:` now - equally passage-defining, so the barrier is unchanged, and
  not schema-checked, which `params:` is: `core.noop.run` declares no inputs, so
  three keys there would be three `unknown_param` errors. Two pre-existing claims
  in the same file (and the matching row in `naming-rules.md`) said any `params:`
  key was "accepted and ignored" - true of the module at runtime, false at load,
  and the same drift this entry is about.

### Fixed

- **A renamed file in the artifact reddened nobody, so the rename was found one
  consumer at a time.** `NIM-377` replaced the plugin's hand-written
  `manifest.yaml` with a generated document, and the consumers naming that file
  were a **list** with no single gate over it. Two were found by hand, months
  apart, each by somebody who had already lost the day to it: `dev/provision.sh`
  killed `make dev-provision` before the service registry was ever seeded
  (`NIM-516`), and the L3b harness killed three live tests at setup while
  reporting it as "the stand didn't come up" (`NIM-515`). Both fixes shipped a
  guard over the consumer just found, which is why the class stayed open — and
  why the third consumer was still broken a release later:
  `scripts/e2e-cloud/lib/preflight.sh` went on requiring `mod-manifest.yaml` in
  `$ARTIFACTS_DIR`, a hard preflight `FAIL` (exit 2, before a single call
  reaches the cloud) on a file that cannot exist any more.

  The default artifact list is binaries now and nothing beside them. Nothing
  replaces the dropped entry: a module carries its own contract in a trailer
  stamped into the artifact, so there is no separate document to stage, and an
  environment that does stage the published `schema.json` adds it through
  `$E2E_ARTIFACTS` — a preflight that demanded it by default would have traded a
  stale name for a new false blocker.

  What closes the class is a sweep rather than a fourth per-consumer guard:
  every committed script, Makefile recipe and CI workflow, whole-line comments
  dropped, checked against the filenames the artifact model has deleted. These
  are the blind spot by construction — a filename there is an unchecked string in
  a file no test binary loads and no compiler reads — and the sweep covers a
  consumer the day it is committed instead of the day it breaks (`NIM-520`).

### Removed

- **`config.VaultInputFloor`** — a list of literal Vault path prefixes guarding
  operator input. It spelled the default mount, so a deployment with a
  non-default `vault.kv_mount` lost the floor entirely, and it named neither
  `herald` nor `provider`. Replaced by `config.PathUnderReservedNamespace`:
  mount-agnostic, comparing whole segments, and reading the same closed reserved
  list as every other surface (NIM-706).
- **`revealable_secrets` in `service.yml`.** The reveal endpoints, the
  `incarnation.view-secrets` right and the `incarnation.secret_revealed` audit
  event are unchanged — what goes is the author-written `vault_ref`, replaced by
  the derivation. The `vault_ref_not_service_scoped` diagnostic goes with it: the
  escalation class it fenced can no longer be expressed.
- **`no_log:` on a task.** It was all-or-nothing and set by the task author, who
  had to know the shape of a result the module produces — so it silenced a whole
  task's diagnostics to hide one field of it. A module now declares
  `secret: true` per **output field** (a plugin-protocol capability on
  `ApplyEvent.output`, only-add on the wire), and the platform masks exactly
  those wherever the output is observable, while the live register keeps the
  value for the next task. Writing the key is now `unknown_key` with a hint,
  and `no_log_on_block_invalid` / `no_log_on_apply_invalid` are gone with it.

### Upgrade notes

- **`type: secret` is breaking, deliberately, with no migration.** Derived paths
  do not match what existing incarnations wrote by hand, so an incarnation
  created before this change fails closed on its next day-2 run. This lands
  before the release.
- **An author-written path into a service's own namespace is refused**
  (`vault_path_in_own_namespace`) in all four spellings: `${ vault(...) }`, a
  `vault:` ref in `params:` or any other value, and the `path:` of
  `core.vault.kv-read` / `core.vault.kv-present`. At load when the path is written
  out; on the evaluated path when it is assembled from variables; and on the
  rendered params, in the keeper dispatcher before `mod.Apply`, for the two
  `core.vault.*` addresses that reach Vault through a parameter rather than
  through `vault()`. All four still work **outside** `<mount>/<service>/`, where a
  cross-namespace read (a shared TLS CA, another service's credential) is real and
  has no replacement yet.
- **Migration 116 drops `apply_run_plan.no_log`.** A scenario still carrying
  `no_log:` does not load; delete the key and let the module declare its own
  secret output.

Read this before upgrading a cluster that already has roles bound to operators.
Several changes alter what an existing grant means; some widen it, some narrow
it, and none of them require a role edit to take effect. The order below is the
order to act in.

- **Upgrade Souls before Keeper.** Two gates now refuse a host rather than let it
  mis-apply: the per-host capability gate (`soul_capability_unsupported` — the
  agent never announced a core module or DSL feature the rendered plan uses) and
  param-level strictness (`module.unknown_param` — the agent's compiled manifest
  does not declare a key the task carries). Both are the deliberate replacement
  for an old agent reading the params it recognizes and reporting OK/CHANGED
  while the thing the author asked for never happened. A Keeper newer than its
  fleet therefore refuses runs it would previously have mis-executed; a fleet
  newer than its Keeper is fine.

  A third gate on the same axis covers `dry_run` on `POST /v1/souls/{sid}/exec`
  and its MCP twin `keeper.soul.errand.run`:
  the target must announce the `dry_run` capability, or the request is refused
  with `409 soul-capability-unsupported` before dispatch. An agent that predates
  the flag ignores it and runs `Apply`, so an operator who asked only to read the
  host would have had a shell command executed for real. Practically nothing
  working stops working — on a current agent every `dry_run` Errand already ends
  as `FAILED errand_dry_run_unsupported` — but a dispatch that used to reach an
  old agent now fails loudly instead of quietly mutating it. A target with no
  session lease still answers `404` as before: an absent presence record is what
  the capability check sees for a host that was never connected, so the refusal is
  cross-checked against the lease rather than blaming that host's binary.

- **A `kind=command` Voyage with `dry_run: true` starts previewing instead of
  applying.** Until this release the flag never reached the hosts, so such a run
  applied for real; it now does what it says. Two consequences to expect on the
  first run after the upgrade, both per-host and both counted by
  `max_failures` / `on_failure` like any other failure: a target whose Soul does
  not announce the `dry_run` capability fails on its row instead of applying, and a
  module the runner does not admit on the `Plan` path fails with
  `errand_dry_run_unsupported`. A recurring `kind=command` Voyage that has been
  *relied on* to change hosts while carrying `dry_run: true` will stop changing
  them — drop the flag there. Recorded runs from before the upgrade did modify
  their hosts, whatever the row and the UI say.

  On the same axis: `dry_run` together with `core.cmd.shell` or `core.exec.run` is
  now refused with `400` on `POST /v1/souls/{sid}/exec`, its MCP twin, and Voyage
  create and preview. The two paths used to differ, and neither was right. On the
  single-SID path the pair was accepted and answered `200` carrying `failed` +
  `errand_dry_run_unsupported`, so a caller that read only the status code saw a
  preview that never ran. On the Voyage path the flag was dropped along with
  everything else described above, so the pair was answered `202` and then **ran
  the command line for real on every resolved host** — no `dry_run` refusal
  appeared anywhere, because nothing downstream was ever told it was a preview.
  Nothing that legitimately worked stops working; the pair has never been
  previewable.

- **Seven new permissions land inside `<resource>.*` grants you already issued.**
  The catalog is closed and a wildcard in the action position expands to every
  known action of that resource, so a role written before this release grants
  more after it. Same mechanism as `incarnation.*` when `incarnation.view-secrets`
  landed ([ADR-0070](docs/adr/0070-secret-reveal-path.md)); it is a property of
  the catalog, not a defect. What changes:

  - **`soul.*` now grants `soul.console`** — an interactive PTY on a host,
    running as the Soul daemon's user, typically **root**. It is not only the
    live terminal: the same right covers the MCP tool `keeper.soul.run-command`
    and playback of **anyone's** recorded sessions over
    `GET /v1/console/recordings…`. It is strictly stronger than `errand.run` and
    independent of it in both directions.
  - **`soul.*` now also grants `soul.forget`** — erasing a host from the
    registry, and with it more than the row named: every foreign key on
    `souls(sid)` cascades, so the host's SoulSeeds, bootstrap tokens,
    incarnation memberships and Choir Voices go with it. It is the only
    irreversible action on the resource and it is not gated on the host's
    status, so a wildcard holder who could previously only read and relabel a
    running host can now remove it. A role that must not be able to do that
    enumerates actions instead of the wildcard. Narrow the grant with `host=`,
    **not** `coven=`: the route puts only `host` into the RBAC context, a missing
    dimension fails closed, so a coven-narrowed `soul.forget` denies every call
    rather than restricting it to that coven. Both ways of attaching a coven
    deny: the `on coven=…` suffix, and a bare `soul.forget` in a role whose
    `default_scope` is a coven (a role's `default_scope` is inherited by its
    bare permissions). `soul.issue-token` and `soul.ssh-target-update` — the
    other two routes on the same selector — behave the same way; `soul.console`
    does **not** (its scope is applied inside the handler, so a coven there
    narrows as written). NIM-588 tracks making `coven=` narrow rather than deny
    on the three.
  - **An unrestricted `role.*` now grants `role.create-root`** — minting a role
    that tracks no parent, i.e. privilege that outlives whatever its author
    held — **and `role.list-all`**, which returns the whole role catalog: every
    role's permission set, scope and the AIDs holding it, which is the cluster's
    privilege map. Both are checked bare, so a *scoped* `role.*` covers neither:
    the grammar has no `role=` dimension to narrow them on, and an unrestricted
    holder is the only sound reading.
  - **An unrestricted `synod.*` now grants `synod.list-all`**, which returns the
    whole group catalog: every group's role bundle and its member roster, i.e.
    which packages of privilege exist and who holds them. The mirror of
    `role.list-all` and checked bare for the same reason — the grammar has no
    `synod=` dimension — so a *scoped* `synod.*` covers it no more than a scoped
    `role.*` covers the role catalog.
  - **`incarnation.*` now grants `incarnation.bind-member` /
    `incarnation.unbind-member`.** Under ADR-080 that is a visibility operation,
    not just bookkeeping: binding a host into an incarnation gives it that
    incarnation's labels, so it moves the host into the scope of every role
    scoped to them. The widening is bounded — the handler applies a second,
    per-host gate requiring every target SID to be inside the caller's own soul
    visibility, all-or-nothing and never a silent trim, because a scope predicate
    on `incarnation=` is satisfied without ever looking at the host. Unbinding is
    grantable separately from binding: it drops a host out of the roster of every
    future run.

  A role that must not gain these enumerates actions instead of the wildcard.
  The full `soul.*` expansion as of this release is `soul.list`, `soul.create`,
  `soul.issue-token`, `soul.coven-assign`, `soul.traits-assign`,
  `soul.ssh-target-update`, `soul.console`, `soul.forget`. That list, and the `role.*`,
  `synod.*` and `incarnation.*` ones, are pinned against
  [the catalog](keeper/internal/rbac/catalog.go) by
  `TestCatalog_WildcardRostersPinnedForReleaseNotes`, so an action added later
  fails a test rather than aging this paragraph in silence.

  `setting.read` / `setting.update` / `setting.delete` are a **new resource**, so
  no `<resource>.*` covers them — only an unrestricted `*` does.

- **Consoles are enabled by default, and switching them off cluster-wide is now
  one statement.** `console: {enabled: false}` in `keeper.yml` — or the
  `cfg_console_enabled` row, since the key is served from the SettingsStore
  overlay — removes **both halves** of the console plane: `GET /v1/console`
  answers **404**, and the MCP tool `keeper.soul.run-command` disappears from
  `tools/list` and answers "tool not found". Sessions already open are closed.
  Omitting the key means `true`, so a `keeper.yml` written before this release
  behaves exactly as it did.

  It is 404 and not 403 on purpose: a 403 would confirm that the cluster has a
  console plane, which is the question a console-free cluster should not be
  answering. Both halves go together because `soul.console` is one privilege
  reached two ways — a switch that closed the WebSocket and left the agent-facing
  tool live would read as a guarantee it does not give.

  **Recorded sessions stay readable.** `GET /v1/console/recordings…` is outside
  the switch: a recording is evidence, and turning consoles off is a decision
  about new sessions, not a way to take last week's root shells away from an
  auditor. Recording itself remains mandatory and has no key at all.

  Two things this does **not** do. It does not close the Errand path —
  `core.cmd.shell` / `core.exec.run` through an Errand, a Voyage or a Cadence has
  its own gate (`console.errand_shell_gate`) and is untouched, so "no console
  plane here" is not "no root shells here". And the host-side `console:
  {enabled: false}` in `soul.yml` gates interactive opens only: a host carrying
  that flag still executes `keeper.soul.run-command`. The Keeper-side switch is
  the one that covers both halves.

  If you rely on the guarantee, pin it in `keeper.yml` rather than in Postgres:
  the file outranks the cluster row, so `setting.update` — which can otherwise
  switch the plane back on — cannot reach a value written in the file.

- **A scoped role is now refused where it used to pass.** `Enforcer.Check` did
  not apply a role's `default_scope` to the bare permissions under it, so a role
  carrying `default_scope: coven=dba` was confined on every read and
  **unbounded on the write path**. It is now matched through its role, which is
  what `ResolvePurview` always did. This expands denials by design, including for
  a context-less cluster operation, and a route that gated a scoped-capable
  action with a context-less check was mis-gated before and now says so. Read
  routes are unaffected — they gate on existence and narrow in the handler.

- **A `coven=`-scoped `soul.forget` / `soul.issue-token` / `soul.ssh-target-update`
  now reaches hosts, where it used to reach none.** These three mutations put only
  the path SID into the RBAC context, and a dimension the context does not carry
  fails closed — so a grant narrowed by `coven=<label>` refused **every** call,
  including one aimed at a host that really was in that coven, and the 403 named a
  permission the operator demonstrably held. The gate now reads the host's own
  `souls.coven` list first and asks the enforcer once per label, admitting on any
  of them, so the grant means what it reads as. **This widens what an existing
  role can do** without a role edit: review any role that carries one of the three
  narrowed by a coven — it was inert and is now live over that coven's hosts, and
  in the case of `soul.forget` that is a destructive right. Both forms of coven are
  covered, the `on coven=…` suffix and a bare permission under a role whose
  `default_scope` is a coven. Grants written `on host=` are unchanged, and a host
  whose row cannot be read (unknown SID, database unreachable) still asserts only
  the host, so nothing widens when Postgres is down. `errand.run` and the live
  `soul.console` keep the old shape — a `coven=` on either still denies.

- **`GET /v1/roles` and `keeper.role.list` no longer return the whole catalog.**
  A caller sees a role exactly when the caller could grant what that role grants.
  A reader who must see roles they hold nothing of — an auditor, a security
  review — needs `role.list-all` **in addition to** `role.list`; because the
  grammar has no `role=` dimension, only an unrestricted holder gets the full
  catalog.

- **`GET /v1/synods` and `keeper.synod.list` no longer return the whole catalog.**
  A caller sees a group exactly when it could add someone to it — that is, when
  it covers the effective rights of every role the group bundles, which is the
  rule `synod.add-operator` already enforced, read backwards. A group that
  bundles nothing stays visible to everyone, and a visible group comes back
  whole, roster included. An auditor needs `synod.list-all` **in addition to**
  `synod.list`; without it, a reader granted the full role catalog was still
  blind to the groups those roles are bundled into.

- **Editing or deleting a role now requires being able to grant what it grants,
  and so does taking a binding apart.** `role.update` and `role.delete` were
  cluster-level: any holder could rewrite or drop any non-builtin role, and
  `role.delete` took no caller at all. That was never escalation — removing
  rights grants nothing, which is why trimming had been free — but it left
  demolition open, and self-lockout only notices when the last `*` admin would
  go. A caller may now administer a role exactly when it could grant what that
  role grants, which makes every holder of a role the administrator of the roles
  derived from it, leaves plain roles to whoever could have created them, and
  leaves an empty role administrable by anyone.

  **This is the one that breaks working automation:** trimming or deleting a role
  whose rights you do not hold now answers 403, where it used to succeed — unless
  the mutation would also leave the cluster with no administrator, in which case
  the self-lockout guard answers `409 would-lock-out-cluster` first, as it did
  before this release. It runs ahead of every caller-side check
  ([ADR-078 §n](docs/adr/0078-rbac-derived-roles.md)): it is the one refusal no
  operator can satisfy by holding more rights.
  `role.revoke-operator`, `synod.remove-operator` and `synod.revoke-role` carried
  no caller at all and are gated the same way — each revoke asks for exactly what
  its matching grant asks for, so unbinding a role weighs that role's rights and
  removing a group member weighs the group's whole bundle. An unrestricted `*` is
  unaffected throughout.

- **Creating a parentless role that grants something now needs
  `role.create-root`.** The default is to derive from a role you hold and let the
  cascade do the rest. The gate judges the shape of the result, not the verb, so
  a `PATCH` that clears `parent_role` — or grows an already-plain role — is gated
  too. Automation that mints roles from a service account will get a 403 until
  that account holds the right or the roles are re-parented.

- **A role mutation that moves the effective rights of any role below it answers
  409 unless the request carries `confirm_cascade`.** Both directions, any depth;
  the refusal names the derived roles that gain rights, the ones that lose them,
  and how many operators hold each. A change nobody below feels still passes
  silently.

  **One case reports the edited role itself: a `PATCH` that takes its parent
  away.** There the permission rows in the request can be identical to the stored
  ones and the rights still move, because the ceiling they resolved under is
  gone — so everyone holding that role gains access nobody granted them, and the
  operator had no way to learn how many that is. Un-parenting therefore answers
  409 until confirmed, even for a role with no children of its own; growing a
  role is still never self-reported, and an un-parenting that widens nothing —
  an unrestricted parent, or a delta that already stated the whole ceiling — is
  not reported either.

- **A label is never inherited, and a rule's subject now names its dimension
  explicitly** ([ADR-008 amendment 2026-08-05](docs/adr/0008-coven-stable-tags.md),
  [ADR-0080 reverted](docs/adr/0080-label-inheritance-union.md), migration 113).
  A host carries exactly the covens and traits an operator attached to it;
  belonging to an incarnation attaches nothing, at read time as much as in
  storage. Every reader says the same thing now — the RBAC scope predicate,
  `soulprint.self.covens` / `.traits`, `GET /v1/souls?coven=`, the bulk soul
  selector, push routing level 2, and Oracle / Augur subjects.

  **Measured against `v0.1.0-beta.1` this is a narrowing, because membership used
  to BE a coven tag.** NIM-124 moved host↔incarnation membership into the
  `incarnation_membership` relation, so anything written `coven: [<incarnation>]`
  in beta.1 selected that incarnation's hosts and now selects only hosts an
  operator tagged with that literal string — usually none. Two places where that
  changes behaviour rather than just counts:

  - **`push.coven_default_providers` level-2 entries fall through to the cluster
    default**, moving the SSH perimeter with no error. Re-read your routing table
    before upgrading.
  - **An Augur `coven`-Rite naming an incarnation stops authorizing its members.**
    A subject matching no Rite is default-denied, so those hosts fail the Augur
    step mid-run rather than going quiet.

  **The replacement is an explicit dimension, not a tag that means two things.**
  The subject of a Vigil, a Decree and a Rite is now exactly one of `sid` /
  `incarnation` / `coven` / `trait`, carried as one nested `subject` object on
  REST and MCP alike (breaking wire change, below). `incarnation: {service, name}`
  addresses a roster by membership. `coven` and `trait` read **both levels**: the
  rule reaches a host carrying the label **and** every member of an incarnation
  carrying it — the union happens at match time, and nothing is written onto
  `souls`.

  ⚠ **Labelling an incarnation is therefore a grant-affecting act.** Adding `prod`
  to an incarnation widens every existing `coven: ["prod"]` subject to its members
  with no rule edited — on a Rite, that hands out a secret; and unbinding a host
  withdraws every rule that reached it that way, on the next request. The
  permissions that can do it are `incarnation.traits-set` and
  `incarnation.bind-member`, so the audit trail records an incarnation change, not
  a grant change. ★ The widening is **targeting only** — an operator's RBAC
  coven-scope still reads a host's own labels and is unaffected.

  `coven=` and `incarnation=` remain different questions and neither subsumes the
  other: `coven=` is a **label** test, `incarnation=` is a **membership** test and
  keeps reading `incarnation_membership`.

- **A `trait.<key>=<value>` RBAC scope value now names a WHOLE value, and every
  reader agrees on which texts that is** ([ADR-047 amendment
  2026-08-08](docs/adr/0047-purview.md), NIM-522 / NIM-521 / NIM-529). The trait
  arm of the scope predicate used to be two operators OR'd together, and the
  second of them — jsonb's `?|` — is not a value test at all: it matches an
  object's **keys** and an array's **string** elements. So `trait.tier=k` reached
  a host whose `tier` was `{"k": "gold"}` (naming a key granted the host),
  `trait.ports=6379` did **not** reach `{"ports": [6379, 6380]}` (a number in a
  list was addressable by nothing), and `trait.ports="[6379, 6380]"` **did** —
  through the container's own rendered text, which was that host's only address.

  The rule is now one rule: a stored **string / number / bool** is reached by its
  own text, an **array** by each of its **scalar** elements whatever their JSON
  type, and an **object**, a **JSON null** and any **container's own text** by
  nothing.

  **What to re-read before upgrading.** Both directions take effect with no role
  edited:

  - **Narrowing (the security-relevant one).** A role scoped on a key whose value
    is an object loses those hosts, and so does one written against a container's
    text (`trait.ports="[6379, 6380]"`). Grep your roles for a scope value
    containing `[`, `{` or a `:`-bearing word — those are the ones that were
    addressing a container or a key.
  - **Widening.** A role scoped `trait.<k>=<v>` now also reaches hosts whose `<k>`
    is a **list containing `<v>` as a number or a bool**. It always reached a list
    containing it as a string.

  **The single-row read stops disagreeing with the list.** The in-Go half of the
  same boundary projected traits from a decoded map and stringified with `fmt`,
  so `1000000` became `1e+06` and `0.0000001` became `1e-07` — texts no scope
  names. A host with a numeric trait therefore appeared in `GET /v1/souls` and
  then `404`ed on `GET /v1/souls/{sid}`. The projection now reads Postgres' raw
  jsonb and takes the texts verbatim; an unreadable one yields no traits and the
  condition fails closed.

  **The trait WRITE gate speaks the same texts**, on both of its surfaces. `POST
  /v1/souls/traits` and `keeper.soul.traits-assign` each carried their own
  hand-written rendering of the pair being stamped, both claiming in a comment to
  match Postgres' `->>` and neither doing so. Stamping `{"asn": 1000000}` was
  refused as the pair `asn=1e+06` to an operator holding `trait.asn=1000000`.
  The same slip also ran the other way, and that direction **leaked**: an
  operator scoped `trait.tier=1e+06` — a text Postgres stores for nothing — was
  admitted to stamp `{"tier": 1000000}`, a pair their scope does not cover. Both
  surfaces now render through one function over the payload's canonical jsonb, so
  a pair an operator may attach is exactly a pair their scope reaches.

- **Labelling an incarnation is now gated by the label, not only by the
  incarnation** (NIM-587). `trait.<key>` is a live scope dimension for
  incarnations exactly as it is for hosts, so a pair stamped on an incarnation
  grants the same visibility a pair stamped on a host grants — and yet `PUT
  /v1/incarnations/{name}/traits` and `keeper.incarnation.traits-set` asked only
  whether the caller held the incarnation. An operator scoped
  `incarnation.traits-set on coven=dba` could stamp `tier=gold` on an incarnation
  in that coven and hand every `trait.tier="gold"` role sight of it — and,
  through a `trait` Rite, of its members — while holding no such pair itself.
  Both surfaces now screen through the same function as the two soul surfaces,
  over the payload's canonical jsonb.

  **What to re-read before upgrading.** A role whose `incarnation.traits-set` is
  scoped on any dimension OTHER than `trait.` — `coven=`, `service=`,
  `incarnation=` — has an EMPTY trait-scope and is now refused every pair with
  `422 trait <k>=<v> is outside operator trait-scope`; an empty payload, which
  only clears labels, still passes. That is the rule the soul surfaces already
  applied, reaching the surface that was missing it, not a new one. A **bare**
  `incarnation.traits-set` with no `default_scope` stays unrestricted and is
  unaffected. To keep a scoped role able to label, give it a separate permission
  constraining trait ALONE (`incarnation.traits-set on trait.tier="gold"`) — a
  disjunct mixing trait with another dimension contributes nothing, by the same
  fail-closed rule that governs `coven`. ⚠ Note that such a grant also **widens
  gate (a)**: scope is per `(resource, action)`, so "may stamp this label" and
  "may write to objects carrying it" cannot presently be separated.

  **Create is deliberately out of scope.** `POST /v1/incarnations` and
  `keeper.incarnation.create` still accept any well-formed trait, so an operator
  refused a label on `traits-set` can still carry it at birth. Whether a create
  is refused or the creator is taken to hold what it stamped is an open
  permissions decision (NIM-622); both create surfaces move together when it
  lands.

- **`POST /v1/incarnations/{name}/scenarios/{scenario}` can now answer `422
  assert_failed` synchronously**, where it previously always answered `202` and
  surfaced a failed topology assert as `error_locked` plus a manual unlock. The
  MCP twin behaves the same. The run does not start: no `apply_id`, no
  `applying`, nothing to unlock. A plan that builds its own roster is still
  admitted on an empty roster — the gate defers exactly when it cannot read what
  the assert reads. **Clients that treat any non-202 from this route as a
  transport error need updating.**

- **`soul-lint` rejects five `async:` / `require:` / `when:` shapes it used to
  accept**, so a definition that linted clean can now fail:
  `require_forward_reference` (a barrier naming a source that starts later
  resolves to nothing and waits for nothing), `async_on_apply_invalid` (an
  applier task fans out into a group that runs sequentially regardless),
  `apply_when_dynamic_unsupported` (below), `async_on_keeper_invalid` (`async:`
  next to `on: keeper`, which render already refused — the refusal just moves to
  lint time), and a `require:` on a `block:`, which was documented as inherited
  and was in fact dropped from the plan — now merged like the other block keys.
  Each was silent before: the key parsed, the plan rendered, the run succeeded,
  and the ordering the author wrote never happened.

- **A `when:` on an `apply:` task must be static, and a dynamic one now fails the
  lint** (`apply_when_dynamic_unsupported`). This is the one entry here that
  changes what a *correct-looking* definition does, so read it even if you write
  no `async:`.

  An applier's condition is answered **Keeper-side, before its destiny is
  rendered**. A static predicate (`input.` / `essence.` / `vars.` /
  `incarnation.`) has one answer for the whole run and keeps working exactly as
  before — false collapses the applier into a single skip placeholder carrying
  its `register:`. A predicate reading `register.*` or `soulprint.*` has no such
  answer: `soulprint` differs per host and `register` does not exist yet at
  render, while the applier expands into **one** group for the whole roster.
  There is nowhere to put "yes" and "no" at once.

  **What was happening instead:** the key was dropped and the destiny applied
  **everywhere the applier targeted — including the hosts the author had gated
  off**. That is configuration written where it was refused, and nothing in the
  run said so. A run that looked clean was not.

  **What to do:** the two replacements already work, and the diagnostic names
  them. A host-variant condition is `where:` — it is the per-host targeting key
  and reads both `register` and `soulprint`. A dependency on a source's outcome
  is `onchanges:` / `onfail:`, which this release also makes reach the group (see
  below).

  ```yaml
  # before — silently applied on every host
  - apply: { destiny: redis, input: {} }
    when: soulprint.self.os.family == 'debian'

  # after
  - apply: { destiny: redis, input: {} }
    where: soulprint.self.os.family == 'debian'
  ```

  The restriction is on `apply:` only. The same `when:` on an ordinary `module:`
  task is untouched and entirely legal — a module task is one task, gated
  Soul-side at its own plan position.

- **An applier's `onchanges:` / `onfail:` / `require:` now reach the destiny it
  applies, and can widen gating that was previously not applied at all.** They
  were dropped before — the render function that expands an applier never
  received the task carrying them — so a scenario whose applier declared a
  requisite ran the group unconditionally. Two consequences on upgrade: an
  applier that names a source now genuinely waits for / gates on it, and because
  the merge is a **union** (as on a `block:`) with `onchanges:` composing as OR,
  an applier-level `onchanges:` **widens** the gating of a destiny task that
  already had its own. Expressing AND needs a wire change and is tracked
  separately; the behaviour matches the `block:` construct that already ships.

- **A requisite naming a `loop:` register now covers every iteration, not the
  last one.** A `loop:` fans out at render into N tasks sharing one `register:`
  ([destiny/tasks.md §7](docs/destiny/tasks.md)), but the name resolved to a
  single task index — the final iteration. So `onchanges: [<loop-register>]` meant
  "if the **last** file changed" rather than "if any did", and a `require:`
  barrier released while its siblings were still writing. Gating that was too
  narrow becomes correct, which means a task that used to be skipped may now run:
  that is the documented contract taking effect, not a new one.

- **A plugin module's `params:` are now checked statically, so a definition that
  linted clean can fail.** Until now the static check covered `core.*` only and
  returned on the first line for any other namespace — an author got no
  diagnostic at all for a plugin task, and an undeclared key was discovered as
  `module.unknown_param` on a host. The four checks (`unknown_param`,
  `missing_required_param`, `param_type_mismatch`, `module_state_unknown`) now
  run for any module whose manifest resolves.

  **What breaks:** a definition passing a key the plugin's manifest does not
  declare. It already failed on the host under the plugin strictness above — this
  moves the failure to lint time, with a line and column, which is the point.
  **What to do:** run `soul-lint validate-scenario … --modules <dir>` over your
  definitions before upgrading Keeper, and fix what it names. There is no
  deprecation window and none is needed: the lint runs where the definition is
  authored, not on a host, so a definition that starts failing is fixed in the
  same session that reported it.

  **Silence now means something.** Where a manifest does **not** resolve — no
  `--modules`, or a plugin the cluster has not allow-listed — you get
  `plugin_params_unchecked`, a hint, once per module address. It fails nothing;
  it exists because "no diagnostics" used to mean either "checked and clean" or
  "never looked", and those are different answers.

- **Keys absent from your `keeper.yml` are now settable cluster-wide over the
  API.** Precedence is built-in default < Postgres < `keeper.yml`, so a file that
  names a key still wins and nothing you have configured changes meaning. But
  23 keys that your file leaves unset can now be changed for the whole cluster by
  a holder of `setting.update` — which `*` covers. The catalog reports
  `source=file` with `cluster_value` and `overridden_locally` where a file
  shadows a cluster value, so the cost of that order is visible rather than
  silent. `audit:` is deliberately outside the overlay.

- **`errand.run` alone no longer reaches an arbitrary shell — in `warn` mode this
  release, `enforce` the next.** See *Security* below for the two numbers to
  check before the flip, and alert on `cadence.skipped_forbidden`: a schedule
  written under the old rule stops producing runs on a timer with nobody looking.

- **Plugin manifests now gate their params.** See *Changed* below; a plugin whose
  manifest under-declares fails the task instead of quietly doing its old job,
  and the fix ships as a new plugin version, since the manifest that gates is the
  one beside the binary on the host.

- **Migrations 101–113 apply automatically when the first Keeper of this version
  starts** — `apply_runs.input` (101), `rbac_roles.parent_role` (102), engine
  provenance columns (103), `console_recordings` (104), `rbac_roles.scope_mode`
  (105), the prune of projected traits (106), `apply_runs.notices` (107), the
  removal of `incarnation.spec.hosts` (108), of `permission.update-hosts` (109),
  of `spec.essence` (110), `state_history.run` (111), the removal of
  `incarnation.spec` itself (112), and the four-dimension subject (113). **Three
  rewrite data.** 106 removes from `souls.traits` the residue of the old
  materialized projection, by value equality against the host's incarnations — a
  pair sharing only a key is host-local and stays. With label inheritance gone
  (above) that prune is a **real narrowing, not a change of storage**: a trait a
  host held only because its incarnation held it is not restored by any read-time
  union, which is the intended end state — an operator who wants it on the host
  attaches it to the host. 110 and 112 strip keys out of existing `spec` jsonb,
  and 113 reshapes the three subject-bearing registries. Take the usual snapshot
  first.

- **The shipped systemd units are `Type=notify`** with `NotifyAccess=main`,
  `WatchdogSec=60s` and an `ExecReload` that `systemctl reload` previously had
  nothing to run. Enablement in the binary is runtime autodetect from
  `NOTIFY_SOCKET` / `WATCHDOG_USEC`, so a locally-modified `Type=simple` unit
  keeps working unchanged — it simply gets no readiness gating and no watchdog.
  `StartLimitIntervalSec` / `StartLimitBurst` moved to `[Unit]`, where systemd
  reads them; in `[Service]` the restart limiter was dead in both units.

- **The bundled `redis` example service installs from a package repository by
  default.** `essence.install_method` defaults to `package` and the addresses
  live in one `install` map in essence; `install_method=binary` previously
  pointed at `nexus.example.com`, an RFC 2606 placeholder that resolves nowhere,
  so that path has been dead for everyone since `v0.1.0-beta.1`. A fleet on an
  internal mirror overrides the one map in `spec.essence`.

- **Telemetry delivery was reading host↔incarnation membership off the raw
  `souls.coven[]` column, and had been fully dead since NIM-124 emptied it.** It
  resolved a host's incarnation with `FROM incarnation WHERE name = ANY(<the
  host's covens>)`, so the effective telemetry config
  ([ADR-072](docs/adr/0072-host-utilization.md)) reached **no host of any
  incarnation**, everything ran on soul-local defaults, and nothing said so.
  Expect member hosts to pick up their service's declared interval and collector
  set on their next connect: for a fleet that has been running since NIM-124 this
  is a **change in collection cadence**, in the direction the service manifest
  always asked for. If a host belongs to several incarnations only one cadence can
  win — the first by name, now logged at WARN with the full membership list so the
  choice is visible.

- **Creating an incarnation from a `name_template` no longer requires an
  unrestricted role, and a scoped role is now gated on what the request declares**
  ([ADR-0079](docs/adr/0079-incarnation-name-template.md) amendment 2026-07-30).
  Under a template the name is composed server-side, so at gate time it does not
  exist — the create gate scoped on `incarnation=<name>`, got nothing, and admitted
  only bare/`*`. With templating becoming the primary way to name an incarnation,
  that made creation the privilege of a cluster-admin. It is now scoped on
  `service=` and the declared `coven=` instead, which the request does carry.

  **What changes for existing roles.** A role scoped by `coven=` or `service=` gains
  templated create — it could not do it before, so nothing it used to do stops
  working. A role scoped **only** by `incarnation=` still cannot create a templated
  incarnation: that dimension is unanswerable before composition, and it stays
  fail-closed. If you have such a role and it needs to create, add a `service=` or
  `coven=` dimension to it.

  **And this closes an escalation, on every create — named ones included.** The
  pre-handler gate is an OR over the declared covens: a request naming one coven you
  hold and one you do not matched on the first and was created **carrying both**.
  Because a `coven` subject reaches every member of an incarnation carrying that
  label, that handed the incarnation's hosts to Vigils, Decrees and Rites written
  by the owner of a coven the caller cannot reach — an Augur Rite among them, and
  a secret with it. It predates
  `name_template` entirely — a named create has always been able to do it.

  Every declared coven is now checked once the effective name is final, and the
  request is refused **whole** rather than trimmed to the part you may have. **A role
  that used to create incarnations declaring a coven outside its scope will start
  getting 403** — the fix is to declare only covens the role covers, or to widen the
  role deliberately. Check your automation's `covens` against its role before
  upgrading; this is the one change here that can break something that worked.

- **`PATCH /v1/incarnations/{name}/hosts` is gone, and two permissions go with
  it — check your roles before upgrading.** The field it edited,
  `incarnation.spec.hosts[]`, is removed whole
  ([ADR-044 amendment 2026-07-30](docs/adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)).
  The endpoint answers 404, the audit event `incarnation.hosts_updated` is
  retired, and `incarnation.update-hosts` together with its deprecated alias
  `incarnation.update` leave the permission catalog.

  **This one cannot be left to sort itself out.** The catalog is a closed enum and
  the enforcer is fail-closed: a role still carrying either string would fail to
  parse, and a failed parse aborts the whole RBAC snapshot load rather than
  dropping one grant. Migration `109_drop_permission_update_hosts` deletes those
  rows for you — bare and scoped forms alike (`… on coven=prod`) — so a normal
  migrated upgrade is safe. A cluster whose roles are restored or hand-edited
  around the migration is not; grep `rbac_role_permissions` for the two names if
  you manage that table yourself. `incarnation.*` grants are unaffected: a
  wildcard expands over the catalog at load time and is simply two actions
  shorter.

  **Nothing is lost functionally.** The field never carried the roster — a run's
  hosts are `incarnation_membership`, and `POST /v1/incarnations` never accepted
  `spec.hosts` — and its `role` had been superseded by the Choir Voice since
  [ADR-044](docs/adr/0044-choir.md) item 2, surviving only as a fallback for
  bootstrap-`create`. That fallback stopped covering anything when the keeper-side
  `core.choir` module landed: a create scenario now writes its own Voices with a
  `core.choir.present` step (`on: keeper`, params `incarnation` / `choir` / `sid` /
  `role` / `position`) before any task reads a role, and day-2 the same is
  `POST /v1/incarnations/{name}/choirs/{choir}/voices`. **A host that no Voice
  places into a part has an empty declared role** — not a default group — which
  was already the rule for hosts outside the declared spec.

  Migration `108_drop_incarnation_spec_hosts` strips the `hosts` key out of
  existing `spec` jsonb, so `GET /v1/incarnations/{name}` stops advertising a
  topology nothing reads. `soulprint.hosts[].role` / `soulprint.self.role` are
  unchanged in the scenario context — they are simply fed from one source now. The
  **actual** role is untouched and still comes only from a live probe +
  `register:` + `where:`.

  The alias-canonicalization machinery in `ParsePermission` is removed along with
  the only alias it served. A future rename follows the pattern of migration
  `095`: drop the old name and migrate the rows.

- **`incarnation.spec` is removed from the API.** `GET /v1/incarnations/{name}`,
  the list endpoint and the MCP twins stop returning a `spec` field, and the
  column is dropped (migration `112`). It was never a designed store: it was the
  create REQUEST, persisted, and every key that turned out to matter was given a
  real home outside it while the copy stayed behind and started lying —
  `hosts[]` to `incarnation_membership` and then to the Voice's role, `traits` to
  the `incarnation.traits` column (where a day-2 `PUT .../traits` edited only the
  column, so `spec.traits` went stale on the first edit), `essence` to nothing.

  **What was `spec.input` is now per-attempt.** Each run's replayable snapshot —
  the operator's input AS-IS plus the git coordinates it rendered with — is
  stamped into that run's own `state_history` row (migration `111`), together
  with the outcome it reached. A client that read `spec.input` to learn "what was
  this created with" reads the incarnation's history instead, where the answer
  cannot silently diverge from what actually ran. It did diverge: caught live
  during NIM-330 on a fixture whose `spec` said one thing and whose recipe said
  another.

  **`rerun-last` therefore replays the service version the attempt USED**, not
  whatever the incarnation is pinned to now. An upgrade between the failure and
  the retry used to substitute the new code silently. It also has one source
  instead of two: the create path read `spec.input` and the day-2 path read
  `apply_runs.recipe`, whose 30-day purge made a day-2 rerun eventually
  impossible. Where an attempt has no snapshot (a terminal recorded without one,
  or a row predating the migration) the endpoint now accepts an `input` in the
  body — and REFUSES one when the attempt is replayable, because "rerun that" and
  "run this instead" are different requests.

  The history feed also stops showing the rerun-transition markers: they carry
  `state_before == state_after` and never gain an outcome, so among state changes
  they read as a run that happened and changed nothing — which is also what a
  failed run looks like.

- **Every service repository renames a directory and a file, and rewrites its
  `essence.` references.** `Essence` is retired: a service's default parameters
  move from `essence/` to **`vars/`**, the baseline layer `_default.yaml` becomes
  **`00-base.yaml`**, and the CEL root `essence.*` merges into **`vars.*`**
  ([ADR-0082](docs/adr/0082-service-vars.md)). One flat namespace now runs
  `<service>/vars/*.yaml` → destiny `vars.yml` → `block:` → task, outermost
  first, and a layer may reference the layers below it. Nothing renames itself:
  a repo that keeps `essence/` resolves to no vars at all, and every
  `${ essence.X }` becomes a compile error naming an undeclared reference — loud
  in both directions, which is the intent.

  **`incarnation.spec.essence` is removed with no replacement.** A fleet that
  needs different defaults **forks the service repo** and re-pins its
  `ServiceRef` — the ansible-role model. There is no override field on the
  incarnation and no successor to it; what an operator supplies is `input:`.
  Migration `110_drop_incarnation_spec_essence` strips the key out of existing
  `spec` jsonb. In practice it matches nothing: the field never had a writer.

  **The two per-host overlays are gone**, `essence/os/<family>.yaml` and
  `essence/coven/<label>.yaml`. Neither was used by any shipped service, and
  both were resolved once per run from the FIRST host of the roster — so on a
  mixed-OS roster they were already answering with somebody else's facts.
  Conditional assembly is now explicit, in **`vars/_stack.yaml`**
  (`file:` / `inline:` / `when:` / `optional:` / `foreach:` + `as:`, plus a
  per-step `strategy: deep|replace`), which was documented for a year and read by
  nothing. Its step context is `incarnation.*` (covens included) and the vars
  accumulated so far — deliberately no `soulprint.self`, so the layer is
  host-invariant by construction. A tag on a HOST alone no longer selects a
  service-vars overlay; a tag on the INCARNATION still does, through
  `foreach: "${ incarnation.covens }"`.

  **Upgrade Keeper first, then Souls — never the reverse.** Two context maps that cross the wire
  lose their `essence` key: `RenderedTask.flow_context` becomes
  `{input, vars, incarnation, self}` and `core.file.rendered`'s `render_context`
  becomes `{vars, self, role}` (plus its conditional `input`). No proto field
  number moves, so the forward-compat rule does not catch this — what changed is
  a key inside a `google.protobuf.Struct`. An older Soul against a newer Keeper
  reads `flow_context["essence"]`, finds nothing, and evaluates its flow-control
  predicates against an empty section rather than failing. The reverse ordering is
  worse and is the one a pull fleet with auto-updating agents drifts into on its
  own: a NEW Soul no longer DECLARES `essence` in its flow-control env, so an old
  Keeper's `when: essence.X` becomes an undeclared-reference compile error on the
  host, per task, after dispatch. A mixed-version fleet across this boundary is not
  supported in either direction.

  `GET /v1/incarnations/{name}` stops returning `spec.essence`. The trial fixture
  key `essence:` becomes `vars:`, and the run error code `essence_failed` becomes
  `service_vars_failed`.

- **Delete the `scry_background` block from `keeper.yml` before upgrading.**
  Its two rule fields — `max_concurrent_in_flight` and
  `min_interval_per_incarnation` — left the config schema with the rule, and
  keeper.yml is strict about unknown keys: an unrecognized field is an ERROR
  diagnostic and the daemon exits at config load, before it ever reaches the
  code that would have warned about an unknown rule *name*. So a config still
  carrying them does not degrade, it refuses to start. The error names the field
  and the YAML path, which is the whole remediation.

- **Drift check is gone, and one of its leftovers can stop a cluster from
  starting.** Migration `113` deletes every `incarnation.check-drift` grant. That
  is not tidiness: the RBAC catalog is a closed enum and the enforcer is
  fail-closed, so a single surviving row would fail the parse and keeper would
  refuse to start on its next cold load — losing the whole cluster's
  authorization, not one permission. The migration also strips
  `incarnation.drift_checked` out of Tiding subscriptions and **deletes whole**
  any rule that named only it, reporting both in `NOTICE` output: a notification
  rule that vanishes is something an operator must learn from the migration log
  rather than from a missing alert. Historical `audit_log` rows keep the event
  type forever; the type is out of the enum, so filtering by it now answers 422.

- **If your only cluster administrator holds `*` through an LDAP/OIDC group, that
  login can now fail instead of demoting them.** Federated role reconciliation
  ([ADR-058(d)](docs/adr/0058-operator-auth-ldap-oidc.md)) revokes the mapped roles
  a user's groups no longer carry, and it did so below the self-lockout guard: an
  identity provider answering with **fewer** groups than it should — an outage, a
  directory reorganisation, a group membership that has not propagated — stripped
  `*` from whoever logged in during it. Do that to the last administrator and the
  cluster has none, with no route back through the API.

  Such a revoke is now refused and the login fails with it, touching no membership.
  A failed login is recoverable by fixing the IdP or the map; an empty admin set is
  not. **Check your `group_role_map` before upgrading:** if a `*`-granting role is
  mapped to a group and no administrator exists outside the federated domain, an
  IdP hiccup that used to demote you silently will now lock you out of *logging in*
  until it is fixed. The configuration to keep is one administrator enrolled
  outside LDAP/OIDC — the `keeper init` bootstrap Archon (`auth_method=jwt`) already
  is one, and the federated mapper never serves it.

  An IdP returning **no** usable groups was already safe and is unchanged: such an
  identity is rejected before reconciliation begins, so nothing was ever revoked on
  that path. The case this closes is the partial answer.

### Added

- **`soul-lint` catches a `compute.*` that will not resolve, before the run.**
  Two rules over one walk of the scenario — its `compute:` block, its tasks
  (including `block:` children) and its `state_changes:`. `compute_out_of_scope`
  reports a `loop.items:`/`loop.when:`, an `on: [covens]` element, an `add:`
  operation's `match:`, or one of the four flow-control keys (`when:` /
  `changed_when:` / `failed_when:` / `retry.until:`) that references the namespace
  — the contexts that lack it and that the offline pass can see. The flow-control
  four are the ones an author is most likely to get wrong, because they sit on a
  task whose `params:` **do** read `compute.*`; the diagnostic names the Soul-side
  sandbox they actually run in and points at the one-line detour through the
  task's own `vars:`. `compute_unknown_name` covers the far
  more ordinary mistake, wherever the namespace *is* in scope:
  `compute.node_cont` for a declared `node_count`, and a reference to an entry
  declared **further down** the block (entries resolve top to bottom, so a forward
  reference has no value yet). The diagnostic lists the names that do exist. Until
  now nothing caught any of this statically: the linter had no notion of `compute`
  references at all, so the first sign was a failed run.

  Both rules ask the CEL parser rather than a regex, so prose in a param, a coven
  label like `compute-cluster` and an `input.compute_timeout` are not flagged; a
  reference whose name is not in the source (`compute[input.key]`,
  `size(compute)`) is left to the run rather than guessed at; and a child of an
  `on: keeper` `block:` is not treated as keeper-side (`on:` is not inherited by
  block children). Three things stay off the offline pass and are caught by the
  run instead: a task spliced in by `include:` (the rule sees the scenario's own
  task list, and the include is resolved on the keeper — a pre-existing limit of
  every per-task rule, not of this one, NIM-655); the isolated destiny pass (the
  destiny linter is handed `destiny.yml`, whose tasks live in a file it never
  receives); and the name rule when an `extends:` covenant failed to resolve,
  since the merged `compute:` block is then unknown and every name would look
  undeclared. A clean `soul-lint` is a narrower statement than a clean run.

- **The composed incarnation name, previewed before it is permanent**
  ([ADR-0079 (g)](docs/adr/0079-incarnation-name-template.md)). When a create
  scenario declares `name_template`, the name is assembled server-side from the
  `input:` components and the request must not carry a `name` — so until now the
  operator first saw it in the refusal or in the row. The name is the immutable
  primary key with no rename, which makes a wrong one cost a destroy and a
  re-create. `POST /v1/incarnations/resolve-name` answers what a create with the
  input so far would compose, how long it is against the 63-character ceiling,
  whether it is a legal name, and whether it is already taken. It creates nothing
  and the create re-checks everything; the resolve is a hint, not a gate.

  Composition stays on the server for a reason. Template blocks are CEL, and a
  browser-side evaluator would spell a number or a bool by its own rules and
  compose a different string from the same input — the operator would approve one
  identity and be handed another, without a word. The endpoint therefore calls the
  same function the create calls, and a guard test pins the two to one answer. The
  form duplicates nothing about composition; it counts characters, which is not
  composition. A preview also runs on half-typed input, so it merges schema
  defaults without the required phase: it rejects *less* than the create, never
  *differently*.

  Whether the name is free is answered by the same endpoint rather than by probing
  `GET /v1/incarnations/{name}` from the form — that probe would turn a status code
  into an existence oracle and let a scoped operator walk names outside their scope.
  The answer is scope-aware in two grains: **taken** for anyone who could create the
  name, **taken by service X** only for someone who may already see that
  incarnation. The same rule now phrases the create's 409, which under a template
  used to name a string the operator had never typed and left them nothing to
  change. The create form knows which mode to open in from the boolean
  `composes_name` the scenario listing now carries — the flag, not the template
  text: the operator is shown the name, not the formula.

- **The interactive console — a real terminal on a host, from the browser**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md),
  [docs/keeper/console.md](docs/keeper/console.md)). `GET /v1/console` is a
  WebSocket carrying many independent panes on one socket: an operator opens a
  wall of terminals across a fleet and each is a pty on its host, run by the Soul
  daemon under its own user. The Keeper↔Soul side is an only-add `console_*`
  contract on its own RPC, so console traffic never queues behind an apply, and
  the plane is cluster-routed — the instance holding the socket is rarely the one
  holding that host's EventStream, so frames cross through Redis and a session's
  ownership claim carries both the Keeper id and the host SID. A Soul may
  therefore only publish frames for the session it actually holds; a mixed-version
  cluster keeps working, since a claim with no SID reads as "host unknown" and
  routes as before.

  Backpressure is explicit rather than unbounded buffering: a slow reader gets
  its oldest output dropped with a marked gap, not a Keeper growing a queue for
  it. Envelope is operator policy in `keeper.yml` (`console:` — sessions per
  Archon, per instance, idle timeout, recording cap); the host has the last word
  through `console:` in `soul.yml`, where `enabled: false` refuses every open
  outright. The right is `soul.console` and it is checked twice — at the socket
  upgrade, and per `open` frame with `host=<sid>`, because the target arrives in
  the frame rather than the URL.

- **Every console session is recorded, and one that cannot be is refused**
  ([ADR-0074(g)](docs/adr/0074-interactive-console-pty.md)). The artifact is
  asciicast v2, written as the session runs, with masking applied once at record
  time and carried across chunk boundaries — a secret split over two writes is
  still masked. There is no key to turn it off: a recorder that fails to start
  fails the session, so "unrecorded console" is not reachable through a
  deployment mistake. Storage is Postgres (migration 104) because Keeper is
  stateless and the instance that held the socket is rarely the one that later
  serves the playback.

- **Recorded console sessions can be read back**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-28).
  Recording became mandatory in the previous change; the artifact was reachable
  only by hand-written SQL. Three read routes now serve it:
  `GET /v1/console/recordings` (paged, filters by host, Archon, kind and time
  window), `GET /v1/console/recordings/{id}` for metadata, and
  `GET /v1/console/recordings/{id}/cast` for the asciicast v2 file itself —
  `application/x-asciicast`, streamed rather than buffered, and replayable with
  `asciinema play`, `agg` or xterm.js.

  **The right is `soul.console`, with the same selectors, and no lighter
  auditor-grade right is minted.** A recording is what an operator typed into a
  root shell and what it printed back — the live session moved in time — so the
  boundary that decides who may watch one has to be the boundary that decided
  who may open one. Otherwise an operator refused `soul.console on host=db-01`
  could read every session anyone ever held on db-01, which is most of what the
  refusal was for.

  Consequence, stated rather than hidden: a **pure auditor** — someone who
  should read the trail without ever holding a shell — cannot be given playback
  without also being given consoles. Withhold it by narrowing the *scope* to the
  hosts they investigate, not by looking for a weaker right; minting one is a
  new entry in a closed catalog and is left to its own decision.

  The gate splits the way the console's own does. A listing names no host, so
  the route can only ask "may this Archon reach consoles at all" and the
  per-host boundary applies to the rows — pushed into SQL for the list, checked
  per object otherwise. **An out-of-scope read answers 404**, identical to an
  unknown id: a 403 would confirm the recording exists and turn the route into
  an oracle for which hosts have been consoled into and by whom.

  Nothing is masked or un-masked on the way out. Masking ran once, when the
  session was recorded, carried across chunk boundaries; the cast is served
  byte-for-byte. Fetching one writes **`console.recording-read`** before the
  first byte leaves, carrying the Archon whose session it was — reading your own
  shell back is routine, reading someone else's is the question an investigation
  asks. The list and metadata routes are not audited: they only say a session
  happened, which `console.opened` already said.

  There is deliberately **no MCP tool** for playback — it would give an agent
  bulk access to the raw content of other operators' shells, and an agent cannot
  watch a replay. A recording of a host since removed from the registry stays
  readable by unrestricted operators and by ones scoped to that host; a
  `coven=`-scoped operator loses it with the host's registry row.

- **The `audit:` block now does what it says** ([ADR-022(i)](docs/adr/0022-audit-pipeline.md),
  amendment 2026-07-27). `audit.enabled: false` used to turn nothing off:
  neither it nor `otel_export` had a consumer anywhere in the write path, so the
  config promised control over the audit trail that did not exist.

  `enabled` is now enforced by a single gate over the shared `audit.Writer`
  where that writer is assembled, so all twenty-odd initiators honor it and a new
  one cannot forget to. `otel_export` switches a dual-write that is now actually
  wired (`auditotel` under `auditmulti`, assembled when `otel.enabled` is set —
  with no OTel stack there is nothing to export to). `retention_days`
  materializes the `purge_audit_old` Reaper rule when none is declared, instead
  of being a number nobody reads.

  **Turning the audit off is itself audited.** `audit.disabled` is written
  in-line ahead of the first event it suppresses, and the reload pair is written
  regardless of the toggle — otherwise the config swap that stops the trail would
  be journaled through an already-closed gate and leave no record of who stopped
  it. `audit.enabled` marks the other end of the window. The block stays out of
  the `SettingsStore` overlay by [ADR-0073(j.2)](docs/adr/0073-keeper-runtime-config-pg.md):
  it is a security gate, so it stays a deliberate edit to a host's `keeper.yml`
  rather than an API call. `keeper init` is not gated — bootstrap of the first
  Archon must leave a record whatever the flag says.

  Both fields are `*bool` now: `audit:` with only `retention_days` set used to
  resolve to "audit off" through the Go zero value, which would have silenced the
  pipeline the moment the flag started being honored.

- **MCP `keeper.soul.run-command` — the non-interactive console**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-27).
  An agent needs what an operator needs — run this on that host — but cannot use
  a terminal: a pty merges stdout and stderr onto one fd, echoes what was typed
  and reports only the shell's exit status. So the MCP form is request/response:
  one command line in, split channels and an integer `exit_code` out, masked and
  capped at 64 KiB per channel.

  **It needs `soul.console`, not `errand.run`** — dropping the tty removes the
  echo, not the privilege, and the command is still arbitrary and still runs as
  the Soul daemon's user, typically root. The check is the same scope-aware
  `host=<sid>` one the WebSocket console runs per pane. The Errand stack is only
  the transport, with the module pinned to `core.cmd.shell` and deliberately not
  exposed as an argument. Every run writes `console.command` (`{sid, status}`,
  correlated by `errand_id`) — the command line is not in the payload, exactly as
  `console.opened` holds no keystrokes.

  Known gap, unchanged by this and tracked as NIM-197: `core.cmd.shell` and
  `core.exec.run` are on the Errand runner's hardcoded allow-list, so
  `keeper.soul.errand.run`, `POST /v1/souls/{sid}/exec` and a `kind=command`
  Voyage still reach an arbitrary shell under `errand.run` alone. Until those
  choke-points are aligned, a role that must not hand out shells withholds
  `errand.run` as well as `soul.console`.

- **`introduced_in` engine metadata and the compat cross-check** ([ADR-0076](docs/adr/0076-engine-compat-window.md)).
  A `compat:` window is written by hand and can go stale — declaring `min: 0.1.0`
  while using something that only exists from `0.3.0` promises a keeper that would
  break. Keeper now derives the floor a definition **actually** needs from its
  body (DSL grammar plus the module / state / parameter metadata of the core
  catalog) and compares it with the declared one: `soul-lint` reports
  `compat_floor_too_low` on the manifest that declared the window, and keeper logs
  it at render, where it means another instance of a rolling-upgraded cluster
  would fail this same definition. Deliberately not a run-time block — the
  rendering keeper carries the feature, and refusing a run that would succeed is
  the worse failure. Inference yields a floor only; the ceiling stays the author's
  to declare. `introduced_in` parses on module manifests (module, state and
  parameter level) and is published by `GET /v1/modules`; it names a RELEASED
  version, so features are stamped when a release is cut (see
  [RELEASING.md](RELEASING.md) step c2).

- **Param-level strictness on the agent, and a deprecation policy for module
  params** ([ADR-0076](docs/adr/0076-engine-compat-window.md), amendment
  2026-07-26). A Soul reads a task's params by key, so a key it did not know was
  simply never read: an agent older than the param did its old job and reported
  OK/CHANGED while what the author asked for never happened. It now checks the
  task against the manifest **compiled into that binary** before Apply — and
  before `Plan`, so a `dry_run` cannot answer "no drift" for a param it never
  reads — and rejects an undeclared key with `module.unknown_param` without
  running the module. Core modules are enforced; custom modules **report but do
  not fail** — their manifests were never enforced before and under-declare in
  practice, so gating them would break scenarios that work today.

  The counterpart is that a module contract can now shrink safely: a param is
  never removed outright but marked `deprecated: {since, removed_in, use?}`
  (`removed_in` **exclusive**, like `compat.keeper.max`) and honored for **at
  least 2 minor releases** — deprecated in `X.Y.0`, removable no earlier than
  `X.(Y+2).0`, which is exactly the window an author may declare in `compat:`.
  Authors get a `deprecated_param` **warning**, never an error, naming the
  deadline and the replacement; at `removed_in` the same task text becomes
  `unknown_param`.

  **Upgrade order: souls first, then keeper** — adding a param to a core module
  is now a loud per-host failure on agents that predate it, rather than a silent
  mis-apply. Same order the capability gate already requires.

- **Intra-host task concurrency — `async:` tasks with named barriers**
  ([ADR-0075](docs/adr/0075-intra-host-async-tasks.md)). A `parallel:` block was
  weighed and rejected: it would have made ordering a property of nesting, which
  is exactly what a long-running fetch beside a long-running install does not
  need. Instead a task marks itself `async: true` and runs concurrently with what
  follows on the same host; a later task collects it with `require: [<name>]` or
  `require: all`. The barrier is Soul-side, so the wait costs no round trip, and
  the ceiling is host policy (`async.max_concurrent` in `soul.yml`) rather than a
  number the plan's author would have to guess about a machine they have not
  seen. `async:` and `require:` reach the wire as first-class fields on
  `RenderedTask`; `require:` on a `block:` is inherited by its children like the
  other block keys.

- **`SettingsStore` — Keeper runtime configuration in Postgres**
  ([ADR-0073](docs/adr/0073-keeper-runtime-config-pg.md)). 23 live reload-able
  keys — Toll, the Tempo rate limits, the Reaper, the Cadence corridor,
  `max_await_timeout`, `logging.level` and `cloud_init` — are settable for the
  whole cluster over `GET`/`PUT`/`DELETE /v1/settings` and the matching
  `keeper.setting.*` MCP tools, under the new `setting.read` / `setting.update` /
  `setting.delete` permissions, audited as `setting.updated` / `setting.deleted`.
  Admission is deliberately narrow: a key qualifies only if it has a **live**
  apply path — a consumer that re-resolves it from the current snapshot — so
  several blocks the config docs describe as reload-able are read once at start
  and stay out. Precedence is built-in default < Postgres < `keeper.yml`: an
  instance's own file outranks the cluster value, and where it shadows one the
  catalog says so with `source=file`, `cluster_value` and `overridden_locally`.
  Writes run the full validation pipeline as a dry run first, from both the
  answering node's view and that of a node whose file is silent on the key, so a
  cross-field invariant fails with 422 before anything reaches Postgres.

- **Derived roles — one role may follow another**
  ([ADR-0078](docs/adr/0078-rbac-derived-roles.md), migrations 102 and 105).
  `parent_role` makes a role a delta over another: it resolves to the
  intersection of what it lists with what its parent grants, so a delegator
  scoped to their own coven can hand out a narrower slice of it and the child
  narrows automatically when the parent does. `scope_mode` records which of two
  opposite intentions a delta carries — `track` (the default; the parent's scope
  cascades in) or `pin` (materialized at write time, so a later widening stops
  there) — because the two produced an identical row and the difference is not
  recoverable from it. Attenuation, the least-privilege floor and self-lockout
  protection all judge a role by what it **grants** rather than by the rows it
  stores; a role whose stored rows the chain no longer covers publishes them as
  `inert_permissions` instead of silently granting nothing. A mutation that moves
  any descendant's effective rights is refused with 409 unless the caller sends
  `confirm_cascade`, and the refusal names who gains, who loses and how many
  operators hold each.

- **Coven and Trait are one label world** ([ADR-0060](docs/adr/0060-traits.md),
  migration 106). A label lives only where it was attached and is never copied
  down: `souls.coven[]` / `souls.traits` label a host, `incarnation.covens` /
  `incarnation.traits` label an incarnation, and neither reaches the other
  (ADR-0080's read-time union was reverted before release — see Upgrade notes).
  One layer, two readers: the RBAC scope predicate (still pushed into SQL) and
  targeting (`soulprint.self.*`, the topology roster, the push inventory, the
  Voyage target filter). The per-host trait write path is first-class again and
  gains the mirror of coven-assign's label gate: the pair attached must lie inside
  the operator's own trait-scope, since a host-attached trait grants visibility
  permanently.

- **Operators can bind an onboarded Soul to an incarnation**
  ([ADR-008 amendment](docs/adr/0008-coven-stable-tags.md)). Until now the only
  act that bound a host was `core.soul.registered` inside a run, so a host
  onboarded out of band could not be adopted without one.
  `POST /v1/incarnations/{name}/members` and
  `DELETE /v1/incarnations/{name}/members/{sid}` do it directly under the new
  `incarnation.bind-member` / `incarnation.unbind-member`, with the screening
  in the domain rather than the handler so the MCP twin cannot drift from it.
  This is the create-without-run → bind → run flow, and it is what gives a
  topology `assert:` a roster to measure.

- **`name_template` composes an incarnation's name from its input**
  ([ADR-0079](docs/adr/0079-incarnation-name-template.md)). A service declares
  how its instances are named — a kebab-case, 63-character name derived from the
  components an operator already supplies — instead of asking for a name whose
  convention lives in somebody's head.

- **Destiny's input contract reaches parity with scenario's** — `validate:` and
  `required_when` now work the same on both sides, and the gate runs at render,
  so a destiny cannot be applied with an input its own contract rejects.

- **`include:` expands inside `block:`** in both layers, so a block can pull in a
  shared task list instead of restating it.

- **A declared Keeper version window on services and destinies**
  ([ADR-0076](docs/adr/0076-engine-compat-window.md)). `compat: {keeper: {min,
  max}}` — half-open, `max` exclusive — states the versions an artifact was
  tested against; the window in force for a run is the intersection of the
  service's and each destiny's, and a missing block is unbounded, so existing
  definitions keep working with no migration. The instance that **renders** is
  the authority, since a rolling upgrade means instances differ, and a
  version-caused refusal is `keeper_version_unsupported` naming the artifact, its
  ref, the window and the running version instead of an opaque `render_failed`.
  `GET /v1/services/{name}/compat` serves the effective window with a
  backend-supplied status, so a UI never re-derives compatibility from two
  numbers.

- **A per-host capability gate before dispatch**
  ([ADR-0076](docs/adr/0076-engine-compat-window.md)). A Soul announces what it
  implements — the DSL features it enforces itself plus one entry per core module
  in its registry — and Keeper derives from the plan it rendered what each target
  host needs, refusing the hosts that never announced it
  (`soul_capability_unsupported`, naming every host and what each is missing, so
  a fleet is upgraded in one round rather than one host per retry). It runs on
  every dispatch path including check-drift, which additionally requires
  `dry_run` of every roster host — a binary ignoring that flag would mutate hosts
  during an operation that promised a pure read. Plugin modules stay off the axis
  on purpose: `core.module.installed` can install one mid-run, long after the
  announcement was made.

- **A run records the engines that executed it** (migration 103): the Keeper
  version that rendered it and the Soul version of each host that applied it,
  written from the same heartbeat entry as the capability set so a stamp can
  never pair a version with capabilities from a different connection.
  Audit-grade — no gate reads it — and it answers "what was this actually run by"
  months later, which a rolling upgrade otherwise makes unanswerable.

- **A deprecated module param is visible before it becomes a failure**
  ([ADR-0076](docs/adr/0076-engine-compat-window.md) amendments (u)–(w),
  migration 107). The window was only worth its surfaces, and both were thinner
  than the ADR claimed: the operator's notice was one agent's log line, so
  `removed_in` arrived as abruptly as if there had been no window. A
  `TaskNotice{code, module, param, message}` now rides `TaskEvent` — collected
  before Apply **and** before Plan, since a dry run is where an operator looks
  before committing — and lands in three surfaces Keeper already serves: the
  `task.executed` audit payload, the SSE frame and `apply_runs.notices`, read
  back per host and deduplicated by `(code, module, param)`. Notices survive
  `no_log`: they are rendered from the manifest, never from a value, and
  suppressing them would blind the operator on exactly the tasks that handle
  secrets. On the author's side `GET /v1/modules` now publishes `deprecated` as
  `{since, removed_in, use}` beside `introduced_in`, which is what an author
  reads *before* writing the task — the lint warning only arrives once the
  definition exists.

- **`GET /v1/deprecations` — which incarnations still pass a param that is going
  away** ([ADR-0076(v)](docs/adr/0076-engine-compat-window.md)). The window above
  tells an operator that a param has a deadline; this answers the question that
  turns the warning into a migration plan — *who still passes it, and where*.
  One row per deprecated param rather than per site, because that is the unit of
  work somebody schedules, and each row carries the incarnation, the service at
  its pinned ref, the scenario and the location inside it.

  Sourced from the **definitions**, never from run history. An aggregate over
  stored runs answers "who passed it in the runs we happened to observe": an
  incarnation nobody ran this month is missing from it, and one fixed yesterday
  still appears in it. Reading definitions instead needs a plugin manifest to
  resolve, which is why this waited for that resolver.

  It reports its own blind spots beside its findings. A definition that fails to
  load, or a module whose contract is unreadable here, lands in `gaps` with the
  incarnations it affects — so an empty `items` list can be read as "the estate
  is clean" only when `gaps` is empty too. The survey also states how many
  incarnations and definitions it actually walked, and sets `truncated` when the
  estate is larger than one pass covers; a partial answer says so rather than
  looking complete. Scope is the caller's own: an undefined scope yields an
  empty survey, never the whole estate.

- **`soulctl` can read apply runs, and prints the deprecation notices on them**
  (NIM-269). The run's notices already reached the audit trail, the SSE frame and
  the stored run, but the CLI could not show a run at all — so an operator
  working from a terminal had no way to see what a run had warned them about
  short of querying the API by hand.

- **The `redis` example generates ACL user passwords instead of demanding a
  pre-seed.** `add_user` and `update_users` used to abort at render unless the
  operator had run `vault kv put` for every new user first — a manual step in
  front of the most frequent day-2 action. Both now mint what is missing through
  a keeper-side `core.vault.kv-present` step (32 alphanumeric characters from
  `crypto/rand`) at the path the render already reads, and the value never leaves
  the Keeper: only the path and field name reach output, audit, logs and OTel.
  Scope is the operator-supplied users only — `default_admin` and the system users
  are deliberately never regenerated, since those are the credentials the running
  instance authenticates with, and a missing one means a broken incarnation and
  must keep failing loudly. Semantics are generate-if-absent; rotation stays an
  explicit write to the same path plus a re-run.

- **`Type=notify` readiness and a watchdog for both daemons.** `keeper` signals
  ready once the operator API socket is bound and serving; `soul` once wired up
  and deliberately **not** on its first Keeper connect, since the reconnect loop
  retries forever and gating readiness on Keeper would fail every agent's unit
  start during a Keeper outage — connection state is reported as `STATUS=`
  instead. A long start extends the start job per step, so `TimeoutStartSec`
  bounds one hung step rather than a cold Vault/PG/plugin-cache start. The
  watchdog pings liveness of the process itself and deliberately not Postgres or
  Redis: a storage outage must not restart the cluster. Enablement is runtime
  autodetect from `NOTIFY_SOCKET` / `WATCHDOG_USEC` — one binary for every
  distribution, and in docker/k8s every call is a no-op with liveness left to the
  orchestrator.

- **Run history records the operator input each run was started with**
  (migration 101), masked on the way in, so "what was this run actually asked to
  do" survives the run.

- `soul-stack-tools` — meta package installing the whole CLI set (`soulctl` +
  `soul-lint` + `soul-trial` + `soul-legion`) in one step — the same four
  binaries the Homebrew cask and the winget package carry, so every channel
  lands the same tool set. Carries no files itself. The `keeper` and `soul`
  daemons stay separate packages on purpose: a server installs only what it runs.

- **The apt publisher refuses to publish a partial or dev-built set.** The
  remote prune deletes whatever staging no longer holds, so a run assembled from
  an incomplete or locally-built `dist/` did not merely publish the wrong
  packages — it removed the right ones. Publishing now stops before touching the
  remote unless the set is complete and release-built, which turns a silent
  half-publication into a refusal an operator can read.

- **`AuditEvent.type` is an enum in OpenAPI, so a client can prove it handles
  every event.** `GET /v1/audit` used to publish the type as a bare string, and
  the only list of what keeper can actually write was a Go const block a
  consumer could not see. A console rendering a human label per type therefore
  had nothing to check its coverage against: a newly added type shipped
  unlabelled until a user noticed it, and labels for retired types sat there
  just as invisibly. The spec now carries the full catalog, generated from those
  declarations rather than transcribed — adding a constant without regenerating
  fails the build, so the published set cannot fall behind the code. Only the
  response field is narrowed; the `?type=` filter stays a free string, because a
  filter has to keep matching historical rows whose type has since been retired.

- **`make check-webui-freshness` — the gate now notices that the embedded UI is
  not the companion's current one.** Merging the UI repository without re-running
  `make sync-webui` leaves keeper serving a bundle nobody built from current
  sources, and nothing failed: `check-webui` prints `skipping` and exits zero
  where the companion is not checked out, and `check-webui-embed` compares the
  bundle against a fingerprint recorded by the same sync that produced it, which
  a stale bundle matches perfectly. That happened on two consecutive web merges — three times in all,
  counting NIM-273 — and a human found it every time. The new check asks the one
  question those two structurally cannot —
  is `WEBUI_SOURCE`'s `commit=` still the tip of the companion branch this tree
  is assembling — with a single anonymous `git ls-remote`: no companion checkout,
  no npm, no token. It is **binding on `release/*` and `hotfix/*`** and advisory
  everywhere else, so a release cannot be assembled around a stale `/ui` while an
  unrelated feature branch stays quiet; hotfix branches are binding because they
  reach `main` without passing a release branch. `WEBUI_FRESHNESS_SKIP=1` is the
  declared escape for offline work, and every failing path names it — most of the
  conditions that fail here (offline, no remote, no provenance file) are not cured
  by the re-sync the message would otherwise be recommending. A checkout that is
  on no branch at all cannot answer "is this a release?", and an unanswerable
  question counts as binding rather than as "not a release": otherwise one
  detached HEAD would turn the check off silently, which is the failure it exists
  to prevent. Three detachments that are not that case are recognised rather than
  counted as unanswerable — a conflicted rebase resolves to the branch being
  rebased and is then judged as that branch, while a bisect stays advisory (it
  lands on old commits on purpose) and so does a checkout sitting exactly on a
  tag, a released version whose bundle is supposed to be frozen. Exactly
  one tree is skipped outright — one with no `assets/` at all, which cannot carry
  a stale bundle; a tree that is not a git checkout (an unpacked source tarball)
  is advisory, which still reports. That single skip is unconditional, and it is
  safe because of something outside this check: `//go:embed all:assets` refuses to
  compile an empty or missing directory, so "delete `assets/` and the gate goes
  quiet" reds the `build` tier first. A bundle *with* no `WEBUI_SOURCE` beside it
  still fails — that is an inconsistency, not an absence, and it is the live state
  of `main` and `hotfix/R5-I-provision-hardening`, so the hotfix branch reds until
  it carries a re-vendored bundle. A pull request is judged by its **base** branch,
  not by `GITHUB_REF_NAME` (which is `<n>/merge` there and could never match
  `release/*`): the review before the merge is where an unpaired bundle is still
  cheap to catch. A merge-queue run is read the same way — its ref spells the base
  out as `gh-readonly-queue/<base>/pr-<n>-<sha>`, and that run is the last gate
  before the commit lands on that base, so taking the ref at face value would go
  quiet exactly there.
  When the companion has no such branch yet — a release train that starts in core —
  the check says so and compares against the branch the bytes were actually
  vendored from, rather than reding until someone opens the branch on the other
  side; a red nobody can clear teaches everyone to stop reading reds. When that
  fallback is unavailable because the bundle records no `branch=` at all — which
  is what `sync-webui` writes for a detached companion, and what it now warns
  about while the companion is still in reach — the red says so instead of
  blaming a branch nobody has to open. And the branch a bundle is *labelled* with
  is judged only after its commit: a release train opens the companion's next
  branch AT the commit already vendored, so for that moment the label reads
  `release/<prev>` while the bytes are the tip of `release/<next>`, and calling
  that stale would red a tree that is exactly up to date at the busiest hour of
  the train.
  It compares pointers, not content, so an inert companion commit still asks
  for a re-sync — deliberate, because `vite.config.ts` carries both the build and
  the test config and no path list can separate the two honestly. When the UI
  really did not change, the whole diff is two provenance lines.
  A red has two shapes and the message names both, because only one of them is
  cleared by re-vendoring. Besides the unpaired merge there is a bundle vendored
  from a companion commit **that was never pushed**, and there `make sync-webui`
  is the wrong move in both directions: from the branch tip it rewinds the UI past
  the work that was vendored, and from the local checkout it re-records the same
  unpublished SHA. That state means the release bundle cannot be rebuilt by
  anyone else, so it earns a red of its own; the fix is to push the companion.
  `git branch -r --contains <sha>` in the companion separates the two shapes,
  while `ls-remote | grep` cannot — an ancestor is published and is the tip of
  nothing.
  Three things the check deliberately does not overstate. It reports "does not
  match the companion tip", never "is behind": `ls-remote` returns one SHA and
  answers equality, not ancestry, and on a force-pushed branch "behind" would
  simply be false. The companion repository is *derived* — every remote of this
  checkout with `-web` appended, origin first, first answer wins — so a fork-based
  checkout can be answered by a `-web` that trails or leads upstream; every
  mismatch therefore names the URL that answered, and `WEBUI_REMOTE_URL=<url>`
  pins a different one, which is what keeps a red that re-vendoring cannot clear
  diagnosable. And `unknown` binds but cannot ask everything: without a branch,
  "was this vendored from the branch being assembled" is unanswerable, so only
  staleness against the recorded branch is checked and the banner says which
  question was skipped. A rebase started from an already-detached HEAD stays
  `unknown` too — git writes the literal string `detached HEAD` as the branch being
  rebased, and reading that as a branch name would have downgraded a release
  worktree to advisory with one `git rebase`.
  Three things a green does not mean, written down because an unwritten limit
  gets read as coverage: nobody hand-edited `WEBUI_SOURCE` (nothing binds
  `commit=` to the bytes beside it, and writing the companion's tip into that
  line greens all three web checks at once); the companion has opened the branch
  being assembled (until it does, the fallback measures against the recorded
  branch, so UI work landing elsewhere stays invisible); and the check actually
  ran (`WEBUI_FRESHNESS_SKIP=1` exits zero and `gate.sh` prints PASS — a known
  boundary of every tier, visible in the log rather than in the summary table).

### Changed

- **`check-webui` fails instead of skipping where the companion is mandatory.**
  Absent companion still skips off a release branch (third-party clones, ticket
  worktrees that never touch the UI), but on `release/*` it is now an error with
  `WEBUI_SKIP=1` as the declared opt-out — a release worktree is exactly where
  the companion is supposed to sit next to core, so "no companion" there is an
  unconfigured tree rather than a legitimate skip. CI keeps skipping the byte
  comparison, knowingly: running it there would mean checking out the companion
  *and* building it on every core push, and `check-webui-freshness` answers the
  question that mattered without either. Both targets read the same answer from
  `check-webui-freshness.sh --context`, so the two cannot drift apart, and when
  that answer cannot be obtained at all the caller assumes the strict one — a
  gate that fails open at the exact moment it cannot tell where it is running is
  not a gate.

- **`make sync-webui` always rebuilds the companion.** It used to build only when
  `dist/index.html` was missing and otherwise mirror whatever was already there,
  while reading `commit=` fresh from the companion's HEAD. A checkout whose `dist/`
  predated its last few commits was therefore mirrored as-is under a provenance
  line pointing at the tip: the bundle stayed stale, its fingerprint matched
  (`check-webui-embed` derives it from those same bytes), and freshness was
  satisfied. All three checks went green over exactly the defect they exist to
  catch — in the one script every one of them recommends as the remedy. A failed
  build now leaves the vendored bundle untouched and says so, pointing at
  `npm ci` for a fresh checkout, and a detached companion records an empty
  `branch=` rather than a branch literally named `HEAD`. Running it with no
  companion beside the repository now prints the "not found, or name it
  explicitly" message it always carried: the default path was resolved with a
  `cd` under `set -e`, so the script died on a raw `cd: no such file or
  directory` before reaching its own guard — and the person most likely to follow
  a "run make sync-webui" red is precisely the one without the companion checked
  out.

- **`compute.<name>` is readable from an `on: keeper` task, and elsewhere a
  context without the namespace now says so instead of blaming the key.** A
  keeper-side task used to be handed no `compute` at all, so `${ compute.x }` in
  its `params:` failed with `no such key: x` — for a name that was declared and
  spelled right. The omission had no reason behind it: `compute:` resolves once
  per run, in the very run-level context an `on: keeper` task renders in, so there
  was never a per-host value to import. It is now in scope there, in `params:` and
  in the task's own `vars:`, exactly as on the Soul side.

  The namespace is genuinely absent from four contexts, each because it runs
  before or beside the point where a computed value means anything: the
  `loop.items:`/`loop.when:` axis, `on: [covens]`, the isolated destiny pass
  ([ADR-009](docs/adr/0009-scenario-dsl.md) V2), and `state_changes` `add:`
  `match:`. Those used to be handed an empty map and produced the same misleading
  `no such key`; they now refuse at **compile**, naming the namespace, the context
  the author is standing in, and what to write instead — with one exception left
  standing: an `add:` `match:` **inside a `foreach`** is routed to the
  context-aware merge-time evaluator so the predicate can read the `as` name, and
  that path still reports an eval-time `no such key`. `soul-lint` refuses the
  shape offline either way, so a scenario that lints clean does not reach it.
  Two consequences worth
  expecting: the check runs before evaluation, so it also fires on a branch a run
  would never have reached; and the previously **silent** forms are silent no
  longer — `has(compute.x)` stopped evaluating to `false` and `size(compute)`
  stopped returning `0` in a context that never had the namespace. A scenario
  relying on either as a feature test will now fail at render; test the underlying
  `input.*`/`vars.*` instead. The full table is in
  [docs/scenario/orchestration.md §2.4](docs/scenario/orchestration.md).

  A fifth context is out of scope and always was, with nothing to fix at the
  engine: `when:` / `changed_when:` / `failed_when:` / `retry.until:` are not
  rendered by the Keeper at all — they travel into the `RenderedTask` verbatim and
  Soul evaluates them in the flow-control sandbox
  ([ADR-012](docs/adr/0012-keeper-soul-grpc.md)(d)), over
  `input`/`vars`/`incarnation`/`soulprint.self`/`register`. That sandbox never
  declared `compute`, so it already refused honestly in cel-go's own words
  (`undeclared reference to 'compute'`) rather than blaming a key. It is called
  out here because the surprise is real — the same task's `params:` **do** read
  `compute.*` — and because the detour is one line: put the value in the task's
  own `vars:`, which is rendered with the namespace in scope, and write the
  predicate against that.

- **`compute` is now a reserved binding name.** `loop.as:`/`loop.index_as:` and
  `state_changes` `foreach.as:` reject it (`loop_var_reserved` /
  `reserved_binding_name`), matching the other context roots. In the loop axis the
  shadow was merely confusing; in `state_changes` it was silent and worse — that
  context *does* have the namespace, so `as: compute` validated, rendered, and
  quietly made every `compute.<name>` mean a field of the element being iterated.

- **A Tiding's `incarnation` selector now binds through `incarnation.run_completed`.**
  It used to bind through `incarnation.drift_checked`, the only run-scope event
  whose payload named a single instance — so when NIM-446 removed that event the
  selector briefly matched nothing at all: an operator could set the filter, the
  form would accept it, and no notification would ever arrive. It is re-pointed
  at `run_completed`, the surviving point event, whose meaning ("this run, on
  this incarnation") is what the filter was asking for in the first place. A rule
  carrying the selector should list `incarnation.run_completed` in its
  `event_types`; on Voyage terminals (many incarnations) and cadence events (bound
  to `cadence_id`) it still does not fire, unchanged. Note the payload key differs
  from drift's — `incarnation`, not `name`.

### Removed

- **Drift detection (Scry) — the whole circuit**
  ([ADR-031 closing amendment](docs/adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)).
  It shipped complete — an on-demand check, a background scan, a permission, an
  audit event, two columns — and then went unused: the background rule was
  default-OFF and stayed off, and the sync endpoint that blocks a request for the
  length of a fleet-wide dry run was not a thing operators reached for. Keeping
  it meant carrying a permission in a closed catalog, an event type in a closed
  enum, and a Reaper rule that dispatches work, for a feature nobody ran.

  Removed: `POST /v1/incarnations/{name}/check-drift` and `keeper.incarnation.check-drift`;
  `soulctl incarnation check-drift` and the `LAST_DRIFT` column of `incarnation list`;
  the permission `incarnation.check-drift`; the audit event
  `incarnation.drift_checked` (and with it the last point type Tidings could
  subscribe to besides `incarnation.run_completed`); the Reaper rule
  `scry_background` with `max_concurrent_in_flight` / `min_interval_per_incarnation`;
  the columns `incarnation.last_drift_check_at` / `last_drift_summary` and their
  keys in the `GET`/`LIST` incarnation body; `DriftReport` and its schemas.
  Migration `114` drops the columns, cleans the grants and subscriptions, and
  **cancels stale `planned` dry-run runs**. That last step is not tidiness: a
  check-drift queued one `apply_runs` row per host with `recipe.dry_run=true`,
  `ClaimNext` has never looked at the recipe, and the recipe no longer carries
  the flag — so a row left behind by a keeper that restarted mid-check would be
  claimed by the new binary and dispatched as a REAL apply of the `converge`
  scenario. A pure-read operation turning into a fleet-wide mutation, days
  later.

  **Not removed, deliberately:** `Plan` pure-read and the `PlanReadSafe`
  capability across the core modules — that is the SoulModule contract, it is
  public API under `sdk/`, and Errand's dry-run exercises it independently; the
  only-add proto fields `PlanEvent.changed` / `ApplyRequest.dry_run`; and the
  incarnation status **`drift`**, which was never reachable only through Scry —
  a legacy upgrade (one with no upgrade scenario for the transition) still leaves
  an incarnation in it, meaning "the DB state is ahead of the hosts". Remediation
  is unchanged: a normal apply returns it to `ready`. `scenario/converge/main.yml`
  also stays exactly where it was, now as an ordinary operational scenario — run
  it to bring hosts back to the declaration.

- **`soulctl incarnation run --dry-run`** — a flag that never worked and said
  otherwise. The client appended `?dry_run=true`, but
  `POST /v1/incarnations/{name}/scenarios/{scenario}` has never bound that
  parameter, so the run was a REAL apply while the operator believed it was a
  rehearsal. It survived from the public beta because the only test covering it
  asserted that the CLIENT sent the parameter, against a fake server that accepts
  any query — a test that builds its own input proves nothing about whether the
  thing is reachable. Nothing sets `Recipe.DryRun` any more either, so the flag
  could not be repaired in place; ad-hoc read-only checks are Errand's
  `--dry-run`, which is wired end to end.

- **`incarnation.spec.hosts[]` and its editing endpoint**
  ([ADR-044 amendment 2026-07-30](docs/adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)).
  The field decided nothing on either axis it appeared to: the roster of a run has
  been `incarnation_membership` since NIM-124, and the declared role has been a
  Choir Voice since ADR-044 item 2 — `spec.hosts[].role` survived only as a
  fallback for hosts without one. Meanwhile the UI's "Add host" button read as
  "add a host to this incarnation" while editing a list no resolver consulted.

  Removed together: the `PATCH /v1/incarnations/{name}/hosts` endpoint and its
  OpenAPI schemas, the permissions `incarnation.update-hosts` and
  `incarnation.update`, the audit event type `incarnation.hosts_updated`, and the
  resolver's spec fallback — a declared role now comes from a Voice or not at all.
  Migrations `108` / `109` clean the `spec` key and the dead grants. See the
  Upgrade notes above, which cover the fail-closed RBAC consequence and the
  replacement (`core.choir.present` in a scenario, `POST .../choirs/{choir}/voices`
  day-2).

  The `topology.Querier` interface lost `QueryRow` in the same change: every read
  the resolver makes is set-shaped now, so "the resolver does not consult
  `incarnation.spec`" is enforced by the compiler rather than asserted by a test.

### Security

- **A password pasted into a `*_ref` field was quoted back into `audit_log`.** Every
  field that must hold a vault-ref rejected a plaintext value by naming it —
  `vault-ref "hunter2" must match vault:<path>[#<field>]`. A keeper-config
  diagnostic does not stop at the operator's terminal: `Store.Reload` hands it to
  `audit.FormatDiagnostics`, which puts it in the `config.reload_failed` payload,
  which is a row in `audit_log` — append-only, retained 365 days, readable by
  everyone who can read the audit trail. The value most likely to be typed there
  by mistake is the credential the field exists to keep out of the config, and it
  outlived the incident that produced it. The same message reaches a second
  surface: the settings API re-validates the merged config and returns the first
  error to the HTTP caller.

  Thirteen fields across two validators did this, not the five the ticket listed:
  nine in the semantic phase (`postgres.dsn_ref`, `redis.password_ref`,
  `redis.sentinel_password_ref`, `auth.jwt.signing_key_ref`,
  `cloud_init.tls_ca_ref`, `auth.ldap.bind_password_ref`, `auth.ldap.tls.ca_ref`,
  `auth.oidc.client_secret_ref`, `auth.oidc.tls.ca_ref`) and four in the schema
  phase (`metrics.auth.basic.password_ref`, `push.host_ca_ref`,
  `push.host_ca_refs[].ref`, `sigil.signing_key_ref`).

  The message is now rendered by one function that **does not take the value as a
  parameter**, so no call site is able to interpolate one whether it remembers to
  or not: `postgres.dsn_ref must be a vault-ref (vault:<path>[#<field>]), got
  ***MASKED***`. The placeholder is `audit.MaskedValue`, the same token the
  payload maskers write — the previously divergent `<masked>` spelling in the
  input-validation and herald paths is retired, so one grep finds every surface.
  The field is still named and the expected form still shown: masking must not
  cost the operator what they need to fix it.

  Diagnostics already written to `audit_log` are not rewritten; they age out with
  the existing 365-day retention.

- **A state field a service declared `secret: true` was published in the clear by a
  force destroy.** Destroying an incarnation with `allow_destroy=true` skips
  teardown and reports what it abandoned, so an operator can clean up the cloud
  resources by hand. That record was masked by the vault-origin and
  key-name-regex layers only — the declarative layer was not consulted, because
  the delete transaction had no access to the service manifest. A secret whose
  key is not named like one (`provisioned_provider`) and whose value is not a
  vault-ref passed straight through, into the API reply, the MCP tool result, the
  WARN line, the `destroy_completed` audit event, and the `status_details` patch
  that lands in `incarnation_archive` — the archive being the copy that outlives
  the incarnation itself.

  All four ADR-010 §7.4 layers now run on that record. The schema is collected
  from the artifact by `incarnation.StateSchemaSecrets` and passed to
  `DeleteAfterTeardown` as a **positional** argument, so a wiring site that
  forgets it is a compile error rather than a destroy that quietly ships a
  declared secret. A caller with no readable artifact passes `nil` and degrades to
  the previous pair of layers.

- **A DSN the Keeper could not parse printed its password to stderr.** `keeper run`
  opens the Postgres pool and then applies migrations; both steps reported a bad
  DSN by handing the parser's own error to the log. Under Kubernetes that line is
  a pod log, shipped to central collection and retained like any other, and the
  DSN behind `dsn_ref` is a credential the operator is otherwise told to read only
  from Vault.

  The pool is the branch an operator actually reaches, because it runs first.
  `pgconn.ParseConfigError` quotes the whole connection string and redacts it with
  a regular expression over `password=`, which misses `password = …` and
  `password= …` — both legal libpq — in *every* branch, including failures about
  an unrelated setting such as a mistyped `sslmode`. And when `net/url` is what
  refused the DSN, pgx returns that error with its wrapper stripped: a `#`, `/` or
  `?` in a password ends the authority early, so the message becomes
  `invalid port ":<password>" after host`. pgx notes the hazard in a comment on
  that method — a static string, it says, would be the safe thing to return.
  Migrations disclosed the same DSN two ways of their own: verbatim (`%q`) when
  the scheme was not a Postgres URL, and through the `*url.Error` golang-migrate
  returns unchanged.

  Both steps now name the **form** and never the value. The pool reports
  `pg: dsn cannot be parsed (…)`, carrying either pgx's own diagnosis with the
  connection string stripped out of it — `sslmode is invalid`,
  `cannot parse pool_max_conns` — or, when the DSN's syntax is what failed, a
  class read off the error's type: `contains an invalid percent-escape`,
  `contains an invalid character in the host name`, or `cause withheld: naming it
  would quote the dsn`. Migrations report
  `migrate: dsn must be postgres:// or postgresql:// URL (got scheme "mysql")`, or
  `got no URL scheme` for the keyword/value form; the scheme is named only when
  the prefix is one by RFC 3986, since `password=p://x` would otherwise hand a
  naive split the password as its "scheme".

  Nothing that worked stops working. Both steps parse with `net/url` before the
  library does, and pgx dispatches on the same two prefixes and runs the same
  parser — so the gate refuses only what pgx was going to refuse anyway.
  Multi-host, IPv6 literals, `?host=/var/run/postgresql`, `@` and `:` inside a
  password, an empty password and percent-encoded passwords all still parse. See
  [deb-onboarding.md §9](docs/operations/deb-onboarding.md) for what each refusal
  means, since the message deliberately does not quote the value to go fix.

- **Revoking an Archon neither took effect where the operator was standing, nor
  said what had happened.** ADR-014's 2026-05-27 amendment promised two things —
  a revoked token answers `401` on verify, and the window is single-digit
  milliseconds. A sweep of a live two-node stand held up neither.

  Of 45 parameterless routes, a revoked token got `401` on 24, `403 operator
  lacks required permission …` on 8, and `200` on 4. The split follows the gate,
  not the intent: `RequirePermission` asks `Check`, which can answer
  `ErrOperatorRevoked`; the existence-gate `RequireAction` asks
  `HoldsAction() bool`, and a bare bool has nowhere to put a reason. So the same
  state came back as a permission problem — telling a client to go ask for a
  grant that cannot exist, since the identity is what was withdrawn. The four
  `200`s (`/v1/me/permissions`, `/v1/permissions`, `/v1/event-types`,
  `/v1/herald-types`) carry no RBAC gate at all and simply kept serving, for as
  long as the token's `exp` allowed. `/auth/ldap/login` and
  `/auth/oidc/callback` answered `403` for the same state, so a login form could
  not tell a removed account from a missing group either.

  The timing failed on the node that matters most. `rbac:invalidate` messages
  are self-filtered by origin KID, and the publishing node had no local refresh —
  so the one node guaranteed not to learn about a revoke was the node that
  performed it, and it fell back to the 10s TTL poll. Measured: a subscriber
  flipped in 0.04–0.06s, the origin took 9.82, 9.82 and 10.19s. The operator who
  presses revoke is normally talking to that node.

  Revocation is no longer a per-route concern. `RejectRevoked` is one middleware
  link installed immediately after `RequireJWT`, so every authenticated route
  answers `401 operator-revoked-token` — the ones that were already right, the
  ones that said `403`, the ones with no gate at all, the two spec routes outside
  `/v1`, and any route added later, which is the point of putting it in the chain
  rather than on the routes. Federated login answers `401` for a revoked operator
  too. The invalidator now also refreshes its own snapshot, wired unconditionally
  so that a Redis-less single-node stand gets it as well. It publishes to the
  cluster first and refreshes locally second: both halves read the same committed
  rows, so the order cannot change what any node converges on — only how fast the
  other nodes hear, and the local refresh is the slower of the two. If that
  refresh fails it is logged and the TTL poll takes over, because the revoke has
  already committed and must not be reported as failed. Also on the cookie
  exchange `POST /auth/token`: a revoked session now gets the same typed `401`
  instead of the generic one, so the browser's own refresh path can tell the two
  apart. The shipped web UI does not yet act on the distinction (NIM-557).

  **Clients must treat `401 operator-revoked-token` as a logout, not a retry.**
  `403` still means what it meant — this identity may not do this — and
  `ErrNoRoleMapping` / `ErrProvisioningDisabled` stay `403` on the login path.
  Nothing that was permitted becomes refused: the only requests whose answer
  changes belong to Archons that were already revoked. No OpenAPI change; both
  federated endpoints already declared `401`.

  This covers the HTTP surface. MCP is a separate listener that verifies the JWT
  itself, so `RejectRevoked` does not run there: a revoked operator is refused
  every one of the 94 tools by the same `Check`, but the refusal still reads
  `forbidden` / "operator lacks required permission X" — the same conflation,
  left standing on the other door. Two MCP paths are not gated at all:
  `initialize` / `tools/list` (handshake and catalog, no data), and the SSE
  stream of an apply the operator started herself, which `authorizeSSE`
  short-circuits before it reaches `Check`. All of it is tracked as NIM-551. The
  timing fix is surface-independent and applies to MCP as well.

- **Federated role reconciliation could empty the cluster's admin set, and no
  operator had to be involved** ([ADR-058(d)](docs/adr/0058-operator-auth-ldap-oidc.md)
  amendment 2026-07-29). LDAP/OIDC login revokes the mapped roles a user's groups no
  longer carry. It writes inside its own transaction, alongside the grants it makes
  in the same breath, so it could not go through `rbac.Service` — and took the
  package-level revoke, which enforced nothing. The self-lockout invariant lived in
  the Service, and the package function's doc said so; a comment is not a boundary.

  The trigger needs no mistake by anyone: an identity provider can be wrong in the
  one direction that matters, answering with **fewer** groups than it should. Map a
  group to a `*`-granting role — the first thing anyone configures — and an outage,
  a reorganisation or an unpropagated membership demotes whoever logs in during it.
  The last administrator going that way leaves a cluster with no way back through
  the API.

  The revoke is now guarded and a refusal fails the login, rolling back the grants
  made alongside it so membership is never half-synced. The names carry the
  guarantee instead of a comment: `rbac.RevokeOperator` runs the probe, and the bare
  DELETE is `RevokeOperatorRow`, reached only by asking for it. The probe is the
  cluster-state precondition of [ADR-078 §n](docs/adr/0078-rbac-derived-roles.md),
  not a permission check — this path has no caller and its grants are equally
  caller-less, so an operator's rights are not the question. An IdP returning no
  usable groups was already refused before reconciliation and is unchanged; the hole
  was the partial answer.

- **`errand.run` no longer reaches an arbitrary shell on its own**
  ([ADR-0074](docs/adr/0074-interactive-console-pty.md), amendment 2026-07-28;
  [ADR-033](docs/adr/0033-errand.md)). `core.cmd.shell` and `core.exec.run` sit on
  the Errand runner's allow-list, and their declared input *is* a command line —
  so the allow-list bounded the module, not what it carried, and the gate written
  for "one named module with declared params" opened a root shell. That made
  `soul.console` (added a day earlier for exactly this action) a half-control:
  anyone holding `errand.run` walked around it in one call.

  Reaching either module through an Errand now requires **`soul.console` in
  addition to `errand.run`**, at every entry point: `POST /v1/souls/{sid}/exec`,
  MCP `keeper.soul.errand.run`, a `kind=command` Voyage (on every resolved host,
  all-or-nothing), a `kind=command` Cadence recipe, and the Cadence spawn.
  `errand.run` stays necessary. The two rights remain independent in both
  directions — `soul.console` alone still does not open the Errand path, and
  `keeper.soul.run-command` still needs no `errand.run`. **An `ErrandReadSafe`
  module is unaffected**: an ordinary Errand does not acquire a console
  requirement. Cancelling a command Voyage is deliberately not gated — the
  emergency brake must not need a stronger right than the accelerator.

  **This breaks existing grants, so it ships behind a one-minor deprecation
  window.** `console.errand_shell_gate` in `keeper.yml` defaults to `warn`: the
  call proceeds and the would-be denial is recorded. It flips to **`enforce` in
  the next minor**. Before flipping, check two numbers —
  `keeper_rbac_shell_errand_legacy_roles` (roles holding `errand.run` with no
  `soul.console`, named in a WARN line when the set changes) and
  `keeper_rbac_shell_errand_gate_total{result="would_deny"}` (the live, scope-exact
  answer, cut by `surface`). Both at zero for a release means the flip cannot
  break you. The key is a `keeper.yml` edit, not a `SettingsStore` one: it is a
  security gate, and that overlay falls back to the more permissive value after a
  Postgres outage ([ADR-0073(j.2)](docs/adr/0073-keeper-runtime-config-pg.md)).

  **Watch schedules in particular.** Every other entry point answers an operator
  reading an HTTP status; a Cadence recipe written under the old rule would simply
  stop producing runs, on a timer, with nobody looking. Under `enforce` the spawn
  skips rather than errors (an error would stall every other due schedule),
  advances `next_run_at` as an overlap skip does, and writes the new
  **`cadence.skipped_forbidden`** audit event `{cadence_id, scheduled_for, reason,
  module}`. Alert on it.

### Changed

- **BREAKING wire change — a Vigil, Decree or Rite subject is one nested
  `subject` object with four dimensions** (migration 113,
  [ADR-008 amendment 2026-08-05](docs/adr/0008-coven-stable-tags.md)). The
  top-level `coven` / `sid` pair is replaced on all three registries, on REST and
  MCP alike, by

  ```
  subject: { sid: [...] | incarnation: {service, name} | coven: [...] | trait: {key, value} }
  ```

  with exactly one dimension set — zero, two, or half a pair is `422
  validation-failed`. `sid` and `coven` keep their old meaning, `incarnation`
  addresses a roster through `incarnation_membership`, and `trait` selects on a
  key/value pair. The reasons the pair moved: a subject had no way to say
  "the members of this incarnation" other than a tag spelled like its name — the
  escalation ADR-008's 2026-08-05 amendment closed — and the address is the pair
  `service.incarnation`, so that incarnation names may stop being globally unique
  without every rule becoming ambiguous.

  **Every request body and every response changes shape.** Migration 113 rewrites
  the three tables in place: `sid` becomes `TEXT[]` everywhere and `rites.coven`
  joins it (it was the lone scalar), four columns are added per table, and each
  `*_subject_xor` check becomes `*_subject_one_of` alongside pair and format
  checks; the `sid` and `rites.coven` indexes are rebuilt as GIN. Existing rows
  keep the dimension they were written with. One deliberate loosening:
  `rites_coven_format` is dropped rather than ported — a per-element CHECK over
  `text[]` needs a trigger to express, which is the call migration 041 already
  made for `vigils`/`decrees`, so label form is enforced at the service layer for
  all three. The audit payload renders the subject as a string — `sid=…` /
  `incarnation=<service>.<name>` / `coven=…` / `trait.<k>=<v>` — where it
  previously carried `coven=<v>` or `sid=<v>` only, so a consumer parsing that
  field needs the two new spellings.

  See the Upgrade notes above for what a `coven` or `trait` subject now reaches,
  and why labelling an incarnation became a grant-affecting act.

- **`make pkg` now builds the packages a release actually ships.** It used to
  drive `nfpm` against a separate set of configs under `deploy/nfpm/`, a second
  description of the same packages that had drifted from the `nfpms:` section a
  release is built from: it produced 3 of the 7 shipped packages, and it carried
  the wrong binary path that broke the systemd units (above). The target is now a
  packages-only `goreleaser --snapshot` build over that one definition, so a new
  or renamed package needs no second edit. `deploy/nfpm/*.yaml` are removed (the
  maintainer scripts under `deploy/nfpm/scripts/` stay — goreleaser references
  them). Consequences: `make pkg` needs `goreleaser` rather than `nfpm` in PATH,
  emits the whole matrix (every package × `amd64`/`arm64` × deb/rpm/apk) so the
  `PKG_ARCH` knob is gone along with the `pkg-keeper`/`pkg-soul`/`pkg-soul-lint`
  variants, and it wipes `dist/` first — re-run `make sbom` if you need both.

- **BREAKING for plugin authors — a custom module's manifest now gates its
  params** ([ADR-0076](docs/adr/0076-engine-compat-window.md), amendment (t)).
  A param that `spec.states.<state>.input` does not declare fails the task with
  `module.unknown_param` and the module never runs, exactly as it already did
  for `core.*`. Until now the same key was logged and the task went on, so a
  module quietly did its old job while the thing the author asked for never
  happened.

  **What breaks:** any plugin whose manifest under-declares — a param the module
  reads but never listed, or a key shared across states and declared on only
  some of them. Those tasks now fail loudly instead of silently doing nothing.
  **What to do:** declare every key the module accepts on every state that
  accepts it, and ship a new plugin version — the manifest that gates is the one
  beside the binary on the host, not a copy in a service repo. The failure names
  the offending keys and everything the host does accept, so one run is enough
  to fix it.

  There is deliberately **no opt-in flag**. A manifest carrying an unknown key
  is not merely ungated on an older Soul — plugin discovery skips the whole slot
  on a decode error, so the module would disappear entirely (`module.not_found`).
  And a declaration that needs a second declaration saying "I mean it" is not a
  declaration: `input:` is the contract in both manifest homes or in neither.

  From here the contract can only shrink through the declared window: mark a
  param `deprecated: {since, removed_in, use?}`, keep honoring it for at least
  two minor releases, and only then drop the key.

- **The static params check is no longer core-only**
  ([ADR-0076(x–z)](docs/adr/0076-engine-compat-window.md)). `validateModuleParams`
  returned on the first line for any namespace but `core`, so the four checks it
  runs — never core-specific, they read a manifest's `StateDef` — were simply
  switched off for plugins. What core actually had was *availability*: its
  manifests are compiled in. A plugin manifest now arrives through
  `ValidateOptions.ModuleManifests` and is applied in a post-pass over the parsed
  document, with the same checks and one implementation of them.

  Two resolvers, because there are two moments an author can be told. **Keeper**
  resolves from its Sigil grants — the byte-exact manifest whose signature was
  verified, i.e. the very one that will gate the task on the host, so the check
  cannot refuse a run the Soul would have accepted. **`soul-lint --modules
  <dir>`** resolves from manifests on disk, for the author with no cluster, and
  indexes by the address a manifest **declares** rather than the directory it
  sits in — a plugin directory is named after its binary
  (`soul-mod-community-redis`) while a task addresses `community.redis`.

  The walk knows nothing about task grammar: any mapping carrying a `module:`
  string is a module task, wherever it sits. Keying off `tasks:` / `block:` /
  `apply:` would silently stop checking the day the grammar grows another
  construct that holds tasks — which is the old early-return's failure mode one
  level up.

  Nothing here refuses a render for an *unresolvable* module: an author usually
  cannot produce somebody else's manifest, and a keeper whose allow-list is
  briefly unreadable must not turn a storage blip into a failed run. Both say
  `plugin_params_unchecked` instead.

- **A scenario run can now be refused before it starts.**
  `POST /v1/incarnations/{name}/scenarios/{scenario}` and its MCP twin evaluate a
  topology `assert:` after input validation and before the runner starts, so a
  roster that does not satisfy the scenario's invariant answers **422
  `assert_failed`** instead of the previous unconditional 202 followed by
  `error_locked` and a manual unlock. This is the flow the two-point gate was
  written for and the one `create` can never be: the incarnation exists and its
  roster is bound. A plan that builds its own roster — all-keeper, or one that
  emits a refresh — is still admitted on an empty roster, carried by a single
  predicate shared with the no-hosts bypass rather than restated beside it. A
  roster-reading assert in a `create` starter is legal and now says so at lint
  time with `assert_roster_deferred_on_create` (WARNING): the construct works,
  only the expectation of a 422 on create was wrong, and that expectation came
  from ADR-009 itself.

- **`soul-lint` rejects three constructs that used to parse, render and do
  nothing.** `require_forward_reference` — a barrier resolves its targets at the
  awaiting task's plan position, so a `require:` naming a source that starts
  later waits for nothing; rejecting it also makes a `require:` **cycle**
  unrepresentable, since a cycle needs at least one forward edge.
  `async_on_apply_invalid` — `async:` on an applier task reaches none of the
  destiny tasks it fans out into, so the group runs sequentially regardless.
  And `require:` on a `block:`, documented as inherited in three places and in
  fact dropped from the plan, is now merged like the block's other keys (union of
  names, `all` on either side absorbing the list). All three ride
  `validateTaskRefs`, so scenario and destiny get them on the same terms, at
  parse and in `soul-lint`, with line and column.

- **An `apply:` task no longer drops the keys written on it.** The function that
  expands an applier into its destiny group was handed the `apply:` block and the
  `register:`, never the task — so `when:` / `onchanges:` / `onfail:` /
  `require:` reached none of the tasks it fanned out into. Where each key is
  answered now follows from where it *can* be answered. The three requisites are
  resolved name→index over the flat plan, so an index means the same thing on
  both sides of the destiny boundary: they merge into every child, exactly as a
  `block:` passes its own down. `when:` is not portable that way — a child's flow
  context is built in the **isolated destiny env**, where `input.` / `vars.` /
  `essence.` name different things than in the scenario the predicate was written
  in — so a static one is decided at render as before, and a dynamic one is
  refused (`apply_when_dynamic_unsupported` offline, `ErrUnsupportedDSL` at render
  for the block-inherited case the offline validator cannot see). Refusing is not
  a new restriction; it is the boundary [ADR-056](docs/adr/0056-staged-render-passage.md)
  already draws for a cross-Passage `when:`, one level up. The restriction was
  the silence: a gated destiny applied everywhere the applier targeted.

- **A register that fanned out through `loop:` resolves to every one of its
  tasks.** The name→index map kept one entry per register while a `loop:` emits N
  tasks under one name, so last-wins silently picked the final iteration. The map
  now holds every index for a name and no consumer changed: `skipOnChanges` /
  `skipOnFail` already run a task if **any** named source changed or failed, and a
  barrier already waits for **all** of its targets — which is what a fanned-out
  name should mean in each case. The same last-wins on the Soul side
  (`asyncFlows.byName`, used by the implicit barrier) is fixed with it, and
  `resolveRequire` now walks per name: pairing a flattened index list back onto
  names by position named the wrong source in the cross-Passage error, or ran off
  the end of the slice.

- **An applier's `vars:` reach the input it passes to its destiny.** A `block:`
  passes its `vars:` to every descendant (destiny/tasks.md §6.5) — except that a
  module descendant could read `${ vars.x }` and an `apply:` one could not:
  `resolveApplyInput` built its env with `hostVars` alone and never called
  `resolveTaskVars`, so the render failed there with "no such key". The applier's
  own `vars:` had the same fate. They are now resolved into the env that renders
  `apply.input`, exactly as the module path does it. Isolation is untouched:
  `apply.input` renders on the **caller's** side, so only the resulting values
  cross, which is what every other `apply.input` value already does — a destiny
  still sees none of its caller's `vars.*`, only its own `vars.yml`.

  ★ This is why the key left the `<key>_on_apply_invalid` family it briefly
  joined. Refusing it could never have reached the case that mattered: the key is
  written on the `block:`, where it is legal, and the descendant carries no key
  of its own — invisible to any offline validator by construction.

- **Module-specific keys on an `apply:` task are refused** (family
  `<key>_on_apply_invalid`: `changed_when`, `failed_when`, `retry`, `timeout`,
  `params`, `no_log`) — the apply-side mirror of the
  `<key>_on_block_invalid` family, for the other construct that expands into a
  group. Unlike the keys above, these never could have worked: an applier invokes
  no module, and render hands its children only the three requisites, so nothing
  else it carries reaches a rendered task. Membership is decided by one rule —
  the key **works on a module task and is lost on an applier** — and each is
  refused with its own reason rather than a shared sentence: no module result to
  re-judge (`changed_when`/`failed_when`), one call's retry or timeout applied to
  a group (`retry`/`timeout`), module arguments where the destiny takes
  `apply.input` (`params`), and a group mask that is not implemented, so the
  output it was written to hide was **logged in full** (`no_log`). A key that
  turns out to be answerable on the caller's side leaves the family instead of
  staying refused — `vars:` did, above.

  ★ They were not simply dropped, which is why refusing beats leaving them: a
  static-false `when:` collapses an applier into one skip placeholder that *does*
  copy `changed_when`/`failed_when`/`timeout`/`no_log`/`id` onto itself. The keys
  were honoured exactly when they could not matter and ignored whenever they
  could.

  `output:` is deliberately **not** in the family. It is unread on every task
  type today, not only on an applier, and belongs to the planned projection of a
  destiny's top-level `output:` into `register.<applier>.<field>`
  ([orchestration.md §2.1.1](docs/scenario/orchestration.md),
  [destiny/output.md](docs/destiny/output.md)) — refusing it here would pre-empt
  a design that slice owns and would report an unimplemented key as a meaningless
  one. `id:` and `loop:` needed no new rule: both already refuse every non-module
  discriminator, an applier included. No example in the corpus carries any of the
  refused keys on an applier (37 applier tasks across 394 files), so nothing that
  ran before stops rendering.

- **`async:` on `on: keeper` is refused offline** (`async_on_keeper_invalid`),
  joining `async_on_block_invalid` and `async_on_apply_invalid` — the third and
  last construct where the flag is meaningless, since a keeper task is executed by
  the keeper's own runner and never reaches a Soul. Render refused it before; this
  moves the refusal to where the author is looking. `require:` on a keeper task
  stays legal — redundant, because the keeper executor runs its tasks in plan
  order.

- **The bundled `redis` example installs from a package repository by default.**
  `essence.install_method` defaults to `package`, and `essence.install_package`
  names the repository — the official Redis one by default, since it publishes
  every version of the operator's enum while a distro repository carries only the
  release's own. The destiny declares the repository before installing and
  composes the per-host apt pin from the upstream version and the host codename.
  All of it lives in one `install` map in essence, because the six scenarios that
  re-apply the destiny each have to pass it, so a fleet on an internal mirror
  overrides one map in `spec.essence`.

- Package renames, dropping a doubled `soul-`: `soul-stack-soul-lint` →
  **`soul-stack-lint`**, `soul-stack-soul-trial` → **`soul-stack-trial`**. The
  binaries (`soul-lint`, `soul-trial`) are unchanged. Both packages declare
  `Provides`/`Replaces`/`Conflicts` on their old names, so `apt`/`dnf` retire the
  old package on upgrade rather than leaving it orphaned.

- `soul-legion` is now a supported artifact ([ADR-004 Amendment
  2026-07-26](docs/adr/0004-binaries.md)), shipped as **`soul-stack-legion`**, so
  operators can size their own clusters instead of trusting the projection table.
  It gained `--version`, and every environment flag (`--keeper-endpoint`, `--ca`,
  `--pg`, `--vault`, `--openapi`) is now **required with no default** — it used to
  fall back to a developer box (`/tmp/keeper-dev` CA, a localhost DSN, Vault token
  `root`), which is not something a released binary may do.

  It is **not** a black-box benchmark: it writes the stub souls' identity straight
  into the cluster (`souls`/`soul_seeds`) and mints their certs from Vault PKI, so
  it needs cluster database credentials and a PKI-issue token. Run it against a
  bench cluster, never production.

### Fixed

- **A `coven=` scope on the three per-host Soul mutations denied every call
  instead of narrowing them.** `NIM-588`. `soul.forget`, `soul.issue-token` and
  `soul.ssh-target-update` were gated by a selector that put only `host=<sid>`
  from the path into the RBAC context. A condition over a dimension the context
  does not carry fails closed, so `soul.forget on coven=web` was not "forget hosts
  in `web`" — it refused the whole permission, and the 403 named a right the
  operator held. Both ways a coven attaches were affected, the `on coven=…` suffix
  and a bare permission inheriting a role's `default_scope`; they meet at the same
  scope expression before the check, so neither was a special case of the other's
  bug. The gate now resolves the host's `souls.coven` list first and offers the
  enforcer one context per label — a host holds several ([ADR-008](docs/adr/0008-coven-stable-tags.md)),
  so a single context could only ever have asked about one — admitting if any
  passes. MCP resolves the identical contexts before the tool body runs, since
  both surfaces are primary ([ADR-004](docs/adr/0004-binaries.md)) and a fix on
  one of them leaves the operator with the same 403 one surface over. A host whose
  row cannot be read still asserts the host alone: coven-scoped grants fail closed,
  `on host=` grants keep working, and a database outage widens nothing. Note the
  scope of the fix — `errand.run` and the live `soul.console` still build a
  host-only context, so a `coven=` on either continues to deny.

- **A second of clock drift between Keeper instances answered `401 invalid
  token` on a token seconds old.** `NIM-621`. Keeper runs as several stateless
  instances over one signing key from Vault
  ([ADR-002](docs/adr/0002-transport-grpc-ha.md),
  [ADR-014](docs/adr/0014-operator-identity.md)), so a token minted by one is
  routinely verified by another — and the verifier validated `iat` with no
  tolerance at all. A node one second behind the issuer therefore rejected a
  one-second-old token as issued in the future, the operator's retry landed on a
  different instance and succeeded, and what they saw was authentication that
  flaps. The answer made it worse: every cause other than expiry collapsed to
  `invalid token`, the same string a forged signature produces, so drifting
  clocks were indistinguishable from an attack and the diagnosis went to the
  signing key. The project already treats skew as normal elsewhere
  ([ADR-018](docs/adr/0018-soulprint-typed.md) warns above 10 minutes rather
  than refusing).

  `iat` and `nbf` now carry a 60s tolerance — RFC 7519 §4.1.4 allows "some small
  leeway", and the budget stays small on purpose because rejecting a made-up
  `iat` is what validating it is for. Past the budget the refusal is its own
  cause: `detail: "token issued in the future"`, not `invalid token`. A client
  matching on the exact string `invalid token` will see the new one for this
  case; that is the only behaviour change on the wire.

  **`exp` is deliberately excluded.** golang-jwt applies one tolerance to every
  time claim and offers no way to split them, so the leeway alone would have
  accepted a token up to 60s past its expiry. On `auth.jwt.exchange_ttl`, whose
  floor is one minute ([ADR-058](docs/adr/0058-operator-auth-ldap-oidc.md)),
  that doubles how long a stolen Bearer keeps working — and it buys nothing,
  because drift on `exp` only ever costs the last second of a token's life,
  when the holder should be getting a new one anyway. `Verify` therefore
  re-checks expiry strictly afterwards, and expiry still arrives as its own
  error: the cookie exchange keeps answering `token expired` rather than the
  generic `authentication required`, which is the difference between telling a
  browser to sign in again and telling it nothing.

  The e2e harness had built a conclusion on the old ambiguity. Its keeper
  identity check read any 401 against a freshly minted token as proof that some
  other process had taken the port, and reported an address to go and
  investigate — a reading that was never sound and is now demonstrably wrong,
  because a wall clock stepping backwards produces that 401 on a stack talking
  to its own keeper. It now branches on the cause the keeper names and says
  "check the clock" for skew. The strings it matches on live in another module's
  `internal/` and cannot be imported, so a guard reads them out of the
  verifier's source instead: rename one there and the guard says which binding
  went stale, rather than the harness quietly falling back to blaming the port
  again.

  The `-1s` durations in gate summaries are clamped in the same pass
  (`NIM-611`): `SECONDS` follows the wall clock, an NTP correction or a WSL2
  resume steps it backwards, and the release table people read the run from was
  printing negative tier times.

- **`make dev-stop` stopped nothing, and took `make dev-down` with it.** `NIM-615`.
  The recipe quoted an inner `grep` pattern with `'...'` inside its own `'...'`
  string, which does not nest — it closes. With `SHELL := /bin/sh` the target was
  therefore never one command: it was the pipeline `bash -c '<truncated script>' |
  node | npm '<the rest>'`. `bash` got a script cut off mid-statement and refused
  it as a syntax error, so not one process was signalled, and make then exited 127
  on the missing `node`. `dev-down` declares `dev-stop` as its prerequisite and
  died there, before ever reaching `docker compose down`.

  The failure mode is the one the target exists to prevent: an orphan `keeper run`
  holding 8080/8081/9090/9442/9443, with the next bring-up failing on `bind:
  address already in use` and the operator told only `Error 127`. Broken since
  2026-07-22 and invisible for two and a half weeks, because a Makefile recipe is
  the one kind of code in this repo that nothing compiles, lints or executes.

  Fixed by quoting the pattern with `"..."`, and gated by the new
  `check-makefile-recipes` tier, which is two layers because one does not cover
  it. It **scans** every `bash -c` in the Makefile for an argument that is one
  fully quoted string — breadth, and safe on recipes that redirect to absolute
  paths — and then **runs** `dev-stop` for real against a throwaway stand, which
  is the only way a defect in what the script does, rather than in how it is
  quoted, can be caught. The run needs no docker and takes no stand slot
  (`DEV_STAND_SLOT` short-circuits allocation ahead of the registry).

- **Every E2E tier could report on code that was not in the tree.** L3a and L3b
  spawn `keeper/bin/keeper` — whatever the last build left there — and L3c
  deploys the `keeper:e2e-k8s` image the local daemon happens to hold. None of
  the three compiled anything, and `make e2e` did not depend on `build`, so the
  verdict was about an artifact rather than about the source. The failure is
  symmetric and both halves are expensive: deleted wiring stayed green until
  someone ran `make build`, and a test "reproduced" a defect the tree had
  already fixed. Absence was loud (the harness skipped when the binary was
  missing) and staleness was silent.

  Two changes, because either alone leaves a way in. The Makefile targets now
  build what they test (`e2e: build`; `e2e-live` gained `build` alongside
  `build-linux` — those produce *different* keepers, and the tier ran the one
  that was not being rebuilt). And each harness now refuses outright: before any
  stand comes up it asks the artifact for its version and compares it with the
  tree's, so a hand-run `go test -tags=e2e` cannot go green on yesterday's
  binary either. Refuses, not skips — a skipped pre-flight is the defect again,
  one level up.

  Two axes, because one cannot cover both cases. The version catches another
  commit; file timestamps catch an uncommitted edit made after the build, which
  no version can see. `-dirty` is stripped before comparing (it is repo-wide, and
  reddening a byte-correct binary over a README edit is how a gate gets turned
  off), and the timestamp axis looks only at uncommitted files under
  `keeper`/`shared`/`sdk`/`proto`. Each message names the command that fixes it,
  and the L3c one says which command does *not*: `make build` produces the host
  binary, while the cluster runs an image.

  In L3a and L3b the refusal happens inside the declared bring-up region, so it
  reaches a reader as STAND-SETUP — correct, in that nothing was asserted, but
  STAND-SETUP's standing advice is "rerun this one alone", and rerunning a stale
  binary reproduces it forever. Both classifiers —
  `scripts/classify-l3a-failure.py` and `scripts/classify-e2e-live-failure.py` —
  now recognise the refusal and say `make build` first. They key on a string in
  the harness's message, so a self-test asserts both of the harness's stale paths
  still carry it; an unchecked copy is the same silent drift one level up. That
  count reads Go string literals only: over raw source, a comment quoting a
  message the harness no longer prints keeps the check green — which is exactly
  what rewording the message leaves behind.

  In both scripts the advice is a pure function, and in both the *join* is
  pinned separately, because a branch and its caller fail differently: a correct
  function reached with the wrong argument still returns confidently. Each
  script's self-test now runs a real stale-binary log through its own entry
  point, as a subprocess, and requires the rebuild advice in the rendered
  report. A fixture that calls the renderer directly cannot do this — it picks
  its own argument, so it cannot tell the real one from an empty string. L3a
  keeps an AST check beside the rendered one: that pins the *shape* (`main`
  hands `next_step` the family it classified) where the log pins the *value*.
  The advice line the rendered check looks for is asked of `next_step` rather
  than spelled out, so rewording a headline does not redden it while the
  headline never reaching the page does — `make build` appears in the paragraph
  above the advice too, and on its own it does not tell the two apart.

  Two things a stamp comparison quietly misses, both closed here. An
  **uncommitted deletion** is invisible to both axes — `-dirty` is stripped, and
  a file that is gone has no mtime — and `tests/e2e` has no `replace` for keeper,
  so deleting keeper source does not even break the tier's build. The mtime axis
  now falls back to the nearest surviving parent directory, which `unlink`
  refreshes. And on L3c, `.Created` is **frozen by BuildKit** across rebuilds: the
  image ID changes, the timestamp does not, so a rebuilt image read as
  hours-old. The provenance check now prefers `.Metadata.LastTagTime` and falls
  back to `.Created` (the tag stamp is zero for a pulled image), with both
  Docker's timestamp renderings parsed.

  The guards derive their subject rather than listing it: "every function that
  resolves the keeper binary" comes out of the AST, minus a named exemption list
  that carries the reason for each entry. A hardcoded list of who-must-check goes
  stale in the silent direction — add a spawner, forget the list, and the guard
  reports green about something it never looked at, which is this ticket's own
  shape. An exemption list goes stale loudly. The L3c deployment manifest is tied
  to the image constant the same way, since committed YAML cannot reference a Go
  const. The rule caught one of this change's own lists: the four directories the
  timestamp axis watches were hardcoded, and copied into three tiers that are
  separate Go modules and cannot share them. They are correct today and would
  have gone quiet the day keeper links a fifth module, so they are now derived by
  walking keeper's first-party import closure and compared against all three
  copies — in both directions, since a missing root is a false green and a
  surplus one is a false red. The copies themselves are found by glob over
  `tests/*/harness/provenance.go` with a floor of three, so a fourth tier joins
  the comparison the day it appears, not the day someone remembers to list it.

  Found on the way: `.dockerignore` excluded `*/bin/`, which is exactly where
  the L3c image Dockerfiles (`tests/e2e-k8s/dockerfiles/`) COPY from — the
  `deploy/docker/` images of the same names build inside a builder stage and
  are unaffected — so `make docker-build-keeper` had been failing since the
  beta, which is the commit that introduced both halves.

- **A restarted dev Vault made `dev-provision` hand you someone else's stand,
  silently.** The dev Vault stores its secrets in RAM (`dev/docker-compose.yml`);
  Postgres stores its data on a named volume. A container restart therefore does
  not reset the stand, it desynchronises it — and `dev/provision.sh`, being
  idempotent, saw a missing key as "not created yet" and minted a fresh one over
  live registries. Nothing errored. `operators` still held its Archons while
  every JWT ever issued to them stopped verifying (401 — the rights are intact,
  only the signature no longer matches); `soul_seeds` still held seeds chaining
  to a PKI root that had just been replaced, so mTLS failed and the souls had to
  be re-onboarded; `plugin_sigils` still held grants signed by an anchor that no
  longer existed ([ADR-026](docs/adr/0026-sigil.md)). The stand looked healthy
  and reported ready throughout.

  Provision now checks the three anchors against the registries that depend on
  them **before it generates any anchor** (step 1b) and refuses, naming the rows
  at stake and ranking the ways out by blast radius. `DEV_VAULT_REISSUE_ANCHORS=1`
  takes the loss on purpose, listing what it destroys as it goes.

  Three things the guard does that the symptom does not suggest. First, the KV
  prefix is per-stand but the `pki/` engine is **one per Vault**, and every
  lightweight stand shares one — so the seed count is taken across every
  `keeper*` database, not just this stand's, and when a neighbour is among the
  losers the refusal stops offering "drop your own database": that advice would
  be false, since the root is regenerated under their souls whatever you do to
  yours. Second, a database list that cannot be read is reported as UNKNOWN
  rather than counted as empty — the whole PKI half of the check hangs off that
  one query, so treating a failed `psql` as "nobody depends on the root" would
  reproduce the silence being fixed. Third, an unreachable Postgres warns instead
  of blocking — with no registry to ask, a first-ever stand is indistinguishable
  from a wiped Vault, and refusing there would break every initial provision. A
  first-ever stand with empty registries generates them without objecting on a
  machine with no neighbours, but not on a shared one: restart the Vault container where somebody
  else already has souls and the next brand-new stand is refused too, because the
  seed count spans every `keeper*` database and the root it would mint is the one
  their seeds chain to. That refusal is the guard working, and it points at the
  neighbour rather than at a database to drop.

  One dev script changes behaviour with the guard. `dev/upgrade-demo/ui-stand.sh`
  runs `make dev-provision` on exactly this symptom — `dev/mint-jwt.sh` failing —
  so on a desynchronised host the demo now stops on the refusal instead of
  quietly reissuing the anchors and coming up over registries it has just
  orphaned. It runs under `set -e` and calls `make` directly, so the last lines
  are the guard's `[provision] [fail]` block and make's own exit, with no
  `[ui-stand][FAIL]` summary after them: take one of the ways out it lists, then
  re-run the script.

- **No fresh dev stand came up, on the release or on any branch off it.**
  `NIM-377` deleted the plugin's hand-written `manifest.yaml` and moved a
  module's contract into a generated canonical-JSON document stamped into the
  artifact itself. Step 9b of `dev/provision.sh` went on requiring the deleted
  file and calling `fail` when it was missing, so `make dev-provision` — the
  first thing both `make dev-stand` and `make dev-smoke` run — died before it
  reached anything else. Existing stands were untouched, which is why this stayed
  quiet for a whole release: the step is idempotent and they already had their
  plugin repo, so only the *first* run in a new `DEV_STAND` broke. Every session
  that owed a live self-check hit it on its first command.

  The step now builds the plugin the way the L3b fixture does, stamps the
  published document into the artifact through `dev/stamp-artifact.go` — the
  trailer format is defined once, in `sdk/schema`, and shell cannot append it
  without keeping a second copy that would go on agreeing with the old model —
  and publishes `dist/<artifact>` alongside `dist/schema.json` into the git repo
  the `plugingit` resolver clones.

  Softening that `fail` to a `warn` would have been the wrong fix, and it is why
  this entry is long. Every way this step can go wrong ends in the same place:
  the resolver fails one catalog entry closed, `ResolveCatalog` demotes it to a
  warning, keeper comes up green, and the plugin is silently absent until some
  scenario calls it — where the cause costs far more to find. An unstamped
  artifact does that. So does a document that is canonical but invalid, a `dist/`
  with no executable or with two, and deleting the single line that calls the
  step. The step therefore checks what git actually recorded before it commits,
  the stamper refuses any document keeper would refuse, and a guard in the gate
  holds the script to each of those acts — anchored on the commands that perform
  them, because the step's own log line names all of them and would stay green
  while the writes it describes were gone.

- **A red module ended the sweep, and the five modules behind it left no trace.**
  Every per-module loop in the `Makefile` — `test`, `test-race`, `vet`, `tidy`,
  `build`, `check-vuln`, `vet-tags`, `test-plugins` — was written
  `for m in $(MODULES); do (cd $$m && …) || exit 1; done`. `MODULES` is eight
  entries in a fixed order, so a failure in `shared`, the third, meant `sdk`,
  `keeper`, `soul`, `soul-lint` and `soulctl` were never tested and nothing in
  the output said so. The tier above printed one honest `FAIL test`, and
  `gate.sh` reported exactly what it was told: one failed tier, zero not run.
  True about tiers, false about modules.

  That is the `NIM-373` defect one level down, and its cost lands where
  `NIM-380`'s did — at release acceptance, on a red run read in a hurry. "That's
  `shared`, we know about that" is a reasonable thing to think, and it is
  compatible with the run having said nothing whatsoever about `keeper`. An
  unperformed check is indistinguishable from a passed one unless something
  names it.

  `scripts/modules-run.sh` now runs the command across every module and reports
  four states rather than two. `PASS` and `FAIL` are the module's own verdict.
  `SKIPPED` means the probe found nothing to run there, and the reason is
  printed beside it. `NOT RUN` means nobody asked — fail-fast was requested
  (`MODULES_FAIL_FAST`, opt-in and off by default) or the run was interrupted —
  and it is never folded into a pass: the summary names those modules and says
  in words that the sweep is silent about them. An interrupted sweep still
  prints the table, which is exactly when "how far did it get?" is worth most.

  Several conflations went with it, all of the same shape — a refusal is only
  worth anything if it gives the right reason:

  - **A broken probe read as an empty module.** The old test,
    `[ -z "$(cd $$m && go list ./... 2>/dev/null)" ]`, is empty both when a
    module has no Go packages and when `go list` could not run at all — a broken
    `go.mod`, an unresolvable import, a missing directory — so a module nobody
    could even enumerate was reported as "no Go packages", skipped, and left the
    sweep green. A probe that breaks is now a failure by default with its own
    stderr quoted; a caller that genuinely wants to skip on it says so
    (`MODULES_PROBE_FAIL=skip`) and gets a **different** sentence in the report,
    because "there is nothing here" and "we could not find out what is here" are
    different answers.
  - **A sweep over zero modules printed a success line.** `test-plugins` did
    exactly that over an empty plugin glob. It is now a refusal that names the
    empty corpus rather than the calling convention, which was fine — sent to
    check the invocation, nobody goes looking for the missing plugins. A blank
    command is refused for the same reason: it satisfies an emptiness test, runs
    as a no-op in every module, and reports a green sweep over work nobody did.
  - **A module that is not on disk read as an empty one.** It has its own verdict
    now — `no such module directory`, decided before any probe runs, so nothing
    about it is ever explained in a probe's words. A directory that exists but
    cannot be entered is a failure too, and that one matters most under
    `MODULES_PROBE_FAIL=skip`, where it used to read as "does not resolve
    offline", report `SKIPPED`, exit 0, and never print the `permission denied` —
    a skip quotes nothing.
  - **One scratch file carries the probe's stderr for the whole sweep**, so a
    probe that fails without writing a word — `exit 1` on a precondition is the
    ordinary shape — could be quoted the previous module's complaint under its
    own name. What prevents it is that the redirect truncates on open, which is
    one character away from not doing so; the guard has the case and the `2>>`
    known-bad is measured beside it.
  - **A module list carrying a newline swept its first line only.** `read -r -a`
    takes one line, so the table would have covered part of the corpus while
    reading as all of it. Nothing calls it that way today; the script refuses a
    sweep over nothing, and quietly sweeping over half is the same claim.

  The scope here is the eight targets listed above. `test-integration` still
  tests its modules with the old `go list` idiom and is left alone in this
  change.

  `vet-tags` and `build` needed one more thing, and it is this ticket's own
  defect surviving inside its fix: `make` runs each recipe line in its own shell
  and abandons the target on the first nonzero exit, so a module sweep that
  reported every module and then returned 1 still meant the four tagged
  directories — and every binary — were never reached and never named. Both are
  now a single recipe line with an accumulated `rc`. For `build` the comforting
  story was that the chain is safe because the binaries link the library modules
  above them, and the `go.mod` files say otherwise: `soulctl` requires none of
  the four, `soul-lint` only `shared`, and `soul` is isolated from `keeper` by
  ADR-011.

  A sweep in which every module skipped now says the command ran nowhere. It
  stays exit 0 — `test-plugins` offline skips every cloud and ssh plugin, and
  that is a real answer — but `0 passed, 0 failed, 3 skipped` with nothing else
  on the line reads as a pass over work that happened in no module at all, one
  state along from the `NOT RUN` that already had a sentence.

  `check-modules-run` guards the reporter, for the reason `check-gate` and
  `check-ci-status` guard theirs: a restored early exit does not crash or print
  an error, it just ends the sweep sooner and still prints a summary, which is
  the most complete-looking thing in the log.

- **A console test failed about once in fifty, and the defect was in the test.**
  `TestConsoleWS_WriteFailureIsReportedAtWarn` waited for `hub.Count() == 0` and
  then read the log in one shot. But `Count()` drops at `unregister`, the
  **first** step of `Hub.Close`: the close dispatch to the soul, the recording
  close and the audit write all happen after it, and the `sessions reaped` line
  the test asserts on is written after all of those. So the test was reading the
  log during a window it had explicitly waited to be past, and whether it won
  depended on the scheduler. The reap itself was never late — nothing was wrong
  with the subject — but a run that goes red for a reason unrelated to the
  change under test costs the same as a real finding at release acceptance, and
  gets believed less the next time.

  Both assertions now poll to a deadline, and the fake soul stalls its close
  dispatch by a fixed 250 ms so the teardown tail is observably long on every
  run. That is the part worth keeping: the delay stays in the test permanently,
  so restoring the one-shot read is red every time rather than one run in fifty.

  The sibling case that asserts a clean disconnect logs **no** failure had the
  same vantage-point problem and a worse version of it, because absence is the
  whole claim: it asserts that *nothing* was logged at WARN, over teardown steps
  that all come after the count it waited on and any of which can warn. It now
  waits for `keeper_console_sockets_active` to reach zero — the final statement
  of the socket handler, after the pumps are joined, the sessions reaped and the
  reap line written — which is the only observation that means "finished" rather
  than "got somewhere". Made an ordinary close report as a failure, the test is
  red from there and green five times out of five from the old vantage point.

- **Every service declaring `modules:` was unappliable on every host.** The two
  ends of the auto-synthesis path (`ADR-065`) disagreed about what address level 1
  means, and each was internally consistent, tested, and green. `NIM-377` renamed
  that level: it stopped being a namespace the artifact declares about itself and
  became the **alias an operator registers it under** — which is why the artifact
  no longer carries a name at all. `core.module.installed` was updated to the new
  meaning and rejects anything but a bare alias; the synthesizer — the only thing
  that produces its steps — kept passing the whole `<alias>.<module>` entry from
  `service.yml::modules[]` verbatim. No value satisfies both, so a manifest
  declaration that is supposed to save the author from writing an install step
  produced a step that could never run. Level 1 is now what reaches
  `params.name`, and a property test asserts the shape of every synthesized step
  rather than one expected string, because the string is exactly what agreed with
  itself across the break.

  Two things the same reading corrected. Several `modules[]` entries served by one
  artifact — `community.redis` and `community.sentinel` are one binary — used to
  synthesize one install **each**, re-fetching the same slot; they now collapse to
  a single step before the earliest consumer, and pinning them to disagreeing
  `ref`s is a schema-validation error (`conflicting_module_ref`) instead of a
  race to overwrite the slot. And the documented escape hatch — write the install
  step yourself and synthesis stands aside — had been silently dead since
  `NIM-377`: takeover was recorded under the alias the operator wrote and looked
  up under the dotted entry, so the operator got an unrequested duplicate next to
  their own step.

  `ADR-065` carries the amendment, because the contradiction was written down
  before it was compiled: the `NIM-377` amendment declared `modules[].name` to
  *be* the alias and left the field's two-level regex standing in the same
  paragraph. The declaration stays an address — it is what a scenario writes, and
  a service naming only the slot would not be saying which module it calls.

- **`dry_run` on `POST /v1/souls/{sid}/exec` reached no module at all.**
  The flag is in [ADR-033](docs/adr/0033-errand.md), in OpenAPI, in the MCP tool
  and — since the previous entry — behind a fail-closed capability gate, and it
  was terminal for every module in the tree. Errand admission and the Plan/Apply
  choice were two different questions answered by one condition: a request had to
  clear `ErrandReadSafe` (or be verb-shell) to be admitted at all, and only then
  was `PlanReadSafe` consulted to decide whether `Plan` ran instead of `Apply`.
  Nothing carries both markers — `ErrandReadSafe` is on `core.http` and
  `core.noop`, `PlanReadSafe` on `core.file` and 12 others — so `dry_run: true`
  ended as `module_not_allowed` before any `Plan` was reached. The ADR never asked
  for that: its item 2 governs **`Apply`**, its contract row for `dry_run` governs
  **`Plan`**.

  Admission is now asked per path. `dry_run: true` requires `PlanReadSafe`, so
  `core.file.present` can be asked what it would change on a host; a module
  without it (including `core.cmd.shell` / `core.exec.run` / `core.http.probe`,
  which have no pure-read `Plan`) answers `failed` +
  `errand_dry_run_unsupported`, exactly as the contract row states. **No module
  gained a write path:** `ErrandReadSafe` is still the only marker that opens
  `Apply` through an Errand, so `core.file` remains refused for ad-hoc apply under
  an explicit `dry_run: false` and under an omitted field alike. Handing
  `core.file` the `ErrandReadSafe` marker would have read like a label and worked
  like a rights extension — arbitrary file writes outside any scenario, with no
  `state_changes` — and was rejected for that reason.

  The module catalog publishes one boolean about Errand, `errand_safe`, and it
  is an Apply-path fact; nothing in it says a module can be planned. For dry-run
  it selects the exact complement of the right answer: the three entries that
  carry it — `core.cmd.shell`, `core.exec.run`, `core.http.probe` — are precisely
  the ones a `dry_run` request is now refused on, and all 13 `PlanReadSafe`
  modules it can reach are marked `false`. Any consumer filtering on it (the Run
  wizard does) therefore offers only modules that cannot be planned, so the new
  capability is reachable through the API and MCP but not through the module
  picker; the catalog needs a way to say this at all (NIM-556).

- **A `kind=command` Voyage with `dry_run: true` applied to every host for real.**
  The flag was accepted by `POST /v1/voyages`, stored in the `voyages` row, echoed
  back by `GET /v1/voyages/{id}` and shown in the UI — and then left behind. The
  per-host leg builds an [Errand](docs/adr/0033-errand.md) dispatch request, and
  that request was assembled without the field, so Go's zero value made every
  target run `Apply`. An operator previewing a change across a fleet changed the
  fleet, and nothing said so: the API answer, the row and the UI all agreed with
  what had been asked for, and only the hosts disagreed. **If you have
  `kind=command` runs recorded with `dry_run: true` from before this release, those
  hosts were modified.** The bug is as old as `kind=command`; the Errand work of the
  previous entries exposed it rather than causing it.

  The flag now rides the per-host request, threaded through the single call the two
  batch frames (`barrier` and `window`) share, so neither can drift from the other.
  Two guard tests, not one: `dry_run: true` must arrive at the spawner, and `false`
  must not arrive as `true` — a preview that quietly applies and an apply that
  quietly previews are both wrong, and one assertion catches only the first. The
  capability gate of the entries above now covers Voyage targets as a consequence:
  a host that never announced `dry_run` fails on its own row instead of applying,
  and that gate was not loosened to let a fleet run through.

- **`dry_run` on a verb-shell module now answers `400`, not `200` with a failure.**
  [ADR-033](docs/adr/0033-errand.md) has promised a keeper-side refusal since it was
  written; only the Soul-side half existed. Asking to preview `core.cmd.shell` got
  `200` carrying `FAILED errand_dry_run_unsupported` — a request that cannot succeed
  on any host, in any fleet, at any agent version, reported as an attempt that
  happened to fail. On the Voyage path it was wrong in the other direction — the
  flag never reached the host, so the command line ran for real on every one of
  them (the entry above). With the flag threaded, that same request would instead
  fan out one identical, unavoidable failure per host, all of it after the operator
  had been told `202`; this is the case the refusal catches at creation.

  Keeper now refuses the pair up front, naming the module, on `POST
  /v1/souls/{sid}/exec`, the MCP twin, and `kind=command` Voyage create and preview
  — one validator called from both entry points, so the surfaces cannot answer
  differently, and ahead of the `errands` row, the `errand.invoked` audit event and
  Voyage scope resolution. `malformed-request`, not one of the `409` capability
  types: nothing about the cluster or the target binary can make it work, so
  pointing at a capability would send you to upgrade an agent that would refuse it
  too. Unlike the capability refusals, "without `dry_run`" is the right advice here,
  and the message says it.

  **The refusal is deliberately narrow.** Verb-shell is two names Keeper already
  holds as a constant (`core.cmd.shell`, `core.exec.run`); whether some other module
  is installed on a given host and whether its `Plan` is pure-read lives on the far
  side of the isolation boundary and differs per host. Those requests are still
  dispatched and still come back per-target `failed` +
  `errand_dry_run_unsupported` — `core.http.probe` included, which is a verb module
  by the ADR's prose but not in Keeper's constant. Keying the check on the module
  catalog's `errand_safe` flag was rejected for the reason the entry above records:
  that flag is an Apply-path fact and marks nearly the complement of what `dry_run`
  admits, so it would have rejected the 13 modules the flag exists for.

- **`keeper init` refused the reference `keeper.yml` we ship.**
  `auth.jwt.ttl_bootstrap: 30d` in `examples/keeper/keeper.yml` is a well-formed
  `duration`: the convention is a Go duration plus an `<N>d` suffix for days, and
  the config validation phase accepts it — which is why `keeper run` came up on
  that file without a word. The `auth.jwt.*` TTLs were then read back with stdlib
  `time.ParseDuration`, a narrower dialect that has never known the day suffix, so
  bootstrapping the first Archon died on `invalid auth.jwt.ttl_bootstrap "30d"`.
  Config that passes validation and then fails at the point of use is the whole
  defect; the example was right and the reader was wrong. All three TTLs
  (`ttl_bootstrap`, `ttl_default`, `exchange_ttl`) now go through
  `shared/config.ParseDuration`, the convention's single entry point, and the
  shipped example bootstraps as written.

  The same split ran through `console.idle_timeout` and
  `console.recording.retention`, where it was quieter and therefore worse: neither
  resolver has anywhere to report an error, so a legal `retention: 30d` came back
  unparsable and collapsed to "take the default" — recordings kept for 90 days
  instead of the 30 the operator asked for, with nothing said anywhere. Both now
  read the field with the parser that validated it.

- **`keeper init --credential-out` could not name anything but a regular file, so a
  containerised bootstrap had no way to hand the token back.** The writer opened its
  target by removing whatever was already there and recreating it `mode 0400` — which
  is how you rewrite a `0400` file without a `chmod` dance, since not even its owner
  can reopen it `O_WRONLY`. Pointed at `/dev/stdout`, the first step is an unlink of
  `/dev/stdout` itself: as a normal user it failed outright (`remove /dev/stdout:
  permission denied`) *after* the Archon was already committed, and running as root —
  a bare-metal or systemd `keeper init`, not the images we ship, which are `nonroot` —
  it would have succeeded and left the operator with no `/dev/stdout` at all. The
  target is now classified with `os.Lstat`, which stops at the symlink instead of
  resolving through `/proc/self/fd/1` to whatever stdout
  happens to be that minute; anything that is not a regular file is opened and
  written **in place**, with no unlink, no `O_TRUNC`, no `chmod` and no `fsync` —
  none of which are meaningful on a stream and each of which would damage the thing
  on the other end.

  A name, though, is not a destination, and the in-place write has none of the guards
  that made the old path safe by construction. So the open re-checks what it actually
  got: `O_NONBLOCK`, so a fifo nobody is reading fails with `ENXIO` instead of
  blocking forever after the Archon is committed — with `signal.NotifyContext` holding
  SIGINT, that hang was not even interruptible, and it left a cluster that reports
  itself initialized and a token that reached nobody. The `ENXIO` message distinguishes
  the two things that raise it: a fifo says it when nobody is reading, which the
  operator fixes by starting the reader, and a socket says it because `open(2)` cannot
  open a socket at all, which no reader will fix — so the remedy is offered only when
  the name itself was a fifo, and a fifo reached through a symlink or as `/dev/stdout`
  does not get it either (NIM-567). Then an `fstat` of the open fd, admitting a
  **character device or a fifo**
  and refuses everything else.

  An allowlist, because the type that got through the denylist was the destructive
  one. `IsRegular()` is false for a block device; a directory and a socket never
  reach the check at all (`EISDIR`, `ENXIO`); `/dev/sda` opens cleanly and takes the
  write at offset zero, so root `keeper init` plus a typo in `--credential-out` was
  the whole distance to a JWT written over a partition table. That corner is now
  closed — but **only that corner**, and the earlier draft of this entry claimed
  more. Every character device is still admitted, and some are no better than the
  block device: `/dev/kmsg` puts the JWT in the kernel ring buffer and thence the
  journal, `/dev/mem` is accepted too (both measured), and opening `/dev/watchdog`
  arms a reboot before any check can run, since the open necessarily precedes the
  `fstat`. Refusing devices by number would be worse than the disease; the honest
  statement is that the path form trusts its target as well as its directory, and
  `docs/operations/bootstrap-rbac.md` now says that.

  A **regular file** keeps its own refusal — but it is *not* a symlink guard, and
  calling it one (as this entry first did) is wrong twice over. Following symlinks
  is load-bearing, `/dev/stdout` being one; and a fifo planted at the credential
  path takes delivery of the token whether it is reached through a symlink or sits
  there directly, so `O_NOFOLLOW` would not close it either. Both were measured.
  The path form is unusable in a directory somebody else can write to, and that is
  a property of the feature rather than a bug this branch fixes. What the branch
  does protect is the promise the flag makes: `--credential-out=<path>` advertises
  `mode 0400`, which can only be kept on a file `keeper init` created itself, so a
  regular file that is already there — carrying whatever mode it already has — is
  refused instead of having a cluster-admin JWT written into it silently.
  *Silently* is the operative word, and the rule is three-valued rather than two:
  a path gets the `0400` guarantee, or a downgrade it is told about (the stderr
  warning plus `credential_is_stream: true`), or an error. `/dev/stdout` is a path
  and gets no `0400`, and that is fine because a stream announces itself; an
  existing regular file is the one destination whose outcome is indistinguishable
  from the path form working — the token on disk exactly as advertised, wearing
  somebody else's mode. `--credential-out=/dev/stdout > archon.jwt` lands on that
  branch and is refused there. Not for want of an `O_TRUNC`: a tail does survive
  `>>` and `1<>` (measured — the shell truncates for neither, and the reopened fd
  writes from offset zero), so the earlier draft's claim that redirection leaves no
  tail held only for plain `>`; it changes nothing, because `O_TRUNC` would not make
  such a file `0400` either. `--credential-out=-` resolves no path, promises no mode
  and says so, which is what makes it the right form for a redirect.

  `--credential-out=-` is the portable form of the same thing: it hands the token to
  the already-open stdout and never touches the filesystem at all — no path resolved,
  no directory created, no mode enforced. This is the only way to bootstrap a
  distroless image, where a token written to a file inside the container cannot be
  read back out (`kubectl exec … cat` has nothing to run; `kubectl cp` needs `tar`
  in the container). A file genuinely named `-` is still reachable as `./-`.

  `keeper init` now also arms `SIGPIPE`. Go lets the default disposition kill the
  process on `EPIPE` for fd 1 and 2, which is right for a filter and wrong for a
  command that commits database rows before it writes. With the reader gone before
  the write — an interrupted `kubectl exec`, a consumer that died during the seconds
  spent on Vault and migrations — `keeper init --credential-out=-` died inside the
  write with exit 141 and an empty stderr, past the point of no return and with the
  token's only copy gone. Measured against a build without the arming: 141, stderr
  0 B, no recovery; with it, exit 1, the reason on stderr, and the token printed for
  recovery. (Not `| head -1`, which an earlier draft of this entry cited: the token
  is one write that fits the pipe buffer, so it lands before `head` exits and the run
  ends 0 on either build. The example was wrong even though the fix is right.) That
  recovery print is a deliberate trade — in a container it puts the JWT in the same
  log pipeline stdout was going to, which beats a bootstrapped cluster nobody holds
  the credential for; `docs/operations/bootstrap-rbac.md` says so where an operator
  will see it.

  A token that went to a stream carries **no permission guarantee**, so `keeper init`
  says so: a warning on stderr and `credential_is_stream: true` on the structured log
  line. **`Bootstrap complete. Token written to …` moved from stdout to stderr** —
  unconditionally, so that `--credential-out=- > archon.jwt` yields a file holding the
  JWT and nothing else. Nothing in the repo parses that line.

- **The Vault AppRole template we ship put a 24-hour stop under every production
  Keeper.** The role operators are told to create — in `docs/keeper/prod-setup.md`,
  `docs/operations/infra.md` and `docs/operations/deb-onboarding.md` — carried
  `token_ttl=1h token_max_ttl=24h`. Keeper logs in through AppRole exactly once, at
  startup, inside `vault.NewClient`, and nothing in the process ever logs in a second
  time; `TokenRenewer` only calls `renew-self`, and `renew-self` cannot carry a token
  past its maximum lifetime. A Keeper that simply stays up for a day therefore loses
  Vault: `vault:`-ref resolution on hot-reload, SoulSeed issuance during onboarding,
  `core.vault.kv-read` and the Sigil signing-key read on anchor reload all begin to
  fail, and the affected instance has to be restarted. The template now hands out a
  **periodic** token (`token_period=1h`) — one that renewal has nothing to run out of.
  `examples/keeper/vault-policy.hcl` stated the opposite of the truth in the same
  breath ("renews it up to `token_max_ttl`, so Keeper doesn't lose access to Vault over
  a long uptime") and now says where the property actually comes from: the role, not
  the policy.

  **Existing installations have to rewrite the role**; nothing in the binary changed.
  Rewriting it is enough on its own for a Keeper that is still renewing — AppRole
  re-reads the role at every renewal — and only the instances whose token already
  expired need a restart. The template also passes `token_max_ttl=0` and
  `token_explicit_max_ttl=0`, because `vault write` on an existing role updates only
  the fields you name: the first clears the stale ceiling (harmless once the token is
  periodic, but it stops the role contradicting itself), the second clears the one
  field that really does cap a periodic token.

  Nothing in the repo executes that snippet — no provisioning path creates a Keeper
  AppRole role at all, dev and all three e2e harnesses run Vault on a root token — so
  no tier could ever have observed the template rotting. `make check` now carries
  `check-approle-template`, which asserts on the role-parameter line itself in all four
  places we ship it.

- **The `keeper` and `soul` packages could not start the service they install.**
  The package drops its binary at `/usr/bin/<name>`, but the systemd unit shipped
  alongside it declared `ExecStart=/usr/local/bin/<name>` — so on a host installed
  from the deb/rpm, `systemctl start keeper` failed with `status=203/EXEC` and the
  daemon never came up. `/usr/local` is reserved for the local administrator and a
  distribution package must not install there, so the units now point at
  `/usr/bin`, where the binary actually is. The stale path came from a second,
  unused set of packaging configs under `deploy/nfpm/` that drifted away from the
  ones a release is built from; those are gone (see below). Docker images are
  unaffected — they place the binary and their entrypoint at the same path.

- **An inline `gpg_key` on `core.repo` now lands under the name apt can actually
  read.** apt picks its keyring parser from the file extension: an ASCII-armored
  key is read only from `*.asc`, a dearmored one only from `*.gpg`. `core.repo`
  wrote every inline key to `/etc/apt/keyrings/<name>.gpg` regardless of its form,
  and armored is the form nearly every upstream publishes — `packages.redis.io`
  included. The mismatch is not something apt reports as a bad file: it answers
  `NO_PUBKEY` and discards the whole repository as unsigned, while the module's own
  report said `changed=true` and everything downstream failed somewhere else, for
  reasons that pointed nowhere near the key. The extension now follows the key's
  content — armored to `<name>.asc`, otherwise `<name>.gpg` — and `signed-by=`
  follows it, in both `Plan` and `Apply`. On a host that already carries an armored
  key at the old `.gpg` name, the first run after upgrading writes the `.asc` file
  and rewrites the `.list` line, reporting `changed=true` once; the stale `.gpg`
  file is left in place, since `/etc/apt/keyrings/` is not a trust root on its own
  and removing operator files is not this module's business. `gpg_key_path` is
  unaffected — it references the name you chose and never renames anything.

- **Keeper could not connect to a Redis that has ACL users at all.** `redis:` grew
  two optional keys, `username` and `sentinel_username`, and both now reach the
  driver in every topology — standalone, sentinel (where the sentinel identity is
  separate from the data-node one) and cluster.

  The old config could only express a password, so the client sent the
  one-argument `AUTH <password>`. That form means user `default`, and a server
  with ACLs enabled answers it `-WRONGPASS` — measured against a live cluster,
  where `AUTH keeper <password>` returns `+OK` for the same credential. So this
  was not a matter of which identity appeared in the server's log; the connection
  never opened, and the operator saw it as a failing `/readyz` redis check with
  nothing in the config obviously wrong. Sentinel mode failed one step earlier
  still: the sentinel credential is what gates master-discovery, so without it
  the client never learned the master's address to begin with.

  Both keys are plain values rather than vault-refs — a username is not a secret,
  the password stays in `password_ref` — and both default to empty, which is
  exactly the pre-ACL behaviour, so configs against a non-ACL Redis are
  unaffected. Worth noting that Soul Stack's own Redis service creates ACL users,
  which made "manages ACL Redis but cannot connect to one" a gap in its own right
  rather than a property of any single installation.

- **CI's verdict on the test tiers said less than it appeared to, in three
  independent ways.** None of these were red builds — they were the arithmetic of
  what a green one covered, which is worse, because the number everyone reads did
  not change while the claim behind it shrank.

  **The race detector ran over the wrong package set.** `-tags=integration` widens
  a package set rather than narrowing it, so `make test-integration` was handing
  the whole untagged unit corpus (~155 packages) to a command carrying `-race`,
  inside the one job that also starts ~40 container sets. Unit tests written
  against uninstrumented timing then failed there on instrumentation speed, and
  three consecutive runs failed on a **different** test each time — the signature
  of chance, where a real regression would have kept failing on the same one. The
  three participants are fixed to measure behaviour instead of the machine: the
  Redis cluster fixture waits for `cluster_state:ok` from every master rather than
  for the first successful dial, `TestRun_CancelDuringTask` waits for the module's
  `Apply` to announce it is running rather than polling `Cancel` every 5 ms from
  the moment `Run` registers the apply-id (which happens well before task 0 is
  dispatched — a poll landing in that window cancelled a run whose task had not
  started, losing ~1 % of runs under `-race` while staying green in `make test` on
  the same commit), and `TestConsole_ThrottledSessionStillTearsDownFast` no longer
  asserts on elapsed wall-clock. Those are synchronisation fixes, not longer
  waits: a raised timeout would only make the wrong outcome rarer while still
  asserting nothing.

  Fixing the three does not fix the class, so the set was corrected too. L1 now
  runs the packages that actually carry `integration`-tagged tests (43 today),
  derived from the tree on every invocation by `scripts/integration-packages.sh`
  rather than kept as a list that would rot. The excluded packages lost no
  coverage — they run in `make test`, now under the detector in `make test-race`,
  and `make vet-tags` still compiles the whole tree under the tag.

  **Nothing ran the detector where the concurrency actually is.** Every concurrent
  subsystem lives in untagged packages — the async runner and its barriers
  ([ADR-0075](docs/adr/0075-intra-host-async-tasks.md)), the console pty pumps and
  write budget, the applybus fan-out — and `make test` runs them uninstrumented. A
  `test-race` target and a nightly job for it both existed and between them
  produced no signal: the workflow had no schedule, and the job was
  `continue-on-error`. `make test-race` is now a blocking CI job of its own (~3
  min) and part of `make check-all`; `make check` states that it did not run the
  detector, the same way it already states which tiers it skipped. The nightly
  copy is gone rather than left as an advisory second opinion.

  **`make test-race` could not fail.** It lacked `-count=1`. That flag is
  load-bearing everywhere in this Makefile, but for the detector it guards
  something sharper: a race is found by observing an interleaving, so a green
  sweep means "no race was observed this run" — a per-run claim. Cached, `go test`
  replays one historical observation for ever; two consecutive sweeps finished in
  8 s reporting `(cached) ok` for all 140 packages.

  **An infrastructure failure was indistinguishable from a caught regression.**
  `CLUSTERDOWN`, `connection refused` against a mapped port and "wait until ready:
  context deadline exceeded" all arrive in the same `--- FAIL` shape as an
  assertion, and the two have opposite answers: rerun that package, versus fix the
  code. Told apart by eye, every red L1 run cost a person an hour of reading
  container logs — and the cheap way out of that hour, "L1 is flaky, rerun it", is
  precisely how a real regression gets waved through. On failure the target now
  labels each failing package **REGRESSION** / **INFRA** / **UNCLEAR** and prints
  the `PKG=` line to rerun a suspect one alone. `UNCLEAR` exists rather than being
  folded into `INFRA` because a false `INFRA` label is the failure mode that
  matters — it is the one that makes a finding disappear — so a signature either
  layer can print stays a finding until a solitary rerun says otherwise. Nothing is
  downgraded: L1 still fails and the target still exits non-zero. No readiness wait
  was loosened to make any of this quieter; that would trade a loud infra failure
  for a silent one.

  Narrowing what L1 runs introduced a failure mode the old `./...` could not have,
  so it is guarded rather than trusted: if the derivation ever returns nothing,
  `make test-integration` would print "skip" for every module and exit 0 — a total
  loss of L1 that looks exactly like a tree with no integration tests.
  `make check` now runs `check-integration-set`, which cross-checks the set
  against a second derivation that shares no machinery with the first (grep for
  build-constraint lines, not `go list`) and fails on a shortfall.

- **The blocking pre-tag gate went red without meaning anything was broken.**
  `make e2e-live` is step (e) of `RELEASING.md` — no tag is cut until it is green
  — and three consecutive runs on one unchanged slice produced three different
  sets of failures, none of which reproduced. Each of those tests died in 3-15
  seconds where the same test passing takes 45-105, i.e. it never reached the
  thing it asserts; the text was always a refused connection to a container the
  suite had just started, on the line after testcontainers reported that the
  container was ready. This is the mirror of the tier problems above and costs
  the same: there, green said less than it appeared to; here, **red did not mean
  broken**, and the only way to tell was to read timings by eye.

  The waits were the cause and are now **stricter**, not longer-and-looser.
  Readiness had been signalled from inside each container while the harness then
  dials the mapped port from the host, and under load that gap is real: postgres
  waited only on log lines and never checked its port at all, vault waited on
  `Root Token:` — printed before dev-mode Vault finishes unsealing, while the
  harness's next act needs an unsealed API — and redis had a correct port check
  on an inherited 10-second budget nobody here had chosen. Each stand now waits
  for the property the harness is about to use (vault for `/v1/sys/health`
  returning 200), on one named budget, with the bound covering all three stands
  *computed* from it rather than written beside it.

  Distinguishing the two outcomes is now the harness's own statement rather than
  a reader's guess. Bring-up entry points declare their failure, and the gate
  labels every test **STAND-SETUP** / **TEST-FAILURE** / **NOT-RUN** from that
  declaration — deliberately with no list of error signatures to match, because
  guessing infrastructure from library text is what produces a false infra label
  on a real regression. Nothing is downgraded: the gate still exits non-zero, no
  test is retried, and a setup failure that survives a solitary rerun on an idle
  machine is a finding about the machine.

  Dropping the signature lists removes one way to get the dangerous answer, not
  the possibility of it, and the mechanism's own two moving parts are now each
  held by a docker-free check in `make check`. **Where the declared region ends**
  decides what gets called infrastructure: an early cut of this work set the flag
  on the last line of `NewStack`, which swallowed `keeper init`, `keeper run`,
  service registration and the entire Soul onboarding path — the one path no
  other suite exercises against a real soul binary — so a regression in it would
  have printed STAND-SETUP on all nine gate tests, which reads as a bad day for
  docker. A guard now parses the harness and fails if a call that runs this
  repo's own binaries falls inside a declared region. **Which test a log line
  belongs to** decides who the declaration speaks for: the classifier followed
  only `=== RUN`, so interleaved output moved the marker to a neighbour and
  inverted both labels at once; it now also follows `=== CONT` and `=== NAME`
  and attributes each result by the name on its own line. Two further guards pin
  the budgets to their application sites rather than to their definitions — the
  stands' shared ctx must be the derived constant, and `WithWaitStrategy` is
  refused outright because it silently re-wraps a strategy in the library's
  hard-coded 60 s. Each of the four was proven by reverting the code to the
  defect it describes and watching exactly that check go red.

  A third moving part surfaced the moment the gate was next run against a real
  stand, and it went straight through the guard just described. That guard knows
  a **list** of this repo's entry points; the call that mislabelled nine tests
  named none of them. `NIM-377` deleted the plugin's `manifest.yaml`, the harness
  read it from the repo tree with a bare `os.ReadFile` inside a declared region,
  and a **deleted file in this repository** printed STAND-SETUP under the words
  "nothing above is a finding about the code" on every gate test — the one
  direction this mechanism must never be wrong in. The region is now also checked
  by a *property* rather than a name: a declared region that reaches `repoRoot`,
  directly or through a helper, fails the guard, because whatever it reads is a
  claim about this repository and the label says machine. Both arms were proven
  by putting the defect back. The fixture repo the harness publishes was
  rebuilt for the post-`NIM-377` artifact besides — no `manifest.yaml` anywhere,
  the schema document stamped into the artifact as a trailer through the public
  `sdk/schema` the plugin author uses, and published beside it as `schema.json` —
  and the document is read *outside* the declaration, next to `go build`, for the
  same reason the build already was.

  None of that would have caught `NIM-377` either: the harness's picture of what
  an artifact **is** had no check that did not cost twenty minutes and a docker
  daemon. It has one now — a second-long test, in the gate's docker-free
  pre-step, holding the five things the fixture builder assumes (the document is
  published where it looks, `manifest.yaml` is still gone, the bytes are
  canonical, the trailer round-trips, and the module the artifact declares is the
  one half of `community.redis` the operator's alias does not supply). It reads
  the model through `sdk/schema` rather than restating it, because a restatement
  is a second definition that keeps agreeing with the model the product has left —
  which is the bug, not a check for it. All five arms were proven by putting each
  defect back.

  What the relabelled gate then reported was not a fixture problem at all but a
  product one — `NIM-524`, the entry above titled "Every service declaring
  `modules:` was unappliable on every host". That is the whole point of the
  label: the first honest run pointed at the code, and the code was where the
  defect was.

  One last way the verdict could be about the wrong run: the transcript the
  recipe greps had a **fixed** path, `$TMPDIR/soul-e2e-live-gate.log`. That is
  one file shared by every worktree on the machine, and this repo is worked in
  several at once — so two gates running together read a file the other is
  writing, `tee` having truncated it at start. A missing `--- PASS` of your own
  can then be supplied by a neighbour's line, and on red the classifier explains
  somebody else's failure. The transcript is now a fresh file per invocation and
  the recipe prints where it put it, which it never did before.

- **The same gate's list of tests held prefixes, not names.** Three of the nine
  entries in `E2E_GATE_TESTS` were the leading part of a test's name rather than
  the name — `TestL3bPluginChannel` for `TestL3bPluginChannel_CatalogAndAllow`.
  `go test -run` takes an unanchored regexp, so the gate did select and run the
  right nine and nobody had reason to look. The two readers that take that list
  as a *name* were the ones being lied to. The per-test `--- PASS` guard — the
  one that exists so a skipped test cannot be reported as covered — matched by
  prefix, so a neighbour named `TestL3bPluginChannel_Other` would have signed off
  for a test that never ran. And the new classifier matches names exactly, so
  every red run printed three `NOT-RUN` lines for tests that had just passed:
  a tool whose only job is making a red gate legible was adding three false
  statements to each one. The entries are now exact names, the mask is anchored,
  the `--- PASS` grep is anchored, and `make check` fails if any entry is not a
  test the suite really has — the list is used in three places, so nothing about
  it is allowed to be checked only by the 20-minute job it configures.

- **The live acceptance ran, and it did not produce three matching runs.**
  Reading a red gate is the repair above; *trusting* the gate is what the repair
  serves, and only a live run says whether it arrived. Four runs on a pinned
  slice, three of them under synthetic load inside the 12-25 band the ticket
  names: 9/9 green in 503s idle, then 8/9, 8/9, 9/9 at a loadavg of 20-42.
  Three consecutive runs, three different answers, on a subject that did not
  change by a byte.

  Neither red was a defect and neither was a container. One died in `core.url`
  resolving github.com, the other in `core.pkg` fetching a `.deb` from
  deb.debian.org. Both were labelled TEST-FAILURE, and correctly so — the
  harness makes no bring-up claim at either point, and the classifier carries no
  signature lists to guess with. What the runs establish is that this gate's
  verdict is decided partly OUTSIDE its slice, which is a larger trust defect
  than the one repaired here and is filed as its own: `NIM-542`.

  The half that is in this repo's hands was demonstrated rather than argued, in
  both directions. A wrong image tag inside the declared bring-up region
  (`postgres:16.99-alpine`) gave STAND-SETUP in 2.7s, the harness's own
  declaration sitting in the transcript. A real product regression — the
  resolver naming a slot's artifact by its filename again instead of by the
  registration alias, the convention NIM-377 removed — gave TEST-FAILURE in
  12.2s. The second is the load-bearing one: it fails while the stand is still
  being built, past `infraUp = true`, and still reads as a finding. That is
  precisely why the flag is not set on the last line.

- **L3a passed test-by-test and failed as a suite, and nothing in a red run said
  which of those it was.** `make e2e` is the tier that drives a real Keeper
  against real containers. Every test in it passed when run alone; a full run
  dropped two or three, a different two or three each time, across families that
  read as unrelated — a `401 invalid token` on an operator call, an
  incarnation-state assert reading an untouched database, a container that never
  came up, a panic naming a test that had done nothing wrong. Taken one at a time
  those are four investigations. They were symptoms of two things, and neither of
  them was flakiness.

  **The stands were waited on by signals that hold on an idle machine and stop
  holding under contention.** Postgres had no port check at all — the
  testcontainers module sets no `WaitingFor` of its own and the host-port check
  lives in an opt-in customizer this harness never called — so the harness
  declared the stand up and `keeper init` then died on `dial tcp 127.0.0.1:32840:
  connect: connection refused`. Vault waited on `Root Token:`, which dev-mode
  Vault prints before it finishes unsealing, while the harness's next act is a KV
  write. Redis was passed no strategy at all and so inherited the module's
  10-second default, the shortest of the three budgets and nobody's decision.
  Each container is now waited on through its **mapped host port** — the hop that
  actually failed — on one named budget, with Vault additionally held until
  `/v1/sys/health` returns 200. The waits get stricter, never looser: a readiness
  wait relaxed to quieten a suite trades a loud infrastructure failure for a
  silent one. The bound covering all three stands in sequence is *computed* from
  that budget rather than written beside it, because a literal there caps all
  three and the last stand in line inherits the remainder and fails as
  `context deadline exceeded` from the parent without naming itself.

  **Three ways one test could reach into another, all closed.** The keeper's log
  writer had no detach: cleanup Kills the process after 15 s and returns while
  `cmd.Wait()` is still draining its pipes into a `*testing.T` whose test has
  finished, and Go answers a log-after-test with a panic in the name of whichever
  test is running by then — the bystander is blamed and the culprit leaves no
  trace. Port reservation closed its listener immediately and returned only an
  address, leaving that address a suggestion across a window that spans a whole
  `keeper init`, inside the same ephemeral range every outgoing connection on the
  host draws from; losing that race either stops the keeper binding (a `/readyz`
  deadline that never mentions a port) or hands the test a **foreign process**
  that an unauthenticated 2xx on `/readyz` is happy to accept. Stacks now hold
  their ports until the last instant and, after `/readyz`, verify that the
  process answering is the keeper they started, using a token their own keeper
  minted seconds earlier. Container teardown reports its errors instead of
  discarding them, which is why "do containers survive a run?" had no answer in
  any log.

  **A red run now labels itself.** `scripts/classify-l3a-failure.py` gives L3a
  what L1 has had since NIM-238 and e2e-live since NIM-406: every test labelled
  **STAND-SETUP** / **TEST-FAILURE** / **TIMEOUT** / **NOT-RUN** from the
  harness's own declaration rather than from matched error text, plus a family
  axis, since L3a is effectively one package and L1's per-package axis carries no
  information here. Nothing is downgraded to a pass and nothing is retried. `make
  e2e` also gains `-count=1` — a tier judged by repeated passes cannot be served
  from the test cache — and a timeout above its ~666 s runtime rather than below
  it; at 10m the suite was being killed mid-flight, which arrives as one panic
  plus a silently truncated tier.

  **What this does not claim.** Three consecutive full runs from the fixed tree,
  on a deliberately busy machine, gave **red / green / red** — so the suite is
  not yet reliably green. Both reds were correctly declared STAND-SETUP, and both
  are outside what harness code can reach: one was the ryuk reaper hitting
  testcontainers' hard-coded 60 s startup timeout, which the library exposes no
  option for, and one was the Docker daemon failing to answer a container inspect
  for the full 120 s budget. What the fix demonstrably removed is the class that
  produced Postgres's refused connection at 4 s and Redis's death at 14.70 s
  against a 10 s budget it never chose. Of the ticket's families, the
  incarnation-state and staged-failover failures did not reproduce in six full
  runs; the assert that reports the first already prints both the actual state
  and the expected subset, and has since the beta. Follow-ups: NIM-532 (ryuk's
  unreachable timeout), NIM-533 (the daemon stalls and the residual red).

- **L3a's red was unreadable and its green was unearned** (NIM-533 / NIM-547 /
  NIM-548 / NIM-549 / NIM-550 / NIM-532). The tier above labels its failures
  since NIM-469; this closes the two ways it could still hand a reader a result
  that means nothing.

  **A wedged docker daemon produced no verdict at all.** With the socket
  accepting connections and no reply ever coming, the run sat in its first
  docker call for the whole test timeout and died as `panic: test timed out`,
  naming no layer — NIM-533's headline symptom, reproduced by the harness meant
  to report it. The cause is upstream and worth stating: testcontainers resolves
  the docker host inside a `sync.Once` that `NewDockerProvider` enters with
  `context.Background()`, so the first docker call in a process is unbounded and
  every later one waits on that `Once`. No caller's deadline could have bounded
  it. The probe now races that call against a timer on its own goroutine — the
  only thing in the process able to end it — and `bringUpStand` asks **before**
  it raises anything, because a post-mortem probe cannot explain a call that
  never returned. It refuses on silence only; a slow daemon still gets its
  stand, which is what the retry is for. The latch that spares the remaining
  tests the same wait is set only by the unbounded lookup, since the `Ping`
  after it does honour a deadline — and it retracts when a late lookup finishes,
  because what it asserted has stopped being true.

  **The probe was built on a call the library memoises**, and that is the defect
  none of the process caught. `DockerProvider.Health` is `client.Info`, cached
  in package variables after its first success, so on a healthy box the first
  stand warmed the cache and every later probe reported a microsecond ping and
  no error regardless of what the daemon was doing: the contention check could
  never fire and a saturated daemon was reported as the *container* — naming the
  wrong layer confidently, which is worse than the raw library error it
  replaced. It is invisible against a daemon wedged from the start, because the
  cache never gets its one success, and that is the only state this tier has
  been runnable in here — which is why three identical runs and a full mutation
  battery agreed it was fine. The probe uses `Ping`, a pass-through that hits
  `/_ping` every call, and the ban on the cached one is a guard rather than a
  comment.

  **A failure below the daemon now names the layer that owns it.**
  testcontainers raises its ryuk reaper before our container and waits on a
  strategy it builds itself, with a hard-coded 60 s and no way in from a
  `GenericContainer` option; the failure still surfaced through our
  constructor, so it was described as ours, and by the time the probe ran a
  minute later the daemon was idle again and the verdict read "the image or its
  configuration did not become ready" — a layer that was never involved. The
  60 s stays upstream's: moving it means leaking containers on every killed run
  or bumping the dependency, and neither is this batch's call. What changed is
  that the red says which thing failed. A daemon that *answered* with an error
  no longer borrows the prose written for one that said nothing, which had
  promised a 15 s wait that never happened and offered a WSL remedy for what is
  usually a `DOCKER_HOST` typo.

  **Stands stopped leaking.** testcontainers returns a live container alongside
  its error by design; the harness dropped that handle, so every failed bring-up
  left a container behind for the rest of the run. It is adopted before the
  error is read. `t.Cleanup(s.Cleanup)` is now registered inside both
  constructors, because a caller's `defer` cannot cover a constructor that
  fatals before it returns.

  **A tier that ran nothing must not report a pass.** The keeper-binary
  pre-flight *skipped*, and `go test` without `-v` prints `ok <pkg> 0.1s` for a
  package whose tests all skipped — byte-for-byte what it prints when they all
  passed. A run that located no binary exited 0, satisfied `make e2e` and every
  gate above it, and never reached `scripts/classify-l3a-failure.py`, which the
  Makefile invokes only on a non-zero status: that tool's `NOT-RUN` verdict was
  unreachable from its only caller. Both entry points now fail — the answer
  missing docker already got — with the bring-up declaration registered *above*
  the pre-flight, so the refusal carries the STAND-SETUP marker instead of
  arriving as a bare `--- FAIL: TestX` on all forty tests at once.

  **The guards derive their subject instead of listing it.** The hand-written
  map of product entry points had the exact failure it was written against: one
  name in it was called only from `_test.go` files, which the source walk
  excludes, so that entry gated zero while reading as coverage. The set now
  comes from three doors — the built binary, the operator API, the schema
  `keeper init` migrated — so a helper written later is covered on the day it is
  written, with the old list demoted to a floor that catches a door narrowing.
  The declared bring-up region likewise no longer ends at the *earliest*
  `infraUp = true`: a second assignment in a branch would silently lift the
  boundary above `runKeeperInit` while the guard reported success, and which
  assignment closes the region depends on the branch taken, so more than one is
  now the finding. Three defects in the classifier's own self-test were found
  the same way, by mutation: an unpinned check order, a fixture named for an
  invariant it did not exercise, and a marker literal nothing compared against
  the Go source.

  **What this does not claim.** The acceptance bar for this batch was three L3a
  runs on an unchanged slice agreeing with each other, and **that has not been
  met — there is no green live run behind these changes.** The docker daemon on
  the development machine has been wedged for the duration (the failure mode the
  first item describes), which the work makes legible but cannot repair: it is
  the host's, not the harness's. Every claim here was instead demonstrated by
  known-bad mutation — 54 defects introduced in the shape of real code, each
  caught by the guard whose statement it breaks, plus one control that stays
  green — and the residual red NIM-533 reported has not been observed since,
  because the tier has not been observed at all. CI runs `make build` before
  `make e2e`, so the new pre-flight is satisfied there by construction.

- **A CI run could be attributed to the wrong commit.** `cancel-in-progress: true`
  is written for a feature branch, where only the newest commit matters. A release
  branch is the opposite case: it *is* the integration target, every squash-merge
  into it is a unit of acceptance, and merges land minutes apart — so each push
  evicted the previous run and the branch could show a green result while the merge
  in question never completed one. Eviction reports `cancelled`, which is neither
  pass nor fail, sits next to `failure` in a run listing, and is remembered as
  neither. Cancellation is now limited to branches that are not `main` or
  `release/*`; on those, runs queue instead. Queueing alone would not be enough — a
  reader can still pick the wrong row — so `make check-ci` answers "has CI verified
  THIS commit?" about a sha it derives from git, and prints `cancelled`, `skipped`
  and "no run exists" as their own outcomes instead of folding them into a verdict.
- **The spec said a group construct's requisite skips the whole group when its
  condition is not met. That is only true for descendants with no requisite of
  their own** ([ADR-009](docs/adr/0009-scenario-dsl.md) amendment 2026-07-30,
  [destiny/tasks.md §6.5](docs/destiny/tasks.md), [scenario/orchestration.md
  §2.1.2](docs/scenario/orchestration.md)). `block:` and `apply:` pass their own
  `onchanges:`/`onfail:`/`require:` into every task of the group, merged with the
  task's own as a **union of names** — and a union of `onchanges:` composes as
  **OR**. So a group-level requisite **widens** the gating of a descendant that
  already had one, rather than narrowing it:

  ```yaml
  # the destiny task, gated on its own source
  - name: Restart DragonFly because the binary or unit changed
    module: core.service.restarted
    onchanges: [dragonfly_bin, dragonfly_unit]

  # the applier over that destiny
  - name: Apply the dragonfly destiny
    onchanges: [df_config]      # reads as "only when the config changed"
    apply: { destiny: dragonfly, input: { … } }
  ```

  The restart ends up gated on `[df_config, dragonfly_bin, dragonfly_unit]` and
  fires on a binary change even when `df_config` never moved. Behaviour is
  unchanged — this is what both constructs have always done (`block:` since the
  C1 pilot, `apply:` since the applier stopped dropping its own keys); what was
  wrong was the documentation, which stated the opposite in one place and said
  nothing about the composition in the others. Note that `when:` on the same
  construct merges by AND and narrows, so the two axes deliberately compose in
  opposite directions.

  **There is no way to spell "outer AND inner" today**, and no workaround
  reproduces it — `when:` on an applier must be static, `where:` selects hosts
  rather than reacting to an outcome. Keep the requisite off the group when a
  descendant's own must stay authoritative. Introducing AND needs a grouped
  requisite on the wire plus a rule for a bracket whose sources are all filtered
  out on a host; it is deferred to NIM-351 and will be a breaking change to the
  behaviour fixed here. Guard tests now pin the semantics on both sides
  (`keeper/internal/render`, `soul/internal/runtime`) so it cannot drift in
  silence.

- **Only an unrestricted role could create an incarnation whose name comes from a
  `name_template`.** The create gate scopes from the request body before the handler
  runs, keyed on `incarnation=<name>` — the one dimension a template does not have
  yet, because the name is composed server-side from the resolved input. No name
  meant an empty context set, and an empty context matches only a permission with no
  effective scope. Passing `name` explicitly is not a way out either: with a template
  it is refused outright. So a scoped operator was locked out of creation entirely,
  on REST and MCP alike.

  Nothing was relaxed to fix it. The gate was discarding two dimensions the request
  already carries — `service`, required by the schema, and the declared `covens`,
  both dimensions of the scope grammar — and both are ceiling-checked by construction,
  since the declared values go into the context and the role's predicate is evaluated
  against them. Claiming more cannot widen anyone's reach; it makes the check fail. An
  absent dimension is omitted rather than sent empty, so a role scoped on it still
  denies. A second gate re-asks once the name is composed, as an AND over every
  declared coven, which also measures the name the caller effectively chose through
  `input` — it becomes a label in the coven plane of member hosts — against their
  ceiling. Both gates read one predicate over contexts from one builder.

- **A role's `default_scope` did not reach the authorization gate.**
  `ResolvePurview` inherited it onto the role's bare permissions;
  `Enforcer.Check` asked each permission alone, and a bare permission matches
  without looking at the context. So the two authorization paths disagreed about
  the same role and **the gate was the wider of the two**: `incarnation.run`
  under `default_scope: coven=dba` was confined on every read and unbounded on
  the write path. The inheritance rule now exists once, as
  `effectiveScope(permission, roleScope)`, read by both, with a guard pinning
  `Check(...) == nil ⇔ ResolvePurview(...).Match(ctx)` over every branch.

- **The role catalog was the cluster's privilege map, readable by anyone holding
  `role.list`.** A role carries its permission set, its scope and the AIDs
  holding it, so a full catalog says who administers what, which covens and
  services exist, and which operator to attack to reach `*` — and a coven-scoped
  operator could read all of it. It is now filtered by the same containment
  predicate the write side uses, judged on each role's effective form, so there
  is one definition of "⊆" and no second implementation free to disagree with the
  decision layer. A caller-less read is refused rather than falling back to
  everything.

- **A role `PATCH` was judged by the rows it stored, not the rights it grants.**
  Dropping `parent_role` while leaving the permission rows identical produced an
  empty string diff, which short-circuited both the least-privilege floor and the
  root-role gate — even though the role's effective rights had just escaped their
  parent's ceiling. An operator holding neither the permission nor
  `role.create-root` could take every holder of a derived role out of
  `coven=prod` and into the whole cluster. Both sides are now resolved into
  effective form and compared by coverage, so any ceiling move is caught —
  clearing the parent, replacing the scope, re-pinning the delta — while a pure
  trim stays ungated.

- **The self-lockout guard had ended up behind the gates that read the caller.**
  Giving `role.update` / `role.delete` a floor, and the revoke paths a caller,
  introduced the first checks on those mutations that measure a role's **whole
  current** right set — which, unlike the least-privilege floor, can never be
  empty and so always demands a caller. On five mutations they ran before the
  "≥1 active Archon with an effective `*` must remain" probe, and a caller-less
  revoke reached a least-privilege refusal naming a missing caller instead of the
  guard. Nothing was exploitable — every handler and MCP tool passes its claims,
  and unbinding a `*`-granting role demands `*`, which implies the guard's answer
  — and that implication was the defect: the guarantee held only because another
  gate happened to be strict enough. The guard is a precondition on the resulting
  cluster state, read under `FOR UPDATE` and unwaivable by any caller, so it now
  runs first ([ADR-078 §n](docs/adr/0078-rbac-derived-roles.md)) and is answerable
  with no subject in the picture — which is what `keeper init` needs, and what its
  guard test caught.

- **Every `create` carrying a topology `assert:` answered 422.** Pre-flight runs
  before `incarnation.Create`, and once membership moved onto a relation whose FK
  requires that row, the roster at pre-flight was not empty by circumstance but
  **impossible** — so `size(soulprint.hosts) == N` was false for every request,
  with no input an operator could send to get past it. An assert is now evaluated
  only if it can read what it reads: one touching `soulprint` is deferred to the
  render fail-safe when the incarnation row is absent, while an assert over
  input, essence or incarnation keeps its 422-before-mutation. The condition is
  "no row", not "this is the create path".

- **Vigils and Decrees scoped `coven: [<incarnation>]` had silently matched
  nothing** since host↔incarnation membership moved out of `souls.coven[]`. The
  roster, the bulk soul selector, the Choir check and form-prep were converted at
  the time; the Oracle was not. A subject now names what it means: an
  `incarnation: {service, name}` dimension resolving through
  `incarnation_membership`, or a `coven` / `trait` label reaching a tagged host
  and the members of a tagged incarnation. A Decree's membership gate is still the
  membership relation and never a label, or a host merely carrying a tag spelled
  like an incarnation would escalate into it.

- **The effective telemetry config had been reaching no host at all.** Delivery
  asked which incarnation a host belonged to with `FROM incarnation WHERE name =
  ANY(<the host's covens>)` — the derived fact NIM-124 retired — so after that
  migration the predicate matched nothing, every host took the legal "no
  incarnation, stay soul-local" branch, and the branch is not an error: it
  returns no config and logs nothing. What hid it for this long is an asymmetry.
  The **reading** half of telemetry had been converted at the time and kept
  showing operators a healthy per-incarnation aggregate over member hosts, none
  of which had ever been handed a config. Delivery now reads the membership
  relation — membership decides which config a host is owed, and a tag spelled
  like an incarnation's name grants nothing.

- **An Augur `coven`-Rite naming an incarnation stopped authorizing its own
  members**, for the same root cause as the Vigil case, but this one denied
  instead of going quiet: a subject that matches no Rite is default-denied, so
  affected hosts failed the Augur step during a run. A Rite subject now carries an
  explicit `incarnation: {service, name}` dimension, and a `coven` / `trait`
  subject reaches the members of an incarnation carrying that label. Nothing is
  inferred from a name: an incarnation is reached by the membership dimension, not
  by a tag spelled like it.

- **A slow operator's console socket stopped writing and went quiet.** Two
  defects stacked. The write budget was a second, stricter liveness rule than
  ping/pong — but backpressure fills the socket buffer *by construction*, so a
  write parks for as long as the operator takes to drain, and a browser that
  stopped reading for ten seconds (a background tab, a GC pause) lost every pty
  on its socket while the read side went on holding that same peer to be alive.
  There is one peer, so there is now one budget. And when the writer did give up,
  nothing tore the socket down: `shutdown()` only closed a channel that a read
  pump parked in `ReadMessage` cannot see, so the connection stayed open with
  nobody writing to it — the wall froze mid-stream, no loss report could be
  delivered, and the root shells behind it kept running until the read deadline
  fired a minute later. Teardown now puts the read deadline in the past, ordered
  against the pong handler re-arming it. Two consequences of the wider budget are
  handled rather than left to grow into twins of the bug: teardown no longer
  waits out a parked writer, and the cluster claim refresh moves off the writer
  goroutine — a claim is Redis state, and serialized behind a socket write a slow
  browser would have starved it past its TTL and lost its shells to the orphan
  sweeper.

- **Console output is compressed on the wire.** It is the only high-volume
  traffic Keeper serves to a browser, enormously redundant, and carried as base64
  inside JSON; `permessage-deflate` is negotiated by the browser unprompted, so
  no frame, client or subprotocol changes. Measured on a real terminal listing,
  32 KiB of output leaves as 7.1 KiB rather than 43.7 KiB — which also works
  against the drops above, since the writer clears its queue about six times
  sooner. Recorded consideration: compressing a TLS-carried stream is the
  CRIME/BREACH class and secrets do cross this socket; compression without
  context takeover confines any length inference to a single frame, and there is
  no attacker-controlled request reflected into the response as there is in the
  HTTP case.

- **A flooded console socket threw away the newest output instead of the oldest.**
  The queue in front of the writer refused arrivals once it was full, so an
  operator watching a chatty build sat pinned to a screen minutes old while the
  output they were waiting for was discarded on arrival — a session that was
  complete and useless. It now evicts the oldest *chunk* and keeps the arrival.
  Two things fell out of that. The queue had to stop being a channel: a channel
  cannot be inspected, and blind head-eviction would have discarded `opened` (a
  panel stuck on "connecting" forever) or `exit` (a terminal that never ends), so
  control frames are stepped over and everything else keeps arrival order. And a
  control frame arriving into a full queue no longer costs the whole socket: it
  used to close the connection and reap every session behind it, so a flood in
  one panel killed an operator's entire wall. The socket now closes only when the
  queue holds nothing but control frames. An evicted frame carries its own
  `dropped_bytes` back into the accounting, so "every loss is counted" does not
  quietly leak. Left undone deliberately: the drop marker keeps its place in the
  queue rather than being given a privileged path past it — the marker sits
  exactly at the gap, and hurrying it would announce the gap ahead of the output
  that precedes it.

- **A console socket died without saying why, or to whom.** The write-failure
  path logged at debug and closed; the operator saw a bare 1006, and the Keeper
  log did not separate "the peer fell behind and lost its shells" from "someone
  closed a tab". The level now follows what happened first. A write that fails
  while a teardown is already running is Keeper reclaiming its own socket and
  stays at debug — the teardown close is what knocked that write over, so warning
  there would have fired on every ordinary tab close and buried the real case. A
  write that fails first is warned with the Archon's identity, because it costs
  that operator every pty on the socket. The reap line carries the reason and the
  number of ptys it killed, neither of which the writer can know. The reason
  became a type rather than a free string: the constant is the Prometheus label
  (a closed set, [ADR-024](docs/adr/0024-observability.md) §2.2) and its text is
  what Soul, the recording and the audit trail are told. Found on the way:
  `Hub.Close` never counted a terminal at all, so `keeper_console_sessions_total`
  moved only for Soul-side exits and orphan reaps — kill-on-disconnect, the idle
  sweep and "recording unavailable" decremented the gauge without ever
  incrementing the counter, and the totals did not reconcile with the gauge that
  the metric's own contract says they should.

- **Cloud provisioning converges instead of colliding.** A second `create` over
  an incarnation no longer dies on the `souls` rows its own first attempt left
  behind, a re-run reconciles hosts that are already up rather than treating live
  ones as pending, and a destroy is confirmed actually gone before it is reported
  destroyed — the delete call is asynchronous, and reporting on its acceptance
  meant reporting a VM that was still running. The wait-until-ready budget is
  sized for a real VM boot, and its diagnostics name the attempt, the budget and
  the elapsed time instead of a bare timeout.

- **A forced destroy no longer reports success as though the resources had been
  released.** `force` skips the teardown scenario by design — it exists for an
  incarnation whose hosts are already unreachable — but it then archived the row
  under the transient status `destroying` and deleted it, and the record of which
  provider and which VM ids that incarnation had been holding went with it. The
  operator was told the incarnation was destroyed; the VMs were still running and
  still billed, and there was no longer anywhere to look up what they were.

  Three things change, none of which needs a migration. The archive now carries a
  terminal status of its own — `destroyed` for a completed teardown,
  `force_destroyed` for a skipped one — so an archived row states which of the two
  it was instead of freezing whatever status the row happened to hold mid-flight.
  A force collects what it is abandoning **before** the delete and in the same
  transaction, and writes it into `incarnation_archive.status_details`: the cloud
  provider, the provisioned VM ids, and the member SIDs — the last of which is
  otherwise lost the instant `incarnation_membership` cascades. The same record
  reaches the caller as `unreleased` on
  `DELETE /v1/incarnations/{name}?allow_destroy=true` and on the MCP tool
  `keeper.incarnation.destroy`, alongside a WARN in Keeper's log and an
  `unreleased` key on the `incarnation.destroy_completed` audit event. A force that
  abandoned nothing omits the key in all of those places rather than writing an
  empty object — `force_destroyed` already records that teardown was skipped, and
  `{}` would only raise the question of whether it means "checked, clean" or
  "could not tell".

  What this deliberately does not do is release anything. `force` still means "I
  know these hosts are gone, delete the record"; what changes is that it now says
  so in the operator's words and hands over the identifiers needed to clean up by
  hand. An explicit resource-level confirmation gate ahead of a destructive force
  is a separate decision, filed as NIM-519.

- **The embedded web UI bundle matched the companion build again.** Keeper serves
  `/ui` from a committed copy of the companion's build, and the companion had
  moved on without a paired re-sync, so a built Keeper served a bundle in which
  the Russian translation keys were not merely different but absent. Why the
  drift survived to the release is now fixed rather than tracked: the companion is
  never checked out in CI, so `check-webui` used to take a silent rc=0 skip branch
  there and in every ticket worktree — exactly where it was supposed to catch this.
  It now has three outcomes instead of two (verified / declared-skip / missing
  companion), and the vendored bundle records the companion commit it was built
  from, so an unpaired web merge is a line a reviewer can read instead of minified
  noise.

- `apt`/`dpkg` installs wait for the lock instead of failing on it
  (`DPkg::Lock::Timeout`), so a package task no longer loses a race with an
  unattended-upgrade run.

- **A check that did not run no longer reads like a check that passed.** The
  local gate and CI both ended in the word "passed" while asserting different
  things, and neither implied the other: `make check` is docker-free by design and
  runs neither the integration tier nor the e2e tier, which CI runs — with the race
  detector, which a hand-typed `go test` silently drops. So "everything is green"
  meant something narrower than any reader assumed, and that is how an integration
  suite skipped for want of an unset environment variable reported success, and how
  four e2e tests failed from the day the service registry became mandatory until
  the first CI run over this release noticed.

  Each of those is now inverted rather than documented harder: the integration
  requirement is the default and skipping it is what has to be said out loud, a
  sweep that lost the race detector fails instead of passing quietly, `make check`
  prints the tiers it did **not** run, and `make check-all` is the one command whose
  green result means what a green CI run means. The e2e job also no longer hangs off
  the gate job, which had let one red lint erase the whole e2e tier and render it as
  "skipped".

- **One suite was still deciding that for itself, and the guard could not see
  it.** The inversion above left `keeper/internal/oracle` reading a bare
  `REQUIRE_DOCKER` — a name nothing sets, neither the Makefile nor CI — so its
  `log.Fatalf` was unreachable and a Postgres container that failed to start
  returned 0 from `TestMain`. The package printed `ok` having run none of its 116
  tests, and the tier stayed green across the hole. It now calls the shared helper
  like the other 36 packages: the same failure is fatal, and the only way to get
  that `ok` back is to declare the skip out loud.

  The drift guard written to prevent exactly this matched two literal names,
  `SOUL_STACK_INTEGRATION_{SKIP,REQUIRE}_DOCKER` — the vocabulary that same change
  had just introduced. It could only ever catch a future fork spelled in the new
  terms, while being blind by construction to the ~35-copy population it exists to
  finish off, every one of which predates those names. It also walked the `keeper`
  module alone, leaving `shared/`, `soul/`, `tests/` and `examples/` outside its
  world — where a second, dormant copy was living in
  `examples/module/soul-cloud-aws` (inverted here too, before that lane enters the
  gate rather than after). The guard now matches by property rather than by
  spelling — any environment variable about docker, or in our own
  `SOUL_STACK_INTEGRATION_*` namespace, less docker's own client configuration —
  parses the source instead of grepping it, and walks the whole checkout.

- **Tag-guarded tests are compiled by the gate again.** Nothing built the
  `integration` sources on a normal PR, so they rotted out of sight: the Soul
  failback suite still called `reconnectLoop` with 12 arguments after it grew to
  14 (console wiring, then sd_notify), and `keeper/internal/redis` called
  `NewClient` without the password resolver. Both broke at compile, so
  `make test-integration` could not run those packages at all — and the one error
  it printed looked like a build glitch rather than several suites going missing.

  `make check` now runs **`vet-tags`**: `go vet` under `integration` across the
  workspace, plus `e2e` / `e2e_live` / `e2e_k8s` / `smoke` for the sets that live
  behind their own tags. Vet compiles without running anything, so the gate stays
  docker-free and the container suites stay opt-in. The failback behavior itself
  was intact — only the call had drifted — and those tests pass again.

- **The blocking live gate downloaded from public github.com on every run**
  (NIM-542). Six
  of the nine `make e2e-live-gate` tests run a live `create` of
  `examples/service/redis`, and that create fetched three release tarballs —
  `node_exporter`, `redis_exporter`, `vector` — from GitHub Releases inside the
  soul container: ~18 downloads per gate run. This is the pre-tag blocking step
  (`RELEASING.md` step e), and its acceptance is "three runs on an unchanged slice
  give the same result"; github.com is not in the slice. It had already produced
  red gates that were nothing but the network.

  The harness now caches those tarballs outside the repo (digest-verified on the
  way in; a wrong-digest file is deleted rather than reused), serves them over
  HTTPS on an ephemeral local port laid out exactly like upstream, and points the
  service at it with a `vars/99-*` layer. The harness verifies all three digests
  itself because the subject does not verify all three: `vector` and
  `redis-exporter` hand `checksum: "${ input.sha256 }"` to `core.url` and would
  reject bad bytes inside the container, but `node-exporter/tasks/install.yml`
  deliberately declares no checksum, so a truncated node_exporter tarball would
  pass the fetch and surface later — a failed unpack, or a service that will not
  start — wearing the costume of a product defect. `make e2e-live-artifacts`
  primes the cache deliberately; the gate runs it as an early, named step so the
  one network-dependent moment happens up front in half a minute rather than
  twenty minutes in as a failed fetch.

  **HTTPS, not plain HTTP — because the subject says so.** All three destinies
  declare `base_url` with `pattern: "^https://[A-Za-z0-9._/:-]+$"`, and `core.url`
  refuses a plain-http target unless the step opts in. Both are properties of the
  service under test, so the first cut of the mirror — an http file server — did
  not fail as a network problem but as
  `input $.base_url … does not match pattern`, twenty minutes in, wearing the
  costume of a product defect. Loosening the destiny would have been bending the
  subject to fit the fixture. Instead the mirror mints a per-run CA and a leaf for
  the address it advertises, and `SpawnSoulContainer` drops that root into the
  container's trust store and runs `update-ca-certificates` before the soul
  starts. The product walks its real fetch path — scheme check, TLS handshake,
  chain validation, and the checksum wherever the destiny declares one — exactly
  as it does against github.

  **The layer goes into the fixture's materialized copy, never into
  `examples/service/redis`.** The example is the subject under test (NIM-211);
  bending it to suit the fixture would leave the gate green about a service nobody
  runs. What the fixture writes is exactly the mirror override `vars/00-base.yaml`
  already documents for an operator without github access.

  An override three YAML layers away from where it is read is a claim that holds
  until it doesn't — rename the file, add a `vars/_stack.yaml`, rename a var, and
  it contributes nothing while the create still passes, from github, green. So the
  mirror **counts what it served** and a successful run that never used it fails;
  docker-free guards catch a fourth external fetch, a version or digest the catalog
  does not carry, a vars layer that out-sorts `99-*`, or a `_stack.yaml`, twenty
  minutes earlier. One of those guards exists because the http/https mistake above
  walked straight into the gap: it starts a real mirror, reads each destiny's own
  declared `pattern` at run time, and requires the URL the overlay generates to
  satisfy it — neither side allowed to hardcode the scheme, or the test would only
  be agreeing with itself. Two more guard the CA half, where the silence is
  quieter still: `update-ca-certificates` reads only `*.crt` under
  `/usr/local/share/ca-certificates` and ignores anything else without a word —
  exit code 0, nothing added — so the file name is checked here rather than
  described in a comment, and so is the property the container actually depends
  on, that `caPEM` carries the CA-flagged root that signed the leaf and not the
  leaf itself. Both mistakes are one-line edits that compile, pass every other
  guard, and surface as `x509: certificate signed by unknown authority` inside a
  container twenty minutes later, worn as a product defect.

  **The container had to be booted first, and nothing had been checking that.**
  Running a command in a soul container the moment it is declared ready exposed a
  readiness check that had been wrong for as long as L3b has existed. It waits on
  `systemctl is-system-running --wait` and accepted exit code 1 as "degraded,
  which is normal for this image" — but systemctl returns 1 just as readily when
  it could not reach the bus at all, which is systemd not being up yet, and
  `--wait` does not help, because waiting is what it does *after* connecting. So a
  container could be handed over mid-boot. `update-ca-certificates`, now the first
  thing to run in one, creates two temp files in `/tmp` and reads them eighty
  lines later; in between, `systemd-tmpfiles-setup.service` reaches the
  `D /tmp 1777 root root -` line of `/usr/lib/tmpfiles.d/tmp.conf`, and `D` with
  `--remove` empties the directory. The files vanished under the running script,
  and the gate went red on a module-delivery test that fetches no artifacts and
  has nothing to do with certificates. Readiness is now judged by the state
  systemctl printed, matched per line; the exit code is kept wide only so a
  genuinely degraded container is not thrown away. Measured on one container under
  load: the old rule would have fired at 244 ms on `Failed to connect to bus`, the
  new one waited for `running` at 761 ms.

  **The real github path stays covered outside the gate.**
  `TestL3bRedisLiveUpstream_ArtifactsFromGitHub` runs the same create against real
  GitHub Releases and is deliberately absent from `E2E_GATE_TESTS` — the one L3b
  test allowed to fail for a reason outside the repository, which a blocking gate
  must never be. It skips (naming the host) if upstream is unreachable *before* the
  stand, and after a failure re-probes to print either "read this as environment"
  or "upstream is still reachable, so read this as a finding". No new classifier
  verdict was introduced: inside the gate the category no longer occurs, and
  outside it the test answers the question itself.

  **Not fixed:** `core.pkg.installed` still reaches `deb.debian.org` and
  `packages.redis.io` on every live create. The tarballs were the scope.

---

## [v0.1.0-beta.1] — 2026-06-15

First tag of the public beta. One git tag = version of all 7 go.work modules ([ADR-011](docs/adr/0011-go-layout.md)); the version is injected into binaries via `-X main.<var>`, printed by `keeper version` / `soul version` / `soulctl version`. Beta distribution is build-from-source ([CONTRIBUTING.md](CONTRIBUTING.md)); the release procedure is [RELEASING.md](RELEASING.md).

### Highlights of the beta

- **OpenAPI code-first pivot** — the OpenAPI 3.1 source of truth is now a huma aggregator in code (`HumaFullSpecYAML`); `docs/keeper/openapi.yaml` is a derived committed snapshot with a `make check-openapi` drift guard ([ADR-054](docs/adr/0054-openapi-code-first.md)).
- **RapiDoc at `/docs`** — built-in OpenAPI spec rendering in Keeper.
- **Retention** — configurable retention of logs/history (audit, state_history).
- **Version injection** — `git describe` → ldflags `-X main.<var>`, a single version across all binaries ([ADR-011](docs/adr/0011-go-layout.md)).
- **Security hardening ahead of beta** — `govulncheck` supply-chain gate in `make check`, Vault least-privilege policy, edge hardening, retention security review.

### Added
- A fully assembled MVP skeleton of the Keeper cluster, Soul agent, soul-lint, and soulctl CLI.
- Tide+Surge subsystems, Beacons/Vigil-Oracle-Decree, Scry drift-detect, Push (Variant C), Errand, Toll detector.
- 17 Soul-side core modules + 3 Keeper-side core modules (cloud/vault/soul-registered).
- 6 CloudDriver plugins (AWS/GCP/Yandex.Cloud/Azure/OpenStack/Proxmox) and SSH providers static/Vault-CA/Teleport.
- E2E harness L3a/L3b/L3c, runbooks under `docs/operations/`, SBOM/deb/rpm packaging, local CI gate `make check`.
- Load-test harness `soul-legion` (`make stress`) — simulates a fleet from 1k to 25k Souls and runs a read load (24 GET endpoints), a write cycle (create→delete), and a Voyage. Confirmed: Keeper holds linear scaling up to 25k+ concurrent streams at ~0.12 MiB per soul (internal scale-verification tool, not part of the runtime).

### Changed
- The source of Soul presence is a Redis lease; the `souls.status` field no longer filters the target resolver (invariant "hot data → Redis, not PG").
- Soulprint in JSONB/template uses a `snake_case` canon (BUG-A).
- `core.pkg`/`core.service` read facts via Soulprint (BUG-B Variant A), apk-version aligned.
- `apply_runs` marks non-matching hosts as `no_match` (FINDING-01).
- Bulk coven-assign got `mode=replace` and `selector.incarnation`; REST↔MCP parity.
- `POST /v1/voyages/preview` got a separate rate limit (`voyage_preview`, 30 per window / burst 60) and no longer shares a limit with Voyage creation (`voyage_create`, 10/20) — frequent preview requests no longer bump into the creation write limit ([ADR-050](docs/adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api) / [ADR-043](docs/adr/0043-voyage.md#adr-043-voyage--unified-batch-run) amendment).

### Fixed
- Large team-scale Voyages (on the order of 10k hosts) no longer hang at finalization. Previously Redis pub/sub subscribed to a separate channel per applyID, and on a large fleet the number of subscriptions ran into the Redis `maxclients` limit — the Voyage never completed. Keeper now does not spin up a cross-keeper bridge for locally connected Souls and uses a fixed set of sharded event channels (`events:shard:<n>`, K=256) instead of a channel-per-applyID. Voyage on 10k hosts: from "never completes" to ~11.6s ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis) amendment).
- §26 audit-log scaling to 100k+ VMs — 5 options considered, decision deferred.
- Tide: cancellation, stop-and-wait, SSE `/v1/tides/{id}/progress` (polling GET exists) — post-MVP.
- Vigil inotify: recursive + throttle — P3.
- ADR-030 S5-final `PortentEvent.data` hard-cut — P3 post-1-prod-release.
- Push S3 (compile-time wire) — no use case, revive on request.
- Multi-cluster federation — rejected, horizontal scale of a single cluster covers the requirements.
- Cloud parity Phase 4 (expansion beyond 6 drivers) — user decision deferred to a following release.
- Shepherd — stream balancing on scale-out, waiting on ADR-006 amend + architect design.
- ADR-018 user-collectors `/etc/soul/soulprint.d/*` — separate ADR.

### Known limitations (beta)
Deliberate beta out-of-scope, not bugs (full list — [docs/known-limitations.md](docs/known-limitations.md)):
- **Cloud provisioning** — CloudDriver plugins and `core.cloud.provisioned` are present, but end-to-end cloud CRUD did not make it into the beta (Cloud parity Phase 4 — next release).
- **MCP cadence** — schedule (Cadence) management via MCP is not yet covered; the path is OpenAPI/soulctl.
- **Audit-log scaling to 100k+ VMs** — on a 100k-VM fleet the audit-log PG-INSERT rate will hit a ceiling; 5 scaling options considered, decision deferred (§26 backlog).
- **No immediate JWT revocation** — operator revocation = `revoked_at`, active JWTs work until `exp` (protection is a short TTL), ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- **Push — narrow profile** — `keeper.push` (Variant C) is covered at a basic level; S3 compile-time wire has no use case, extensions on request.
- **External pentest — post-beta** — an independent security audit is planned after the beta, before GA.

### Security
- mTLS Keeper↔Soul per [ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add); the Soul private key never leaves the host (CSR onboarding).
- [ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index) Sigil — a Keeper-signed digest index for all plugins, default-deny when unsigned.
- JWT authentication of operators ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)), signing key via Vault KV `secret/keeper/jwt-signing-key`.
- RBAC default-deny, multi-coven AND-merge in Tide ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override) fail-closed).
- Augur ([ADR-025](docs/adr/0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)) — a Keeper-side broker for Soul's external access, default-deny.
- Vault least-privilege policy + recovery-enable procedure (`docs/operations/disaster-recovery.md`).
- The migration-CEL sandbox forbids `vault/now/register/soulprint/essence/input` ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

---

## Roadmap → Released — feature-complete MVP

This section covers the actual state of the code and documentation as of 2026-05-27.

### Keeper cluster
- Stateless N-Keeper on top of shared PG/Redis ([ADR-002](docs/adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)); SID lease in Redis ([ADR-006](docs/adr/0006-cache-redis.md#adr-006-cache-and-coordination--redis)).
- Bootstrap of the first Archon via `keeper init --archon=<aid>`, AID validation, partial unique index ([ADR-013](docs/adr/0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon)).
- Identity registry `operators` + JWT auth, FK on all audit fields ([ADR-014](docs/adr/0014-operator-identity.md#adr-014-operator-identity-model-archon)).
- Config hot-reload with write-back YAML ([ADR-021](docs/adr/0021-hot-reload-config.md#adr-021-hot-reload-of-config-with-write-back-yaml)); Toll leader uses `UpdateConfig` + RWMutex snapshot.
- Conclave (registry of live instances) + Watchman (isolation detection + soul-shedding) + refuse-guard `acolytes=0` warn.
- Toll cluster-wide detector of mass Souls attrition ([ADR-038](docs/adr/0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition)) with hot-reload and webhook diff-recycle.
- Tide + Surge — invocation-time scope chunking, AND-merge target, REPLACE concurrency, abort+continue, per-Surge state commit, Acolyte-style lease for failover ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Reaper — leader via Redis lease, cleans up pending/zombie/expired seeds; soft-delete/archive of state_history.

### Souls (agents)
- gRPC bidi `EventStream` over mTLS, `oneof payload`, thematic `.proto` files, forward-compat only-add ([ADR-012](docs/adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)).
- Typed Soulprint ([ADR-018](docs/adr/0018-soulprint-typed.md#adr-018-soulprint-typed-schema-mvp)): `SoulprintFacts` (Os/Kernel/Cpu/Memory/Network), `pkg_mgr`/`init_system` collected on the Soul side.
- State_schema migration DSL ([ADR-019](docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)): flat (`rename`/`set`/`delete`/`move`) + CEL in `set.value` + structural `foreach`; one PG transaction, snapshot per step.
- Soul-reconcile dispatched-orphan ([ADR-027(g)](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)); WardRoster in proto.
- The same `soul` binary works in pull (daemon) and push (oneshot) — modules apply the same way.

### Scenario / Destiny
- Coven = stable tags only, role is NOT Coven ([ADR-008](docs/adr/0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)); the declared role lives only in the host's Choir Voice (it was `incarnation.spec.hosts[].role` until the [ADR-044 amendment 2026-07-30](docs/adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role) removed that field).
- Full scenario-DSL set ([ADR-009](docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)): `on:`/`where:`/`serial:`/`run_once:`/`apply:`/`state_changes` + two-level resource resolution.
- Templating engine ([ADR-010](docs/adr/0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)): CEL for YAML expressions, Go text/template + sprig allowlist for files, `${ … }` marker, strict mode, secret masking.
- Top-level `output:` in `destiny.yml`, read via `register:` on the applier task.
- `soulprint.hosts` — scenario-only accessor of the run's hosts with stable facts.
- Scry drift detection ([ADR-031](docs/adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): a pure-read Plan contract over 14 core modules, on-demand check-drift + background `scry_background` (default OFF).
- Trial runner `soul-lint trial` ([ADR-023](docs/adr/0023-trial-test-runner.md#adr-023-test-runner-trial-soul-trial-and-dsl-coverage)) with L0/L1/L2 coverage, `assert.state_after`.

### Modules (core MVP)
- 17 Soul-side core ([ADR-015](docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list) + extensions): `pkg`/`file`/`service`/`user`/`group`/`exec`/`cmd`/`cron`/`mount`/`git`/`archive`/`sysctl`/`firewall`/`http`/`line`/`repo`/`url`; `core.file.rendered` is the only render step.
- 3 Keeper-side core ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)): `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`.
- Errand pull-ad-hoc exec outside a scenario ([ADR-033](docs/adr/0033-errand.md#adr-033-errand--pull-ad-hoc-exec-outside-a-scenario)) with E1..E5: HTTP handler, cross-keeper routing, Reaper purge, MCP tools, soulctl commands, `?module=` filter.
- Per-module canon doc at `docs/module/core/<name>/README.md`.

### Plugins (SDK Phase 2)
- A single gRPC-stdio handshake for SoulModule/CloudDriver/SshProvider/soul_beacon ([ADR-020](docs/adr/0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)).
- Sigil ([ADR-026](docs/adr/0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index)) — Keeper-signed digest index, soul-side cache.
- Companion repository `soul-stack-plugins/` + template `soul-mod-template`.
- `soul-lint plugin-init` CLI via `go:embed` for scaffolding a new plugin.
- 3 pilot official plugins: `soul-mod-docker-container`, `soul-mod-nginx-vhost`, `soul-mod-postgres-user` (namespace `official`).

### Beacons / Vigil-Oracle-Decree
- Event-driven monitoring ([ADR-030](docs/adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor)): an edge-triggered check + reactor contour.
- Vigil scheduler on Soul + a 4th plugin kind `soul_beacon` (`port_closed`/`disk_full`/`process_absent`/`http_unhealthy`/`service_down`/`file_changed`).
- Oracle Portent-reactor on Keeper: Vigil/Decree CRUD registries, OpenAPI+MCP, RBAC, audit, scenario-enqueue.
- Circuit-breaker auto-disable of Decree, Oracle+beacon metrics.
- Typed `PortentPayload` (parity with ADR-018 Soulprint), inotify P0/P1, `action=scenario-only`.

### Push (Variant C)
- Multi-host destiny push without incarnation/scenario ([ADR-032](docs/adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-without-incarnationscenario)).
- S0..S7: SSH transport (CA-signed host certs), Deliverer (SHA-256 cache), Cleaner, SshDispatcher, orchestrator, multi-CA, `souls.ssh_target` jsonb, `push_providers` PG table, auto-import legacy, multi-keeper bootstrap in kind, multi-provider routing.
- SSH providers: `soul-ssh-static`, `soul-ssh-vault` (variant B, ephemeral SSH CA), `soul-ssh-teleport` (proxy_jump via bastion).

### Cloud
- 6 CloudDriver plugins: `soul-cloud-aws`/`gcp`/`yc`/`azure`/`openstack`/`proxmox`.
- `core.cloud.provisioned` (`created`/`destroyed`) — keeper-side ([ADR-017](docs/adr/0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)).
- Cloud-init bootstrap MVP (`keeper/internal/cloudinit/`).
- Credentials flow A + Status/List only-add API.

### Work-queue / Tide
- Acolyte/Ward/Summons work queue ([ADR-027](docs/adr/0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)): PG claim, retry/backoff, recovery-reclaim, S6 Soul-reconcile dispatched-orphan.
- Tide PG schema 055, claim loop, FinalizeWithOwnership, real surge loop, spawn-await, decision-gate ([ADR-040](docs/adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).
- Audit canon: handler emit, `souls_in_surge`; HTTP/MCP/soulctl REST↔MCP parity.

### Observability
- Prometheus-primary + OTel bridge ([ADR-024](docs/adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)).
- Oracle/beacon/Toll/Acolyte/Ward metrics + `soul /metrics` basic-auth (`password_file`).
- OTel collector + Jaeger in the dev environment.
- Audit pipeline ([ADR-022](docs/adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)): PG + multi-sink + OTel export, GET `/v1/audit`.

### Security
- mTLS Keeper↔Soul, JWT operators, Vault integration (Soul-safe client in `shared/vault`, server-side in `keeper/internal/vault`).
- Sigil plugin trust (default-deny, Keeper-signed digest).
- RBAC default-deny, per-Coven/service scope for incarnation, multi-coven AND fail-closed.
- Augur — a Keeper-side broker for Soul's external access.
- archive zip-slip protection, edge hardening for the AWS pilot, refuse-guard multi-keeper.

### Documentation
- [docs/architecture.md](docs/architecture.md) — 40 ADRs (ADR-001…040, excluding ADR-034/036/037).
- [docs/scenario/](docs/scenario/README.md) — concept/orchestration/tide-state-commit.
- [docs/keeper/](docs/keeper/README.md) — modules/rbac/storage/push/reaper/augur/cloud/mcp-tools/openapi/prod-setup.
- [docs/module/core/<name>/README.md](docs/module/) — per-module documentation (mandatory canon).
- [docs/operations/](docs/operations/README.md) — bootstrap-rbac/deployment/disaster-recovery/scaling/upgrade/monitoring/faq/recovery-reclaim-apply-runs.
- [docs/soul/](docs/soul/README.md) — concept/connection/identity/onboarding/soulprint/modules/config.
- [docs/testing/e2e.md](docs/testing/e2e.md) — L1/L2/L3a/L3b/L3c levels ([ADR-039](docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).
- [docs/templating.md](docs/templating.md), [docs/migrations.md](docs/migrations.md), [docs/observability.md](docs/observability.md), [docs/soul-lint.md](docs/soul-lint.md), [docs/roadmap.md](docs/roadmap.md), [docs/ideas.md](docs/ideas.md).
