# ADR-0093. Maintenance windows and freezes — when a run of an incarnation may start

**Status:** accepted 2026-10-27; number 0093 stamped at its squash-merge into `release/NIM-1069`, shipped without the service-TLS epic NIM-1070 by release NIM-1133
**Amends:** [ADR-043](0043-voyage.md) (a `scheduled` Voyage becomes the carrier of a run deferred to a window, and the claim asks the gate; the create request gains `in_maintenance_window` and `confirm_freeze`), [ADR-046](0046-cadence.md) (the run recipe gains `in_maintenance_window`; a spawn during a freeze is deferred, never confirmed), [ADR-010](0010-templating.md) (render CEL gains its first time-dependent function; NIM-1073 records the function and its contexts), [ADR-017](0017-keeper-side-core.md) (the Reaper's cert rotation never starts during a freeze; recorded in NIM-1077's amendment of that ADR, which takes effect with epic NIM-1070)
**Leaves unchanged:** [ADR-0091](0091-scenario-linear-barrier.md) — a run that has started is not cut off, and nothing here reaches inside it; [ADR-028](0028-rbac-storage.md) — one name joins the closed permission catalog, the model does not move
**Implemented by:** NIM-1072 (calculator, tables, REST, RBAC, audit, MCP), NIM-1073 (CEL), NIM-1074 (run and Voyage fields, deferral, freeze consent), NIM-1075 (Cadence), NIM-1076 (UI) — epic NIM-1069; NIM-1137 (one-off windows, the cleanup of past ones; [amendment 2026-10-30](#amendment-2026-10-30-nim-1137-one-off-windows-and-the-cleanup-of-past-ones)); NIM-1138 (the cluster default for incarnations without windows; [amendment 2026-10-30](#amendment-2026-10-30-nim-1138-the-cluster-default-for-incarnations-without-windows)); NIM-1081 (cert rotation in a window) takes effect with epic NIM-1070; this ADR is NIM-1071

### Problem

A production incarnation must not be changed at a random moment. Some services pick up a new
certificate only on a restart or a reboot (NIM-1070), and a fleet's operators agree change windows
with the owners of each service. The platform has no notion of *when* an incarnation may be
changed:

- a Cadence fires at its interval or its cron, in UTC, and knows nothing else
  (`keeper/internal/cadence/cadence.go:110`);
- the Reaper's `rotate_due_certs` spawns a rotation Voyage on the tick a certificate comes due.
  Today it is `commitRotation` in `keeper/internal/reaper/rotate_certs.go`. With epic NIM-1070,
  NIM-1084 removes that signing implementation, and NIM-1081 brings the rule back under the same
  name as a policy engine;
- an operator's run starts the moment it is asked for
  (`POST /v1/incarnations/{id}/scenarios/{scenario}`, `keeper/internal/api/huma_incarnation.go:68`,
  MCP `keeper.incarnation.run`).

Nothing holds work back during a change freeze either: a holiday, a sale, an incident.

One piece already exists. A Voyage can start later: a `schedule_at` in the future puts the row in
status `scheduled` (`keeper/internal/voyage/voyage.go:60`, set in `crud.go` `Insert`), and the
worker claims a `scheduled` row only once `schedule_at <= NOW()`
(`keeper/internal/voyage/claim.go`, `ClaimNext`).

### Decision

**1. Two entities, and each belongs to one incarnation.**

| Entity | What it is |
|---|---|
| `MaintenanceWindow` | A weekly slot: days of the week, a start time, a duration, an IANA timezone, a comment. An incarnation has zero or more, and their union is the time a run may start. A second kind, one interval, joined with NIM-1137 (amendment 2026-10-30). |
| `Freeze` | A one-off interval: a start, an end, a comment. While it lasts no automatic run starts, and an operator's run needs explicit consent (§5). |

- Each row belongs to exactly one incarnation. There is no shared named window and no reference
  from one incarnation to another's (user, 2026-10-27). The UI may copy windows from another
  incarnation (NIM-1076); the copy is independent from then on.
- Storage is two tables, `incarnation_maintenance_windows` and `incarnation_freezes`. Each carries a
  foreign key to the incarnation with `ON DELETE CASCADE`, so destroying the incarnation removes
  them. The column names and the validation rules are NIM-1072's.
- Windows and freezes are separate tables because they answer different questions: a window says
  when a run *may* start, a freeze says when it *must not*. A freeze is not a window with a
  negative sign, and a freeze with no window around it is meaningful.
- Each weekly window keeps its own timezone, so a slot means the same wall-clock time on both sides of a
  DST change. A slot may cross midnight, slots may overlap, and a freeze may cut a slot.

**2. A window gates the START of a run, and nothing else.**

- **Open**, at a moment, means: no freeze of the incarnation covers it, **and** the incarnation has
  no windows or the moment is inside one of them. `OpenAt(t)` answers exactly that, and
  `NextStart(t)` is the first open moment at or after `t`. Every "open" in this ADR, in the API's
  status block and in CEL is this one.
- A run that started inside a window runs to its end, even when the window closes first. v1 has no
  "stop at the end of the window" mode.
- A run that asked for a window and could not start in one is deferred to `NextStart`.
- An incarnation with no windows may start a run at any time outside a freeze. A non-empty cluster
  default changes that: such an incarnation is held to the default's windows
  ([amendment 2026-10-30, NIM-1138](#amendment-2026-10-30-nim-1138-the-cluster-default-for-incarnations-without-windows)).
- The gate is decided **when the run starts, not when it is asked for**. A deferred run is told a
  time, and that time is a forecast. When it comes, the gate is asked again; if a freeze was added
  or a window changed in the meantime, the run is deferred again instead of starting.

**3. The carrier of a deferred run is a `scheduled` Voyage.**

- A deferred run is a scenario Voyage with `schedule_at = NextStart(now)`, status `scheduled`
  (`keeper/internal/voyage/voyage.go:60`). No new run state, no new table, no new worker. A request
  that carries its own `schedule_at` and `in_maintenance_window` starts at `NextStart(schedule_at)`.
- A run asked for through `POST /v1/incarnations/{id}/scenarios/{scenario}` starts through the
  scenario runner, not through a Voyage. When it has to be deferred, it becomes a `scheduled`
  Voyage over that one incarnation (NIM-1074).
- **The claim asks the gate for every scenario Voyage**, not only for the deferred ones: a deferred
  row, an automatic row (spawned by a Cadence or by the Reaper), and an operator's row with its own
  `schedule_at`. A freeze added after the row was created is caught here. So the row carries
  `in_maintenance_window` and the consent of §5 — which freezes it covers, not a bare flag — and
  the claim honours them as §4 says.
- **The gate is decided before the row leaves `pending` or `scheduled`** (NIM-1074). The claim
  (`voyage.ClaimNext`) peeks at the next claimable row together with the database clock, asks the
  gate at that instant, and only then claims that row by id with a CAS on the same predicate. A
  row the gate holds back is put back to `scheduled` at the gate's time with `started_at` still
  NULL and `attempt` untouched, and the next row is tried. The database clock is the gate's "now"
  because it is the clock the claimable predicate uses: a Keeper running behind it would otherwise
  find a due row still closed and defer it to the moment it already is.
- A row whose `started_at` is set is a reclaim after a lost lease, not a start, and the gate is not
  asked again: a run that started runs to its end (§2).
- The row records the request in two columns (migration 134): `in_maintenance_window` and
  `confirmed_freeze_ids`, the ids of the freezes that covered the start when consent was given.
- **One start for the whole Voyage** (user, 2026-10-28: the simplest; the per-target deferral is
  left to a later epic that reworks this functionality). The start of a Voyage over several
  incarnations is the first moment at which every target's gate is open at once
  (`maintenance.CommonStart`). A target with neither windows nor freezes never holds the others.
  A single target, and targets without windows, always have a start: a freeze of any length is
  waited out. Only slots can fail to meet: when the targets' windows leave no common moment within
  366 days after the last of their freezes ends (or after the latest of their single starts, when
  that is later), the request is refused with 422; when the
  windows are edited afterwards so that they stop meeting, the claim fails the row before it starts
  and every target carries `no_common_window`, since waiting would never end. The same failure
  covers a row whose targets cannot be read or whose windows the Keeper can no longer evaluate,
  because an error there would hold every Voyage queued behind it. The operator splits such a run
  into separate requests.
- A claim-time deferral is logged, not audited; the Voyage's moved `schedule_at` is what the
  operator sees (an audit event for it is NIM-1112). `POST /v1/voyages/preview` refuses in the same
  places as the create.
- A deferred run is visible in the runs list with its time, and it can be cancelled like any
  `scheduled` Voyage (NIM-1074, NIM-1076). The list of an incarnation's Voyages, its deferred runs
  among them, is `GET /v1/voyages?incarnation=<id>`: the incarnation's own runs list reads
  `apply_runs`, and a deferred run has no row there until it starts.
- A deferred `POST /v1/incarnations/{id}/scenarios/{scenario}` answers 202 without `apply_id`, with
  `voyage_id` and `schedule_at` of the scheduled Voyage (user, 2026-10-28).

**4. Who asks for the window.** The gate concerns runs of an incarnation: scenario runs and
`kind: scenario` Voyages and Cadences. A `kind: command` Voyage or Cadence addresses hosts, and a
host has no window of its own (Not in v1).

| Run | Window | Freeze |
|---|---|---|
| An operator's immediate run (REST, MCP, the UI's Run dialog, `POST /v1/voyages`) without `in_maintenance_window` | Not asked; starts now | Needs consent (§5) |
| An operator's run with `in_maintenance_window: true` | Deferred to `NextStart` | Deferred past it; no consent needed, because the run does not start inside the freeze |
| An operator's Voyage with its own `schedule_at`, without `in_maintenance_window` | Not asked; starts at `schedule_at` | Consent is checked against `schedule_at` at request time; at the claim, a freeze the consent does not cover defers the row past it |
| Rerun-last and the upgrade's scenario | Not asked; never deferred, so `in_maintenance_window` is not accepted on them | Needs consent, checked before the request commits anything (user, 2026-10-28: consent only) |
| A Cadence spawn with `in_maintenance_window` | Deferred to `NextStart` | Deferred past it |
| A Cadence spawn without `in_maintenance_window` | Not asked; starts at its tick | Deferred to the first moment no freeze covers; there is no consent for a Cadence |
| The Reaper's cert rotation (takes effect with epic NIM-1070) | Per the certificate's `rotate.action` (NIM-1081); the claim re-checks the window only once the row carries `in_maintenance_window` (NIM-1106) | Never starts inside a freeze |
| An incarnation's destroy (`DELETE /v1/incarnations/{id}`, MCP `keeper.incarnation.destroy`) | Not asked | Needs consent, checked before the destroy commits anything (NIM-1085) |
| A Decree reaction (Oracle) | Not asked | **Not held back** — a reaction is a repair (NIM-1085) |

- **An operator's run is never refused because of a window.** A window holds back only the runs
  that ask for it. The user decided a forced RUN is always allowed; the Run dialog's checkbox is how
  an operator asks for the window.
- **An automatic run never starts during a freeze** (user, 2026-10-27): a Cadence spawn and the
  Reaper's cert rotation. Until epic NIM-1070 that holds for the rotation's Voyage, not for its
  signing (the `commitRotation` bullet below).
- **A Decree reaction is the one exception** (user, 2026-10-27, NIM-1085). A reaction is usually a
  repair — a service went down and the Decree restarts it — and holding a repair until the freeze
  ends is worse than running it. It reaches the work queue through
  `keeper/cmd/keeper/oracle_enqueuer.go` (`applyrun.InsertPlanned`), outside both the runner and the
  Voyage claim, so no gate of this ADR may sit on a path it shares.
- **A destroy is an operator's act and needs consent like a run** (user, 2026-10-27, NIM-1085):
  `confirm_freeze: true` on `DELETE /v1/incarnations/{id}` and on `keeper.incarnation.destroy`,
  else 409 `freeze-active`, checked before anything commits. Built with NIM-1074.
- With epic NIM-1070, a certificate that expires inside a freeze is rotated in the last window
  before it;
  NIM-1081 owns that pull-forward, and also the case where the freeze is added after that last
  window has passed, so the certificate would expire inside it. Under NIM-1081 the signing happens
  inside the rotation run, so deferring the run defers the signing with it; the registry never
  records a certificate the hosts do not have.
- **Until epic NIM-1070, the Reaper's rotation is `commitRotation`.** It signs and records the new
  certificate on the tick, then inserts a scenario Voyage that carries neither
  `in_maintenance_window` nor consent. The claim asks the gate for that row like any other (§3):
  a window does not hold it, and a freeze defers it past its end. While it waits, the registry holds
  a certificate the hosts do not have yet; NIM-1081 closes that gap.
- "The first moment no freeze covers" is not `NextStart` when the incarnation has windows: it is the
  end of the active freeze, taken again while another freeze covers that end (`ActiveFreeze`).
- The field is the same name everywhere it appears: on the run request, on `POST /v1/voyages`, and
  on the Cadence recipe, because a Cadence's recipe is the Voyage create field set
  (`keeper/internal/cadence/cadence.go:110`). A spawned Voyage carries it, so §3 applies unchanged.

**5. A forced run during a freeze needs explicit consent.**

- In the UI, the Run dialog shows the active freeze and its comment. A checkbox consenting to work
  during the freeze is mandatory, and the run button stays disabled until it is ticked.
- In the API and MCP the same consent is `confirm_freeze: true`. Without it, a run whose start
  falls inside a freeze is refused with **409**, problem type **`freeze-active`**, and the answer
  names the frozen incarnations with the end and comment of their freeze (the covering one that
  ends last). Without this refusal, MCP clients and scripts would start work in a freeze silently.
  Only a run or a Voyage is offered `in_maintenance_window` as the alternative; rerun-last, the
  upgrade and the destroy are offered consent alone.
- Consent is checked against the moment the run would start: now for an immediate run,
  `schedule_at` for a scheduled one.
- **Consent covers the freezes it was given for, not freezes in general.** The run records which
  freezes covered its start when the request was made. At the claim there is no request left to
  answer with 409, so a row whose start falls into a freeze outside that set — one added later — is
  deferred past it. Nobody consented to that freeze.
- Rerun-last and the upgrade commit before the runner starts: `UnlockForRerunWithInput` and
  `UpgradeStateSchema` (`keeper/internal/api/handlers/incarnation_typed.go`). The freeze check runs
  before those transactions and refuses the whole request, so a 409 never leaves the incarnation in
  `applying` or with an upgraded schema and no run.
- **A Voyage over several incarnations is one request and gets one answer.** Without consent, any
  target inside a freeze refuses the whole request with 409 — the same shape as the refusal of an
  `error_locked` target (`keeper/internal/api/handlers/voyage.go`, the `locked` list in the scope
  resolution). The answer groups the frozen targets by freeze and caps what it names, ten
  incarnations per freeze and ten freezes, then counts, because a target can be the whole fleet.
  With consent, it covers every target.
- `confirm_freeze` outside a freeze changes nothing, so a client that always sends it is not
  refused.
- The run's audit payload records both fields, so a run started in a freeze is findable afterwards.

**6. One permission for the writes.**

- `incarnation.maintenance-set` creates, updates and deletes both windows and freezes. Reading them
  stays under `incarnation.get`.
- It is a separate permission (user, 2026-10-27). A holder of `incarnation.run` may push one run
  through a freeze, with recorded consent. They may not move the windows and freezes that every
  other run, and every automatic run, obeys.
- The scope is the same as every other incarnation mutation's.

**7. The surface.**

| What | Name |
|---|---|
| REST | `/v1/incarnations/{id}/maintenance-windows`, `/v1/incarnations/{id}/maintenance-windows/{window_id}`, `/v1/incarnations/{id}/freezes`, `/v1/incarnations/{id}/freezes/{freeze_id}` |
| Audit | `maintenance_window.created` / `.updated` / `.deleted`, `freeze.created` / `.updated` / `.deleted` |
| Request fields | `in_maintenance_window: bool`, `confirm_freeze: bool` (`confirm_freeze` as a query parameter on `DELETE /v1/incarnations/{id}`) |
| Problem type | `freeze-active` (409); MCP code `freeze-active` |
| Voyage columns | `voyages.in_maintenance_window`, `voyages.confirmed_freeze_ids` (migration 134; the user named the second, 2026-10-28) |
| Target error code | `no_common_window` |
| List filter | `GET /v1/voyages?incarnation=<id>` |
| CEL | `incarnation.maintenance_window.text`, `incarnation.maintenance_window.next_start`, function `in_maintenance_window()` |
| Go package | `keeper/internal/maintenance` |

- The list endpoint also returns a status block `{open_now, next_start, active_freeze}`:
  `OpenAt(now)`, `NextStart(now)`, `ActiveFreeze(now)`. The UI's line "now: closed · next: Thu
  12-10 02:00" is the server's answer, not a second implementation of the calculator in the browser
  (NIM-1072).
- `keeper/internal/maintenance` holds one pure calculator over an incarnation's windows and freezes,
  with the clock injected: `OpenAt(t)`, `NextStart(t)`, `ActiveFreeze(t)`, `Text(t)`. Every consumer
  (the run path, the Voyage claim, Conductor, CEL, the status block, and with NIM-1070 the Reaper) asks it, so
  "open" means one thing everywhere.
- The field `in_maintenance_window` and the function `in_maintenance_window()` share a spelling and
  are two things. The field asks for a deferral; the function answers "is the incarnation open
  now". They never meet in one place: a request field is not CEL, and CEL has no request.

**8. Scenarios can read the window.** `incarnation.maintenance_window.text` is the windows in
human-readable form (`Text(now)`), `incarnation.maintenance_window.next_start` is `NextStart(now)`,
and `in_maintenance_window()` is `OpenAt(now)`. With no windows and no active freeze the function
is true and `next_start` is now. A scenario uses them to say in a notification when the next window
is, or to skip a disruptive step outside one. Settled by NIM-1073 (2026-10-28):
- **"Now" is the moment the Keeper renders the step.** The run reads the windows and freezes again
  before every step and asks the calculator about one instant, so every host of a step gets one
  answer and a freeze added mid-run reaches the next step. A step keeps its answer: a later pass
  renders it again with the window it ran with, so a window that closed since does not re-decide an
  include, an applier's `when:` or a loop of a step that ran and renumber the plan. The render reads
  no clock: the answers arrive with the pass (`render.MaintenanceWindow`, per step), and a claim
  that renders the step again gets the run's answers through its recipe rather than asking again.
  Under the deprecated staged dispatch, whose passages do not follow the plan's order, the window
  is read once per run.
- **Where it is answered:** every cell the Keeper renders for the step — `params:`, `where:`,
  `vars:`, `apply: input:`, `loop:`, `on:`, `assert:` — and every `when:` the Keeper decides (a
  step's under linear dispatch, a keeper task's, an include's). **Refused** where an expression is
  evaluated later, on the Soul, or for more than one step: `changed_when:`/`failed_when:`/
  `retry.until:`, a bare capture `match:`, a destiny's predicates, a host task's `when:` under
  `execution_model: staged`, `compute:`, service `vars/`, `validate:`, migration-CEL. The full list
  is in
  [templating.md §2.3](../templating.md#23-registered-cel-functions-starting-minimum).
- `next_start` is an RFC3339 string in UTC; with no windows `text` is `""`, not `Text`'s
  `any time`.

### Not in v1

- No "stop at the end of the window" mode (§2).
- **Runs that are not about one incarnation are outside the gate by construction.** A
  `kind: command` Voyage or Cadence, an Errand and a console session address hosts, and a host has
  no window of its own. A command run on a host of a frozen incarnation is not held back.
- No shared named windows and no window on a service or a Coven (§1).
- No Cadence schedule kind "at the start of every window" (user, 2026-10-28, NIM-1075). A cron
  with the window's period and `in_maintenance_window` gives one run per window.

### Settled points

- **The deferral of a Voyage over several incarnations** (user, 2026-10-28) — one start for the
  whole Voyage (§3). Rejected for v1: a not-before time per target, which needs the orchestrator to
  resume a Voyage between parts, and one Voyage per distinct next start, which turns one request
  into several runs whose batch and failure settings no longer span the whole. The same answer
  covers a freeze on one target at the claim: the whole row is deferred past it.
- **Deferring rerun-last and the upgrade's scenario** (user, 2026-10-28) — consent only, never
  deferred. Both start through the runner with `FromLocked`, which a scenario Voyage cannot carry.
- **`overlap_policy` and a deferred child** (user, 2026-10-28) — a child waiting for its start holds
  back `parallel` as well as `skip` and `queue`; see the
  [amendment 2026-10-28 (NIM-1075)](#amendment-2026-10-28-nim-1075-a-cadence-spawn-and-the-window).
- **Determinism of `in_maintenance_window()`** (NIM-1073, 2026-10-28) — the answer is sampled
  once per step by the run and is a value of the render's input, and a step keeps it when
  re-rendered, so no cache or re-render holds a clock or re-decides a step that ran; what is
  evaluated later than the render, or for every step at once, refuses the function (§8).

### Rejected

- **A shared, named window that incarnations refer to.** The user chose per-incarnation ownership:
  an edit to one window must not move the schedule of incarnations nobody looked at.
- **A window that also bounds a run's end.** It would interrupt a migration or a rolling restart
  halfway, which is worse than finishing late. v1 gates the start only.
- **One table with a "kind" column for windows and freezes.** The two have different shapes (a
  weekly slot against an absolute interval) and opposite meanings, so every reader would branch on
  the kind first.
- **A new run state for "waiting for a window".** `scheduled` with `schedule_at` already says
  "not yet, and here is when"; a second state would need its own claim path and its own reclaim.
- **Refusing an operator's run outside a window.** The user decided a forced RUN is always
  allowed; a window holds back only a run that asks for it.
- **Holding a Decree reaction during a freeze** (NIM-1085). A reaction is a repair. A service left down
  for the length of a freeze is worse than the change a freeze guards against.
- **Letting a destroy through a freeze silently** (NIM-1085). It is the most destructive operator
  act on an incarnation, and it is exactly what a freeze must not let happen by accident.

### Amendment 2026-10-28 (NIM-1075): a Cadence spawn and the window

Built by NIM-1075 (`keeper/internal/conductor/cadence_window.go`, `cadence_spawn.go`). The rules of
§4 for a Cadence, made exact:

- **The spawn decides the start at its tick.** A `kind: scenario` spawn reads the windows and
  freezes of every incarnation its target resolved to, in the spawn transaction, and inserts the
  child `scheduled` with the start below when it lies ahead, else `pending`. The claim asks the
  gate again (§3), so a freeze added after the tick defers the child further.
- **One Voyage, one start, common to all its targets** — the choice NIM-1074 made for a Voyage
  request (user, 2026-10-28). With `in_maintenance_window` it is the first moment every target is
  open (`maintenance.CommonStart`); without it, the first moment no freeze of any target covers
  (`maintenance.CommonStart` over freeze-only gates), the windows not asked. The spawn asks
  `voyage.MaintenanceGate` of the row it is about to insert — the gate the claim asks — so the tick
  and the claim cannot disagree.
- **No common open moment is a recorded skip** (user, 2026-10-28) — none within the bound the
  claim uses (`maintenance.CommonStart`: 366 days past the targets' last freeze end or latest single
  start, at most 1000 search steps). A
  request in that position gets 422 and a row at the claim fails; a schedule has nobody to answer.
  Nothing is spawned, `next_run_at` advances, and `cadence.skipped_no_window` names up to 20
  incarnations with their own next start, a window that no longer compiles first. The next tick
  asks again.
- **A deferred child holds back `parallel` too** (user, 2026-10-28). Under `skip` and `queue` a
  `scheduled` child is live already (ADR-046 §5). Under `parallel` it is the one child that counts
  (`cadence.HasDeferredChild`, status `scheduled`): the tick is a `cadence.skipped_overlap` and
  advances. Otherwise every tick of a wait adds a run, and all of them start together when the
  window opens. A running child still does not hold `parallel` back.
- **`in_maintenance_window` on a `kind: command` recipe is refused** with 422, and by the CHECK
  `cadences_in_maintenance_window_scenario_only` (migration 135): it would be accepted and do
  nothing. A command spawn reads no windows and no freezes.

### Amendment 2026-10-30 (NIM-1137): one-off windows and the cleanup of past ones

Decided by the user at the demo of release NIM-1133 (2026-10-30); built by NIM-1137
(`keeper/internal/maintenance`, migration 136, `keeper/internal/reaper/maintenance_purge.go`). The
use case: an incarnation has its regular windows, a problem needs a fix tomorrow, and the operator
adds a window for that one occasion and schedules the work into it.

- **A window has a kind, `weekly` or `once`, in the same list** (§1's table gains a second shape;
  the user chose it over a separate entity beside the freeze). `weekly` is the slot set of §1.
  `once` is `{kind: once, starts_at, ends_at, comment}`, the half-open `[starts_at, ends_at)` in
  RFC 3339, `ends_at` after `starts_at`, of any length, kept to the whole second — the precision
  the render reads a window at (§8). A request without `kind` is `weekly`, so a client written
  before the kinds means what it meant. **Each kind carries exactly its own fields:** a body that
  sends a field of the other kind, empty or not, is refused with 422 (MCP `validation-failed`)
  naming that field, never read past; the column `incarnation_maintenance_windows.kind` and the
  CHECK `incarnation_maintenance_windows_kind_columns` hold the same rule in the table. A `PUT`
  replaces the kind with the rest. This is not the rejected "one table with a kind column for
  windows and freezes": both kinds say when a run *may* start, so only the calculator branches on
  the kind, and a freeze stays its own table.
- **The cap of 50 counts both kinds, past `once` windows included**, because the cap keeps the
  calculator small and the calculator still reads a past row until it is deleted. With the default
  retention a past one leaves within three days; with `maintenance.window_retention: 0`, or with
  the Reaper off, an incarnation that adds a window per occasion reaches the cap, and the 422 asks
  the operator to delete one.
- **A past one-off window does not count.** "Open" of §2 reads: no freeze covers the moment, and
  the incarnation has no window **that counts at that moment**, or the moment is inside one. A
  weekly window always counts; a `once` window counts until its `ends_at`. So an incarnation whose
  only windows are `once` windows that have ended is open at any time outside a freeze, and
  `HasWindows(t)` never turns from false to true as `t` grows. A freeze cuts a `once` window as it
  cuts a slot. `CommonStart`, the claim, the Cadence tick, the status block and the scenario CEL
  read this one answer; `Text(t)` lists only the windows that count at `t`, a `once` window as
  `once 2026-11-05T22:00:00Z for 6h` (UTC, like `next_start`).
- **The calculator decides it per instant, `NextStart` included**, so a forecast agrees with the
  gate that will be asked when it comes (§2: the gate is decided when the run starts). Three
  consequences, accepted with the rule:
  - an incarnation with no weekly window, whose `once` windows a stretch of freezes covers to their
    end, has its next start at the end of that stretch, outside any window: by then they have
    ended, and a request made at that moment would start at once;
  - a run deferred into a `once` window and claimed only after its `ends_at` (a Keeper down, a
    queue held) starts at the claim, outside any window, when the incarnation has no weekly window;
    with one, it is deferred to the next slot as usual;
  - in a run over several incarnations, a target's `once` window that meets no common moment with
    the others is passed over, not refused: the common start lands after it ends, where that
    target no longer has a window. Only weekly windows can fail to meet (§3).
- **Past ones are deleted, each kind of row by its own key** (runtime settings of
  [ADR-0073](0073-keeper-runtime-config-pg.md), so a change needs no restart):
  `maintenance.window_retention` deletes a `once` window whose `ends_at` is more than that long
  ago, `maintenance.freeze_retention` a freeze; both default to `72h`, and `0` keeps them for ever.
  Settings keys `cfg_maintenance_window_retention` / `cfg_maintenance_freeze_retention`, bounds
  `[0s, 8760h]` on a `PUT`; `keeper.yml` refuses only a negative value. A weekly window is never
  deleted. Until a past row is deleted it is still listed by the API, so the UI can hide it behind
  "show past" the way it hides past freezes.
- **The Reaper deletes them**, on every pass of an enabled Reaper, one `reaper.batch_size` batch of
  each per pass, by the database clock, longest-ended first (rules
  `purge_expired_maintenance_windows` / `purge_expired_freezes`, not `reaper.rules` entries: the
  retention is their switch). `reaper.dry_run` applies: a dry pass deletes nothing. Each deleted
  row is audited `maintenance_window.deleted` / `freeze.deleted` with the row's fields and
  `reason: "expired"`, `source: keeper_internal`, no archon — the Reaper's convention, best-effort
  after the delete commits. Every window event now carries `kind`. **Deleting a past row changes no
  answer**, since it no longer counted — up to the skew between the Keeper's clock, which the
  status block reads, and the database's, which the delete reads: no deferred run loses a window
  it waits for, because a window it waits for has not ended.
- **Copying windows from another incarnation is the UI's** (NIM-1076; the copy of the kinds is
  NIM-1136): it copies the weekly windows and the `once` ones that have not ended, not past ones.
  Core has no copy endpoint; until NIM-1136, the UI of NIM-1076 copies every listed window as a
  weekly one, and a `once` window among them is refused with 422.

### Amendment 2026-10-30 (NIM-1138): the cluster default for incarnations without windows

Decided by the user at the demo of release NIM-1133 (2026-10-30), variant A1, "as simple as
possible"; built by NIM-1138 (`keeper/internal/maintenance/defaults.go`, the settings key in
`keeper/internal/settingsstore/registry.go`). The question from the demo: can the policy for an
incarnation with no windows be changed, as one default for all of them.

- **One cluster-wide list of weekly windows, `maintenance.default_windows`**, a runtime setting of
  [ADR-0073](0073-keeper-runtime-config-pg.md) (`PUT /v1/settings/cfg_maintenance_default_windows`,
  no restart). Each entry has the shape of §1's weekly window (`days`, `start_time`,
  `duration_minutes`, `timezone`, `comment`) and is checked by the rules an incarnation's weekly
  window is checked by, in one implementation for both (`config.NormalizeWeeklyMaintenanceWindow`);
  at most 50, the cap of one incarnation. Empty by default. It is the one structural value the
  overlay holds (ADR-0073, amendment of (j.3)).
- **It applies to an incarnation that has no weekly window of its own.** One weekly window of its
  own replaces the default entirely; the two are never unioned. One-off windows add to whichever
  weekly set is in effect, the incarnation's own or the default, and a one-off window, ended or not,
  does not switch the default off. Freezes are unchanged. An incarnation that must not be held to the
  default sets its own window every day at `00:00` for 1440 minutes in `UTC`; in a zone with
  daylight saving a 24-hour slot leaves an hour uncovered on the night the clock goes back.
- **One function decides which windows count, `Rules.Effective`, and every calculator a reader
  asks is compiled from it**: a run request and a Voyage request (`PlanRunStart`), the claim
  (`voyage.MaintenanceGate`), the Cadence tick (`cadence.skipped_no_window` included), the scenario
  CEL (`in_maintenance_window()`, `incarnation.maintenance_window.*`, and the create path's
  pre-flight, where the incarnation has no row yet and so the default alone), the status block and
  MCP. A reader reads the setting at each read, from the live config the overlay swaps on every
  instance when the setting changes (ADR-0073(g)). A run deferred before a change is asked again at
  its claim (§2).
- **The list says where the windows come from.** `GET /v1/incarnations/{id}/maintenance-windows`
  and `keeper.incarnation.maintenance-window.list` gain `defaults`, the default's windows, present
  only when they apply, and `status.source`: `own` (a weekly window of its own, or with no default a
  one-off window that has not ended), `default` (the default's windows, with the incarnation's
  one-off windows added) or `none` (nothing counts: any time outside a freeze). `items` stays the
  incarnation's own rows. A forced run is still held to no window (§4).
- **What it supersedes, for a non-empty default.** With the default empty every line below reads as
  before, answer for answer.
  - §2: "open" reads "no windows **in effect**", and "an incarnation with no windows may start a run
    at any time outside a freeze" holds only for one the default does not reach.
  - §3: "a target with neither windows nor freezes never holds the others" — the default holds it.
  - §8: "with no windows and no active freeze the function is true and `next_start` is now" — the
    default's windows are its windows, rendered as its own would be.
  - The NIM-1137 amendment's "an incarnation with no weekly window" means one the default does not
    reach either: under the default, an incarnation whose `once` windows have ended is held to the
    default, not open at any time.
  - §1 and *Rejected* refused a shared window because "an edit to one window must not move the
    schedule of incarnations nobody looked at". The default does exactly that, by the user's choice:
    one edit moves every incarnation without a weekly window of its own. It is not a named window
    incarnations refer to; an incarnation leaves it by having a weekly window.
  - §6: the default is edited under `setting.update` (ADR-0073), cluster-wide and with no
    incarnation scope, and audited `setting.updated`, not `maintenance_window.*`; the windows of an
    incarnation stay under `incarnation.maintenance-set`.
  - The NIM-1075 amendment's "the tick and the claim cannot disagree" assumes one default on every
    instance. A `keeper.yml` that sets `maintenance.default_windows` wins on its instance
    (ADR-0073(b)), so the key belongs in the cluster setting, or in the same form in every file.
- **Not in this amendment:** a default per service or per Coven (§1 stands), and the ticket's
  variant B, a switch "no windows means closed", which the user did not choose.
