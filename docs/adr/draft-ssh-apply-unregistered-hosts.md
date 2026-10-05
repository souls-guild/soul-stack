## ADR-draft. `core.ssh.apply` — a destiny applied over SSH to hosts addressed by a list, not by the registry

**Status:** accepted 2026-10-05 (number stamped at squash-merge, `docs/adr/draft-ssh-apply-unregistered-hosts.md` until then)
**Amends:** [ADR-063](0063-bootstrap-token-delivery.md) (its 2026-09-12 ready-made VM chain gains a destiny form beside the shell one; on teleport the join wait stops retrying a rejected host key, and an identity file with no SSH CA is refused at start and at every dial), [ADR-0089](0089-scenario-push-branch.md) (strikes "bootstrapping a bare VM" from its *still not solved* list — for applying a destiny; module delivery stays there), [ADR-032](0032-push-orchestrator.md) (the push executor is shared by two addressings)
**Leaves unchanged:** [ADR-0088](0088-task-transport-key.md) §4 — the `transport:` key still names no address; [ADR-012](0012-keeper-soul-grpc.md)(d) — params are rendered on the Keeper, the Soul executes
**Implemented by:** NIM-905

### Problem

A bare VM cannot be given a destiny. The push branch is picked by the host's own registry
row — `souls.transport` ([ADR-0089](0089-scenario-push-branch.md) §1) — and the row a freshly
created machine gets, if any, is the one `core.bootstrap.issued` writes: `pending`, with
`transport='agent'` as a literal (`keeper/internal/coremod/bootstrap/issuer_pg.go:40-45`). The
push branch cannot reach that host, and `souls.ssh_target` could not address it anyway — it has
no address column, its host is the SID, and a machine minted seconds ago is not resolvable by
its SID (`keeper/internal/push/target_pg.go:81-89`). The only thing that reaches such a host is
`core.ssh.run`, which runs shell: a service that installs the agent re-states the whole install
as commands, while the procedure already exists as a destiny (`soul-stack-destiny/soul`) that
nothing can apply to a host without a running agent.

Half of what is needed was built. Before this decision `push.SshDispatcher.SendApply` did, in one
method: provider lookup → **`transport=ssh` check → target resolution** → Authorize → ephemeral
keypair → Sign → connect with host-cert verification → **delivery of the agent binary →
`soul apply` with stdin = ApplyRequest → NDJSON → `RunResult` plus the `TaskEvent`s it hands to
`onEvent`**. The second half is exactly what a bare VM needs; the first half asks the registry.

### Decision

**1. A new state `apply` on the keeper-side core module `core.ssh`.** The address is
`core.ssh.apply` — a verb, like its sibling `run`: the step is an act performed over SSH, and the
naming rule gives an imperative state the verb form (`docs/naming-rules.md`, the `state` row).
Its base `core.ssh` is already in the keeper-side catalog (`keeperSideCore`, `shared/coremanifest/side.go`),
so the step is keeper-side by address ([ADR-0087](0087-task-side-derived-from-module-address.md))
and the catalog stays at seven bases. The parameters are `core.ssh.run`'s, so the family
addresses one way and a scenario does not learn two:

```yaml
- name: Mint bootstrap tokens
  module: core.bootstrap.issued
  params: { sids: "${ register.vms.sids }", reissue: true }
  register: mint

- name: Install the agent on the new VMs
  module: core.ssh.apply
  params:
    # {sid, bootstrap_token, …} | {sid, onboarded: true} — with reissue: true no entry is
    # token_held. No address, so this list as written runs on keeper.yml::push.transport:
    # teleport (dial by SID).
    hosts: "${ register.mint.hosts }"
    ssh_provider: teleport                  # required; on teleport only audit metadata (no Authorize/Sign)
    destiny: soul                           # a service.yml destiny[] entry, as apply: destiny:
    input:                                  # the same for every host
      keeper_host: "${ vars.keeper_endpoint_host }"
      keeper_bootstrap_port: "${ vars.keeper_bootstrap_port }"
      keeper_event_stream_port: "${ vars.keeper_event_stream_port }"
      keeper_ca: "${ vault(vars.keeper_ca_path + '#ca') }"
      binary_url: "${ vars.soul_binary_url }"
      binary_sha256: "${ 'sha256:' + vars.soul_binary_sha256 }"
    input_from:                             # per host: destiny input name → field of that host's entry
      bootstrap_token: bootstrap_token

- name: Wait for the agents
  module: core.soul.registered
  params: { sid: "${ register.vms.sids }", await_online: true, await_timeout: 20m, refresh_soulprint: true }
```

On the direct transport every entry that is dialed needs a `primary_ip`, and
`core.bootstrap.issued` emits none — the scenario then builds `hosts` from the machine
provider's register, carrying the address beside the token.

`ssh_user`, `ssh_port` and `join_wait_timeout` are `core.ssh.run`'s, with the same defaults.
`sid` is required per entry, `primary_ip` only on the direct transport, an entry with
`onboarded: true` is skipped and not dialed, and a `token_held: true` entry is refused by name —
one parser for both states (`parseHosts` / `hostFromStruct`, `keeper/internal/coremod/ssh/run.go`), so the two cannot come
to disagree about an entry.

**2. Per-host values: `input:` is the same for every host, `input_from:` is each host's own.**
A keeper-side step renders once for the whole run, so `input:` cannot say "this host's token".
`input_from:` maps a destiny input name to a field of the host's entry — the idea of
`stdin_from:` extended, and like it limited to **top-level string fields**
(`hostFromStruct`, `keeper/internal/coremod/ssh/run.go`). A name in both `input:` and `input_from:` is
refused, and so is an entry **that will be dialed** and lacks a field `input_from:` names — an
`onboarded: true` entry carries no token and is skipped before its fields are read, as in
`core.ssh.run`, so a repeat run over a half-onboarded batch is not refused. Rejected: matching host
fields to input names implicitly (an unrelated field a producer adds would reach the destiny
silently), and a `host.*` CEL root (a new root, and a keeper task rendered more than once).

**3. Two addressings, one executor.** The shared core is the second half of `SendApply`, pulled
out of the `SshDispatcher` struct into a function over an open session: deliver the agent, run
`soul apply` with the rendered `ApplyRequest` on stdin, parse the NDJSON, hand every `TaskEvent`
to the caller, return the `RunResult`. It has to leave the struct because an `SshDispatcher`
cannot be built without SshProvider plugins and Vault host CAs
(`NewSshDispatcher`, `keeper/internal/push/dispatcher.go`), and a Keeper on the teleport transport needs
neither. Each addressing keeps its own first half:

| addressing | where the target comes from | how the session opens | who |
|---|---|---|---|
| registry (exists) | the SshProvider router (`push.PGRouter`) + a `souls` row with `transport=ssh` + `souls.ssh_target` + the task's `transport:` override | `SendApply`'s Authorize → Sign, then `push.Dial` with the host cert checked against the Vault host CAs | registered push hosts |
| list (new) | the `hosts:` param | `core.ssh.run`'s dial: `keeper.yml::push.transport` — direct (`primary_ip`; Authorize → Sign once, then the connect with the host cert checked against the Vault host CAs) or teleport (by SID, identity file); only the connect is inside the bounded join wait | anything the scenario names, registered or not |

The list addressing takes the dial from `core.ssh.run`, not from `SendApply`: a bare VM is
reached the way that module reaches it, including the teleport transport the registry branch
does not have and the join wait a freshly created machine needs (NIM-872). Delivery reaches
`core.ssh` through its own wiring, not through the dispatcher's — the daemon builds the
dispatcher only past three early returns that have nothing to do with delivery
(`setupPushDispatchers`, `keeper/cmd/keeper/daemon.go`). It is resolved **once, at registration**, by
`push.DeliveryFromConfig` over the same startup `keeper.yml::push` the dispatcher reads
(`setupCoreModules` already reads it, `keeper/cmd/keeper/daemon.go`), so both branches
ship the same file. An unreadable `soul_binary_path` adds **no new start refusal**: today it
stops the daemon only past the dispatcher's three early returns (`setupPushDispatchers`), so a
teleport-only Keeper starts with it. Resolved at
registration, the error is kept and `core.ssh.apply` refuses with it by name; the dispatcher's
own startup refusal is unchanged.

**4. The module does not consult the registry; it is not "register first".** The addressing
comes from the parameters, whether the host has no row, a `pending` row from
`core.bootstrap.issued`, or any other. Considered and rejected: write a `transport=ssh` row first
and let the registry branch do the rest. It needs an address `souls.ssh_target` has no column
for (see Problem), a `transport` value that is false for the length of the chain and flips to
`agent` once the agent the destiny installs comes up — a second write path for the field that
picks the branch — and the teleport transport the registry branch lacks. For a destiny that does
not install an agent at all, the row would describe a Soul that will never exist.
[ADR-0088](0088-task-transport-key.md) §4 already places bare-VM bootstrap outside the transport
model; this keeps it there.

**5. The module neither reads nor writes `souls`, and creates no `push_runs` row.** `push_runs`
rows are created only by `pushorch.Store.Insert` (`keeper/internal/pushorch/store.go:85`), the
record of a bare `POST /v1/push/apply`. A `core.ssh.apply` step is recorded the way every
keeper-side step is: the run's keeper `apply_runs` row, its register in `apply_task_register`, a
`task.executed` event, and its audit event (decision 12). The inner run's task events are not
stored per host — `apply_task_register` references `apply_runs`, and a listed host has no row
there; what survives per host is the audit event's task list and, when the step succeeds, the
roster in its output — a failed step has the message and the audit record, not an output.

**6. The Keeper renders, the Soul executes — as for an `apply:` destiny.** The Soul on each host
runs every task of the destiny itself (`soul apply`: the modules, the `.tmpl` files of
`core.file.rendered`, `when:`/`changed_when:`); the Keeper substitutes `${ … }` in params, because
that is where `vault()`, the git access to the destiny and the `input:` contract check live, and
[ADR-012](0012-keeper-soul-grpc.md)(d) keeps Vault tokens off the host. The Keeper side runs
**inside the module, at Apply**, through the run's render pipeline handed to it on the module
context — the channel `StateOpEvaluators` already use
(`keeper/internal/scenario/keeper_dispatch.go:371-375`). The alternative — teaching the render
pipeline to expand this module's destiny into a new `RenderedTask` field — was rejected: it would
reshape how a keeper-side step is rendered and stamped, in exactly the code NIM-907 is about to
rework. A step whose
params read `register.mint` sits in a later Passage than the mint
(`shared/config/passage.go:126-137`), so a render error comes after the mint either way; here it
is a failed step. The destiny is:

- **resolved** through the run's own destiny resolver: a `service.yml destiny[]` entry at its
  ref ([ADR-007](0007-versioning-git-ref.md)); an undeclared name gets the refusal `apply:` gets
  (`keeper/internal/scenario/destiny.go:168`), and a run with no destiny source refuses it as
  `apply:` does (`DestinyResolver not configured`);
- **rendered per host**, as a synthetic one-task `apply:` scenario whose roster is that one host,
  carrying the run's `IncarnationMeta` — so `incarnation.*` names the run's incarnation and
  service, and the §7 vault fence ([ADR-0083](0083-declared-secret-state-fields.md)), keyed on the
  service, stays on. One difference is by construction: `incarnation.host_count` is the roster of
  the render, which is `1`;
- **with no Soulprint.** The host's `soulprint.self` holds only its `sid` (and empty `covens`,
  `role`, `choirs`, `traits`), so a destiny reading `soulprint.self.os.*` fails the render with
  a no-such-key error naming the field. That is the honest answer — nothing is known about the
  machine yet — and the `soul` destiny reads no fact;
- **fed values by reference, never re-parsed as templates.** The synthetic task's
  `apply: input:` reads `${ input.<name> }`, and the values ride in as the scenario input; CEL
  does not re-interpolate a value it returns (`shared/cel/eval.go:151-183`). A register value is
  data from a plugin or a cloud API, and one that contains `${ … }` must reach the destiny as
  that text. `pushorch` builds its synthetic task the opposite way — literal values as
  `apply: input:`, evaluated again, with an empty service and so no fence
  (`keeper/internal/pushorch/run.go:306-328`) — and that construction must not be copied.

**7. Every host is rendered before any host is dialed.** All per-host input is resolved and the
destiny rendered for every entry that will be dialed first; only then does the first connection
open. A missing
`input_from:` field, an input that violates the destiny's contract and a render error each fail
the step with nothing executed on any listed host. `core.ssh.run` does not hold this for one case
today: it refuses `token_held`, a missing `primary_ip` on direct and both secret guards before any
dial, but a missing `stdin_from:` field fails inside the per-host loop — by then every earlier
host in the list has been dialed and run all its steps, and this host its steps before that one
(`applyRun` / `runHost`, `keeper/internal/coremod/ssh/run.go`).

**8. Delivery is mandatory, and the host path is fixed.** Answering the ticket's second question:
the registry branch gets its exec path from `souls.ssh_target.soul_path`, whose default is the
delivery path and whose only purpose is to *opt out* of the pairing
(`keeper/internal/push/target_config.go:24-33`). A listed host has no override, so it execs
`push.HostSoulBinaryPath` (`/var/lib/soul-stack/bin/soul`) — exactly the file the `Deliverer`
wrote and verified by SHA-256 in the same session. A bare host has no agent of its own, so
`keeper.yml::push.soul_binary_path` unset is a **refusal by name before any dial**, where the
registry branch logs a warning and execs whatever is already there. The file is not removed
afterwards. Nothing checks that it matches the Keeper's version or the host's architecture — it
is whatever the operator points `push.soul_binary_path` at, as on the registry branch.

**9. The host needs no seed and no identity.** Answering the ticket's first question:
`soul apply` loads `soul.yml` only if it exists, never loads a SoulSeed, and builds the core
module registry with no trust anchor (`soul/cmd/soul/main.go:744-827`). A factless host falls
back to runtime detection for package manager and init system
(`soul/internal/coremod/util/osfacts.go`). The bound that follows: a **plugin** module fails
closed there (no Sigil, no anchor), and module delivery over push is not built (ADR-0089), so the
destiny must use core modules only — `soul` does. The announcement gates of ADR-0089 §4 do not
arise: a listed host is never in a `DispatchPlan` (the step targets the Keeper), and no inner
event is stored.

**10. Host verification is not weakened, and a rejected host key is not a wait.** Direct dials
verify the host certificate against the Vault host CAs, and an empty set is a refusal, not a
blind connect (`push.Dial`, `keeper/internal/push/session.go`); teleport verifies against the identity
file's `known_hosts`. Neither path uses `ssh.InsecureIgnoreHostKey`, and **this ticket adds a
source guard** over the push and `core.ssh` packages that holds it so — today behavioural tests
hold the empty-CA refusal (`TestDial_RequiresCA`,
`TestApply_EmptyHostCAsIsARefusalNotABlindConnect`) and the foreign-CA rejection
(`TestHostCertCallback_ForeignCA`, `TestHostCertCallback_MultiCA_NoMatch`), and nothing reads the
source.
On direct, a provider's `Authorize` deny happens before the join wait, and the wait retries only
a failure to establish the TCP connection, so a rejected host certificate fails at once. On
teleport the wait retried **every** error (`teleportJoinRetry`, `keeper/internal/coremod/ssh/run.go`),
so a rejected host key reaches the scenario as "node not reachable via Teleport within
join_wait_timeout". This ticket tags a host-key rejection on both transports — the callback's
error, raised by our wrapper around the callback, not parsed from text; x/crypto and Teleport's
`trace` both keep it reachable by `errors.As` — and the wait never retries it; `core.ssh.run`
gets the same behaviour, since the dial is shared. ★ **The wrapper keeps a nil callback nil.** A
Teleport identity file without an SSH CA yields no callback, and today every dial is refused by
Teleport `api/ssh`'s own check (`config HostKeyCallback must be set`), ahead of x/crypto's; a
wrapper that called through nil would panic in the key exchange, and one that read nil as
"accept" would be a blind connect that no search for `InsecureIgnoreHostKey` finds. Because that
refusal is deterministic, it is refused in two places: the teleport dialer's startup preflight —
which already builds the SSH client config — stops the daemon, and since every dial reloads the
identity file (an operator reissues it), each dial refuses a nil callback with the same tagged,
never-retried error **before the proxy is asked anything**, so a reissue without an SSH CA fails
the step at once instead of waiting out the join budget. The test fixture that calls itself a minimally-valid identity carries no
`known_hosts` today; it gains an SSH CA, and the preflight is not loosened to fit it.
Bound, stated: whether a Teleport RBAC refusal at `DialHost` arrives as a typed access-denied
error is the proxy's behaviour and is unverified here; until it is, it stays in the wait and is
named in the message the wait ends with.

**11. All hosts at once; any failure fails the step.** A SID listed twice is refused before
anything is dialed — two installs racing on one machine would redeem one token twice. Hosts run in
parallel, one goroutine each,
so the step lasts as long as its slowest host — the join wait of a fresh VM is minutes, and the
sum over a batch would not fit the run's ceiling (decision 14). The fan-out is unbounded, as
`pushorch`'s already is: a batch is the handful of VMs one scenario creates, and on teleport each
host's join wait re-reads the identity file and opens a proxy client every 12–16 s, so N hosts
are N such loops at once. Every host runs to its end; if any
failed, the step fails afterwards and names every failed host (B1-strict, as `core.ssh.run`, so
the run never commits over a group that is only partly installed). The output is the roster, in
list order, and nothing the destiny printed — the same shape as `core.ssh.run`'s plus `changed`:

```yaml
hosts:   [{sid, ran: true|false, skipped: true|false, changed: true|false}]
count:   2
skipped: 1
changed: 1
```

`RunResult.status` says whether a host failed; `changed`, the failed task and its reason come
only from the inner run's `TaskEvent`s. The events themselves are dropped as each host's run is
parsed; what is kept until every host has finished is a status per task, for the audit record.
A host's failure message names the host, the failed task (by its name in the
rendered destiny) and the agent's reason, with every secret value the module knows replaced
before it leaves: the values of secret-named host fields, the value of each of the destiny's
`secret: true` inputs on its own, the whole value of every sealed cell of the destiny's params,
and every value the run sealed in this step's own params — a `${ vault(…) }` handed to an input
the destiny does not declare secret is still a secret. The second covers a token interpolated
into a longer string, which the third masks only as that string.

**12. The audit records what was executed, under the existing `ssh.run` event.** On the host this
step executes the destiny's tasks, not shell; so the record is the destiny and, per host, each
task with its outcome — no params and no output, which is where a token would be:

```yaml
event_type: ssh.run
correlation_id: <the run's apply_id>
payload:
  action: apply
  destiny: soul@v1.2.0
  ssh_provider: teleport
  transport: teleport
  count: 3
  skipped: 1
  sids: [vm1.example, vm2.example, vm3.example]
  hosts:
    - sid: vm1.example
      status: success
      tasks: [{task: "Ensure the soul state directory exists", module: core.directory.present, status: changed}, …]
    - sid: vm2.example
      status: failed
      error: "connect: node not reachable via Teleport within join_wait_timeout (…)"
    - sid: vm3.example
      skipped: true
```

A host that failed before `soul apply` answered — an `Authorize` deny, a join wait that ran out,
a failed delivery — has no tasks, and its `status` and masked `error` say why. Of a host that did
run, a task after the one that failed is `skipped` — the agent reports it so — and `not_run` marks
a task the agent never reported, a run cut off or cancelled. The destiny is named at the ref `service.yml`
declares for it — for a branch ref the branch, not the commit, the same resolution `apply:`
records — and with no ref when no host was rendered (every entry skipped).

It is written once per step after every host has finished, failed or not — the record of what
ran matters most when something failed. A step refused before any host is dialed — for example a duplicate SID,
a `token_held` entry, a missing `input_from:` field, a render error, no delivery (unset or
unreadable), no host CAs or an unknown provider — writes none: nothing reached a host, and the step's own refusal is the record. The write runs
on a context detached from the run's (capped at a few
seconds), because the failure that matters most is the run being cut off, and a write on the
cancelled context would be lost exactly then. `correlation_id` joins it to the run. A failed
audit write fails the step even when every host succeeded, as it does for `core.ssh.run`: a run
must not commit over an act it could not record.

The cost of the shared type, accepted with it: until now the payload's `action` only repeated the
type's (`run`), and from here it distinguishes two states, so a filter on `type=ssh.run` returns
both and an apply is told apart by `action`. No event type is added, so the generated catalog,
the OpenAPI enum and the web UI's labels do not move; `EventSSHRun`'s doc is rewritten to cover
both payloads.

**13. No stream, so no onboarding barrier of its own.** A listed host stays unregistered by this
step and streamless. `await_online` and `refresh_soulprint` in `core.soul.registered` are about
the **agent the destiny may have installed**: after the `soul` destiny the chain is the one in
decision 1, and the barrier is that agent's own stream. After any other destiny there is nothing
to await.

**14. The run's ceiling bounds the step — today's conflation, not fixed here.** The step has no
task-level limit; what bounds it is the join wait per host and the run-wide ceiling: 5 min
(`keeper/internal/scenario/scenario.go:139`), raised to `max_await_timeout` + 10 min only when
the plan carries a refresh emitter (`effectiveRunTimeout`, `keeper/internal/scenario/run.go`). The ceiling is
the run timeout doing a task timeout's job. NIM-907's `settings:` separates the two
([settings.md](../scenario/settings.md): `scenario_timeout`, default 24h, replacing the 5-minute
run timeout, beside `task_timeout`), and it is not built. Until it is, a chain that installs agents
needs `refresh_soulprint: true` on its `core.soul.registered` (as decision 1 writes it), and the
slowest host's install has to fit in that ceiling.

**15. The `transport:` key is not involved.** Every keeper-side task refuses it
(`transport_on_keeper_invalid`, `shared/config/scenario_transport.go:379-405`), this one
included. The addressing lives in the module's parameters, a different entity from the key
ADR-0088 §4 keeps address-free.

### Interaction with linear dispatch ([draft](draft-scenario-linear-barrier.md), NIM-907 — design only)

Today ([ADR-056](0056-staged-render-passage.md)) a keeper-side step runs in its Passage before
that Passage's host dispatch; under NIM-907 it runs at its position (its decision 3). Either way
it has no cross-host barrier, because it targets no roster host. Its destiny travels **whole**,
one `soul apply` message per host, which is NIM-907's decision 6 — *destiny is unchanged*: the
Keeper-side evaluation of `when:` and the rendering of post-module predicates apply to scenario
steps, not to the destiny's tasks inside this message. `settings: {on_failure, tolerate}` see one
keeper step and not the hosts inside it, so the step stays B1-strict under any policy. Its
`scenario_timeout` replaces the ceiling decision 14 lives under; whether its `task_timeout`
bounds this step as a whole, or each host's run inside it, is for that work to decide — the step
has one outcome and many hosts. The Acolyte path does not touch it: keeper-side steps run
locally.

### Rejected

- **Register first** (decision 4).
- **Rendering on the Soul** — moving `${ … }` substitution to the host would put Vault access and
  the destiny's git source there, against ADR-012(d) and NIM-907's decision 4 ("the Soul receives
  only rendered inputs"); the time a batch takes is the SSH join and the install, which the Soul
  already does, not the render.
- **An address on `transport:`** — ADR-0088 §4 refuses it for a reason that still holds: the key
  would read as bootstrapping a bare VM inside the transport model.
- **`apply:` with a host list** — a second roster beside the scenario's, written as an
  orchestration key. Everything `apply:` promises about its hosts — a Soulprint, a per-host
  register, `where:` over the roster — would be false for exactly these hosts.
- **A new audit event type** — a new type would move a generated catalog, an OpenAPI enum and a
  web label; the owner chose to keep `ssh.run` and let its `action` tell the two states apart
  (decision 12 states the cost).

### Consequences and bounds

- The inner run's task events are not kept per host as rows (decision 5); the audit event carries
  the per-host task list, and a failure carries its reason in the step's message.
- Plugin modules cannot run in the destiny (decision 9); `/var/lib/soul-stack/bin/soul` is left on
  the host (decision 8).
- The destiny's input contract is checked when the step runs, not by `soul-lint`; the params'
  types are checked offline from the module manifest.
- `core.ssh.run` changes where its dial does (decision 10): on teleport a rejected host key, or an
  identity file with no SSH CA, fails the step at once instead of after the join wait, and a Keeper
  whose identity file carries no SSH CA at startup refuses to start — it could dial nothing before;
  on both transports a refused host key reads `host key not verified: …`. Its shared connection
  parsing also refuses an `ssh_port` outside 1..65535 at the step, where only `Validate` did —
  a rendered port is not checkable offline.
- `push.DeliveryFromConfig` gains a second caller that does not refuse the start, so its godoc,
  which defines an error as "the daemon refuses to start", is rewritten with it; so are
  `audit.EventSSHRun`'s, which described the `run` payload alone, the `push.soul_binary_path` and
  `push.transport` field docs (`shared/config/keeper.go`), and the `push.soul_binary_path` row in
  `docs/keeper/config.md` with its twin in `docs/keeper/push.md`.
- Documents that name `core.ssh`'s states move with it: `docs/naming-rules.md` (the `core.ssh.run`
  row gains a sibling), `docs/module/README.md` (the catalog), `docs/keeper/modules.md` (the
  ready-made VM chain and the `core.ssh` section), `docs/module/core/ssh/README.md`,
  `shared/coremanifest/mod_ssh.go` and `side.go`'s catalog comment, the "only state" wording in
  `keeper/internal/coremod/ssh/run.go`, and the daemon's `core.ssh.run`-only log and error texts
  for the teleport dialer.
- The `soul` destiny repository (`soul-stack-destiny/soul`) names this state `core.ssh.applied` in
  its README; correcting that and calling `core.ssh.apply` from it is a follow-up in that
  repository.
