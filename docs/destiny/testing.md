# Testing destiny

> **Tool form is fixed in [ADR-023](../adr/0023-trial-test-runner.md)** (2026-05-22): runner - **Trial**, binary `soul-trial` (separate artifact in module `keeper`, `keeper/cmd/soul-trial`). Two metrics: **`trial coverage`** (DSL - tasks/CEL branches/`enum`/`state_changes`) and **`code coverage`** (Go). Levels **L0 render-only + L1 migration - MVP**; **L2 single-host (docker) - implemented test-only** (Option A, build-tag `integration`, see ADR-023); **L3 multi-host - deferred**. DSL-coverage hook - `CoverageSink` to `shared/cel` to `Engine.EvalExpression`. ADR-023 closed open Q No. 1/6/7/8 below; with the implementation of L2, **No. 4** (real-only) and **No. 5** (idempotency with opt-out) are closed, **No. 3** (sandbox) is closed **partially** (single-host docker; multi-host `stand:` for L3 remains open). Forks and solutions - `.pm/tasks/2026-05-22-testing-framework/`.

Testing destiny on an ephemeral stand with coverage measurement. The layout and format of test cases are fixed (see below); details of L0-fixtures/assert and L2/L3-sandbox are being worked out.

This is **not** part of `soul-lint`. According to [ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) `soul-lint` is strictly offline and static (does not execute); `soul-trial` - run. The dividing line: "does not perform → soul-lint, performs → soul-trial."

## Intent (rude)

- Each destiny comes with a set of test cases: input parameters, expectations on `output` / state of the host, possibly a sequence of several runs with different actions.
- By default, execution is on an ephemeral stand in docker: a container with a real `soul` binary and core modules + a sandbox container as the "target host". Per-test isolation.
- Tasks and expressions `when` are instrumented - a trace is collected: which tasks were executed, which conditions were triggered, which modules were called.
- Based on the results of the run, a coverage report is published. The goal is 100% coverage; a hole is a reason to add a test case.
- Visualization in UI Keeper (when it appears - see open Q "UI Keeper" in [architecture.md](../architecture.md)) - a list of destiny / service / roles with coverage numbers and highlighting of uncovered tasks and branches.

## Test case layout next to destiny

The format is fixed: tests for destiny live in the `tests/` subdirectory next to `destiny.yml`. One test - one folder with an arbitrary human-readable name; entry point is `case.yml`. This selection closes open Q #2 (see below).

```
destiny-<name>/
├── destiny.yml
├── tasks/
│   └── main.yml
├── templates/
└── tests/                              # all tests of this destiny
    ├── install-and-ping/               # one test case
    │   ├── case.yml                    # required entry point
    │   ├── prepare.yml                 # OPT.: tasks before the main apply
    │   ├── verify.yml                  # OPC: if there are a lot of assertions, put them here
    │   └── cleanup.yml                 # OPTS: tasks after verify (external resources)
    └── failover-restart/
        └── case.yml
```

A working example with explanations for each block is in [examples/destiny/redis/tests/install-and-ping/case.yml](../../examples/destiny/redis/tests/install-and-ping/case.yml).

### What `case.yml` declares

The minimal case describes four things:

1. **`stand:`** - ephemeral environment (`driver` / `image` / `mode: push` / `init`). **Single-host format closed** (open Q No. 3 partially): ephemeral docker-container per-case, `mode: push` (Keeper-side render → oneshot `soul apply` in container). Image digest pin (`image: name@sha256:…`) is allowed and encouraged (reproducibility). The network is enabled by default (needed for `core.pkg`), disabled explicitly `stand.network: none`. The container init system type is set by `stand.init` (see below). **Multi-host `stand:` (cluster topology) - remains open** (extension for L3, propose-and-wait on topology format - see open Q No. 3 below).
2. **`input:`** — values ​​of the destiny parameters for the run. There is no need to specify the name destiny: the framework takes it from `../../destiny.yml`, next to which are `tests/`.
3. **`expect_idempotent:`** - double pass flag, **default `true`** (open Q No. 5 is closed: idempotency is required with opt-out). Second apply the same `input` → all `register.changed == false`; otherwise the case will fall. A case with a consciously non-idempotent step disables the check with an explicit `expect_idempotent: false`.
4. **`verify:`** — checking the result.

### `stand.init` — init system of the stand container

The optional field `stand.init` specifies what runs as PID 1 inside the L2 stand. Enum, default `none`:

| Meaning | What gives |
|---|---|
| `none` (or no field) | Lightweight container without init: process `sleep infinity` as PID 1, `soul apply` runs on top. Suitable for most L2 cases - packages, files, exec checks. This is the previous behavior, a default. |
| `systemd` | Container with real `systemd` as PID 1 (`/sbin/init`, `privileged` + `cgroupns=host` + tmpfs on `/run` and `/run/lock`). Need L2 cases that really pull `systemctl` and units. |

**When `systemd` is needed.** Only if the case tests behavior that requires a live init system:

- `core.service.restarted` with `daemon_reload`: overwritten the unit file (`core.file.present`) → `systemd` sets `NeedDaemonReload=yes` → the restart must pick up the new unit definition;
- `enable`/`start` real services (not stub processes) and checking `systemctl is-active` / `is-enabled`;
- systemd timers and other units whose status is asked via `systemctl show`.

For everything else, `init: systemd` is redundant - it takes `privileged` and starts harder, so `none` defaults.

**Requirements and skip.** Mode `systemd` runs **only** under build-tag `integration` and requires `docker` + `privileged`. In an environment without them (or without Docker at all), the case does **skip, not fatal** - failure to run is not considered a regression (the default `make test` marks such cases `Skipped`, as well as the entire L2 form). The stand image reuses the common **debian-12 systemd profile** (same `debian-12.Dockerfile` as e2e-live): the stand is built from this Dockerfile with `Entrypoint /sbin/init`. The `image` field with `init: systemd` is required according to the scheme, but is actually ignored (the stand is taken from the Dockerfile) - the reference tag for readability is indicated in the case.

Brief example of a bench with real systemd:

```yaml
stand:
  driver: docker
  image:  debian:12-slim    # is required by the scheme; at init: systemd is actually ignored
  mode:   push
  init:   systemd
```

The entire working case (proves `daemon_reload=auto`: rewritten unit → `NeedDaemonReload` → restart applies v2) - [`examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload/case.yml`](../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload/case.yml).

### Verify - through the same destiny as in production

Verify block is structured as a sequence of tasks, each of which executes a destiny/module and compares the fields of their `output:` with those expected through the `expect:` block (see working example [`examples/destiny/redis/tests/install-and-ping/case.yml`](../../examples/destiny/redis/tests/install-and-ping/case.yml) - `verify:` with `expect: { stdout_contains: PONG }`, etc.). There is **no** separate DSL assertions - this is a conscious decision:

- If the host state is checked through `redis-cli ping` or `systemctl is-active`, we write this explicitly through `core.exec.run` and check `output.stdout`.
- If the check repeats what destiny itself can do (`action: ping`, `action: replication_status`) - reuse it. Self-consistent verification: destiny is both an implementation and part of a contract.
- If you need a special "file_present" / "service_running", this means that the core modules lack the corresponding `state` (or its `output:` fields), and the problem should be solved there, and not duplicated in a separate DSL for tests.

The specific set of keys `expect:` (`stdout`, `stdout_contains`, `exit_code`, `output_equals`, ...) is not yet **fixed** - this intersects with **open Q No. 8** (what exactly to measure). At the example level we use at least `stdout` / `stdout_contains` / `exit_code`; the complete list will be recorded separately, before the implementation of the framework begins.

### Optional neighbors

All three files next to `case.yml` are optional; the case runs without them:

| File | When needed |
|---|---|
| `prepare.yml` | preconditions to main `input` apply: raise Vault-stub with test secrets, download fixture files, deploy dependent service in side container. |
| `verify.yml` | if there are a lot of assertions, we remove them from `case.yml` and connect them as `verify: !include verify.yml` (the exact syntax of include is the backlog test framework; the expression template engine is fixed [ADR-010](../adr/0010-templating.md), [`docs/templating.md`](../templating.md)). |
| `cleanup.yml` | tasks after verify are needed if the case has enclosed external resources (S3 objects, DNS records, real cloud-VMs via cloud-driver). When driver=docker is usually not needed - demolishing the container destroys everything. |

### Where tests do NOT live

- **service-level `tests/*.yml`** (smoke/system-test after successful `incarnation.create`) is a **different** entity: run on real Souls in a real incarnation, not on an ephemeral stand. The folder name `tests/` is intentional. The sample services currently use **script level** tests - `scenario/<name>/tests/<case>/case.yml` (L0 tests with `fixtures:`, see [`examples/service/redis/scenario/create/tests/`](../../examples/service/redis/scenario/create/tests/)) - and migration tests (`migrations/<NNN>_to_<MMM>/tests/`).
- **Coverage-metrics and run-tool** are not part of the test files. Tests are declarative; what is collected from the run and in what form is stored - a separate layer (open Q No. 6, No. 8).

### Three levels of tests: destiny-molecule vs scenario-test vs service-smoke

After [ADR-009](../adr/0009-scenario-dsl.md), scenario has its own test mechanism - this is a **third** entity, separate from the two above:

| | destiny-molecule | scenario test | service-smoke |
|---|---|---|---|
| **What does it test** | one destiny on one host | one script operation on a cluster topology | running incarnation after `create` |
| **Layout** | `destiny-<name>/tests/<case>/case.yml` | `scenario/<name>/tests/<case>/case.yml` | `service-<x>/tests/<name>.yml` (flat) |
| **Stand** | ephemeral, **single** host | ephemeral, **multi-host** (topology) | real Souls, real incarnation |
| **Asserts** | `output:`/state host, idempotency | + topology (who is master/replica), `incarnation.state` after commit | smoke/system-checks |
| **Format `case.yml`/`verify:`/`expect:`** | this document | **reuses** this format, `stand:` extended to multi-host | flat `tests/<name>.yml` |

Format `case.yml` / `verify:` / `expect:` for scenario testing **reused from here** without separate DSL assertions. Delta scenario test (multi-host `stand:`, topology assertions and `incarnation.state`) - in [`docs/scenario/orchestration.md §6`](../scenario/orchestration.md#6-two-level-resource-resolution).

> **Open Q extension #3 (sandbox).** Single-host `stand:` (destiny-molecule, L2) **closed** (ephemeral docker, `mode: push`). Multi-host `stand:` for scenario test and topology assertions/`incarnation.state` cross-host - **extension** for L3, remains open (topology format - propose-and-wait), and not a closed solution. Does not close silently; before solving multi-host scenario cases - declarative-stub `stand:`.

### Case.yml format levels: L0 vs L2

In `case.yml`, **two forms** coexist, corresponding to different test levels ([ADR-023](../adr/0023-trial-test-runner.md)). They differ in the composition of the keys:

| Form | Level | Composition | Status |
|---|---|---|---|
| `fixtures:` (input/essence/soulprint/vault/state + `default_destiny_source` for apply:destiny) / `mocks:` / `assert.{rendered_tasks,state_changes,state_after}` | **L0** (render-only, hermetic, single-host sugar) | fixtures for the render pipeline + expected task plan / delta sets / final `incarnation.state` | **MVP, executing** `soul-trial run` |
| `fixtures.hosts: [...]` (roster of N hosts) instead of `fixtures.soulprint` + the same `assert.*` | **L0** (render-only, hermetic, multi-host roster) | list of run hosts → `soulprint.hosts`-projection / `.where(...)` / `size()` / nodes-determinism / `state_changes` on multi-host | **closed** (amendment 2026-06-22, ADR-023), in progress `soul-trial run` |
| `stand:` / `verify:` / `expect:` (+ `input:` without `fixtures:`) | **L2** (docker single-host, real modules; opt. `stand.init: systemd` - stand with real systemd as PID 1) | ephemeral stand + checks via `core.exec.run` | **implemented test-only** (build-tag `integration`); prod-`soul-trial run` it **skips** (`Skipped`) to a separate task prod-CLI-enable |
| `assert.dispatch` + committed cross-host `incarnation.state` + multi-host `stand:` | **L3** (multi-host dispatch/orchestration) | who is the real master, cross-host committed state, stand topology | **postponed** (Phase 3 ADR-027 + multi-host `stand:`, open Q #3) |

`assert.state_after` — L0 section: expected final `incarnation.state` = base `fixtures.state` + rendered `state_changes.sets` (product commit mirror), full reconciliation (an extra key in the end is a discrepancy). `assert.dispatch` - L3 section (meant only on multi-host), the L0 loader rejects it with a strict decode.

#### L0 multi-host roster — `fixtures.hosts`

`fixtures.soulprint` (single map) describes a **single** synthetic run host - this is sugar for single-host L0 cases (most destiny-molecules). For multi-host render-invariants (`size(soulprint.hosts)`-guard, `soulprint.hosts.where(...)`-projection, nodes-determinism master/replica), the case specifies **roster** via `fixtures.hosts` — a list of host records:

```yaml
fixtures:
  hosts:
    - sid: node-1.example.com
      covens: [prod, redis]          # real stable tags only (no incarnation.name); membership = presence in fixtures.hosts (see below)
      role: primary                  # opt: declared-role (bootstrap-create)
      soulprint:                     # opt: per-host facts (soulprint.self.* of this host)
        network: { primary_ip: 10.0.0.1 }
        os:      { family: debian }
      choirs: [voters]               # optional: Choir membership (ADR-044)
    - sid: node-2.example.com
      covens: [prod, redis]
      role: replica
      soulprint:
        network: { primary_ip: 10.0.0.2 }
```

Host record fields:

| Key | Obligation | What sets |
|---|---|---|
| `sid` | yes | Host SID (FQDN). The roster order in `soulprint.hosts` is determined by sorting by `sid` (regardless of the order in YAML). |
| `covens` | no | Host Coven Tags — **real stable tags only** (cluster / project / environment / datacenter), **not** the incarnation name (`incarnation.name` is no longer a Coven, [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). Used only to model `on: [coven]` intersection filters. **Membership** is modeled by **presence in `fixtures.hosts`** (this list IS the run roster = the members), not by any coven tag — an empty `covens` host is still a member. |
| `role` | no | Declared role (`soulprint.hosts[].role`), available only in bootstrap-create (ADR-008). |
| `soulprint` | no | Per-host facts (`soulprint.self.<path>` of this host and `soulprint.hosts[].network/os`). Missing → empty fact context. |
| `choirs` | no | Host Choir names (`soulprint.hosts[].choirs`, ADR-044). |

`fixtures.soulprint` and `fixtures.hosts` **mutually exclusive** - both in the same case → strict decode error (in the spirit of a strict harness decode, like unknown-key/`assert.dispatch`). Single-host cases continue to use `fixtures.soulprint` without changes.

**Level boundary.** `fixtures.hosts` remains **L0 render-only**: Keeper-side render-plan and `soulprint.hosts`-projections on the roster are checked, without actual per-host execution. Which host actually executes the master (`assert.dispatch`), committed cross-host `incarnation.state` after the barrier and multi-host docker `stand:` - **L3, deferred** (ADR-023 amendment 2026-06-22).

The L2 form is executed **in Go tests of the keeper module** (testcontainers, oneshot `soul apply`, in-process Keeper-side render - the same `renderCase` as L0). Examples with `stand:`/`verify:` (e.g. [`redis/tests/install-and-ping/case.yml`](../../examples/destiny/redis/tests/install-and-ping/case.yml)) are marked in the header `# L2 fixture (docker stand)`; prod-`soul-trial run` skips them (`Skipped`), not taking non-passing as a regression. The L0-form is the current pilot of the framework and is always executed.

#### L0-molecule for standalone destiny - scenario wrapper `apply:destiny`

`soul-trial` L0 renders **scenario** (harness is tied to `scenario/<name>/main.yml`), and not standalone destiny directly. For a reusable (standalone) destiny L0-molecule is a **minimal scenario wrapper with a single task `apply:destiny`** for the same destiny. destiny is not rendered in a separate way: the single invocation point (scenario `apply:` / Keeper) is preserved in tests.

The canonical wrapper layout is a subdirectory of destiny itself, so that each destiny has its own L0-harness:

```
destiny-<name>/
├── destiny.yml
├── tasks/
├── templates/
└── _trial/                                   # L0-harness of this destiny
    ├── service.yml                            # announces destiny[]: [{name: <name>, ref}] - sales mirror
    └── scenario/
        └── apply/
            ├── main.yml                       # scenario: one task apply:destiny <name>
            └── tests/
                └── render-defaults/
                    └── case.yml               # L0-form: fixtures (+ default_destiny_source) + assert.rendered_tasks
```

**Resolve `apply:destiny` in L0 mirrors the product model** ([slice A](../scenario/orchestration.md), [ADR-023](../adr/0023-trial-test-runner.md)), and does not iterate through directories using heuristics:

1. **`apply:destiny`-name → `service.yml::destiny[]`.** The name must be declared in `destiny[]` of the case service (for a standalone wrapper - its `_trial/service.yml`). **An undeclared dependency is rejected** with the same error as in production (`apply:destiny` only references a declared dependency, ADR-007).
2. **URL → sealed `file://`.** `case.yml::fixtures` carries the key **`default_destiny_source`** (same name as `keeper.yml`) - URL template with scheme `file://` and substitution `{name}`, path relative to the service-root of the case (e.g. `file://../../destiny/{name}` for cross-location, `file://destiny-{name}` for destiny within the service). Per-entry `destiny[].git` override defeats the template, but L0 must also have `file://`. Any non-`file://` circuit → obvious error "L0 is sealed." `{name}` must live in the last segment of the path; The securejoin boundary is at destiny-root (the part of the URL before `{name}`), so `{name}` will not break through `../` beyond the declared directory of destiny.

The previous wording "the resolver goes up a couple of levels to the destiny directory" has been removed: the heuristic search `[serviceRoot, parent, grandparent]` did not reflect the product and lied on the cross-location layout (service and standalone destiny in different subtrees). Working examples - [`examples/destiny/node-exporter/_trial/`](../../examples/destiny/node-exporter/_trial/scenario/apply/main.yml), `redis-exporter/_trial/`, `service-reload/_trial/` (cross-location: service-root = `_trial/`, destiny itself is a higher level in `examples/destiny/`); cross-location service → [`examples/service/redis/`](../../examples/service/redis/service.yml) (destiny in `examples/destiny/`). Launch: `soul-trial run examples/destiny/<name>/_trial`.

## Ideas for coverage metrics (also for development)

Rough list - what is *probably* worth measuring, composition revised during design:

- **Task coverage** — each task destiny (`tasks:` element) is completed in at least one case.
- **Branch coverage by `when`** - for each expression, both truthy- and falsy-results are collected (or all branches, if this is a switch by enum).
- **Enum-value coverage** — each value of the `enum:` input parameter is used in at least one case. Complements static check No. 1 in [soul-lint.md](../soul-lint.md): statics says "the literal in the expression is legal", runtime says "the value was actually tested on the bench".
- **Module coverage** — each custom module from `required_modules:` was called at least once in at least one of its state forms. If not, either the entry is redundant or there is an uncovered scenario. (Core modules are not declared in `required_modules:` - see ["Addressing modules"](../architecture.md).)
- **Output coverage** - each field declared in `output:` of some task is actually assigned in at least one run.

## Open questions

Everything needs to be decided **before** implementation.

1. **Tool form and name.** Separate binary? Subcommand `keeper`? Part of the tuling around `soul`? The name is proposed-and-wait and is not fixed silently.
2. ~~**Test case format.** Separate YAML next to destiny vs built-in section inside destiny.~~ **Closed:** separate files, layout `destiny-<name>/tests/<case-name>/case.yml` (+ opt. `prepare.yml` / `verify.yml` / `cleanup.yml`). See the section "Test case layout next to destiny" above.
3. **Sandbox container.** **Single-host - CLOSED** (L2 Option A, [ADR-023](../adr/0023-trial-test-runner.md)): ephemeral docker-container per-case, `stand: {driver, image, mode: push}`; image digest-pin is allowed/encouraged (tightness), network is enabled by default (needed for `core.pkg`), `stand.network: none` is an explicit opt-out. **L0 render-multi-host (`fixtures.hosts`) - CLOSED** (amendment 2026-06-22, ADR-023): a roster of N hosts is driven by a standard harness, render invariants are checked (`size(soulprint.hosts)`-guard, `soulprint.hosts`-projection, nodes-determinism, `state_changes`) - this hermetically sealed render layer, without docker and without dispatch. **Multi-host `stand:` (L3-dispatch) - REMAINS OPEN:** docker stand on the topology, `assert.dispatch` (who actually executes the master) and assertions on committed `incarnation.state` cross-host - extension for L3, fixed separately through propose-and-wait according to the topology format, **does not close silently** (see. [`docs/scenario/orchestration.md §8`](../scenario/orchestration.md#8-open-questions-extensions-do-not-close-silently)).
4. ~~**Real vs mock modules.**~~ **Closed: real-only.** L2 runs real modules in docker; the fast "mock mode" is not introduced by a separate entity - its role is played by **L0 render-only** (assert on the task plan / state without a host). One mock level (L0), one real level (L2), without a third intermediate one.
5. ~~**Idempotency.**~~ **Closed: required with opt-out.** `expect_idempotent` defaults to `true` - the second apply of the same `input` must give all `register.changed == false`. A case with a consciously non-idempotent step disables the check with an explicit `expect_idempotent: false`.
6. **Storage coverage.** Postgres (part of Keeper) vs CI artifact only vs both. Determines whether history and comparison between wounds are available in the UI.
7. **CI gate.** Default coverage threshold (warn / fail) and its scope (per destiny / per service / globally).
8. **What exactly to measure.** The list of metrics above is a starting point, not a final set.

## Dependencies

- The expression template engine is fixed [ADR-010](../adr/0010-templating.md), the normative spec is [`docs/templating.md`](../templating.md): instrumentation `when:` is based on CEL (the same engine as static checks `soul-lint`).
- Open Q "UI Keeper" - determines whether coverage visualization will appear as part of the release.
- Related to static checks `soul-lint` (see [soul-lint.md](../soul-lint.md)): static and runtime-coverage are different layers, both are needed. Statics catches dead literals and typos; runtime catches uncovered scripts.

## See also

- [concept.md](concept.md) - what is destiny.
- [tasks.md](tasks.md) - `tasks/main.yml` format that is being tested.
- [input.md](input.md) - destiny-`input:`, the values ​​of which are passed to `case.yml`.
- [`docs/scenario/orchestration.md`](../scenario/orchestration.md) - scenario tests: multi-host `stand:`, topology assertions/`incarnation.state`.
