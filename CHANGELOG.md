# Changelog

Format — [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Artifact versioning — via git ref ([ADR-007](docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field)), there is no separate `version:` field on Service/Destiny/Module.

## [Unreleased]

### Upgrade notes

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

- **Six new permissions land inside `<resource>.*` grants you already issued.**
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
  `soul.ssh-target-update`, `soul.console`. That list, and the `role.*`,
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

### Added

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

### Changed

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
