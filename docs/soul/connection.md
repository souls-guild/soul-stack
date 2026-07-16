# Connecting Soul to the Keeper cluster

The specification of the `priority + failback` algorithm for the pull mode (`transport: agent`). In the push mode (`transport: ssh`) the algorithm **does not apply** — Keeper itself initiates the SSH session to the host, see [architecture.md → Push mode](../architecture.md#push-режим-keeperpush).

## Conventions

- The Soul config declares a list of Keeper endpoints with a numeric `priority` field.
- **A smaller number = more preferred** (like DNS MX, systemd, ip route).
- **The default is `priority: 1`** — all equal, the most preferred level. We set it explicitly only when we want to demote someone.
- Between priorities — **sequentially** (1 → 2 → 3).
- Within a single priority — **sequentially** with **randomization of the endpoint order** on each attempt (shuffle). This:
  - gives an even load: 1000 Souls with the same set of priority-2 endpoints do not stampede synchronously into the first one on the list;
  - is simpler to implement correctly (no need to cancel parallel TLS handshakes once a winner appears);
  - the cost is a bit more time if the first one from the shuffled list is unavailable (a `handshake_timeout` is needed, then the next one). With reasonable `handshake_timeout` values (10s) this is tolerable.

## YAML config

```yaml
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k2.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k3.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k4.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k1.dc2.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 3

  retry:
    max_attempts: 2          # per-endpoint attempts before spray; default 2
    backoff:
      initial: 1s
      max: 30s
      jitter: true
    handshake_timeout: 10s

  failback:
    enabled: true
    interval: 1h
    spray: 10m          # actual moment = interval ± spray, uniformly
```

The full `soul.yml` layout (including `paths`, `tls`, `soulprint`, `cleanup`, `logging`, `metrics`, `otel`) is in [config.md](config.md).

## Two phases, two ports

Keeper holds two gRPC listeners on **different ports** (ADR-012): Bootstrap (server-only TLS) and EventStream (mTLS). The cluster hosts are the same for both phases, so an endpoint carries one `host` and two ports — the list is not duplicated.

| Phase | Command | Port | TLS |
|---|---|---|---|
| Onboarding | `soul init` | `bootstrap_port` | server-only TLS (Soul verifies Keeper against `keeper.tls.ca`) |
| Working cycle | `soul run` | `event_stream_port` | mTLS (SoulSeed cert ↔ cluster CA from the seed) |

Both ports are **required** when an endpoint is present ("security first": explicitness matters more than brevity, in production the ports are usually different; there is no silent fallback of bootstrap onto the event_stream port).

`priority` orders **both** phases (the hosts are the same), but the traversal algorithms differ. **EventStream** shuffles endpoints within a single priority (shuffle / spray, see §Algorithm). **Bootstrap** does not do this: the one-shot traversal goes by priority from smaller to larger **without in-group shuffle** — the order is deterministic. Also **failback does not apply** to bootstrap (onboarding is one-shot, there is no down/up switching; failback works only in the EventStream phase).

## Parameters

- `endpoints[].host` — the host of a Keeper instance (FQDN or IP), shared by both phases.
- `endpoints[].event_stream_port` — the port of the EventStream listener (mTLS, the `soul run` phase). Required, 1..65535.
- `endpoints[].bootstrap_port` — the port of the Bootstrap listener (server-only TLS, the `soul init` phase). Required, 1..65535.
- `endpoints[].priority` — an integer ≥ 1, default `1`. Orders both phases.
- `retry.max_attempts` — how many times in a row to try **one** endpoint on a retriable error (see §Error classification) before spraying to the next endpoint. **Default `2`** (not `5`): we keep per-endpoint persistence low — worst-case failover grows as `N×max_attempts×handshake_timeout`, while resilience is provided by spray across the fallback list + the external exponential reconnect (§Establishing the first connection, step 5). Omitted/`0` → `2`.
- `retry.backoff.initial` / `retry.backoff.max` / `retry.backoff.jitter` — the parameters of the exponential backoff between **full passes** over the fallback list (the external reconnect loop, §Establishing step 5 and §Lease-held). **Between attempts to a single endpoint** (per-endpoint retry) the pause is different: `backoff.initial` is taken **flat** (without exponential growth), with `±25%` jitter when `backoff.jitter: true`. The cap grows only between full passes, not between attempts to a single endpoint. There is no separate config key for the inter-attempt pause (`backoff.initial`/`backoff.jitter` is reused). ⚠️ The inter-attempt pause is **restart-required**: it is read once when the EventStream client is assembled; SIGHUP hot-reload does not update it (unlike the reconnect backoff `keeper.retry.backoff.*`, which is re-read per iteration).
- `retry.handshake_timeout` — the timeout for establishing the TLS+gRPC connection to a single endpoint.
- `failback.enabled` — whether to try to return to a more preferred priority after switching down.
- `failback.interval` — how often to launch a failback attempt (typically 1h).
- `failback.spray` — a uniform jitter of ±spray around interval. **Does not stretch the interval**, it only protects against the herd effect (thousands of Souls must not wake up synchronously). The base `interval` is preserved.

## Algorithm

### Establishing the first connection

1. Group `endpoints` by `priority`, sort the priorities ascending.
2. Take the minimal priority, **shuffle** its endpoints, try them **sequentially**: for each endpoint — a per-endpoint retry loop (`max_attempts` `dialOne` attempts **in a row** to the SAME endpoint, with a flat pause `backoff.initial ± jitter` between attempts). A retry to the same endpoint is done **only on a retriable error** (§Error classification); a non-retriable one (lease-held / auth / contract rejection) — immediately spray to the next endpoint without retrying.
3. The first to successfully establish a gRPC stream wins; the following endpoints are left untouched. The current priority is locked in.
4. If an endpoint exhausts `retry.max_attempts` (or returns a non-retriable error) — spray to the next endpoint of this priority; when all endpoints of a priority have dropped out — move to the next priority, repeat step 2 (with a new shuffle).
5. If all priorities are exhausted — this is one full pass over the fallback list; the external reconnect loop waits `delay` (exponential growth between passes, capped to `retry.backoff.max`) and starts over from the minimal priority. It is exactly here, and NOT between attempts to a single endpoint, that the exponential backoff growth operates.

### Failback (return to a more preferred priority)

6. After the stream has been established at priority `current` and `failback.enabled: true`:
   - start a `failback.interval` timer with a random offset within `±failback.spray`;
   - on firing — a sequential attempt over priorities **from 1 to current-1**: at each priority the endpoints are shuffled and tried in turn;
   - the first success at priority K (K < current): open a new stream, **then** close the old one (zero-downtime), `current := K`, the timer restarts;
   - if all attempts failed — wait for the next `failback.interval ± spray`, without fast retries. No hurry.

### Error classification (what is retried per-endpoint, what is sprayed immediately)

The per-endpoint retry loop (step 2) repeats `dialOne` to the **same** endpoint only on a transient transport error; an unrecoverable rejection is not cured by retrying — a different endpoint is needed, so such a failure immediately breaks the loop and proceeds to spray. The matrix is normative (matching by gRPC status code):

| Class | gRPC codes | Per-endpoint behavior |
|---|---|---|
| **retriable** | `Unavailable`, `DeadlineExceeded`, `Internal`, `Unknown`, `Aborted` + local handshake timeout (not a gRPC status → `Unknown`) | retry to the same endpoint up to `max_attempts`, with a flat pause `backoff.initial ± jitter` between attempts |
| **non-retriable (spray-on)** | `AlreadyExists` (lease-held), `Unauthenticated`, `PermissionDenied`, `InvalidArgument`, `FailedPrecondition`, `Unimplemented` | exactly **one** `dialOne` attempt, then immediately spray to the next endpoint |
| **default** | any unclassified code | retriable (conservatively) |

The logic: `Unauthenticated`/`PermissionDenied` — an auth problem (cert/RBAC), it will not fix itself within `backoff.initial`; `InvalidArgument`/`FailedPrecondition`/`Unimplemented` — a contract rejection; `AlreadyExists` — another Keeper holds the SID lease (see §Lease-held below). In all these cases a retry to the same endpoint is pointless. A transient transport flake (`Unavailable`/timeout/…) — on the contrary, a second attempt to the same endpoint often succeeds.

Relation to §Lease-held: `AlreadyExists` is deliberately **not retried** per-endpoint (exactly one `dialOne` per lease-held endpoint). This is complementary to the lease-held soft-failure backoff of the external reconnect loop — per-endpoint retry does not create churn on the surviving Keepers while the lease is still held; the fast return after a force-release is provided precisely by the modest cap of the reconnect loop, not by a retry to a single endpoint.

### Lease-held soft-failure (reconnect after the holder crashes)

A separate branch of the reconnect backoff, **inside** the algorithm above — not a new parameter, but a distinction of the cause of the Dial failure.

**What it is.** After the crash of the Keeper instance that held the stream (the holder), this Soul's SID lease (`soul:<sid>:lock`) lives until the Conclave presence of the former holder expires (~30s). While the lease is held, a reconnect of the same SID to the **surviving** Keepers is rejected at the gRPC handshake with the code `AlreadyExists` (the session is discarded, even though the transport came up). Soul distinguishes this **lease-held soft-failure** from an ordinary transport failure.

**The backoff is not the same as for a transport failure.** On lease-held Soul does **not** cap the backoff with the general transport cap `retry.backoff.max` (30s by default), but with a separate modest cap of **3s** (an internal invariant, not a config key). The goal is recovery latency:

- do not hammer the surviving Keepers with log noise and churn for the whole presence window;
- reconnect within seconds after the lease is released, instead of waiting out the inflated general cap.

The lease is released by the **keeper side**: after the presence of the former holder expires, a surviving Keeper does a presence-gated force-release of the SID lease (a provably-dead holder → a CAS takeover of the key by a new KID). The Soul side is complementary — it patiently retries with a modest backoff until the keeper releases the key. The keeper-side details (presence gate, split-brain safety, the residual window ≤Conclave-TTL) are in [recovery-reclaim-apply-runs.md → presence-gated force-release SID-lease](../operations/recovery-reclaim-apply-runs.md#presence-gated-force-release-sid-lease--сокращение-окна-невидимости-soul-а).

**Spray is not affected.** `AlreadyExists` on **one** endpoint does not break the fallback-list traversal — the next endpoint may already have taken over the lease after the force-release. The modest cap kicks in only when **all** tried endpoints returned `AlreadyExists` (meaning the lease is still held everywhere); if at least one failure is not `AlreadyExists`, it is a transport failure → the general exponential up to `retry.backoff.max`.

## Guarantees

- At any moment Soul holds exactly one active stream to one Keeper.
- Within a single priority — sequentially with randomization of the endpoint order; between priorities — sequentially.
- Failback at most once per `interval`, with a random offset of ±`spray`.
- The scenario "one DC-local priority-1 Keeper, two priority-2 backups, one cross-DC priority-3, return once an hour" is expressed through exactly these three parameters.

## See also

- [config.md](config.md) — the full `soul.yml` layout, including the `keeper:` block.
- [onboarding.md](onboarding.md) — `soul init` uses the same algorithm on the first CSR.
- [architecture.md → Soul connection: priority and failback](../architecture.md#подключение-soul-priority-и-failback) — a short architectural overview and the relation to the push mode.
- [architecture.md → ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) — the rationale for the bidi stream and the HA Keeper cluster.
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — a working config example.
