# ADR-072. Host-Utilization — lightweight host utilization telemetry over the presence channel

- **Context.** When entering an incarnation, the operator needs **fresh host utilization** (CPU%/load/mem/disk/uptime) — "is the instance choking right now" — **without deploying Prometheus**. There is nowhere with live utilization today: `SoulprintReport` ([ADR-018](0018-soulprint-typed.md)) carries **static** grains (refresh 5m, CEL-addressable `soulprint.self.*` — targeting facts, not live load); node-exporter (detailed pull path for host metrics — Prometheus-primary [ADR-024](0024-observability.md), reference alongside log-shipping [ADR-067](0067-vector-log-shipping.md)) — **expensive opt-in**: requires scrape infrastructure and isn't deployed everywhere. We need a **third, cheap push layer** on top of the already-existing Soul→Keeper presence stream — so latest utilization is at hand immediately, without external infra.

- **Decision.**

  - **(a) A new independent Host-Utilization layer.** Utilization dynamics are **volatile** and live in a separate layer. Distinguish from Soulprint (static grains, [ADR-018](0018-soulprint-typed.md)) and node-exporter (detailed pull). Precedent for an independent observability layer — [ADR-067](0067-vector-log-shipping.md) (Vector — a log plane alongside metrics). Host-Utilization is **NOT a targeting fact** (not CEL-addressable, doesn't land in the `soulprint.*` namespace) and is **NOT a Prometheus replacement** (a coarse latest window, not metric history).

  - **(b) Transport B — piggyback on the presence stream via a new `FromSoul.host_utilization = 10`** (message `HostUtilization`, new file `proto/keeper/v1/utilization.proto`). **Alternative A** was rejected (reserved fields 8-14 in `SoulprintFacts` [ADR-018](0018-soulprint-typed.md)): semantic violation (static→live in one message), the slow 5m soulprint cadence is unacceptable for "right now", pollutes the `soulprint` CEL namespace with volatile fields. Only-add per [ADR-012(c)](0012-keeper-soul-grpc.md) — a new oneof slot, numbers are not reused; breaking changes only through `proto/keeper/v2/`.

  - **(c) Its own economical pulse.** Default interval **30s**, floor **10s** (anti-DoS: interval below floor → clamp + warn), separate from the 5m soulprint cadence. Soul-side ticker; every `Send` comes from the `handleSession` select-loop (**single writer** on the stream — no send races).

  - **(d) Redis-only storage** (hot, not PG — project invariant "hot data → Redis", [ADR-006](0006-cache-redis.md)): latest — Hash `soul:<sid>:util` + **TTL 3x interval** (90s by default); a short sparkline window — list-ring `soul:<sid>:util:win` (`LPUSH` + `LTRIM`, N=60). Choice of **list-ring, not RedisTimeSeries**: the stand runs on `redis:7-alpine` (no TimeSeries module), the target DragonFly also lacks it → the solution is **portable** between both backends.

  - **(e) Liveness invariant.** Host-Utilization **does not affect liveness authority** — that remains the sole `soul:<sid>:lock` lease ([ADR-006](0006-cache-redis.md)). Like any app message ([ADR-012](0012-keeper-soul-grpc.md)), the pulse updates `last_seen_at` — but that's not a liveness indicator (authority is the lease), and its Send goes through the same single session select-loop, so the pulse can't "outlive" a hung loop and mask a dead agent. Missing utilization (old agent / collector disabled) is a **graceful degrade**: presence, lease, and UI don't break.

  - **(f) Freshness.** The API returns a `stale` flag (age of `received_at` > TTL, or key missing) — **stale data is never presented as fresh**. Pattern mirrors the Soulprint `collected_at`/`received_at` skew ([ADR-018](0018-soulprint-typed.md)): the collection moment is set by Soul, the receipt moment by Keeper.

  - **(g) Authenticity.** Utilization is accepted only from an **authenticated SID** (mTLS peer cert, `authenticatedSIDFrom`), **NEVER from the payload** — spoofing someone else's host is impossible (pattern from [ADR-012](0012-keeper-soul-grpc.md): SID in the payload is an echo for logs, authority is the certificate).

  - **(h) API.** `GET /v1/souls/{sid}/telemetry` — latest + window + freshness for one host; `GET /v1/incarnations/{name}/telemetry` — aggregate across an incarnation's hosts (scope `coven && ARRAY[name]`).

- **What it standardizes, what it defers.** **Standardizes** the layer, transport (`FromSoul` #10 / `HostUtilization`), storage (Redis latest + list-ring), invariants (liveness/freshness/authenticity), API (`/telemetry`). **Defers:** delivering config to the agent + essence-override + collector toggles — **NIM-87**; the HostsTab web panel — **NIM-88**; extensibility of the collector set (new metrics only through only-add of new `HostUtilization` fields, no generic map is introduced).

- **Consequences.**
  - New file `proto/keeper/v1/utilization.proto` + `FromSoul.host_utilization = 10`.
  - Soul package `soul/internal/utilization` (collection + ticker).
  - Keeper: `events_utilization.go` (receives the oneof) + `redis/utilization.go` (latest Hash + list-ring + freshness).
  - Two huma endpoints `/telemetry` (per-soul + per-incarnation aggregate).
  - A line in [naming-rules](../naming-rules.md) — message `HostUtilization` (+ nested `DiskUtilization`) and its fields.
  - Amendment to [ADR-024](0024-observability.md) (Host-Utilization as an additional lightweight utilization layer).
  - Optional `utilization:` block in `soul.yml` (pulse interval).

- **Trade-offs.**
  - **push-pulse vs pull.** Pulse is cheap and universal (no scrape infra), but carries fewer details: node-exporter remains for depth (per-core, detailed counters).
  - **list-ring vs RedisTimeSeries.** Ring is portable between `redis:7-alpine` and DragonFly, but without server-side downsampling/aggregations; acceptable for the tiny sparkline window (N=60).
  - **typed fixed fields vs generic-map.** Fixed fields give type-safety and a clean OpenAPI, but new metrics require an only-add proto change (not "drop a key into a map").

- **Amends / Related.** **Amends [ADR-024](0024-observability.md)** — adds a lightweight utilization layer alongside metrics (Prometheus pull, §a) / traces (OTel bridge) / logs ([ADR-067](0067-vector-log-shipping.md), push). Related (NOT amend): [ADR-018](0018-soulprint-typed.md) (static grains — a neighboring layer, Host-Utilization neither complements nor replaces them); [ADR-012](0012-keeper-soul-grpc.md) (only-add `FromSoul` #10, SID authenticity); [ADR-006](0006-cache-redis.md) (Redis storage + lease liveness authority).
