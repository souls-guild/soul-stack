# dispatch-tour

One small scenario per behaviour of a linear run
([ADR-0091](../../../docs/adr/0091-scenario-linear-barrier.md)): steps and the barrier
between them, the failure radius, `onfail:` rescue, a canary group, a `when:` the Keeper
settles, and the three timeouts of [`settings:`](../../../docs/scenario/settings.md).
Every scenario uses only `core.exec.run`, `core.file.present` and `core.state.set`, so
any host with a Soul runs it.

The scenarios are written for an incarnation of **three hosts**: most of them make one
host fail and show what happens to the other two.

## Running it

1. Three connected Souls. On the dev stand: `make dev-souls-docker SOULS_COUNT=3`.
2. Register the service and create an incarnation of it. The service has no create
   scenario, so creating the incarnation runs nothing.
3. Bind the three hosts: `POST /v1/incarnations/{id}/members`.
4. Run a scenario: `POST /v1/incarnations/{id}/scenarios/{scenario}` or **Run** in the
   web UI, with the scenario's input.

Two inputs pick the host that behaves differently:

| Input | Scenarios | Meaning |
|---|---|---|
| `fail_sid` | `fail_scenario`, `fail_host`, `rescue`, `rescue_host`, `rescue_block`, `canary` | SID of the one host whose deploy (or check) exits 1. Any other host succeeds. |
| `yes_sid` | `when_split` | SID of the one host whose probe answers `yes`. |

A run that fails, or finishes without some of its hosts, leaves the incarnation
`error_locked`; unlock it before the next run. The routes are in
[operator-api/incarnations.md](../../../docs/keeper/operator-api/incarnations.md).

## The scenarios

### Steps

| Scenario | What a run does |
|---|---|
| `steps` | Four top-level host tasks are four steps. Each step starts only after every host finished the one before, and the run records one result per host per step. `write marker` puts each host's own SID into `/tmp/dispatch-tour-marker`, and `read marker` reads it back. |
| `many_steps` | Twelve `true` commands. The barrier wakes on the write of the last host's result, not on a poll, so steps follow each other in milliseconds: twelve steps over three docker Souls took about 0.8 s on the stand it was demonstrated on. |

### Failure radius

| Scenario | `settings:` | What a run does |
|---|---|---|
| `fail_scenario` | none, so `on_failure: scenario` | `deploy` fails on the `fail_sid` host and the run ends `failed` after that step. `after deploy` reaches no host. |
| `fail_host` | `on_failure: host` | The same failure drops only the `fail_sid` host. The other two finish `after deploy`, and the run ends `partial_failed`. |

### Rescue

| Scenario | `settings:` | What a run does |
|---|---|---|
| `rescue` | none | `deploy` fails on the `fail_sid` host and the run ends, yet `rollback` (`onfail: [deploy]`) still runs on that host, and only there. `ordinary step` runs nowhere. |
| `rescue_host` | `on_failure: host` | The dropped host gets only its `rollback`. The other two run `ordinary step` and no rollback. |
| `rescue_block` | none | `onfail:` written once on a block reaches each of its three children: `stop service`, `restore config` and `start service` run as three steps on the `fail_sid` host only. |

### Canary

`canary` rolls `upgrade` and `check` as **one group**, one host at a time
(`serial: 1`, `settings.serial_scope: block`): the first host gets both, then the
second, then the third. The `check` fails on the `fail_sid` host, which stops the roll:
hosts after it are not touched. Without `serial_scope: block` the block is not one
group: it rolls by task, `upgrade` on every host first and `check` after
([settings.md §3.1](../../../docs/scenario/settings.md)).

### Decided on the Keeper

| Scenario | What a run does |
|---|---|
| `when_split` | The probe answers `yes` only on the `yes_sid` host. The Keeper settles each `when:` from the registers before it sends anything: `only where probe says yes` runs on that host and is `SKIPPED` on the other two, and `only where the previous step changed nothing` runs on the other two. A skipped host is sent nothing. |
| `keeper_mid` | A host step, a keeper-side `core.state.set` of `marker`, a host step. The capture runs between the two host steps, where it is written, and `incarnation.state.marker` ends up `set-after-host-work`. |
| `render_fail` | The second step reads `register.probe.no_such_key`, which the probe never produced. The first step finishes on every host; rendering the second fails, and the run ends between the two steps. |

### Results

| Scenario | What a run does |
|---|---|
| `failed_when` | `true` succeeds, and `failed_when: register.self.exit_code == 0` turns the task `FAILED` with code `flowcontrol.failed_when`. The error names the task, not the predicate. |

### Timeouts

| Scenario | `settings:` | What a run does |
|---|---|---|
| `task_timeout` | `task_timeout: 10s` | A 30 s sleep is cut after about 10 s: the task ends `TIMED_OUT` with code `task.timed_out`. |
| `task_timeout_override` | `task_timeout: 10s` | The task's own `timeout: 60s` wins, and the 30 s sleep passes. |
| `soul_timeout` | `soul_timeout: 1m`, `on_failure: host` | Kill one Soul during the 120 s sleep (`docker kill`). About a minute later that host fails as unavailable (`soul_unavailable`) and leaves the run; the other two finish. A graceful stop (`docker stop`, `systemctl stop`) shows something else: the Soul cancels its apply and reports it at once as `apply.cancelled`. `1m` is also the default: the scenario writes it to show the key. |

## Tests

Each scenario has L0 cases under `scenario/<name>/tests/`
([testing.md](../../../docs/destiny/testing.md)), run by `make trial`. L0 renders the plan
once, against a roster of fixture hosts and with no live one, so a case can check:

- the plan in order, and each task's rendered params;
- which branch of `fail_sid` / `yes_sid` a host gets — one case on the host the input
  names, and for `fail_scenario`, `canary` and `when_split` one on another host;
- the effective `timeout` of a task (`task_timeout`, `task_timeout_override`), its
  `failed_when` text and its `onfail:` sources (the three `rescue` scenarios);
- the `when:` a task carries, the render error of `render_fail`, and the final state of
  `keeper_mid`.

It cannot check anything the barrier decides: step order across hosts, the failure
radius, `partial_failed`, who receives a rescue, which hosts a `when:` skips, the canary
wave, and every timeout firing. The difference between `fail_scenario` and
`fail_host`, or `rescue` and `rescue_host`, is invisible to L0: their L0 plans are the
same.
