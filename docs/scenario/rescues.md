# Rescues — a rescue that succeeds handles the failure

Built in NIM-951 (a rescue reaches its host across steps) and NIM-993 (a rescue that succeeds
handles the failure), for a linear run (`keeper/internal/scenario/failure_state.go`,
`keeper/internal/render/rescue.go`). The design is the
[ADR-0091 amendment of NIM-993](../adr/0091-scenario-linear-barrier.md#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure);
what it has not built yet is listed in
[known-limitations.md](../known-limitations.md#rescues-what-is-not-built-yet).

A **rescue** is a task that is not keeper-side, a `block:` or an `apply:` whose `onfail:` names the
register of another task, its **source**. A rescue names its sources from anywhere in the scenario, and one rescue may name
several. For one source, the **rescue unit** is the outermost construct whose own `onfail:` names
it, with everything inside it: a `block:` with `onfail:` is one unit, not one per child. A unit
**succeeds** on a host when at least one of its tasks ran there and none failed. A unit in which
nothing ran — its `when:` false, every task skipped — has not succeeded.

**Example.** Three hosts, h1 to h3, and the default radius `scenario`:

```yaml
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: /opt/app/deploy.sh }

  - name: reload the balancer
    module: core.exec.run
    params: { cmd: /opt/app/reload-lb.sh }

  - name: roll back
    onfail: [deploy]
    block:
      - name: restore the previous release
        module: core.exec.run
        params: { cmd: /opt/app/rollback.sh }
      - name: check the service
        module: core.exec.run
        register: health
        failed_when: "false"
        params: { cmd: /opt/app/healthcheck.sh }
      - name: not handled while the service is down
        module: core.task.failed
        when: register.health.exit_code != 0
        params: { message: "the rollback did not bring the service back" }

  - name: warm the cache
    module: core.exec.run
    params: { cmd: /opt/app/warm.sh }
```

`deploy` fails on h2.

1. A unit of `deploy` comes later, so h2's failure is **held**. The run does not end, and h2 is not
   dropped. `soulprint.hosts` lists h2 with `held: true`, `dropped: false`.
2. `reload the balancer` goes to h1 and h3. h2 gets a SKIPPED record for it.
3. The three children of `roll back` go to h2 alone, one step each. On h1 and h3, where `deploy`
   succeeded, the Keeper records them SKIPPED and evaluates nothing, so `register.health` is never
   read there.
4. The health check passes on h2. `core.task.failed` is skipped by its `when:`, so the unit has
   succeeded and the failure is **handled**. h2 is back in the run: `warm the cache` goes to all
   three hosts. The run ends `success`, and the incarnation `ready`.
5. If the health check fails instead, `core.task.failed` fails the unit. No other unit of `deploy`
   is left, so `deploy`'s failure is judged at that barrier by `deploy`'s radius, `scenario`. The
   run ends `failed`, its error names `deploy` and the unit's error, and `warm the cache` is not
   sent.

**When a failure is held.** At the barrier of the step where a host failed, the failure is held
when a unit at a later step names the failed task's register, or its applier's, and the `on:` of
that unit, and of every construct around it, selects the host. A host's failures at one step are
held only if all of them are. Otherwise the host is judged at once by all
of them ([settings.md §4](settings.md#4-how-a-barrier-judges-failures)), and its rescues still
reach it, but only as compensation: they cannot return it. In a `serial:` step the later waves go
on while a host is held. A failure tied to no task
([settings.md §4](settings.md#4-how-a-barrier-judges-failures)) is held
the same way when the task its records mark FAILED is a source with a unit ahead.

Never held:

- a host the Keeper failed before sending it a group rolled as one: it is judged at once;
- a keeper-side task's own failure: it still aborts the run at once (`keeper_dispatch_failed`).

**A held host:**

- receives only the units of its held sources. Every other task is recorded SKIPPED for it;
- has no `where:` evaluated for it — the rescue's or an enclosing block's — and no block's
  `run_once:` read, because they would read registers the host stopped producing. A rescue's `when:`
  is settled on the Keeper, as for any host. A `run_once:` rescue task goes to the first host by SID
  it fires on, and decides only that host's failure;
- is listed in `soulprint.hosts` with `held: true` and `dropped: false`. The hosts that receive tasks
  now are `soulprint.hosts.filter(h, !h.dropped && !h.held)`
  ([orchestration.md §4.1](orchestration.md#41-soulprinthosts---list-of-run-hosts-scenario-only-accessor));
- is not counted by `tolerate`, does not end the run, is not audited `apply.host_dropped`, and counts
  as still in the run for "no host is left".

**Units are tried in plan order, and the first that succeeds handles the failure.** A unit that
fails, or does not succeed, handles nothing, and the next unit gets its chance. A unit's own
`on_failure` is not read. The host returns once every failure it holds is handled, from the step
after the unit that handled the last one:

- it gets every later ordinary task, except the rest of a default block whose hosts were chosen at
  its first step while the host was held. It rejoins at the first step after that block;
- the later units of the handled source are skipped on it;
- the steps it missed are not replayed. A later step that reads the register of a missed task
  finds a skipped register, `{changed: false, failed: false, timed_out: false, skipped: true}`, with
  no output fields. For a missed `apply:` with a `register:`, all four are `false`. Reading an output
  field of it fails as for any skipped task, with `no such key`, so guard such a read with
  `!register.<name>.skipped`.

**If no unit succeeds**, the failure is judged at the barrier of the last unit that could reach the
host, by its source's radius. Under `scenario` the run ends there. Under `host` the host is dropped
there, audited `apply.host_dropped` with the source task, and counted by `tolerate`. The entry
names the source task and carries the errors of the units that failed.

**A handled failure is not undone, and it is no entry.**

- The source keeps its FAILED record, and `register.<source>.failed` stays `true`.
- A run whose every failure was handled ends `success`, and the incarnation `ready`. A run that also
  dropped hosts ends `partial_failed`.
- A `failed` or `orphaned` row whose every failure was handled is marked `apply_runs.handled`
  (migration 132). It keeps `failed_plan_index` and `error_summary`, reads `handled: true` on the
  runs API, and counts as no failure in the run's status
  ([operator-api/incarnations.md](../keeper/operator-api/incarnations.md)). A run that commits
  `ready` writes the marks in the same transaction; the other ends write them best-effort, and a row
  left unmarked reads as a failure.

**A rescue that has nothing to rescue is not evaluated.** On a host that holds no failed source of a
unit, the Keeper settles the unit's tasks SKIPPED and evaluates none of their `where:`, `params:` or
`when:`. So a unit task may read an output of its own unit, as `not handled while the service is
down` reads `register.health` above, without failing the hosts where nothing failed. This holds for
every unit with no source before it in its own message. The exceptions are evaluated as before: a
unit inside a group rolled as one that has a source sibling written before it, and a destiny's own
rescues, which the destiny's message decides. What fixes the plan's layout is still evaluated,
without a host: a `loop:`'s items, and an `apply:` unit's host-invariant input. An `apply:` unit
whose static `when:` is false leaves a live host nothing at all, as any static-false `apply:` does.

**A unit task that fails where its unit did not fire is an ordinary failure** — for example a
`when:` that errors on a host where its source succeeded. It is judged like any task, by its own
effective `on_failure`. No failure that no rescue handled is ever swallowed.

**Inside one message a rescue is still compensation.** In a destiny, or in a group rolled as one, a
failure stops the ordinary tasks of the message, the rescues of the failed source still run, and the
message ends `FAILED` even when they succeed ([destiny/tasks.md §8](../destiny/tasks.md#onfail)). The
Keeper then treats that failure as not handled. A rescue at a later step can still handle it.

**Past the end of the run.** A barrier whose verdicts end the run does not abort it at once. The run
walks on to the steps holding a rescue whose source failed at an earlier step (`config.RescueSteps`),
skips every other step, and does not read the roster again at a refresh boundary. Past the end, no
`where:` is evaluated on any host: a rescue goes to the run's hosts that its `on:` chain selects,
where one of its sources failed and no task of its unit has failed yet. A host where the source
succeeded gets nothing from it, not even a record. A step rolled in `serial:` waves goes on after
the wave whose failure ended the run, each later wave sending its hosts their rescues alone. A held
host's rescues still reach it, and one that succeeds handles its failure, which then leaves no
entry, but there is no run left to return to. The run ends after the last rescue's barrier,
`failed`, its error the whole failure list.

**A Cancel ends the run at once**, and no rescue runs. A failure still held then is added to the
run's error.

**A host's failures are fixed at the barrier that judged them.** A result the Soul relays later
(`result_relay`, [settings.md §2.2](settings.md#22-the-three-timeouts--never-one-knob-for-two-meanings)) still reaches the audit log and the host's registers, so a later step's
`register.<source>.failed` can read `true`. The run does not judge it again, and an `onfail:`
naming that source does not fire for it.

**The records of a failure tied to no task**
([settings.md §4](settings.md#4-how-a-barrier-judges-failures)) are written at the barrier that judged it, before
that barrier replays the step's failures.

**The work-queue.** No Keeper instance announces the capability `rescue_hold` yet
([ADR-027 amend (v)](../adr/0027-apply-work-queue.md)), so a run sends steps inline rather than
through the work-queue in two cases:

- some rescue has no source written before it in the same group rolled as one, which is true of
  every scenario rescue outside such a group: every step is sent inline;
- some rescue names a source at an earlier step (`config.RescueSteps`): every step from the run's
  first failure on is sent inline.

Any other scenario queues as before.

⚠ **Not covered:**

- a keeper-side task's own failure still aborts the run at once (`keeper_dispatch_failed`), so no
  rescue of a keeper-side source runs after it;
- a block rolled as one group takes a percentage `serial:` of its live hosts only, though a rescue
  inside it may also reach held or dropped ones;
- in the waves after the one whose failure ended the run, a host whose applier rescue fires but
  whose destiny has no task for it is not sent the terminal, so a later rescue reading
  `register.<applier>` finds none there.
