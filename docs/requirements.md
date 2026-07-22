Project requirements:
- MODULAR INFRASTRUCTURE
This means that you need to try to separate some global things into separate folders and entities, in the end perhaps even a binary


General requirements:
- Everyone publishes metrics
- OpenTelemetry support out of the box
- HotRealod config support
- Hot configuration change
- Overwriting configuration on disk after application
- Built-in log rotation by default
- SAFETY FIRST
- Integration with Vault (mandatory infrastructure dependency along with PostgreSQL/Redis - [ADR-053](adr/0053-dependency-tiers.md); integration out of the box)
- Built-in RBAC support
- Built-in MCP support
- Built-in OpenAPI support
- Backend-driven dynamic data in the UI: companion-web does not hardcode dynamic directories (permissions, modules, statuses, selector keys) - the backend gives them to OpenAPI directory endpoints, the UI fetches them. See [ADR-042](adr/0042-backend-driven-ui.md).
- Protection against resolver-DoS from the operator: per-AID rate-limit (**Tempo**) for resolver-heavy write endpoints - **third anti-DoS layer** after body-limit and [Toll](adr/0038-toll.md). Redis token-bucket per-AID, fail-OPEN when Redis is unavailable (availability > reinsurance). See [ADR-050](adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api).

Security prerequisites (threat model):
- Vault - external trusted secret-store; Keeper stores only client access to it; secrets are not materialized on the Keeper cluster disk.
- Transport Keeper↔Soul - mTLS; Soul identity authority - peer-cert, not payload (ADR-012).
- RBAC is an authoritative source of rights; JWT-roles informational, authz is rechecked against a DB snapshot with each request (ADR-028).
- Redis is a TRUSTED intra-cluster channel. It supports RBAC invalidation (topic `rbac:invalidate`, B2) and SSE cluster-bridge (forward apply events between Keeper instances). The envelope of these messages is NOT authenticated: falsifying a message in the worst case forces an unnecessary reread from the database (invalidation) or the delivery of an apply event, which still passes the RBAC subscription check on the receiving instance - injection of rights through Redis is impossible. Prerequisite: Redis is located inside a trusted perimeter (private network, mTLS/ACL on the Redis deployment side). When Redis leaves the trusted perimeter, an HMAC-signed envelope is required for both channels (postponed until such a requirement becomes available).

Keeper Requirements:
- 

Souls Requirements:
- 
