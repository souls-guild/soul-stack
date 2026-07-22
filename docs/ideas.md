# Soul Stack Idea Box

Shelved ideas for future releases - **not current scope**. Each is captured with a rationale and design sketch so you can return to it without losing context. Transferring an idea into work is a separate decision of the user (do not enter silently).

Related "idea banks" in open questions - [architecture.md → Open questions](architecture.md#open-questions) (Q11, Q21, etc.); as they mature, such points can move here in separate sections.

---

## Shepherd - Souls load balancing when scale-out

**Status:** deferred to future releases (user decision 2026-05-25). The name `Shepherd` is committed to [naming-rules.md](naming-rules.md) (propose-and-wait passed).

**Problem.** When adding a new Keeper instance behind LB, existing long-lived EventStream streams **stick** on old instances: LB balances only new connections, Soul itself does not reconnect until the stream ends, and failback is triggered only at `priority>1`. The new Keeper is idle until the natural churn (hours or days).

**Design sketch.**
- Instances publish a **load snapshot** (number of active streams; optional Acolyte queue depth) to the [Conclave](naming-rules.md#modules-and-subsystems-inside-keeper)-record `keeper:instance:<kid>`, with the same renew tick (10s).
- An instance that sees its load skewed above its fair share (`sum of streams / Conclave.CountLive`) dumps **excess** of its streams - partial `StreamManager.CloseAll` (not all, unlike Watchman in isolation) with jitter/cap against herding.
- Reset Souls are reconnected and scattered: in the priority group - via in-group shuffle ([connection.md](soul/connection.md)), behind one VIP - via HAProxy least-conn.
- **Balancing domain = priority group / one VIP** (different priorities = failover hierarchy, not balance).
- **drain mode** (auto-by-threshold / semi-auto / manual) - TBD.

**Cheap alternative / possible first step.** Passive max-connection-age stream recycling (gRPC server `MaxConnectionAge` / soul `max_stream_age` + spray) - background without operator participation, but "dumb" (recycling and with perfect balance) and with a constant handshake price. Watchman/CloseAll already gives half the mechanism (full reset when isolated) - Shepherd adds a partial reset for the sake of uniformity.

**What the implementation will require:** amend [ADR-006](adr/0006-cache-redis.md) (Conclave recording carries a load-snapshot), architect-design of the drain mode, checking interaction with in-flight apply ([ADR-027](architecture.md) Ward/recovery - will the long apply survive the closure of the stream).

**Related:** Conclave, Watchman (soul-shedding), [ADR-002](adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) failback model.
