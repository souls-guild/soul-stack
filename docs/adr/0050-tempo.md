# ADR-050. Tempo — per-AID rate-limiting write-API

> **Status: active.** The name **Tempo** was chosen by the user (propose-and-wait passed); the design — architect. The implementation (Redis token-bucket primitive, middleware, problem-type, config block + wire + guard-tests) is done (slices S-R1..S-R4).

**Amendment (2026-06-17, a separate bucket `voyage_preview` — architect design, variant a).** Before this, `POST /v1/voyages/preview` reused the `voyage_create` bucket (a single limit for create+preview). The problem found: preview and create **shared one per-AID quota** — frequent previews (a pre-show of the batch count in the UI with a late-binding target) ate the budget of the real create, and vice versa. **Decision:** preview gets its **own bucket `voyage_preview`** with its own rate/burst. Defaults — **`rate: 30 rps, burst: 60`** (softer than create `10/20`). Rationale for the asymmetry: preview is **read-like in effect** (no INSERT into `voyages`/`voyage_targets`, no audit record), so it deserves a softer limit; but preview is **resolver-heavy in cost** (the same Purview scope-resolve + page-CEL over the souls as create) → **not unlimited**, but a separate, wider ceiling. The per-AID Redis keys `tempo:<aid>:voyage_create` and `tempo:<aid>:voyage_preview` are independent (the key form from (a) already ensures this — different bucket names give different keys). Forward-compat additive: a new field `tempo.voyage_preview.{rate,burst}` in the config, omission → default; no breaking changes. Fixed in (c)/(e)/(f).

**Context.** Resolver-heavy write endpoints of the Operator API (`POST /v1/voyages` — resolving the operator's scope boundary via [Purview](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор), intersection with the invocation target, page-CEL over soulprint/state on a 100k-soul fleet) are expensive by nature. Two anti-DoS layers already exist: **body-limit** (cut off an oversized body before parsing) and **[Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** (cluster-wide block of the write-API on mass departure of Souls). Not covered is the **third vector** — a single authenticated Archon (its own `claims.Subject` / AID) hammering resolver-heavy create in a loop: the body is valid (body-limit lets it through), the cluster is healthy (Toll is not triggered), but the resolver load from one operator takes down the instance. A **per-AID rate limiter** on calls to these endpoints is needed — a third anti-DoS layer.

**Decision.** The entity **Tempo** is introduced — an end-to-end per-AID rate limiter of an operator's requests to resolver-heavy write endpoints. It fires after authentication (the `claims.Subject` = AID is known) and before the handler (before the resolvers run). The metaphor — a musical line next to [Conductor](../architecture.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)/[Cadence](0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)/[Choir](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации): "the acceptable tempo of an operator's calls to the API". The boundary with Cadence: Cadence decides **when to spawn** a Voyage (a schedule), Tempo — **how often an operator hits the API** (request rate-limit).

**(a) Redis-backend — the authority of the limit.** The token-bucket lives in Redis (hash `{tokens, last_refill_ts}` + `PEXPIRE`), key **`tempo:<aid>:<bucket>`** (per-AID, per logical bucket of an endpoint). Refill+take — **atomically in one Lua script** (read-modify-write of the bucket in one round-trip, without a race between instances). **In-memory per-instance REJECTED:** under stateless-HA ([ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) the limit would multiply ×N instances (10 rps per instance × N = N×10 rps per operator) and would depend on LB distribution — incoherent. Redis — the shared hot layer of the cluster ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)), a natural authority.

**(b) fail-OPEN on Redis-down — a deliberate security trade-off.** If Redis is unavailable (the script failed / connection refused), Tempo **turns off (passthrough)** — the request passes without a rate-check. This is the **same behavior as [Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** (Toll on a Redis failure does not raise degraded). **Fixed as a deliberate security trade-off, accepted by the user (2026-06-09): availability > over-caution.** Fail-closed (503 on unavailable Redis) is **rejected** — a Redis failure would block the whole resolver-heavy write-API of the cluster, turning a hot-layer failure into a full failure of the control plane; rate-limit is protection against abuse, not a safety-critical gate, its temporary disabling is acceptable. NB: this is the **opposite** of the fail-closed invariant of scoped visibility ([ADR-047 S3b](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор), on doubt it hides) — there uncertainty of scope = a data leak, here uncertainty of rate = a temporary loss of throttle; different risks, different strategies.

**(c) MVP coverage.** **`POST /v1/voyages`** (create, bucket `voyage_create`) + **`POST /v1/voyages/preview`** (dry-resolve of scope, bucket `voyage_preview`). **Each path — its OWN bucket** (see amendment 2026-06-17 below): the per-AID Redis keys `tempo:<aid>:voyage_create` and `tempo:<aid>:voyage_preview` are independent — exhausting one does not throttle the other. Other write endpoints under Tempo — **additive later** (a new bucket in the config + attaching the middleware, without a breaking change). The read-API is not put under Tempo (cheap, not resolver-heavy).

**(d) Exceeding — 429 + Retry-After + problem+json.** On bucket exhaustion — **HTTP 429** with the header `Retry-After` (seconds until at least one token is replenished) and a body `application/problem+json` (RFC 7807, problem-type **`tempo-exceeded`** — symbolic `TypeTempoExceeded`). A unified format with [Toll](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)-503 (the same `Retry-After` pattern, the same problem+json skeleton — [operator-api.md → Error format](../keeper/operator-api.md#error-format-rfc-7807)), a different code and type: Toll = 503 cluster-degraded, Tempo = 429 per-AID-rate.

**(e) Defaults.** Per-AID, by bucket:
- `voyage_create`: **`rate: 10 rps, burst: 20`**;
- `voyage_preview`: **`rate: 30 rps, burst: 60`** (softer than create — preview is read-like in effect, without persist/audit — but NOT unlimited: dry-resolve is just as resolver-heavy; see amendment 2026-06-17 below).

Burst — the depth of the bucket, rate — the refill speed. Tuned so that "a human / a normal automaton has plenty of headroom, loop-abuse is cut off".

**(f) Config `tempo:`** ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml), hot-reload):

```yaml
tempo:
  enabled: true            # default-ON when Redis is present (footgun-guard, like Conductor/Toll)
  voyage_create:
    rate: 10               # rps, bucket refill speed
    burst: 20              # bucket depth
  voyage_preview:          # SEPARATE bucket (amendment 2026-06-17), does not share the quota with create
    rate: 30               # rps, softer than create (preview read-like, but resolver-heavy)
    burst: 60              # bucket depth
```

`enabled` / `voyage_create.{rate,burst}` / `voyage_preview.{rate,burst}` — hot-reloadable (atomic swap of the fields; the new limit applies from the next request, the current buckets in Redis live out their own `PEXPIRE`). Omission of any `voyage_*` block / field → the default from (e). Normative typing of the block — [`docs/keeper/config.md → tempo`](../keeper/config.md#tempo) (docs-writer at implementation).

**(g) Metrics.** `keeper_tempo_allowed_total{endpoint}` / `keeper_tempo_rejected_total{endpoint}` (counter). The label `endpoint` (= bucket name, `voyage_create`); there is **NO AID label** — cardinality (the number of operators is unbounded, an AID in the label would blow up the time-series). Who exactly exceeds — visible in audit/logs by `claims.Subject`, not in metrics.

**Rationale.**
- **A third anti-DoS layer, orthogonal to the first two.** body-limit cuts by body size, Toll — by cluster health (cluster-wide), Tempo — by per-AID rate. Three independent vectors, they do not duplicate each other.
- **Alignment with [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)/[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis).** The Redis-backend is coherent under stateless-HA; no new infrastructure — reuse of the hot Redis layer.
- **Security first, but availability-first on a hot-layer failure (b).** The trade-off is fixed explicitly, not "an implementation detail".

**Consequences.**
- **Middleware** on the Operator API (after auth, before the handler) for the endpoints in coverage (c).
- **Redis primitive** token-bucket (Lua, key `tempo:<aid>:<bucket>`) — `shared/`/`keeper/internal` (layout — slice S-R1).
- **problem-type `tempo-exceeded`** (`TypeTempoExceeded`) — [naming-rules.md → Error codes](../naming-rules.md#error-codes), catalog [operator-api.md → Error format](../keeper/operator-api.md#error-format-rfc-7807) (docs-writer).
- **Config block `tempo:`** — [`docs/keeper/config.md`](../keeper/config.md) (docs-writer).
- **Metrics** `keeper_tempo_allowed_total` / `keeper_tempo_rejected_total` — [naming-rules.md → Metrics](../naming-rules.md).
- **OpenAPI** — add a `429` response (`application/problem+json` + `Retry-After`) to `POST /v1/voyages` and `POST /v1/voyages/preview` (docs-writer / S-R4).

**Relation to ADRs.**
- **[ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)** — `POST /v1/voyages` (bucket `voyage_create`) + `/v1/voyages/preview` (bucket `voyage_preview`) — the only MVP coverage; preview — a SEPARATE bucket (amendment 2026-06-17), does not share the quota with create.
- **[ADR-038](0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)** — an adjacent anti-DoS layer (cluster-wide write-block), NOT a conflict: Toll = 503 by cluster health, Tempo = 429 per-AID by rate; a unified problem+json/`Retry-After` skeleton.
- **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — Redis-backend (token-bucket).
- **[ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)** — config block `tempo:` hot-reloadable.
- **[ADR-047](0047-purview.md#adr-047-purview--scoped-rbac-видимость-узлов-role-default_scope--расширенный-селектор)** — the resolver-heaviness of voyage-create (Purview scope-resolve) — the motive for the limit.

**Rejected alternatives.**
- **(a) In-memory per-instance rate-limit.** The limit ×N instances, dependence on LB distribution — incoherent under stateless-HA. Rejected (a).
- **(b) fail-closed (503 on Redis-down).** A hot-layer failure would block the whole write-API. Rejected (b).
- **(c) AID label in metrics.** Cardinality — the number-of-operators is unbounded. Rejected (g).

**Slice plan R.** S-R0 — canon (this ADR). S-R1 — Redis token-bucket primitive (Lua + key `tempo:<aid>:<bucket>`). S-R2 — middleware + problem-type `tempo-exceeded`. S-R3 — config block `tempo:` + wire + attaching to the router (`POST /v1/voyages`). S-R4 — guard-tests (rate/burst/fail-open/per-AID isolation) + OpenAPI 429 response. **Amendment 2026-06-17** — a separate bucket `voyage_preview` (30/60): config field `tempo.voyage_preview`, re-attaching the preview route, guard-tests "create and preview do not share the quota".
