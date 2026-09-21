# ADR-0089. The scenario dispatcher's push branch, and what `register:` does on it

**Status:** active (NIM-880)
**Supersedes nothing. Amends:** [ADR-032](0032-push-orchestrator.md) (roster, dispatch), [ADR-0088](0088-task-transport-key.md) (the key now has a production path)

## Context

[ADR-0088](0088-task-transport-key.md) (NIM-870) added `transport:` to a scenario task:
a closed enumeration, refused offline by `soul-lint`, with the task's
`ssh_provider`/`user`/`port` beating `souls.ssh_target` and `keeper.yml::push.*`. It was
live-proven against a real sshd.

Nothing in production set it. The scenario dispatcher had exactly one implementation of
[`scenario.ApplyDispatcher`](../../keeper/internal/scenario/scenario.go) — the gRPC
stream — and `POST /v1/push/apply` had no field for the key. Writing it was possible;
travelling by it was not. Half a feature, and the half that showed was the finished-looking
one.

Two facts made the gap airtight rather than merely unwired:

- `push.SshDispatcher.SendApply` refuses a host whose `souls.transport` is not `ssh`, so
  "push to an agent host" was never an option;
- a `transport: ssh` host is `pending` for its entire life (`connected` is written by the
  agent's Bootstrap RPC and by nothing else), and the scenario roster excluded `pending`
  outright. So the branch could not have been reached by any registry state an operator
  can produce.

And one question [ADR-0032](0032-push-orchestrator.md) left explicitly open, which any
push branch has to answer before it ships: **`register:` rides on `TaskEvent`, `RunResult`
has no field for it, and `apply_task_register` has a foreign key to
`apply_runs(apply_id, sid, passage)` — a row a bare push run never writes.** A scenario
task executed over push would therefore leave `register.<name>` unresolved and any
`require:` over it unreleased. Silently: the barrier would go green having waited for
nothing.

## Decision

### 1. A scenario run dispatches over push, and the HOST picks the branch

`dispatchPassage` resolves a per-host transport before any send. The effective transport
is the host's own `souls.transport`; a task's `transport:` may not contradict it, and
naming the other one fails the run with `transport_mismatch`.

This is not a weakening of "the task beats the registry". The three things that key
overrides — the SshProvider, the account, the port — still beat `souls.ssh_target` and
the cluster defaults, which is the entire Level 0 of ADR-0088. What the key may not do is
retype a host: the address, the credentials and the existence of a push target are all in
the registry row, and a key that could invent them would be a key that bootstraps a bare
VM, which ADR-0088 explicitly is not.

Four further refusals. ★ WHERE each is raised is part of the decision, because a refusal
discovered at a host's turn aborts a run whose earlier hosts have already executed their
ApplyRequests. None of them is raised there — but they are not all knowable at the same
moment, and the honest split is:

- `push_not_configured` is a property of the PROCESS — a pull-only Keeper has no push
  dispatcher for the whole life of the run — so it is settled ONCE against the starting
  roster, before the first Passage and before any keeper-side task. That ordering is
  load-bearing: `core.cloud.provisioned` runs among those tasks, and refusing afterwards
  would leave a run that could never be applied holding the machines it had just created.
  The per-Passage pass keeps the same check for a host that joins at a refresh boundary
  (ADR-0061 §S3).
- `transport_mismatch` — the task names a transport the host's registry row does not
  carry, or targets a host the roster does not hold.
- `transport_disagreement` — two tasks of one Passage describe two different dispatches
  for one host. A host receives ONE ApplyRequest per Passage (composite PK
  `apply_id, sid, passage`), so there is nothing to split, and first-wins would dispatch a
  task down a line its own file does not name. ★ The comparison is the whole decision, not
  the transport's NAME: `transport: ssh` beside `transport: { ssh: { user: deploy } }` is
  one transport and two connections, and letting the later one win would drop a Level 0
  override with no diagnostic — the same defect the name check exists to prevent, one
  field over.
- `provider_not_routed` — no level of the ADR-032 router produced an SshProvider and the
  task named none. It cannot join `push_not_configured` in the up-front pass: the task's
  `transport: { ssh: { ssh_provider: … } }` is Level 0, so which provider a host gets is a
  property of the Passage's plans, not of the process.

The last three are properties of a PASSAGE's rendered plans, and a staged Passage's
targets are placeholders until the Passage before it has run. They are raised before that
Passage's first wave — the earliest point at which they are facts. ⚠ **So a staged run can
apply Passage 0 and then refuse Passage 1**, and this ADR says so rather than rounding it
up to "before anything ran".

Only the tasks that survived the cross-passage requisite gate vote on a host's transport.
A task the gate dropped for a host runs nowhere there, so letting it disagree would abort
a run over a line that is not being dispatched.

### 2. ★ A scenario push run MINTS an `apply_runs` row and fills `register:` through the same path

This is the open question, answered.

The row is not a new obligation: the cross-host barrier polls `apply_runs`, so a push host
that is waited on has one by construction. Once it exists the foreign key is satisfied, and
the decision reduces to which code writes the accumulator. It is the SAME code: both
branches feed one [`applysink.Sink`](../../keeper/internal/applysink), which the EventStream
handler was refactored onto in the same change. A Soul emits byte-identical protobuf whether
it was reached over the stream or exec'd over SSH, so "the register fills the same way on
both transports" is a property of the code rather than a promise about two implementations.

It follows that on a push run the register, the per-task failure reason, the run's
notices, the `task.executed` audit events (which the changed-task rollup and the
cross-passage `onchanges`/`onfail` gating read back out) and the operator SSE frames all
behave exactly as on the stream.

**A bare `POST /v1/push/apply` run still fills nothing, and that is also decided.** It
writes `push_runs`, mints no `apply_runs` row, has no scenario around it to read a
`register.<name>` back and no barrier to release. Minting a row for it would invent an
incarnation it does not belong to. The seam says so in one place: the orchestrator passes
a nil [`push.EventHandler`](../../keeper/internal/push/ndjson.go), the scenario branch
passes a live one.

The rejected alternative was to keep the branch and **refuse** a barrier over a push
register. It is strictly worse: the refusal would have to be discovered by the author at
run time, the two transports would differ in the DSL they accept for no reason a scenario
author can act on, and the transition later — when someone did mint the row — would break
every scenario written around the refusal.

### 3. The scenario roster admits `transport: ssh` members

A second query, [`rosterPushSQL`](../../keeper/internal/topology/resolver.go), beside the
agent one. The two are disjoint by a `transport` clause rather than by a dedup pass, and
the agent predicate is unchanged: `pending` means "a bootstrap token is issued and no
identity exists yet" there, and targeting such a host is meaningless whatever the dispatch
path can do. On the push side `pending` is the only status a host ever holds.

The split also fixes the row the single predicate got wrong in the other direction: an ssh
host left at `status='connected'` (the agent→ssh migration nothing writes yet) used to pass
the agent filter and reach a stream dispatch it cannot answer.

### 4. A push host is exempt from the two announcement gates

The plan-requirement gate (ADR-0076(i)) and the staged passage gate (ADR-056 §S5) both ask
a presence source what a host announced on Hello. A push host opens no stream and announces
nothing, so asking returns "lacking every capability" and would reject every run that
touched one.

For the PLAN-REQUIREMENT gate the exemption stands on its own: what runs on a push host is
the binary this Keeper delivers from `push.soul_binary_path`, so the operator's answer to
"which soul is on that host" is a `keeper.yml` key rather than an announcement, and a
module the binary does not carry fails that host's run loudly.

★ For the PASSAGE gate that argument does NOT hold, and pretending it did would have
shipped silent data corruption. With `push.soul_binary_path` unset the run execs whatever
is already at the target's `soul_path`; a binary that echoed `passage: 0` under a Keeper
passage of 1 would have `applysink` key its register and failure reason on that echo while
the terminal is keyed on the Keeper's own number. The Passage-1 failure would land its
reason in the Passage-0 row and Passage 1 would go terminal with a bare `failed` — no hang,
no diagnostic, wrong data, precisely the silent mode §S5 exists to prevent.

**So the gate is replaced, not dropped: the echo is CORRECTED, and checked.** The Keeper
put `passage` in the ApplyRequest, so an echo that disagrees is a protocol violation rather
than a fact — the Keeper's number is stamped back on before the event is stored, and the
host then fails `soul_passage_unsupported`, the same reason the streamed gate aborts with.
The `RunResult` carries a Passage too and gets the same check, because `run.completed` is
stamped from it.

⚠ **Correcting rather than DROPPING is the decision, and it was made the hard way.** An
earlier revision dropped the mismatched events. That closes the corruption and opens a
worse hole: `SendApply` is not aborted, so the request runs to completion on the machine —
a Passage that installed packages and wrote files would leave the host changed and the run
record holding no trace of what changed it.

⚠ **With the in-tree agent this is a backstop, not the common path.** `soul apply`
unmarshals with a strict `protojson.Unmarshal`, so a binary predating the field rejects an
ApplyRequest carrying `passage > 0` outright and the operator sees `push_transport_failed`.
The guard stands between a third-party or future binary that ACCEPTS the field and ignores
it and a silently mis-filed run. proto3 omits `passage: 0` from the wire, so a non-staged
run never exercises any of this.

### 5. The work-queue path refuses a push host

With `keeper.acolytes > 0` a run writes planned assignments and an Acolyte claims and
dispatches them over the EventStream. It has no push branch, so a planned row for a push
host would be claimed and then fail `soul_not_connected`. The refusal is raised before the
run's keeper-side tasks, not just before the first planned Insert: `core.cloud.provisioned`
runs there, and aborting after it would leave a run that could never have been applied
holding the machines it created. `dispatchPlanned` keeps the same check as
defence-in-depth, so a future caller reaching it another way still cannot write a planned
row nothing will close. Giving the work-queue its own push branch is separate scope: it
needs the claim, the fencing epoch and the SSH session to agree on one owner, which the
in-goroutine path does not have to solve.

### 6. `POST /v1/push/apply` carries `transport`

Same two forms as the DSL key, validated at the boundary (422) by the same decoder the
rendered plan goes through, so a body the API accepts cannot be refused later for its
shape. `agent` is refused there: that endpoint IS the ssh transport. The port range the
DSL validator enforces offline is enforced here too — the MCP tool schema declares it, and
a schema asserting a check that exists nowhere is worse than no schema.

## Consequences

- A `transport: ssh` host bound to an incarnation is now targetable by a scenario, and a
  task's `transport:` has a production path from both the DSL and the operator API.
- `apply.dispatched` gains the transport fields for a push host (`transport`,
  `route_source`, `ssh_provider`, `ssh_user`, `ssh_port`). That is where a scenario run
  pays ADR-0088's visibility debt; the bare push API pays it in
  `push_runs.summary.hosts[]`.
- The EventStream handler no longer owns the per-host event persistence; `applysink` does,
  and `keeper/internal/grpc` delegates. A new channel added to one transport is now added
  to both or to neither.
- **Still not solved, and still separate scope:** bootstrapping a bare VM. `souls.ssh_target`
  carries no address column and `core.bootstrap.issued` writes `transport='agent'` as a
  literal. Nothing here changes that, and `transport:` must not be read as solving it.
- **Still not solved:** module delivery over push (ADR-004 says all registered modules
  transfer; `ShaDeliverer`'s flat layout would not be found by the Soul's alias-slot walk),
  and the work-queue push branch above.

## Live proof

`keeper/internal/scenario/push_dispatch_integration_test.go` (`-tags=integration`): a real
Postgres, a real sshd on a real OS, the real `soul` binary built from this tree, driven
through the production roster resolver, dispatcher, deliverer and barrier. It asserts the
barrier releases, the `apply_task_register` row carries output the HOST produced, the file
the task wrote exists on the machine, and the `apply.dispatched` event names the task as
the source of the route. Its companion asserts that a failing push task breaks the barrier
carrying the task's own reason, not a bare `failed`.

Both go red when the event handler is dropped or the roster's push half is narrowed —
checked by mutating the code, not by reading it.
