# Scenario settings — the `settings:` block

> **Status: design, not implemented.** Specified by the draft ADR
> [`draft-scenario-linear-barrier.md`](../adr/draft-scenario-linear-barrier.md) (NIM-907), fork 4.
> Nothing below is accepted by today's parser — an older Keeper refuses the `settings:` key outright.
> Every rule here is a user decision of 2026-09-30 / 2026-10-01 unless marked **open**.

A scenario declares how its own run behaves — what a host's failure kills, how many failed hosts it
tolerates, how long it waits for a lost agent, a task, and the whole run — in one `settings:` block.
That behaviour is a property of the scenario, versioned in git and reviewed with it. **Nothing at run
time overrides it, in either direction**: the operator has no say. (The Voyage `--on-failure`
flag — `soulctl/internal/cmd/run_scenario.go:133-134` — is a different thing: it decides whether a
Voyage starts its *next incarnation*, not what happens inside one run.)

## 1. Shape

One block, **one schema, one validator**, writable at every level: the scenario itself, a top-level
task, a `block:`, and a child at any depth inside a block. Each level's `settings:` is merged **key
by key** over the level above — the `vars:` rule, so a child overrides only what it names and may move
a value in either direction.

```yaml
name: converge

settings:                       # scenario level: the default for everything below
  on_failure: scenario          # host | scenario
  tolerate: "10%"               # read only where the effective radius is `host`
  soul_timeout: 1m              # a lost agent stream is waited for this long
  task_timeout: 0               # 0 = no limit
  scenario_timeout: 24h         # the whole run

tasks:
  - name: probe replication lag
    module: core.exec.run
    register: lag
    settings:
      on_failure: host          # a failed probe drops that host, the run goes on

  - name: rolling config update
    settings:                   # set once for the whole group
      on_failure: host
    block:
      - name: render config
        module: core.file.rendered
      - name: migrate schema
        module: core.exec.run
        settings:
          on_failure: scenario  # but a failed migration ends the run
          task_timeout: 30m
```

The scenario top level is a flat list of thirteen keys today (`shared/config/scenario.go:27-85`), none
of which governs execution behaviour. `settings:` is the first such container, so the timeouts and
whatever arrives later have one home instead of a key each.

A block composes its keys by **several** rules, and `settings:` takes the `vars:` one. The authority
is `mergeBlockInheritance` (`keeper/internal/render/block.go:280-291`):

| Key on the block | Against the child's own key |
|---|---|
| `when:`, `where:` | **AND** — one more IF over the group, can only cut off the block or hosts |
| `vars:` | **merged, child wins** |
| `onchanges:`, `onfail:`, `require:` | **union** — which *widens* ([tasks.md §6.5](../destiny/tasks.md)); for `require:`, `all` on either side absorbs the other (`mergeRequire`, `block.go:367-380`) |
| `transport:` | **child wins, whole map** |
| `serial:` | today **the block's width** — a module child's own `serial:` is ignored (`block.go:65`, `:228-236`). Under this design, a default each child may override — unless the block has `serial_scope: block` (§3.1) |
| `settings:` | **merged key by key, child wins** — the `vars:` rule |

## 2. The keys

Every key belongs to one of **four kinds**, and the kind alone decides where it may be written:

| Kind | Read from | Writable on |
|---|---|---|
| **task** — about one task on one host | that task's merged settings; for a failure tied to no task, the barrier owner's (§4) | every level |
| **barrier** — about the hosts gathered at one barrier | the barrier owner's merged settings | every level that is a step or contains one — the scenario, a top-level task, a `block:`, an applier, a child of a default block; on a child of a group it is unread and `soul-lint` warns (rule 4) |
| **block** — about how a block rolls | the block's own merged settings | every level; read only on a block |
| **run** — about the run as a whole | the scenario's settings | the scenario level only |

The **barrier owner** is the step whose barrier gathers the hosts — a step as the ADR's decision 1
defines it.

| Key | Kind | Values | Default | Meaning |
|---|---|---|---|---|
| `on_failure` | task | `host` \| `scenario` | `scenario` | What a host's failure at this task kills. `scenario` — the run ends. `host` — that host leaves the run, the rest continue; a run that finished without some hosts ends `partial_failed`, the incarnation `error_locked`, and a Voyage counts it as failed (user, 2026-10-02; the ADR's item 4). |
| `tolerate` | barrier | `N` \| `"N%"` | no limit | Counts the hosts dropped under radius `host`: the run ends once **more than** N of them — or more than N% of the run's hosts (§6) — have failed in total. A failure under radius `scenario` ends the run without consulting it, so beside `scenario` it is unused, not an error. |
| `soul_timeout` | task | duration | `1m` | How long a lost agent stream may stay lost before the host fails *for unavailability*. |
| `task_timeout` | task | duration, `0` | `0` = no limit | How long one task may execute. A task's own `timeout:` is shorthand for it — §2.3. |
| `scenario_timeout` | run | duration | `24h` | How long the whole run may take. |
| `serial_scope` | block | `task` \| `block` | `task` | What one `serial:` wave carries on a block: one task — each child is its own step — or the whole block, rolled as one group (§3.1). It decides whether the block is one step with one barrier or its children are steps, each with its own, so it is read from the block's own merged settings, not from a barrier owner — and only on a block that has `serial:`, its own or inherited; anywhere else unused, not an error. |

### 2.1 `on_failure` and `tolerate` — the Ansible model

The two keys reproduce Ansible's, deliberately, so they read the way an operator already expects:

| Here | Ansible | Behaviour |
|---|---|---|
| `on_failure: scenario` | `any_errors_fatal: true` | **any** failure on **any** host ends the whole run |
| `on_failure: host` | `any_errors_fatal: false` | a failed host leaves the run; the others go on |
| `tolerate: N` / `"N%"` | `max_fail_percentage` | the run ends once failed hosts **exceed** the threshold |

The value names what a failure **kills** rather than instructing the engine, and its vocabulary is
deliberately disjoint from [ADR-043](../adr/0043-voyage.md)'s `abort`/`continue`: a Voyage decides
whether to start the next incarnation, where going on is usually right; this decides whether to keep
changing *this* one after part of it failed, where stopping is usually right. With disjoint values, a
Voyage setting can never be misread as a barrier setting.

**`tolerate` counts distinct failed hosts across the whole run** — not per task and not per block. A
host fails at most once (it leaves the run), so the count only grows, and twelve tasks losing one host
each are twelve hosts against the threshold, not one per task. The count is checked **at every
barrier**, against that barrier owner's merged `tolerate`: a task may tighten or loosen the threshold
from its own barrier on. `N%` is of the run's hosts — which set that is, is open (§6) — and **rounds
down**, the direction that stops
sooner — so `"10%"` of fewer than ten hosts is zero, which is radius `scenario`.

⚠ One difference from Ansible, stated so it is not assumed away: Ansible checks `max_fail_percentage`
per `serial` batch, because its `serial` batches the whole play. Here `serial:` batches one task, so
the percentage is over the run.

### 2.2 The three timeouts — never one knob for two meanings

| Key | Governs | Default |
|---|---|---|
| `task_timeout` | how long one task may **execute** | none — Soul applies a limit only when one is written (`soul/internal/runtime/applyrunner.go:1035`) |
| `soul_timeout` | how long a lost **connection** may last before the host is declared unavailable | `1m` |
| `scenario_timeout` | how long the whole **run** may last | `24h` |

Keeper holds a live stream to every daemon agent. When a host's stream has been gone for
`soul_timeout`, the host fails for unavailability and is judged by `on_failure` like any other
failure; a slow task on a live host is never mistaken for a dead one. What this needs, none of it on
the inline path today:

- **A row left `running` by a lost stream must be resolved.** Only a `dispatched` row can be set
  `orphaned` (`keeper/internal/applyrun/crud.go:971-979`), so a Soul that dies mid-task leaves its row
  `running`, and nothing ends it short of the whole run aborting, which force-fails it unless an
  operator cancelled (`keeper/internal/scenario/run.go:1361-1372`). And a result finished while the stream was down must still reach
  the Keeper when it returns, or the wait buys nothing.
- **A send to a host whose stream is gone must wait, not fail.** Today the send fails at once
  (`keeper/internal/grpc/outbound.go:150-155`) and the dispatch reports `send_apply_failed`
  (`keeper/internal/scenario/dispatch.go:336-340`); `soul_timeout` has to cover that wait too.
- **The Keeper running the run may not hold the stream.** Under HA the stream can sit on another
  instance (`dispatch.go:358-362`), so noticing the drop needs that instance to say so. A push host
  has no stream at all; what `soul_timeout` means for it is open (§6).

`scenario_timeout` **replaces** today's `defaultRunTimeout` of 5 minutes
(`keeper/internal/scenario/scenario.go:136-139`), whose stated job — "guards against an eternal
barrier (Soul hung)" — `soul_timeout` now does properly. Five minutes also killed any legitimate task
running longer, which contradicted "no task limit by default".

It replaces the provision-run ceiling as well. Today a plan with a refresh emitter gets
`max(5m, max_await_timeout + 10m)` (`keeper/internal/scenario/run.go:996-1023`, `deployBudget` at
`keeper/internal/scenario/scenario.go:141-150`), and `max_await_timeout` is a `keeper.yml` key — an
operator's value bounding a scenario's run, which the opening of this document rules out.
**Decided (user, 2026-10-01):** that floor goes; `scenario_timeout` is the only bound on a run, and its 24-hour default
already covers onboarding.

### 2.3 `timeout:` is shorthand for `task_timeout`

Every task already has a `timeout:` key, in a scenario and in a destiny alike, and it stays — a task
is a task wherever it is written. In a scenario, `timeout: X` on a task **is** `settings:
{task_timeout: X}` on that task, so it inherits and overrides by the same merge. Both spellings are
allowed, for convenience; when one task writes both and they differ, **the explicit
`settings.task_timeout` wins** and `soul-lint` warns. In a destiny, which has no `settings:`,
`timeout:` remains the only spelling.

The rest of the block is reached through `settings:` itself — a slow link is the plain case:

```yaml
- name: sync the mirror over the WAN link
  module: core.exec.run
  params: { cmd: rsync, args: [-a, "mirror::repo/", /srv/repo/] }
  timeout: 2h                 # the command itself
  settings:
    soul_timeout: 5m          # the link drops for minutes at a time — do not fail the host for it
```

⚠ **A task that restarts its own agent or reboots its own host is not covered.** Its result is never
reported — the process that would report it is gone — so no value of `soul_timeout` makes
`systemctl reboot` succeed. That needs its own design (Ansible has a dedicated `reboot` module for
exactly this); §6.

`timeout:` is refused on a block today (`shared/config/scenario_task.go:1104-1111`) and on an
applier (`:1263-1266`). Lifting it on a block is NIM-910's amendment, not this document's;
`settings: {task_timeout: X}` on a block or an applier needs no such lift, and is the default for
every task beneath it.

## 3. Rules

1. **Inheritance merges key by key**, scenario → task or block → child → nested child. Replacing the
   whole block would make a child that names one key silently reset every other one.
2. **Any level may change any task-kind key in either direction.** The author owns every line.
3. **`on_failure: scenario` beside `tolerate` is not an error** — `tolerate` is unused. Whether it is
   used is decided on the **merged** settings of each task, after every level is applied, not on what
   one level wrote.
4. **A barrier-kind key on a block's child depends on how the block rolls** (ADR fork 3). In a
   **default** block each child is its own step with its own barrier, so the key is read like
   anywhere else. In a block **rolled as one group** the children share one message and one barrier,
   so a child's barrier-kind key — and its `serial:` — is not read, and `soul-lint` warns.
5. **A run-kind key below the scenario level is refused**, with a hint to the task-kind equivalent:
   `scenario_timeout: 1h` on a task would read as a bound on that task and bound nothing.
6. **Inside an applier, the destiny's tasks inherit the applier's settings.** A destiny is not a
   scenario and gains no `settings:` key, so its tasks can change none of them — except through their
   own `timeout:`, which is `task_timeout`'s shorthand everywhere (§2.3) and, as the lower level, wins.

**Where `settings:` may not be written:** a keeper-side task — `on: keeper`, or a keeper-side core
address such as `core.state.*` (it has no cross-host barrier, and a
keeper failure always ends the run — `keeper/internal/scenario/run.go:791-793`); an `assert:` (it
fails the render, not a host); an `include:` for now (each spliced task is its own barrier, so the
meaning differs from a block's and deserves its own decision; refusing is additive to lift later).

**Two rules for whatever is added next**, so the single validator survives growth: an **unknown key**
inside `settings:` is an error, never ignored — that is what makes a new key or a nested sub-block an
additive change, failing loudly on an older Keeper instead of being dropped; and a new key must
**declare its kind** (task, barrier, block or run), which is its whole placement rule.

### 3.1 A block rolled as one group — the canary

By default a `block:` is an overlay: each child is its own step, and the block only supplies defaults.
A **canary** — change and check two hosts, then the next two — needs the steps to travel together, so
a block can ask to roll as **one group** with `serial_scope: block`: one message per host, one wave
loop, `serial:` widening the whole group at once.

```yaml
- block:
    - name: upgrade redis
      module: core.pkg.installed
      params: { name: redis, version: "7.2" }
    - name: check redis answers
      module: core.exec.run
      params: { cmd: redis-cli, args: [ping] }
      failed_when: register.self.stdout != "PONG"
  serial: 2
  settings:
    serial_scope: block
```

With `serial_scope: block`, hosts 1–2 get "upgrade + check" before 3–4 are touched, and a failed check
stops the roll at two hosts. With `task`, the default, `upgrade` reaches all ten in pairs before
`check` runs once. Written at the scenario level, `serial_scope: block` makes every block with
`serial:` a canary.

**The default was kept after this evidence** (user, 2026-10-01; the ADR's fork 3):
`examples/service/dragonfly/scenario/restart/main.yml:104-132` is this canary written as a plain block
with `serial: 1`, and under the default it would restart every replica before checking any. It gets
`serial_scope: block` in the change that switches the default, and every block with `serial:` — its
own or inherited — and two or more host-dispatched tasks beneath it, counted through nested blocks on the list after
`include:` expansion, must take its
`serial_scope` from a line the author wrote — on the block or on any level above it, either value.
Otherwise the shared validator refuses it, so the Keeper does too, not only `soul-lint`: service
repositories are not linted.

## 4. How a barrier judges failures

Each failed host is judged by the effective `on_failure` of **the task it failed at**, resolved
through its inheritance chain. Failures go into an **append-only list, closed when the run ends**:

- Once a failure **ends the run** — radius `scenario`, or `tolerate` exceeded — no new ordinary task
  is dispatched to anybody. Under radius `host` a failure only drops that host: it receives no new
  ordinary task, and the run goes on for the rest. **Hosts already working are not stopped** —
  telling every agent to stop at once is costly — they finish, and every failure they report is
  appended. Two hosts failing in different parts of the fleet are two entries.
- **`onfail:` is the exception to both** (the ADR's fork 1). A host whose task failed still receives
  every `onfail:` task whose source failed on it, at that task's position, whether it was dropped or
  the run is ending. The run ends after the last such rescue's barrier, and only then is the failure
  list closed. A rescue's own failure is judged like any task's, by its own effective `on_failure`:
  under `scenario` it is one more entry — the host leaves two errors, the source's and the
  rescue's; under `host` the host, already out, stays out and the log records that it left. The
  radius is the rescue's own: a rescue left at the default `scenario` ends the run for every host
  when it fails, even if its source failed under `host`.
- If any recorded failure on a host is at a task whose radius is `scenario`, the run ends: a fatal
  failure that happened is fatal whatever else happened on that host. Under `async:` several children
  can fail on one host (`soul/internal/runtime/applyrunner.go:466-478` — siblings are not cancelled),
  and none is lost.
- A failure **tied to no task** — the dispatch itself failed (`send_apply_failed`,
  `keeper/internal/scenario/dispatch.go:336-340`), a push transport failed, or `soul_timeout` ran
  out — is judged by the **barrier owner's** radius. A Keeper-side evaluation error, such as a
  failing `when:`, is tied to its task and judged by that task's radius.
- **The FAILED records the Keeper writes for such failures are not judged again.** A failure with no
  task row, or a group that fails before dispatch, gets a FAILED record for every task its message
  would have carried — a SKIPPED one for a child the Keeper had decided to skip on that host
  ([predicate-rendering.md §1](predicate-rendering.md)) — so that rescue routing and applier
  resolution can see it. Those records are not "recorded failures" for the "any recorded failure"
  rule two bullets up: the failure is judged once — by the barrier owner's radius when it is tied to
  no task, by the child's radius when a group fails before dispatch.
- Actively stopping in-flight work when the run ends is a separate, opt-in idea: NIM-911.

⚠ **Almost none of this exists today.** The barrier returns on the first failed host
(`dispatch.go:426-429`) and the abort force-fails every row still running
(`keeper/internal/scenario/run.go:1361-1372`) on the assumption that no result will arrive — false
for a live host. The only failure record per host is one first-arrival slot, `failed_plan_index`
(`keeper/internal/applyrun/crud.go:425-451`), which the barrier does not even read: its one reader,
`failedPlanIndex` (`dispatch.go:537-549`), has no callers. A dead host is caught only on the Acolyte
path, which linear runs do not take: only a row marked `dispatched` — which the claim does,
`claim.go:211` — can be set `orphaned` (`crud.go:971-979`). The
audit log does record every task failure, and the cross-passage gate already reads it
(`run.go:808`).

## 5. Naming

The YAML key is `settings:`. The word also names the Keeper's cluster-wide settings — `SettingsStore`,
the `keeper_settings` table, `GET /v1/settings`, `setting.read`
([naming-rules.md](../naming-rules.md), rows at lines 122, 407, 1009-1011) — but those are different
settings in a different area and never share a file. The concept enters the dictionary as **scenario
settings**, Go type `ScenarioSettings`, with a row stating it is unrelated to `SettingsStore`.

A cluster-wide default in the Keeper's runtime config ([ADR-0073](../adr/0073-keeper-runtime-config-pg.md))
is **not** adopted: the same scenario would behave differently on two clusters with nothing in the
scenario saying so.

## 6. Open

- **Which hosts are "the run's" for `tolerate: "N%"`.** The roster is re-read at every refresh
  boundary (`keeper/internal/scenario/run.go:655-662`): it grows during provisioning and can lose an
  offline host with no failure recorded. Recommended: the distinct hosts the run has dispatched at
  least one task to, counted at the barrier being checked.
- **A dropped host must stay dropped across that re-read** — the ADR's item 5. A refresh boundary
  would otherwise re-admit a host `on_failure: host` removed, if it is still online.
- **A task that restarts its own agent or host** (§2.3) — its result never arrives.
- **`soul_timeout` for a push host**, which holds no stream (§2.2).

- **A per-machine `soul_timeout`.** A machine-level value was mentioned before the scenario-level one
  was chosen. A scenario-level value covers the case that motivates it — a scenario that reboots hosts
  needs a longer one — so a machine layer is left for later.
- **A run with every host dropped** — the next step's host map is empty, a no-op
  (`dispatch.go:66-73`). Recommended: it ends as failed.
