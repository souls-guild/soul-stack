# ADR-0093. Maintenance windows and freezes — when a run of an incarnation may start

**Status:** accepted 2026-10-27; number 0093 stamped at its squash-merge into `release/NIM-1069`, shipped without the service-TLS epic NIM-1070 by release NIM-1133
**Amends:** [ADR-043](0043-voyage.md) (a `scheduled` Voyage becomes the carrier of a run deferred to a window, and the claim asks the gate; the create request gains `in_maintenance_window` and `confirm_freeze`), [ADR-046](0046-cadence.md) (the run recipe gains `in_maintenance_window`; a spawn during a freeze is deferred, never confirmed), [ADR-010](0010-templating.md) (render CEL gains its first time-dependent function; NIM-1073 records the function and its contexts), [ADR-017](0017-keeper-side-core.md) (the Reaper's cert rotation never starts during a freeze; recorded in NIM-1077's amendment of that ADR, which takes effect with epic NIM-1070)
**Leaves unchanged:** [ADR-0091](0091-scenario-linear-barrier.md) — a run that has started is not cut off, and nothing here reaches inside it; [ADR-028](0028-rbac-storage.md) — one name joins the closed permission catalog, the model does not move
**Implemented by:** NIM-1072 (calculator, tables, REST, RBAC, audit, MCP), NIM-1073 (CEL), NIM-1074 (run and Voyage fields, deferral, freeze consent), NIM-1075 (Cadence), NIM-1076 (UI) — epic NIM-1069; NIM-1081 (cert rotation in a window) takes effect with epic NIM-1070; this ADR is NIM-1071

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
| `MaintenanceWindow` | A weekly slot: days of the week, a start time, a duration, an IANA timezone, a comment. An incarnation has zero or more, and their union is the time a run may start. |
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
- Each window keeps its own timezone, so a slot means the same wall-clock time on both sides of a
  DST change. A slot may cross midnight, slots may overlap, and a freeze may cut a slot.

**2. A window gates the START of a run, and nothing else.**

- **Open**, at a moment, means: no freeze of the incarnation covers it, **and** the incarnation has
  no windows or the moment is inside one of them. `OpenAt(t)` answers exactly that, and
  `NextStart(t)` is the first open moment at or after `t`. Every "open" in this ADR, in the API's
  status block and in CEL is this one.
- A run that started inside a window runs to its end, even when the window closes first. v1 has no
  "stop at the end of the window" mode.
- A run that asked for a window and could not start in one is deferred to `NextStart`.
- An incarnation with no windows may start a run at any time outside a freeze.
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
  with the clock injected: `OpenAt(t)`, `NextStart(t)`, `ActiveFreeze(t)`, `Text()`. Every consumer
  (the run path, the Voyage claim, Conductor, CEL, the status block, and with NIM-1070 the Reaper) asks it, so
  "open" means one thing everywhere.
- The field `in_maintenance_window` and the function `in_maintenance_window()` share a spelling and
  are two things. The field asks for a deferral; the function answers "is the incarnation open
  now". They never meet in one place: a request field is not CEL, and CEL has no request.

**8. Scenarios can read the window.** `incarnation.maintenance_window.text` is the windows in
human-readable form (`Text()`), `incarnation.maintenance_window.next_start` is `NextStart(now)`,
and `in_maintenance_window()` is `OpenAt(now)`. With no windows and no active freeze the function
is true and `next_start` is now. A scenario uses them to say in a notification when the next window
is, or to skip a disruptive step outside one. The contexts that may call the function and the CEL
type of `next_start` are NIM-1073's. With no windows `Text()` renders `any time` (NIM-1072);
whether CEL passes that through or gives `""` is NIM-1073's.

### Open points

- **`overlap_policy` and a deferred child.** The overlap check counts every non-terminal child
  (`docs/adr/0046-cadence.md` §5), a `scheduled` one included. So `skip` and `queue` hold back
  while a child waits for its window, but `parallel` spawns one deferred Voyage per tick, and they
  are all released together at `NextStart`. NIM-1075 decides how `parallel` treats a deferred
  child.
- **Determinism of `in_maintenance_window()`.** It is the first time-dependent function in render
  CEL: `now()` is refused as unsupported (`shared/cel/functions.go`, `Engine.guardUnsupported`).
  A render result that is cached, memoised or re-rendered on a retry must not outlive the moment
  it describes in a way that matters. NIM-1073 owns this, with the clock injected.

### Not in v1

- No "stop at the end of the window" mode (§2).
- **Runs that are not about one incarnation are outside the gate by construction.** A
  `kind: command` Voyage or Cadence, an Errand and a console session address hosts, and a host has
  no window of its own. A command run on a host of a frozen incarnation is not held back.
- No shared named windows and no window on a service or a Coven (§1).

### Settled points

- **The deferral of a Voyage over several incarnations** (user, 2026-10-28) — one start for the
  whole Voyage (§3). Rejected for v1: a not-before time per target, which needs the orchestrator to
  resume a Voyage between parts, and one Voyage per distinct next start, which turns one request
  into several runs whose batch and failure settings no longer span the whole. The same answer
  covers a freeze on one target at the claim: the whole row is deferred past it.
- **Deferring rerun-last and the upgrade's scenario** (user, 2026-10-28) — consent only, never
  deferred. Both start through the runner with `FromLocked`, which a scenario Voyage cannot carry.

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
