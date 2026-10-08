# Scenario settings — the `settings:` block

> **Status: parsed, not acted on.** Specified by the draft ADR
> [`draft-scenario-linear-barrier.md`](../adr/draft-scenario-linear-barrier.md) (NIM-907), fork 4.
> Since NIM-927 the shape (§1), the keys and their values (§2), the §3 refusals — a run-kind key
> below the scenario, `settings:` where it may not be written, an unknown key — and the §2.3
> warning are enforced by the one validator in `shared/config` (the Keeper refuses what `soul-lint`
> refuses); `config.MergeSettings` merges two levels key by key, and render applies it so far only
> from a block to its children. Since NIM-940 §3.1's two refusals are enforced the same way and
> `soul-lint` gives rule 4's warning. Since NIM-942 `serial_scope` decides how a block rolls — but
> only in a run that executes linearly, and no run does until NIM-952 moves the default execution
> model (`execution_model`, [naming-rules.md](../naming-rules.md)). Since NIM-948 a linear run's
> barrier reads `on_failure` and `tolerate` (§4), on the same terms. **Not yet:** the timeouts and
> the `onfail:` rescue across barriers (§4; the rest of epic NIM-926). A Keeper older than NIM-927 refuses the
> `settings:` key outright. Every rule here is a user decision of 2026-09-30 /
> 2026-10-01 unless marked **open**.

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

The scenario top level was a flat list of thirteen keys, none of which governed execution behaviour.
`settings:` is the first such container, so the timeouts and whatever arrives later have one home
instead of a key each.

A block composes its keys by **several** rules, and `settings:` takes the `vars:` one. The authority
is `mergeBlockInheritance` (`keeper/internal/render/block.go:480-492`):

| Key on the block | Against the child's own key |
|---|---|
| `when:`, `where:` | **AND** — one more IF over the group, can only cut off the block or hosts |
| `vars:` | **merged, child wins** |
| `onchanges:`, `onfail:`, `require:` | **union** — which *widens* ([tasks.md §6.5](../destiny/tasks.md)); for `require:`, `all` on either side absorbs the other (`mergeRequire`, `block.go:568-581`). `require:` reaches this merge only in a destiny's block: a scenario refuses it (`require_outside_destiny`) |
| `transport:` | **child wins, whole map** |
| `serial:` | today **the block's width** — a module child's own `serial:` is ignored (`block.go:72`, `:358`). Under this design, a default each child may override — unless the block has `serial_scope: block` (§3.1) |
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
| `tolerate` | barrier | `N` \| `"N%"` | no limit | Counts the hosts dropped under radius `host`: the run ends once **more than** N of them — or more than N% of the run's hosts (§2.1) — have failed in total. A failure under radius `scenario` ends the run without consulting it, so beside `scenario` it is unused, not an error. |
| `soul_timeout` | task | duration | `1m` | How long a lost agent stream may stay lost before the host fails *for unavailability*. |
| `task_timeout` | task | duration, `0` | `0` = no limit | How long one task may execute. A task's own `timeout:` is shorthand for it — §2.3. |
| `scenario_timeout` | run | duration | `24h` | How long the whole run may take. |
| `serial_scope` | block | `task` \| `block` | `task` | What one `serial:` wave carries on a block: one task — each child is its own step — or the whole block, rolled as one group (§3.1). It decides whether the block is one step with one barrier or its children are steps, each with its own, so it is read from the block's own merged settings, not from a barrier owner — and only on a block that has `serial:`, its own or inherited; anywhere else unused, not an error, except that a line written on a task with `serial:` or above it answers §3.1's transition refusal. |

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
from its own barrier on — a barrier where nothing failed included. `N%` is of the run's hosts — **the
hosts the run has targeted with at least one task by the barrier being checked**, dropped ones
included (user, 2026-10-08, NIM-948): every host with a row in the run so far, whether it was sent
the task, skipped it on the Keeper's decision or failed before the send — and **rounds down**, the
direction that stops sooner — so `"10%"` of fewer
than ten hosts is zero, which is radius `scenario`. The set grows with the run: a first step that goes
to one host (`run_once:`) checks `"20%"` against one, so a failure there ends the run, and a step
after the roster grew checks it against more.

⚠ One difference from Ansible, stated so it is not assumed away: Ansible checks `max_fail_percentage`
per `serial` batch, because its `serial` batches the whole play. Here `serial:` batches one task, so
the percentage is over the run.

### 2.2 The three timeouts — never one knob for two meanings

| Key | Governs | Default |
|---|---|---|
| `task_timeout` | how long one task may **execute** | none — Soul applies a limit only when one is written (`soul/internal/runtime/applyrunner.go:1037`) |
| `soul_timeout` | how long a lost **connection** may last before the host is declared unavailable | `1m` |
| `scenario_timeout` | how long the whole **run** may last | `24h` |

Keeper holds a live stream to every daemon agent. When a host's stream has been gone for
`soul_timeout`, the host fails for unavailability and is judged by `on_failure` like any other
failure; a slow task on a live host is never mistaken for a dead one. What this needs, none of it on
the inline path today:

- **A row left `running` by a lost stream must be resolved.** Only a `dispatched` row can be set
  `orphaned` (`keeper/internal/applyrun/crud.go:1100-1108`), so a Soul that dies mid-task leaves its row
  `running`, and nothing ends it short of the whole run aborting, which force-fails it unless an
  operator cancelled (`keeper/internal/scenario/run.go:1557-1580`). And a result finished while the stream was down must still reach
  the Keeper when it returns, or the wait buys nothing.
- **A send to a host whose stream is gone must wait, not fail.** Today the send fails at once
  (`keeper/internal/grpc/outbound.go:150-155`) and the dispatch reports `send_apply_failed`
  (`keeper/internal/scenario/dispatch.go:564-577`); `soul_timeout` has to cover that wait too.
- **The Keeper running the run may not hold the stream.** Under HA the stream can sit on another
  instance (`dispatch.go:595-599`), so noticing the drop needs that instance to say so. A push host
  has no stream at all; what `soul_timeout` means for it is open (§6).

`scenario_timeout` **replaces** today's `defaultRunTimeout` of 5 minutes
(`keeper/internal/scenario/scenario.go:138-141`), whose stated job — "guards against an eternal
barrier (Soul hung)" — `soul_timeout` now does properly. Five minutes also killed any legitimate task
running longer, which contradicted "no task limit by default".

It replaces the provision-run ceiling as well. Today a plan with a refresh emitter gets
`max(5m, max_await_timeout + 10m)` (`keeper/internal/scenario/run.go:1188-1201`, `deployBudget` at
`keeper/internal/scenario/scenario.go:143-152`), and `max_await_timeout` is a `keeper.yml` key — an
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

`timeout:` is refused on a block (`shared/config/scenario_task.go:1110-1117`) and on an applier
(`:1271-1275`), and stays so: NIM-910 made `timeout:` and `retry:` on a block a default for its
children and was reverted (user, 2026-10-07) — a key on the block reading as each child's default is
not obvious. `settings: {task_timeout: X}` on a block or an applier is the default for every task
beneath it once NIM-949 reads it; until then write `timeout:` on the task.

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
   so a child's barrier-kind key — and its `serial:` — is not read, and `soul-lint` warns
   `group_child_key_unread`, for a descendant at any depth.
5. **A run-kind key below the scenario level is refused** (`settings_run_key_below_scenario`), with a
   hint to the task-kind equivalent: `scenario_timeout: 1h` on a task would read as a bound on that
   task and bound nothing. A `task_timeout` larger than the run's `scenario_timeout` is legal — both
   bounds hold, and the smaller one ends the task.
6. **Inside an applier, the destiny's tasks inherit the applier's settings.** A destiny is not a
   scenario and gains no `settings:` key, so its tasks can change none of them — except through their
   own `timeout:`, which is `task_timeout`'s shorthand everywhere (§2.3) and, as the lower level, wins.

**Where `settings:` may not be written:** a keeper-side task — `on: keeper`, or a keeper-side core
address such as `core.state.*` (it has no cross-host barrier, and a
keeper failure always ends the run — `keeper/internal/scenario/run.go:933-936`) —
`settings_on_keeper_invalid`; an `assert:` (it fails the render, not a host) —
`settings_on_assert_invalid`; an `include:` for now (each spliced task is its own barrier, so the
meaning differs from a block's and deserves its own decision; refusing is additive to lift later) —
`settings_on_include_invalid`. A destiny has no `settings:` key at all: written in one, it is
`unknown_key`.

**Two rules for whatever is added next**, so the single validator survives growth: an **unknown key**
inside `settings:` is an error (`unknown_key`), never ignored — that is what makes a new key or a
nested sub-block an additive change, failing loudly on an older Keeper instead of being dropped; and
a new key must **declare its kind** (task, barrier, block or run), which is its whole placement rule —
the kind is a column of the key table in `shared/config/scenario_settings.go`.

**Values.** `on_failure` and `serial_scope` take their enum (`enum_invalid` otherwise); `tolerate` an
integer ≥ 0 or a string `N%` with N in 0..100 (quoting it is customary, not required); the three timeouts a duration string (`30s`, `5m`,
`1h30m`, `2d`). Zero is legal only for `task_timeout` — spelled `0` or `"0s"`, "no limit"; a zero
`soul_timeout` or `scenario_timeout`, and any negative duration, is `value_out_of_range`.

**`timeout:` beside `settings.task_timeout`** (§2.3): when both are written on one task and differ,
`soul-lint` warns `timeout_shadowed_by_task_timeout` on the `timeout:` line, and the Keeper accepts
the file. Under this design the explicit key wins; until task timeouts are read from `settings:`
(NIM-949) the Soul still applies `timeout:`, which is why the warning says both.

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
`examples/service/dragonfly/scenario/restart/main.yml:105-135` is this canary; it was a plain block
with `serial: 1`, and under the default it would restart every replica before checking any, so it
writes `serial_scope: block` since NIM-940, ahead of the switch. Every block with `serial:` — its
own or inherited — and two or more host-dispatched tasks beneath it, counted through nested blocks on the list after
`include:` expansion, must take its
`serial_scope` from a line the author wrote — on the block or on any level above it, either value.
Otherwise the shared validator refuses it, so the Keeper does too, not only `soul-lint`: service
repositories are not linted. The code is `serial_scope_implicit`, reported once, at the outermost
such block; a line written there settles every block beneath it.

**The same change between top-level tasks** (user, 2026-10-06, NIM-940). Today one wave loop carries
a whole Passage of the derived plan at its narrowest `serial:` (`keeper/internal/scenario/dispatch.go:946-957`),
so `{change, check}` under `serial: 2` rolls as a canary, and so does a step *without* `serial:`
written before one with it. Under linear dispatch each step rolls on its own. So a `serial:` that
shares a derived Passage with another host step — in either order — must also take its
`serial_scope` from a line the author wrote, on the task, above it or in the scenario's `settings:`,
either value: neither stretches a wave over the next task. Otherwise it is refused as
`serial_scope_implicit_across_tasks`. Keeper-side tasks share no wave and are not counted, so a
`serial:` task followed by `core.state.<verb>` captures (`long-runner/scenario/serial_waves`) passes.
A block without `serial:` is an overlay whatever its `serial_scope`, so its children are counted as
steps of their own; a module or applier child's own `serial:` is not read inside a block today, so it
carries none, and neither does a block with `serial:` whose host tasks all sit in nested blocks without
one — today's render gives them no width (`keeper/internal/render/block.go`, `renderBlockTask`). The rule reads the derivation, so it goes when the derivation goes, after the service
repositories are audited. The canary of the dragonfly example trips it as well: "Restart the current
master last" (`restart/main.yml:142`) shares the replicas' Passage, so today it restarts in its SID's
place among the width-1 waves — its `serial_scope: block` answers both refusals.

Both refusals run on the plan the author wrote, after `include:` expansion: the Keeper checks before it
synthesizes `core.module.installed` steps from `modules[]`, which `soul-lint` never sees.

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
  when it fails, even if its source failed under `host`. ⚠ **Not built yet (NIM-951):** today a
  dropped host receives no later task, a rescue included, and is recorded SKIPPED there; a run that
  ends runs no later rescue at all.
- If any recorded failure on a host is at a task whose radius is `scenario`, the run ends: a fatal
  failure that happened is fatal whatever else happened on that host. Under `async:` several children
  can fail on one host (`soul/internal/runtime/applyrunner.go:468-480` — siblings are not cancelled),
  and none is lost.
- A failure **tied to no task** — the dispatch itself failed (`send_apply_failed`,
  `keeper/internal/scenario/dispatch.go:564-577`), a push transport failed, or `soul_timeout` ran
  out — is judged by the **barrier owner's** radius. A Keeper-side evaluation error, such as a
  failing `when:`, is tied to its task and judged by that task's radius.
- **The FAILED records the Keeper writes for such failures are not judged again.** A failure with no
  task row, or a group that fails before dispatch, gets a FAILED record for every task its message
  would have carried — a SKIPPED one for a child the Keeper had decided to skip on that host
  ([predicate-rendering.md §1](predicate-rendering.md)) — so that rescue routing and applier
  resolution can see it. Those records are not "recorded failures" for the "any recorded failure"
  rule two bullets up: the failure is judged once — by the barrier owner's radius when it is tied to
  no task, by the child's radius when a group fails before dispatch.
- **A capability refusal is not a host's failure.** A host whose Soul does not announce what the run
  will send it (`soul_capability_unsupported`) ends the run whatever the radius — no host is dropped
  and `tolerate` does not count it. What is known before the run is refused before the first step,
  over the whole plan; what only a step can know, at that step, before it sends anything
  ([draft ADR item 10](../adr/draft-scenario-linear-barrier.md), NIM-946).
- Actively stopping in-flight work when the run ends is a separate, opt-in idea: NIM-911.

**Built in NIM-948, for a linear run** (`keeper/internal/scenario/failure_policy.go`). The barrier
(`Runner.waitStep`) waits for every host a step — or a `serial:` wave of it — was sent to, and
`stepJudge.verdict` judges each failed host by every task it reported failed: the row's first failure
(`failed_plan_index`) and the step's FAILED/TIMED_OUT `task.executed` facts in the audit log, the
source the cross-step requisite gate reads; without an audit reader, the first failure alone. A host
the Keeper failed before sending is judged by its cause alone. The list is `runFailures`, held by the
run goroutine. A send that fails no longer stops the wave: the row is failed `send_apply_failed` and
judged with the rest. A dropped host:

- leaves every later step's roster, a roster re-read at a refresh boundary included;
- stays in `soulprint.hosts`, at its SID position, with `dropped: true` (user, 2026-10-08): all hosts
  are `soulprint.hosts`, the ones still in the run `soulprint.hosts.filter(h, !h.dropped)`. It keeps
  its entries in `register.hosts.<name>`, and a `core.state.<verb>` step after the drop writes as
  usual (user, 2026-10-08) — the incarnation ends `error_locked` either way;
- gets a SKIPPED `task.executed` — "not reached", what a Soul's fail-stop writes for the rest of its
  message — for each later task whose `on:` selects it. Its `where:` is not evaluated: it would read
  registers the host stopped producing. A `run_once:` task covers no dropped host;
- is audited once, as `apply.host_dropped` (`sid`, `step`, `reason`, and `task` when the failure is
  tied to one).

A barrier that would drop the last live host of the run's roster ends the run `failed` (§6). The
roster is the incarnation's hosts the run holds, targeted by a step or not: a run whose steps all go
to one coven of a larger incarnation goes on, as a no-op, after that coven's last host drops, and ends
`partial_failed` — the hosts outside the coven are still in it. A run that reaches
its end without some hosts marks each dropped host's failed row `apply_runs.dropped` (migration 122),
so the run reads `partial_failed`; the incarnation goes `error_locked` with reason `partial_failed`,
and `incarnation.run_completed` says `partial_failed`. A run that ends early for any reason marks
nothing, and its rows fold to `failed`. `failed_plan_index` keeps its public meaning, a row's first
failure (`shared/api/wire/incarnation.go`, read by `keeper/internal/applyrun/runsview.go`).

⚠ **A staged run keeps today's barrier** until NIM-952 retires it: it returns on the first failed
host (`classifyExcept`, `keeper/internal/scenario/dispatch.go`) and the abort force-fails every row
still running (`ensureTerminalApplyRun`, `keeper/internal/scenario/run.go`). A dead host is caught
only on the Acolyte path — which since NIM-961 carries a linear run's step when the work-queue takes
it ([ADR-027 amend (q)](../adr/0027-apply-work-queue.md)), and an inline step does not: only a row
marked `dispatched` — which the claim does — can be set `orphaned` (`OrphanDispatched`,
`keeper/internal/applyrun/crud.go`). A linear run judges such a row, which carries no task, by the
barrier owner's radius, on either path: a queued step's barrier is the inline one's.

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

- **A task that restarts its own agent or host** (§2.3) — its result never arrives.
- **`soul_timeout` for a push host**, which holds no stream (§2.2).

- **A group's nested `serial:` is only warned about** (NIM-940). Inside a block rolled as one group,
  a nested block's own `serial:` narrows today's derived wave (a nested block enters the Passage's
  minimum width) and is not read under linear dispatch, so the wave widens. Rule 4 makes that a
  `soul-lint` warning, `group_child_key_unread`; the Keeper accepts the file. A group exists only
  where an author wrote `serial_scope: block`, which no scenario predating NIM-927 can carry, so the
  change is never inherited unnoticed — but an unlinted repository gets no word of it. Whether it
  becomes a refusal is open.
- **A per-machine `soul_timeout`.** A machine-level value was mentioned before the scenario-level one
  was chosen. A scenario-level value covers the case that motivates it — a scenario that reboots hosts
  needs a longer one — so a machine layer is left for later.

Decided (NIM-948, user, 2026-10-08): the denominator of `tolerate: "N%"` (§2.1); a dropped host stays
dropped across a roster re-read at a refresh boundary, which would otherwise re-admit it while it is
still online; a run left with no live host in its roster ends `failed` at the barrier that would
drop the last one (the recommendation; §4 says which roster); `soulprint.hosts` and `core.state.<verb>` after a drop (§4). Whether
`soulprint.hosts` becomes a map keyed by SID instead of a list read by position is NIM-978.
