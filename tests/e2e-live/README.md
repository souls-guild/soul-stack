# tests/e2e-live — L3b real-soul-in-container (ADR-039)

An L3b smoke loop with a **real** `soul` binary in a Linux container (Debian-12
systemd-PID-1). Unlike L3a (`tests/e2e/`, a soul-stub helper package), L3b:

- uses a real `soul` binary inside a privileged container with systemd-PID-1;
- goes through the real CSR Bootstrap flow (CSR → `Keeper.Bootstrap` → leaf cert);
- a real `core.pkg.installed name=nginx` installs a real package;
- a real `core.service.running name=nginx` starts a real service through systemd.

## Running

```sh
# Two pre-requisites, and they are different binaries:
make build-linux    # soul-linux-amd64, mounted into the container
make build          # keeper, run NATIVELY on the host by the harness

# Full L3b run (depends on both of the above)
make e2e-live

# Or a single test directly. Do the two builds first: a missing binary SKIPS
# (reads as `ok`), and only a stale KEEPER fails — see the two sections below.
SOUL_BIN_LINUX=$(pwd)/soul/bin/soul-linux-amd64 \
  go test -tags=e2e_live -run TestSmokeNginxLive ./...
```

Requires **docker** with support for privileged containers (cgroup-mount + systemd-PID-1).

### The keeper binary must be this tree (NIM-490)

L3b runs the keeper as a host process from `keeper/bin/keeper` — a file, not
something the harness compiles. `NewStack` now asks it which commit it carries
(`keeper version`, stamped from `git describe`) and **fails** when the answer is
not this tree, or when a keeper-side source is uncommitted and younger than the
binary. The refusal is a declared bring-up failure (`STAND-SETUP`), placed below
the declaration and above `infraUp = true`, so it lands before the containers are
paid for and does not read as a finding about the code; `provenance_test.go`
guards both ends.

Absence is still a **skip** here, and that asymmetry with L3a — where NIM-533
made a missing binary fatal, on the grounds that a tier which ran nothing must
not report `ok` — is a known gap, not a decision: NIM-625. It is out of
NIM-490's scope so the two changes stay separable. Staleness is a failure
regardless: a tier that silently reports on last week's build is worth less than
no tier.

The trap this closes is L3b-specific and was undocumented: `make e2e-live` used
to depend on `build-linux` **only**, which produces `keeper-linux-amd64` for the
soul container and never touches the native keeper the harness actually spawns.
Rebuilding the obvious thing did not fix the run. The target now depends on
`build` as well, and the failure message says outright that `make build-linux` is
not the remedy.

### The soul binary is checked for presence only (NIM-636)

The other pre-requisite, `soul/bin/soul-linux-amd64`, gets no such treatment: the
harness checks that the file exists and mounts whatever `make build-linux` last
produced, however old. `make e2e-live` depends on `build-linux`, so the common
path rebuilds it — a direct `go test -tags=e2e_live` does not, which is the
invocation the section above exists to make safe. The keeper's mechanism does not
transfer as-is (different source roots), so it is NIM-636 rather than a one-line
addition here.

### WSL2 + Docker Desktop

On native Linux, the soul container dials into keeper via
`host.docker.internal` (host-gateway) — the default, nothing to configure.

On **WSL2 + Docker Desktop**, containers live in the DD-VM, while the keeper process
runs in the WSL2 distro (different network namespaces). From the container, `host.docker.internal`
resolves to the DD-VM gateway (`192.168.65.254`), where keeper is NOT listening →
bootstrap fails with `connection refused`. The real WSL2 host IP is reachable
from the container — pass it via `E2E_KEEPER_HOST` (the harness wires it into the
`soul.yml` endpoint, ExtraHosts, and the keeper cert's **TLS-SAN**):

```sh
make build-linux
cd tests/e2e-live
E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') \
  go test -tags=e2e_live -run TestL3bSmokeNginxLive -timeout 25m
```

Without `E2E_KEEPER_HOST` behavior doesn't change (CI default `host.docker.internal`).

## The subject of the two service tests lives outside this tree (NIM-876)

A service is its own repository (NIM-871), so the tier's real subject — a `state_schema`
with declared secrets, a vars ladder, a destiny brick, a plugin — is not in here. It is
fetched:

- **pinned** — `github.com/soul-stack-services/redis` at a full 40-hex commit named in
  [`harness/servicecatalog.go`](harness/servicecatalog.go). Not a tag: a tag moves, and
  "the gate runs v1.2.0" then stops being a fact.
- **cached** — outside the repo and outside `$TMPDIR`
  (`$SOUL_STACK_E2E_SERVICE_CACHE`, else `$XDG_CACHE_HOME/soul-stack/e2e-live/services`),
  so it survives `git clean` and a reboot. The clone lives under `repo/`, and each pin is
  extracted to `tree/<alias>/<commit>` — **the commit is in the path**, so an extracted
  tree cannot hold anything but the commit it is named after, and a pin bump adds a
  directory rather than rewriting one.

Both halves are load-bearing. Without the pin the gate proves whatever happened to be in
some directory; without the cache its verdict depends on github.com being up, which is the
dependency NIM-542 spent a ticket removing from this same gate.

```sh
# Prime deliberately (idempotent; also a named step of the gate)
make e2e-live-services

# Then prove the subject needs nothing from the network
SOUL_STACK_E2E_SERVICE_OFFLINE=1 make e2e-live-gate
```

### Two overrides, and they are not interchangeable

| variable | what changes | may the gate run under it |
|---|---|---|
| `SOUL_STACK_E2E_SERVICE_REMOTE_<ALIAS>` | where the pinned commit is fetched FROM — a local clone, an internal mirror | **yes**: the commit is still verified after the fetch, so the bytes are the same |
| `SOUL_STACK_E2E_SERVICE_DIR_<ALIAS>` | the subject itself — a working tree instead of the pin | **no**: `make e2e-live-gate` refuses to start, because the verdict would be about a directory nobody can name |

The second one is for developing a service change and an engine change together; the
harness logs loudly when it is in force, so a transcript always says which subject it was
about. A hand-run `go test -tags=e2e_live` honours it.

### Bumping a pin is a decision

The new commit is what the blocking pre-tag gate will prove. There is no CI on the service
repository and nothing else looks at it, so the procedure is the whole of it: run
`make e2e-live-gate` against the candidate commit, then change the pin.

## Release tarballs: the mirror is gone, the tripwire is not (NIM-542 → NIM-876)

A service create used to fetch `node_exporter`, `redis_exporter` and `vector` from pinned
GitHub releases — six of the then-nine gate tests ran such a create, ~18 downloads per run
from inside the container. NIM-406 measured the cost: three runs on a slice that had not
changed by a byte gave three different answers. So the harness grew a local mirror (a
digest-verified cache, an https server over it, and a `vars/99-…yaml` layer written into
the fixture's copy of the service), and `artifactCatalog()` held the three pins.

NIM-871 then cut the service those six tests created, taking with it the guards that held
that catalog against the service's own declarations — leaving an unverified copy of a
service that existed nowhere. NIM-876 removed the mechanism, because the way to make that
copy *true* is to ask what the subjects declare, and no live subject declares such a fetch:
the in-tree fixtures download nothing, and the pinned service installs Redis from an apt
repository, which is a different mechanism.

What stays is [`harness/upstreamfetch.go`](harness/upstreamfetch.go): the scanner, plus a
docker-free guard that fails the moment any live subject — in-tree fixture or pinned
service — declares `default(vars.<prefix>_base_url, 'http…')` again. The alternative to
dead machinery is not "no machinery", it is "an outbound dependency nobody notices". If a
subject legitimately needs one, the mirror is in git whole, at the commit that removed it.

### What is still not hermetic

`core.pkg.installed` reaches `deb.debian.org` and `packages.redis.io`: the nginx smoke
installs from Debian's own mirror, the redis service from the apt repository its vars name.
This tier has never been offline-clean, and the guard above is about one class only — the
class that was measured making this gate's verdict random.

## Frequency

- **L3a** (fast loop) — every PR via `make e2e` (~30-60 sec per test).
- **L3b** (smoke loop) — **nightly cron / on-demand** (~5-15 min per test).
- **L3c** (k8s loop) — weekly / pre-release.

## Layout

```
tests/e2e-live/
├── README.md
├── go.mod                          # separate go module (deps don't leak)
├── cmd/service-cache/              # `make e2e-live-services` — primes the pinned-service cache
├── dockerfiles/
│   └── debian-12.Dockerfile        # privileged systemd-PID-1 base image
├── harness/                        # copy of L3a harness + L3b-specific helpers
│   ├── servicecatalog.go           # the pinned out-of-tree subjects + their cache (NIM-876)
│   ├── upstreamfetch.go            # "no live subject fetches release tarballs" (NIM-542 → NIM-876)
│   ├── stack.go                    # NewStack — PG/Redis/Vault/keeper
│   ├── bootstrap.go                # IssueBootstrapToken
│   ├── container.go                # SoulContainer + SpawnSoulContainer (privileged Debian-12)
│   ├── asserts.go                  # keeper-side + container-side asserts (AssertHost*)
│   ├── expectations.go             # LoadExpectations / AssertExpectations (+ host_state)
│   ├── coven.go                    # AddMember (roster via incarnation_membership)
│   ├── seed.go                     # direct-SQL seeds + CreateIncarnationOnRoster (bootstrap order)
│   ├── operator.go                 # Operator API HTTP client
│   ├── vault.go                    # InitVaultTestSecrets / IssueKeeperServerCert / Seed+ReadVaultKV
│   ├── config_builder.go           # buildKeeperYAML
│   ├── probe.go                    # waitForReady
│   ├── git.go                      # SetupGitRepo
│   └── destiny.go                  # MaterializeDestinies
├── smoke-nginx-live/  staged-probe-live/  when-gate-live/  module-delivery-live/
│   └── …/expectations/             # per-example host_state expectations
├── smoke_bootstrap_test.go         # TestL3bBootstrap_OneSoul
├── smoke_nginx_live_test.go        # TestL3bSmokeNginxLive_InstallAndStart
├── module_delivery_live_test.go    # TestL3bModuleDeliveryLive_SynthesisFetchHotRegister
├── plugin_channel_test.go          # TestL3bPluginChannel_CatalogAndAllow
├── redis_service_live_test.go      # TestL3bRedisServiceLive_{CreateFromSouls,Day2AddUser}
├── staged_probe_live_test.go       # TestL3bStagedProbeLive_WhereTargetsOnlyMaster
└── plugin_beacon_test.go           # TestE2EBeaconPlugin_FullLoop
```

## Bootstrap order of a live stand (NIM-192)

A test that brings up a NEW incarnation on already-onboarded Souls goes through
`Stack.CreateIncarnationOnRoster` and never hand-rolls the order:

1. seed the `incarnation` row (`status='ready'`, empty spec/state) — direct SQL;
2. bind each Soul as a member (`incarnation_membership`);
3. wait for each member's first `SoulprintReport`;
4. run the create scenario as an ordinary explicit run.

Two constraints pin that order and admit no other:

- **Membership cannot come first.** Since NIM-124 membership is a first-class
  relation with FK `incarnation_membership_incarnation_fk` → `incarnation(name)`
  (migration 099). Binding before the row exists is `SQLSTATE 23503`.
- **The run cannot come first.** A create run that rolls onto a ready roster
  (`provision: {enabled: false}`) resolves the roster at run start, so an unbound
  roster aborts with `no_hosts` before dispatch (`run.go` §3).

`POST /v1/incarnations` inserts the row **and** starts the create run in the same
call (`lifecycle.auto_create`), leaving no window between them — hence the direct
seed. The consequence to keep in mind when writing expectations: create is an
explicit run here, so it writes `incarnation.scenario_started`, **not**
`incarnation.created`. POST's own create path stays covered by the handler unit
tests and, for the BARE variant (no starting scenario), by L3a (`tests/e2e`) —
L3a hit the same FK ordering and moved to the same helper (NIM-210), so the
auto-started create run is no longer covered end-to-end at either tier.

`AddMember` on its own remains correct for an incarnation that already exists
(e.g. `fc5_when_gating_test.go`, which seeds a ready incarnation and runs a
day-2 scenario); it fails fast with the order instruction if the row is missing.

## Slices

L3b is implemented iteratively. Slice map (architect consultation `a0af3d90ec118aafd`):

| Slice | Content | Status |
|---|---|---|
| **L3b-1** | Layout + Dockerfile + `make build-linux` + `harness.NewStack` skeleton (PG/Redis/Vault/keeper, WITHOUT soul-container). | done |
| **L3b-2** | Real Bootstrap flow: `IssueBootstrapToken` + `SpawnSoulContainer` (privileged Debian-12 + soul-binary mount + CSR Bootstrap RPC). | done |
| **L3b-3** | First L3b example `smoke-nginx-live` (actually installs nginx via apt + systemctl start). | done |
| **L3b-4** | Container-side asserts (`AssertHostPkgInstalled` / `AssertHostServiceActive` / `AssertHostFileExists` / `AssertHostFileContent`). | done |
| **L3b-5** | Multi-host (`redis-cluster-live` with 3 soul containers) + YAML expectations loader (`harness.LoadExpectations` / `Stack.AssertExpectations`). The multi-host FIXTURE left with `examples/service/redis` (NIM-871); the loader and the multi-container machinery it delivered are still in use. | done, subject removed |
| **L3b-6** | ~~Drift-live~~ — removed with the drift circuit (NIM-446). It was the only live exercise of `core.file.Plan` end to end; the module's own `Plan` is still covered by its unit tests, but nothing drives it over a real Soul any more. **Errand's dry-run is the intended replacement, and since NIM-488 it reaches `Plan` at all** — the runner used to ask the `ErrandReadSafe` (Apply-path) condition on the dry-run path too, and since the two marker sets do not intersect, every `dry_run: true` Errand was terminal before reaching a `Plan`. Admission is now per-path (`dry_run: true` requires `PlanReadSafe`), so `core.file.present` plans on request while its `Apply` stays closed to ad-hoc. **It is not yet an equal replacement:** the `PlanEvent` the module sends is dropped on the floor (`errandrunner` builds a plan collector and never reads it; `ErrandResult` has no `changed` field at all — NIM-487), so a drifted host and a clean one return byte-identical results. A live test written today can assert "the host was not touched" and "Plan's body actually ran", but not "drift detected" — the verdict L3b-6 used to check. Writing that test is NIM-555 (NIM-455 asked for it first but was closed without it), and it needs NIM-487 to carry the verdict back. The inventory that used to be duplicated here now lives in one place, [`errandrunner/whitelist.go`](../../soul/internal/runtime/errandrunner/whitelist.go). | removed, superseded by NIM-555 |

## Tests

| Test | Souls | What it checks |
|---|---|---|
| `TestL3bBootstrap_OneSoul` | 1 | Real CSR Bootstrap flow + `souls.status=connected` after `SpawnSoulContainer`. |
| `TestL3bSmokeNginxLive_InstallAndStart` | 1 | Real `apt install nginx` + `systemctl start nginx` + `core.file.rendered` site config. Uses `harness.LoadExpectations` + `Stack.AssertExpectations`. |
| `TestL3bStagedProbeLive_WhereTargetsOnlyMaster` | 2+ | **staged-render probe→where on a live soul** (ADR-056): a real probe step emits a per-host register, and the Passage action `where: register.*=='master'` is genuinely applied ONLY on the master host. L3b analog of `TestE2EStagedFailover_2Passage`, but via a real apply instead of a stub. |
| `TestE2EBeaconPlugin_FullLoop` | 1 | Real `soul_beacon` plugin (gRPC-over-stdio): inotify portent → Vigil → Decree → Oracle → fired scenario on a live soul. |
| `TestL3bRedisServiceLive_CreateFromSouls` | 1 | **A service is brought to a working state** (NIM-876): the pinned out-of-tree redis service, rolled onto the roster through `POST /v1/incarnations` with `source: { roster: true }`, ending in a live Redis that answers to the credential it minted for itself into Vault at a keeper-derived path. |
| `TestL3bRedisServiceLive_Day2AddUser` | 1 | **A service is operated** (NIM-876): a day-2 scenario reads the state the create wrote, upserts one ACL user, re-renders `users.acl` whole and reaches the live ACL — while the credential an existing client already holds keeps working. |

## Known coverage blockers (NOT-L3b-able)

### ★ The machine half of a create is not covered here (NIM-876)

NIM-871 took this tier's service create and its six day-2 operations
(`TestL3bRedisLive_Day2{AddUser,UpdateConfig,Restart,UpdateUsers,Destroy,RotateTls}`) out
with `examples/service/redis`. Two of those claims are back as
`TestL3bRedisServiceLive_{CreateFromSouls,Day2AddUser}`, driving the pinned out-of-tree
service; four are not, because `update_config`, `restart`, `destroy` and `rotate_tls` are
scenarios the published service does not have yet.

What remains structurally out of reach here is the **bootstrap path**: the published
service's own `create` raises libvirt VMs through a keeper-side provider, hands them an SSH
host certificate through cloud-init, installs the agent over `core.ssh.run` and redeems a
bootstrap token. A docker tier whose souls are already onboarded cannot host any of that,
so the gate drives `create_from_souls` — the same rollout onto a roster that already
exists. The machine half is proven by a hand-run workstation stand (the service
repository's README records what it costs) and by nothing automated.

### Layer-1 finding: the form invariant "secret = vault:-ref only" is unenforceable

`pattern: "^vault:.*"` on a secret input (e.g. redis_password) **doesn't work**:
the vault: ref is resolved into a literal BEFORE value validation
(`ResolveInputValuesVault`: merge → vault-resolve → validate), so the pattern
ends up checking the already-resolved value and always fails. The pattern was removed from
`scenario/create/main.yml` and `scenario/add_replica/main.yml` (replaced with
a comment). Candidate for a separate input key `vault_ref_required` (validation
BEFORE resolve) — **out of MVP**, needs_architect.

## Expectations YAML loader (L3b-5)

`harness.LoadExpectations(t, path)` → `*Expectations` parses YAML with
`apply_runs` / `incarnation_state` / `audit_events` / `metrics` / `host_state`
(strict-mode `KnownFields(true)` — typos in keys are caught at startup).
`Stack.AssertExpectations(t, exp, applyID, incName)` applies the whole set
of checks in one call (including per-soul container-side asserts via `host_state`).

The format is fully symmetric with L3a (see [docs/testing/e2e.md](../../docs/testing/e2e.md))
plus a new `host_state` section — a list of per-soul expectations
(packages/services/files). Resolving `host_state[].soul` (by FQDN) →
`Stack.SoulContainers[i]` happens inside the loader, the caller doesn't work with indices.

## Why harness code is duplicated from L3a

Architect verdict `a0af3d90ec118aafd`: the harness package `tests/e2e/harness/`
is under Go-internal rules and unreachable from `tests/e2e-live/` (a different
module root). Duplication is acceptable because L3a/L3b are **independent
test frequencies** with different contracts (stub vs real soul). After
both stabilize — extracting a shared core into `tests/e2e-shared/` as a separate slice.

## Source of truth

- [ADR-039 § E2E three levels](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity).
- Architect consultation `a0af3d90ec118aafd` (5-slice L3b map).
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — L3a canon for fixtures/expectations
  (L3b format symmetric + container-side expectations).
