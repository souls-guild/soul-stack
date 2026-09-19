# ADR-0088. `transport:` on the task — a polymorphic key, and the task beats the registry.

**Status:** accepted. Owner's decision 2026-09-14 (NIM-870), on the NIM-866 reconnaissance. Amends [ADR-009](0009-scenario-dsl.md) (the orchestration delta gains a key) and [ADR-032](0032-push-orchestrator.md) (the 3-tier provider resolve gains a level above it — that ADR's "3-tier" and its `decision_source` cardinality of 3 are superseded by the four levels below; see the amendment note there).

**Context.** Push mode delivers a destiny over SSH ([ADR-004](0004-binaries.md), [ADR-032](0032-push-orchestrator.md)) and, since NIM-869, actually works — a live test drives a real `soul apply` on a real host. What the DSL had no way to say was *which* way a task should travel. The NIM-866 reconnaissance offered two forms and recommended the second:

- **A — a flag on the task.** Reads directly, visible in the scenario. Against it: the platform had been moving transport decisions OUT of tasks (`side:` into the module manifest, NIM-749), and `core.ssh.run` records the principle in so many words — "which way an installation reaches its hosts is a property of the installation, not of one task".
- **B — the transport declares the target.** `souls.transport` already exists and is already the branch point the dispatcher checks. Against it: the scenario stops saying out loud what is happening, and a minting step that writes `transport=ssh` + `ssh_target` from a creation register does not exist on any module.

**Decision (A, with the form settled).**

1. **`transport:` lives at the TASK level**, beside `on:` / `where:` / `serial:` / `run_once:` in the orchestration delta — **not** inside `apply:`. The level is deliberate: the key is about delivery, so over time it covers a `module:` task and not only an applier.

2. **The key is polymorphic — a scalar or a map, and the map is keyed by the TRANSPORT'S NAME.**

   ```yaml
   transport: ssh
   transport: { ssh: { ssh_provider: "vault-bastion" } }
   ```

   **Exactly one key** in the map; two is `transport_multiple`. Scalar-or-map is already the idiom here (`on:` is `"keeper" | []`, `require:` is `[] | "all"`, `serial:` is `int | "50%"`); the decoder is `config.TransportSpecOf`, modelled on `RequireSpecOf` so the two forms cannot drift apart. The scalar and the map-with-no-params decode identically — the scalar IS the map with no params.

3. **The value space is a closed enumeration** — `agent` (the gRPC EventStream, what a task without the key gets) and `ssh` (the push flow). Closed so `soul-lint` judges the key **offline**: with an open namespace a typo is indistinguishable from a transport this build does not carry. Adding one is a deliberate act — `TestTransportRegistry_IsClosed` fails until the params table and the doc row move with it.

4. **The `ssh` transport takes `ssh_provider`, `user`, `port` — and NO address.** Those are exactly the three fields a task may take away from `souls.ssh_target`. An address parameter is refused by omission, because `ssh_target` has no address column (its `Host` is the SID) and a key that could name one would be read as bootstrapping a bare VM, which it does not do.

5. **The task wins** over `souls.ssh_target` and over the cluster defaults in `keeper.yml::push.*`. Chosen by the owner over "the task says only what the registry has no column for" and over "the registry wins". In the push router this is a Level 0 above the existing three, with `source: task`.

6. **★ The effective values are part of the decision, not a nicety.** (5) knowingly creates a THIRD source of truth for provider/user/port. The price is paid by writing what actually answered into `push_runs.summary.hosts[]`: `ssh_provider`, `route_source ∈ {task, soul, coven, cluster}`, `transport` whenever the task NAMED one (the scalar `transport: ssh` included, which names a transport and overrides nothing), and `ssh_user`/`ssh_port` when the task set them. A summary with no `transport` key is a run no task routed. The converse is deliberately not claimed: a missing `ssh_user` means the RESOLVER answered, and only the resolver knows whether that was the `souls.ssh_target` row or its own `root`/`22` default — a label there would be a guess in the one field whose job is provenance.

**Four deliberate refusals.**

- **A keeper-side task refuses the key** (`transport_on_keeper_invalid`). This is the question NIM-866 left open to the implementation. A keeper task never leaves the Keeper, so there is no host at the far end and no transport to name; the side is derived from the module address ([ADR-0087](0087-task-side-derived-from-module-address.md)), so the refusal fires with no `on:` written — a rule keyed on `on: keeper` would miss the half that ADR records as the `when:` hole. `core.ssh.run` keeps its own `ssh_provider`/`hosts` params and its direct/teleport mode keeps coming from `keeper.yml::push.transport`: that module dials hosts itself, and one word covering two different things is worse than two words.
- **Interpolation is refused** (`transport_interpolation_unsupported`). The key is decided ONCE PER TASK, before hosts are resolved — it lands on the dispatch plan, not on a per-host rendered task — so there is no env to resolve `${ … }` against and no answer that could differ per host. A per-host transport is a different design; until it exists, the alternative to refusing is shipping the literal `${ vars.x }` to the dialer as a provider name.
- **A key that is WRITTEN but does not decode fails the RUN**, rather than falling back to the registry — the transport's shape as well as a param's value. The key exists in order to beat the registry; answering from the registry after failing to read it is the silent wrong answer the whole audit requirement above is about. Offline validation refuses every such shape, so a linted scenario never reaches this; `pushorch` takes the raw DSL value from its own API and can.

- **It is refused inside a destiny.** A destiny is rendered per host and shipped whole to the ONE transport the scenario task that applies it chose, so a second name there has nothing to act on. `serial:`/`run_once:`/`on:` are refused in that position for the same reason.

A fifth case is a refusal without being a rule: `transport: agent` inside a **push** run is a contradiction — the run *is* the ssh transport — so `pushorch` fails it rather than dispatching over a transport the author explicitly did not name.

**What this does NOT do — stated because the opposite prose has already misled a reconnaissance once.**

It does not bootstrap a bare VM. The host, its address and its provider still come from the registry: `core.bootstrap.issued` writes `transport='agent'` as a literal and refuses `ssh`, and `souls.ssh_target` carries no address at all. Every item of the NIM-866 work list stands.

**It had no end-to-end production path when this ADR was written, and said so. Both halves were closed by NIM-880** ([ADR-0089](0089-scenario-push-branch.md)) — the scenario dispatcher grew a push branch, and `POST /v1/push/apply` / `keeper.push.apply` carry a `transport` field. The paragraph is kept rather than deleted because it is the record of what "half a feature" looked like from the inside, and because the two bullets are still the shape of the question: *who sets this key, and by what path does the value travel*.

**Amendment 2026-09-14 (NIM-880): the key does not move the BRANCH.** Which way a host is reached is its own `souls.transport`; a task naming the other one is refused (`transport_mismatch`), not obeyed. The three fields the key carries — `ssh_provider`, `user`, `port` — still beat `souls.ssh_target` and `keeper.yml::push.*`, which is the whole of the Level 0 decided here. What the key never claimed and now explicitly may not do is retype a host: the address, the credentials and the existence of a push target are registry facts, and a key that could invent them would be the bare-VM bootstrap this ADR rules out one paragraph above. The key therefore states the transport out loud — offline-checkable, visible to a reader of the file — and carries the overrides; the registry picks the branch, and the two must agree.

**Rejected alternatives.**

- `apply: { destiny: X, ssh: true }` — the reconnaissance's own sketch. Rejected with the level: it binds the key to the applier, and the key is meant to cover a `module:` task too.
- Variant B, "the transport declares the target" — the reconnaissance's recommendation. Rejected by the owner: it removes the scenario's ability to say what is happening, and the work list is the same either way.
- An open transport namespace resolved at runtime — rejected: it costs the offline check, which is the whole reason the enumeration exists.
- A per-host `transport:` resolved through CEL — deferred, not rejected. It needs a host-independent root set and a place on the rendered task rather than the plan.

**Block inheritance.** On a `block:` the key is an inherited default, merged into each descendant by `mergeBlockInheritance` alongside `when:`/`where:`/`vars:`/requisites. The descendant's own key wins, and because the merge happens per level rather than at the dispatch plan, the **nearest** enclosing block wins on nesting. The two maps are not merged: a block's `{ ssh: { user: x } }` beside a child's `{ ssh: { ssh_provider: y } }` is the child's statement whole, because the map is a discriminator carrying one transport's parameters and half-inheriting them would describe a connection neither line wrote.

**Guards.** `shared/config/scenario_transport_test.go` (the two forms decode identically; the one-key rule; the closed registry; the keeper-side refusal), six `soul-lint/testdata/scenario-broken/scenario-transport-*` fixtures with a golden counterpart (the OFFLINE half), `keeper/internal/render/transport_plan_test.go` (the plan seam, block inheritance, child-wins), and `TestIntegration_PushRun_LiveSSHD_TaskTransportBeatsTheRegistry` — a real host with both overridden sources seeded WRONG in ways that each kill the run alone, so a green run is evidence the task won them.
