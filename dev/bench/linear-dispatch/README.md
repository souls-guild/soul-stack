# Linear-dispatch benchmark (NIM-950)

Measures what the [draft ADR on linear dispatch](../../../docs/adr/draft-scenario-linear-barrier.md)
§Price states in advance — item 14 of its "Before merge" list — by running one scenario under both
execution models on the **same commit**, on a fleet of real Souls. The model is picked per Keeper
start through `keeper.yml::scenario.execution_model` ([config.md](../../../docs/keeper/config.md#scenario)).

## What runs

- **Scenario** — [`service/scenario/bench/main.yml`](service/scenario/bench/main.yml): twelve cheap
  top-level tasks (`core.exec.run true`), no register reference between them, two of them reading a
  secret through `vault()`. ADR-056 stages it as one Passage (one message per host); the linear
  model makes it twelve steps. `TestBenchFixture_TwelveStepsOnePassage` holds that shape.
- **Fleet** — the stand's docker Souls (`dev/souls-docker-up.sh`, real `soul run` daemons in
  Debian-12 systemd containers), one incarnation per fleet size (`bench-h1`, `bench-h4`, `bench-h8`),
  plus a **push host**: an sshd container (`push-host.Dockerfile`) bound to incarnation `bench-push`,
  reached with `transport: ssh` through the `soul-ssh-static` provider (pinned in
  `scripts/plugin-source.sh`). Keeper dials it as `127.0.0.1:<2222+offset>` — the SID is the
  address, since `ssh_target` has no address column.
- **Matrix** — `execution_model` × `acolytes` (`0`: the run goroutine sends each step inline; `>0`:
  the step goes through the work-queue, NIM-961) × added delay × target. Each cell runs one
  discarded warm-up and `BENCH_REPS` measured runs, strictly one at a time. The push host is
  measured with `acolytes: 0` only: under `acolytes>0` the one-Passage staged fixture with a push
  host is refused before dispatch (`dispatch_failed`), and a linear step with one stays inline
  whole (`keeper/internal/scenario/dispatch.go` `queuesStep`).
- **Delay** — `tc netem delay <d>ms` on every bench container's egress, so the Keeper↔Soul round
  trip grows by `d`. The measured TCP connect time Soul→Keeper is recorded per delay.

## How each number is derived

Everything comes from what `bench.sh` saves per run under `<out>/<commit>/<cell>/<rep>/` — the run's
`apply_runs` rows and its audit timestamps (`run.json`), and metric snapshots taken before and after
it — and `report.py` recomputes the tables from that directory alone.

| Number | Source |
|---|---|
| wall | `incarnation.run_completed` minus `incarnation.scenario_started` (audit `created_at`; the start row is written after the API answered, so both models lose the same few ms) |
| steps, msgs/host | distinct `apply_runs.passage`; rows per host — one row is one ApplyRequest↔RunResult round trip |
| lead / step / gap / tail | `apply_runs.started_at` (row written, just before the send) and `finished_at` (result stored): start→first dispatch, dispatch→slowest host done, step done→next dispatched, last done→terminal |
| work | next dispatch minus the first barrier tick that could see the step done: the ticker (`keeper/internal/scenario/scenario.go` `defaultPollInterval`, 200 ms) starts once the wave is sent, taken as the step's last row insert — Keeper's own inter-step work. Not meaningful for the push host, whose send starts in a goroutine after an audit write, so the ticker starts later than the row. Printed only for a commit before NIM-984: from it the barrier wakes on the write of the row (`meta.json` `barrier_wakes`), and `gap` is the work itself |
| rt / queue | per row `finished_at − started_at`; on the queue path `claim_at − started_at` |
| soul_run | Δ`soul_apply_duration_seconds` sum/count on each Soul — the Soul's `Run` per message |
| outside | `rt − queue − soul_run`: network, the Soul's work around `Run`, Keeper's handling of the result |
| renders, render ms | Δ`keeper_render_duration_seconds` count/sum — one Render call per pass |
| Vault reads | read requests for the bench secret in a Vault file audit device — exact, unlike the Keeper metric, which also counts its background reads |
| SSH sessions | Δ`Accepted publickey` in the push host's sshd journal |

A run whose recorded `execution_model` differs from its cell, or that did not end `success`, is
excluded and listed above the tables: the first would mean the configuration never reached the run.
A queue cell with any row that carries no `claim_at` had messages sent inline and is marked †.

## Bring-up

A **dedicated** stand (own Postgres/Redis/Vault; the benchmark enables a Vault audit device and
restarts its Keeper). Every command from the repository root; none may inherit a Vault address or
token from the shell.

```bash
export DEV_STAND=nim950 DEDICATED_INFRA=1
H=$(hostname -I | awk '{print $1}')
env -u VAULT_ADDR -u VAULT_TOKEN make dev-up
env -u VAULT_ADDR -u VAULT_TOKEN DEV_KEEPER_EXTRA_IP=$H make dev-provision
env -u VAULT_ADDR -u VAULT_TOKEN bash dev/keeper-run.sh
env -u VAULT_ADDR -u VAULT_TOKEN DEV_KEEPER_EXTRA_IP=$H make dev-provision
env -u VAULT_ADDR -u VAULT_TOKEN VAULT_TOKEN=root bash -c '. dev/stand-env.sh >/dev/null && ./keeper/bin/keeper init \
    --archon=archon-alice --config="$STAND_DEV_DIR/keeper.dev.yml" --credential-out="$STAND_DEV_DIR/archon-alice.jwt"'
make build-linux
env -u VAULT_ADDR -u VAULT_TOKEN make bench-linear-dispatch
```

`setup` is idempotent; `BENCH_*` variables (header of `bench.sh`) narrow the matrix. Results land in
`/tmp/keeper-dev-<slug>/bench-linear-dispatch/<commit>/results.md`.

## Limits

- One machine: Keeper, Postgres, Redis, Vault and every Soul share 24 cores, so a fleet of 8 adds
  CPU contention a real fleet does not have. The delay is emulated; the base round trip is the
  Docker Desktop bridge (~2 ms).
- The tasks are deliberately cheap — the case §Price names as the one where per-message cost may
  dominate. Real tasks dilute every fixed cost measured here.
- One Keeper instance; HA behaviour is NIM-961's e2e, not this benchmark.
- Every duration taken from `apply_runs` and the audit log is a difference of Postgres `NOW()`
  stamps (`soul_run` and render come from metric deltas). On WSL2 that clock steps: in the
  published run it went back by ~110–125 ms inside 24 runs, 9 of them with impossible negative
  intervals. Medians absorb a step; p90 and single gaps do not.
- Before NIM-984, a step close to a 200 ms tick boundary landed on either side of it, so a cell whose steps ran
  ~200 ms (the push host) moved by a tick per step on a small shift in the step itself: push steps
  of median 220 ms ran 4.9 s, and in another Keeper start (`acolytes: 2`, where they also went
  inline) steps of median 200 ms ran 3.2 s.
- The push host is bound by a direct `incarnation_membership` row: the members API binds only a
  `connected` host, and a push host never streams.
