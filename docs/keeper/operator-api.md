# Operator API - Keeper HTTP facade

Regulatory specification of Keeper HTTP endpoints for operators (Archons). Each endpoint is strictly bound 1:1 to permission from [rbac.md → Permissions directory](rbac.md) and to MCP-tool named `keeper.<resource>.<action>` ([rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint)).

This document defines **conventions + mapping + key request/response schemas**. Source of truth in **form** Operator API (paths, bodies, schemas) - **Go-types of handlers** (huma v2 full-typed, code-first): request/reply are described by Go-structs with huma-tags in `keeper/internal/api/huma_*.go`, and the OpenAPI spec is **derived** (huma-aggregator `HumaFullSpecYAML`, see § Full OpenAPI YAML). Spec-first-framework `oapi-codegen` + package `keeper/internal/api/oapi` **deprecated** ([ADR-054](../adr/0054-openapi-code-first.md) replaced [ADR-051](../adr/0051-operator-api-codegen.md)). Operator API - REST/HTTP, no gRPC service. Former form source `proto/operator/v1` **deprecated** previously (amend [ADR-011](../adr/0011-go-layout.md)).

## Why a separate facade

Soul Stack exposes **three transports** out, and the Operator API is one of them:

| Transport | Who is the user | Contract | Listener |
|---|---|---|---|
| **OpenAPI (HTTP/JSON)** - this document | Archons (people + machine-identity, JWT) | Go-types huma full-typed (`keeper/internal/api/huma_*.go`); OpenAPI spec - derivative (huma aggregator → [`openapi.yaml`](openapi.yaml)) | `listen.openapi.addr` ([config.md](config.md)) |
| **MCP** | LLM agents on behalf of the Archon | the same tools, 1:1 with endpoints | `listen.mcp.addr` ([config.md](config.md)) |
| **gRPC bidi Keeper↔Soul** | `soul`-binaries (mTLS) | `proto/keeper/v1/` ([ADR-012](../adr/0012-keeper-soul-grpc.md)) | `listen.grpc.bootstrap.addr` (server-only TLS, onboarding) + `listen.grpc.event_stream.addr` (mTLS, long-lived stream) |

Operator API **does not overlap** with Keeper↔Soul gRPC: Souls do not go to OpenAPI, operators do not go to gRPC bidi. Authentication and identity model are also different - JWT/Archon for the operator ([ADR-014](../adr/0014-operator-identity.md)) vs mTLS/SoulSeed for Soul ([`../soul/identity.md`](../soul/identity.md)).

## Conventions

| Decision | Value | Rationale |
|---|---|---|
| **URL prefix** | `/v1/` | Short, explicit. Symmetry with `proto/keeper/v1/` / `proto/plugin/v1/`. Health/meta/docs endpoints (`/healthz`, `/readyz`, `/metrics`, `/openapi.yaml`, `/docs`, `/docs/assets/*`) - **outside** `/v1/` prefix, so as not to depend on the major version of the API. |
| **Versioning** | `v1` in URL; breaking changes - `/v2/` only. Forward-compat only-add inside `v1` (new fields are `optional`, deleting fields is prohibited). Symmetry with ADR-012(g) for gRPC. |
| **Auth** | `Authorization: Bearer <jwt>` - required for all `/v1/*`. The JWT format is [ADR-014](../adr/0014-operator-identity.md). See [§ Auth](#auth). |
| **Bootstrap-bypass** | `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)) - administrative subcommand on the keeper instance itself, **not HTTP**. The OpenAPI facade is not used at the bootstrap moment: the first Archon is created directly in Postgres under the PG advisory lock, the JWT is placed in the file `mode 0400`. All subsequent statements are via `POST /v1/operators` from the JWT of the parent Archon. |
| **Content-Type request** | `application/json` | Standard. |
| **Content-Type response success** | `application/json` | Standard. |
| **Content-Type response error** | `application/problem+json` | RFC 7807 Problem Details. See [§ Error format](#error-format-rfc-7807). |
| **Resource naming in path** | Plural lowercase kebab-case: `/v1/incarnations`, `/v1/operators`, `/v1/souls`, `/v1/push-providers`, `/v1/push`. | REST standard. |
| **JSON field naming** | `snake_case` for all request/response body fields. Go body types (huma full-typed handler-structs) carry `json:"<snake_case>"` tags and are serialized with the standard `encoding/json`; the same tags power the huma circuit of the derivative [`openapi.yaml`](openapi.yaml). | Same as `keeper.yml` / `soul.yml` convention; produces readable JSON symmetrical to `components/schemas` in [`openapi.yaml`](openapi.yaml). |
| **Enum serialization** | In the JSON API, enum values ​​are short lowercase forms without family-prefix (`"ready"` / `"connected"` / `"agent"`). The canonical list of values ​​for each enum is specified in Go code - native enum directory `keeper/internal/api/huma_enums.go` (for example `IncarnationStatus`); `enum: […]` in the derivative [`openapi.yaml`](openapi.yaml) is its huma-generator. The historical remark "the proto-constant would have the form `INCARNATION_STATUS_READY`" is left in the circuit descriptions for context, but is not used in the wire format. | Short forms in JSON are the convention of this API; source of the list - Go-catalog, derivative spec. |
| **Schema names** | `CamelCase` for schema names in `components/schemas` derived [`openapi.yaml`](openapi.yaml) (`OperatorCreateRequest`, `IncarnationGetReply`, `ProblemDetails`); The names of the schemes are derived by huma from the Go-struct handlers of the same name (`keeper/internal/api/huma_*.go`). | OpenAPI standard. |
| **ID in path** | `name` for Incarnation (`/v1/incarnations/{name}`), AID for Operator (`/v1/operators/{aid}`, regex `^[a-z0-9][a-z0-9._@-]{1,127}$` from [naming-rules.md → Identifiers](../naming-rules.md); AID may contain `.`/`@` for email-like external names - in path segment they are **URL-encoded**, like FQDN-SID), SID for Soul (FQDN). SID is used in path in `/v1/souls/{sid}/issue-token` (sub-resource action) - FQDN **URL-encoded** in the path segment (dots are allowed in path without escaping; only reserved characters according to RFC 3986 are escaped, which are not in a valid FQDN). In the list response, the SID is given as is. Read-by-SID (`GET /v1/souls/{sid}`) remains deferred - no permission `soul.get`. |
| **Pagination** | Query `offset` (int, ≥0, default `0`) + `limit` (int, 1..1000, default `50`). The list endpoint's response is `{items: [...], offset, limit, total}`. Cursor-pagination - post-MVP if necessary. |
| **Async operations** | See [§ Async operations](#async-operations) below. |
| **Status codes** | `200` (sync read/update), `201` (POST resource created), `202` (async accepted), `204` (delete/revoke without body), `400` (malformed JSON/syntactic), `401` (no/invalid JWT), `403` (RBAC deny), `404` (not found), `409` (conflict - `error_locked`, self-lockout invariant), `422` (validation error - semantic), `500` (internal error). |
| **Time format** | ISO-8601 / RFC 3339 in UTC: `"2026-05-20T15:30:00Z"`. |
| **Duration format** | Go-duration string in JSON (`"30s"`, `"24h"`) for symmetry with [config.md](config.md). |
| **Tracing** | Each request receives OTel-span ([requirements.md](../requirements.md)); `traceparent`/`tracestate` headers are forwarded. The `archon.aid=<aid>` attribute is written after JWT authentication ([ADR-014](../adr/0014-operator-identity.md)). |

### Async operations

Endpoints that trigger long runs (creating/changing incarnation, push) return `202 Accepted` + body `{"apply_id": "<ULID>"}` (ULID - `proto/keeper/v1/apply.proto → ApplyRequest.apply_id`).

Poll status in MVP - through two endpoints:

- `GET /v1/incarnations/{name}` - current `status` (`ready`/`applying`/`error_locked`/`migration_failed`/...) and `status_details`.
- `GET /v1/incarnations/{name}/history` - records `state_history` with field `apply_id`; an entry with the specific `<ULID>` appears after a successful commit. Polling clients can pass `?apply_id=<ULID>` to directly search for a row of a specific run (see below).

There is no separate `/v1/applies/{apply_id}` endpoint **in MVP** - there are no corresponding `apply.*` permissions in the [rbac.md](rbac.md) directory either. Will appear as a separate task if necessary.

Symmetrically in MCP - see [mcp-tools.md → `_apply_id`-convention](mcp-tools.md).

## Auth

JWT Bearer is the only auth method in MVP ([ADR-014](../adr/0014-operator-identity.md)).

```
Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJrZWVwZXItZXUtd2VzdC0wMSIsInN1YiI6ImFyY2hvbi1hbGljZSIsImlhdCI6MTcxNjIwNjIwMCwiZXhwIjoxNzE2MjkyNjAwLCJyb2xlcyI6WyJjbHVzdGVyLWFkbWluIl19.<signature>
```

### Claims

| Claim | Type | Meaning |
|---|---|---|
| `iss` | `string` | `keeper.yml → auth.jwt.issuer` (default — instance KID, [config.md → auth](config.md#auth)). |
| `sub` | `string` (AID) | Archon AID; FK on `operators(aid)` in Postgres. |
| `iat` | `int` (unix-sec) | Release time. |
| `exp` | `int` (unix-sec) | Expiration date. The default TTL is `auth.jwt.ttl_default` (`24h`) for regular tokens, `auth.jwt.ttl_bootstrap` (`720h` = 30 days) for bootstrap tokens. |
| `roles` | `list<string>` | List of roles from `keeper.yml → rbac.roles[]`. The source of truth is the registry at the time of request authentication, claim is a cache for quick solutions. |
| `bootstrap_initial` | `bool` (optional) | `true` for the first Archon released `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)). Used in auditing. |

### Bootstrap-bypass

Bootstrap of the first Archon **does not go through OpenAPI**: the `keeper init --archon=<aid>` command is executed on the keeper instance itself by an operator with access to the keeper host. The `operators` registry is empty → the first Archon is created under the PG advisory lock → JWT is written to the file `mode 0400`. This JWT is the only operator path in the Operator API after bootstrap. Details - [rbac.md → Bootstrap of the first Archon](rbac.md).

After bootstrap, the Operator API becomes the only channel for creating additional operators (`POST /v1/operators` with permission `operator.create`).

### Served-spec `/openapi.yaml` + `/openapi.json` for JWT

`GET /openapi.yaml` (outside `/v1`) since 2026-06-15 requires a valid Bearer token - required `RequireJWT`, but without `/v1` binding (RBAC/audit/maxBody/metrics), without permission-check (any authenticated Archon reads the spec). Previously, served-spec was public; the change enhances security - an anonymous visitor cannot upload a full map of endpoints ([ADR-054 §OpenAPI viewer](../adr/0054-openapi-code-first.md)). Without token → `401 application/problem+json`. Nearby is a JSON version of the same spec - `GET /openapi.json` (behind the same JWT): it is rendered inline by the RapiDoc viewer `/docs` (RapiDoc `loadSpec` accepts a parsed object, not a URL - the url fetch would not be carried by our Bearer and would run into 401). `/openapi.yaml` remains for people and tools. The visual viewer `GET /docs` loads the spec as a separate `fetch /openapi.json` with a Bearer header (mechanism A - see [§ Health / Meta / Docs](#health--meta--docs)). UI-vendor (soul-stack-web) and `soulctl` are not affected - they consume **committed** [`openapi.yaml`](openapi.yaml), not live-served.

### Secret masking in logs and traces

JWT tokens returned in the response body `POST /v1/operators` / `POST /v1/operators/{aid}/issue-token` are **not written** in OTel span attributes, access logs or audit-trail. Output masking according to the rule:

- Header `Authorization: Bearer <jwt>` - replaced by `Bearer ***`.
- JSON fields `jwt`, `token`, `bootstrap_token`, `private_key`, `password`, `credentials_ref`, `secret`, `signing_key`, `signing_key_ref` - are replaced by `"***"` before recording to any observability channel. `signing_key` / `signing_key_ref` - extension based on the use of suffix in `keeper.yml → auth.jwt.signing_key_ref` and symmetrically masked-keys in audit-pipeline (`shared/audit.maskedKeys`, ADR-022(k)).
- Vault-ref values ​​(format `vault:<path>`) - replaced with `"vault:***"` (the path is hidden, the vault-ref attribute is saved for diagnostics).

Specific implementation - middleware on OTel-exporter / log-pipeline; normalizing the list of masked fields is a separate security task before release.

#### Masking `state` / `spec` in GET responses (defense-in-depth)

Masking applies not only to observability channels, but also to the **JSON response itself** reading incarnation:

- `GET /v1/incarnations/{name}` and `GET /v1/incarnations` - The `state` and `spec` fields are run through `shared/audit.MaskSecrets` before serialization.
- `GET /v1/incarnations/{name}/history` - fields `state_before` and `state_after` are run through the same masking.

The rule is the same as for observability (substring-match by sensitive-key: `token`/`secret`/`password`/`private_key`/… and vault-ref-marker `vault:secret/`); sensitive values ​​are replaced with `***MASKED***`, non-sensitive fields and object structure are preserved.

**Only the response is masked - in Postgres `state`/`spec` are stored unchanged** (last known-good for apply / migrations / unlock should not depend on masking on read). This is the second layer of protection: the first is that sensitive values ​​from tasks with `no_log: true` do not physically fall into `incarnation.state` (probe-register no_log tasks are not accumulated in the state graph, [scenario/orchestration.md §7.1](../scenario/orchestration.md)).

> **Important for the client:** The JWT returned in the response body `POST /v1/operators` / `POST /v1/operators/{aid}/issue-token` is **issued once** and is not stored anywhere. The operator is obliged to securely store the token immediately upon receipt (file `mode 0400` or secret-manager); a lost token cannot be restored, only a new one can be issued through `operator.issue-token` (from another Archon with this right).

### Self-lockout invariant

You cannot delete or revoke the last Archon with active `*`-permission ([rbac.md → Built-in roles](rbac.md)). When trying through `POST /v1/operators/{aid}/revoke`, the API returns `409 Conflict` with `type: would-lock-out-cluster`.

## Error format (RFC 7807)

All errors `4xx`/`5xx` are returned as `application/problem+json`:

```json
{
  "type": "https://soul-stack.com/errors/incarnation-locked",
  "title": "Incarnation is in error_locked state",
  "status": 409,
  "detail": "Incarnation 'redis-prod' is locked after failed apply 01HABCDEFGHJKMNPQRSTVWXYZ; use POST /v1/incarnations/{name}/unlock first",
  "instance": "/v1/incarnations/redis-prod/scenarios/restart"
}
```

| Field | Type | Meaning |
|---|---|---|
| `type` | `string` (URI) | Stable error URN for machine-parsing. List - § Error Types. |
| `title` | `string` | Short title (English, fixed for each `type`). |
| `status` | `int` | HTTP status code (duplicated for client convenience). |
| `detail` | `string` | Human-readable message, may contain values ​​(names, ULIDs, reasons); free text. |
| `instance` | `string` (URI) | The specific request URI that resulted in the error. |

### Error types

All `type` are stable URNs under the `https://soul-stack.com/errors/` domain. List expansion is only-add (new types can be added, existing types cannot be renamed).

| `type` URN suffix | HTTP | Meaning | Where does |
|---|---|---|---|
| `unauthenticated` | 401 | JWT is missing, invalid or expired. | Any `/v1/*` endpoint. |
| `forbidden` | 403 | RBAC check failed. `detail` contains the required permission and context. | Any `/v1/*` endpoint after JWT authentication. |
| `not-found` | 404 | The resource does not exist. | Any endpoint with path-param. |
| `validation-failed` | 422 | Semantic validation error: incarnation input does not match scenario `input:`-scheme ([destiny/input](../input.md)); `create_scenario` in `POST /v1/incarnations` is empty if there are create scenarios (`detail` starts with `create_scenario_required:` + list of valid ones) or points to a scenario outside the create set (`create_scenario_invalid:`, see [operator-api/incarnations.md → Selecting a starting scenario](operator-api/incarnations.md)); profile `params` does not match CloudDriver `profile_schema` ([cloud.md](cloud.md)); request `issue-token` for Soul with `transport: ssh` (ssh host does not have bootstrap phase - `POST /v1/souls/{sid}/issue-token`). `detail` - path to a specific field or reason. |
| `assert-failed` | 422 | Scenario `assert:`-predicate did not pass at the pre-flight gate of the run CREATION ([ADR-009](../adr/0009-scenario-dsl.md)/[ADR-027](../adr/0027-apply-work-queue.md) amendment 2026-06-23, form A): the roster of the run (connected souls by Coven incarnation) does not converge with the invariant of the scenario topology - eg cluster size-guard `size(soulprint.hosts) == shards*(1+replicas_per_shard)`. **Incarnation is NOT created**, fail status (`error_locked`) is NOT set - failure at the model stage BEFORE committing. `detail` — `message` assert tasks + text of the failed predicate. Separate URN from `validation-failed`: "topology does not match" ≠ "input field does not match the schema." | `POST /v1/incarnations` (when the `create` scenario has the `assert:` task). |
| `malformed-request` | 400 | JSON syntax / incorrect query params. |
| `incarnation-locked` | 409 | Incarnation in `error_locked` - `POST /v1/incarnations/{name}/unlock` is needed before a new run ([architecture.md → Atomicity and `error_locked`](../architecture.md)). For `rerun-last` - also "status not `error_locked`" (nothing to restart). | `POST /v1/incarnations/{name}/scenarios/{scenario}`, `DELETE /v1/incarnations/{name}`, `POST /v1/incarnations/{name}/upgrade`, `POST /v1/incarnations/{name}/rerun-last`. |
| `rerun-input-unavailable` | 409 | `rerun-last` cannot restore the input of a fallen day-2 run (fail-closed): `apply_runs.recipe` is unavailable - the run fell to dispatch (render_failed / no_hosts / pre-flight, the terminal line was written without a recipe), the recipe was cleared by retention Reaper (`purge_apply_runs`) or a legacy run without a saved recipe (`recipe IS NULL`). Separate URN from `incarnation-locked` (machine-readable difference "input lost → `unlock` + manual `run` with explicit input" from "status not `error_locked`"). | `POST /v1/incarnations/{name}/rerun-last`. |
| `migration-failed` | 409 | Incarnation in `migration_failed` - manual parsing of state_history ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)) is required. | `POST /v1/incarnations/{name}/scenarios/{scenario}`, `POST /v1/incarnations/{name}/upgrade`. |
| `would-lock-out-cluster` | 409 | The operation would leave the cluster without an active Archon with an effective `*`-permission (an effective `*` may come through Synod). | `POST /v1/operators/{aid}/revoke`; role-operations (`DELETE /v1/roles/{name}`, `PATCH /v1/roles/{name}/permissions`, `DELETE /v1/roles/{name}/operators/{aid}`); synod operations (`DELETE /v1/synods/{name}`, `DELETE /v1/synods/{name}/operators/{aid}`, `DELETE /v1/synods/{name}/roles/{role_name}`). |
| `synod-not-found` | 404 | The synod group with the specified `name` is not in the registry `synods` ([ADR-049](../adr/0049-synod.md)). | `PATCH /v1/synods/{name}`, `DELETE /v1/synods/{name}`, `POST /v1/synods/{name}/operators`, `POST /v1/synods/{name}/roles`. |
| `synod-already-exists` | 409 | `name` Synod group is already occupied in the registry `synods`. | `POST /v1/synods`. |
| `synod-builtin` | 409 | Group with `builtin=true` - cannot be deleted (check **before** self-lockout). | `DELETE /v1/synods/{name}`. |
| `incarnation-already-exists` | 409 | An Incarnation with the specified `name` has already been created. | `POST /v1/incarnations`. |
| `operator-already-exists` | 409 | The AID is already occupied in the registry `operators`. | `POST /v1/operators`. |
| `soul-already-exists` | 409 | The SID is already registered in the registry `souls`. | `POST /v1/souls`. |
| `bootstrap-token-active` | 409 | Soul already has an active (not burned, not expired) bootstrap token - invariant `UNIQUE (sid) WHERE used_at IS NULL`. Re-release - with `?force=true` (expire old + reissue). | `POST /v1/souls/{sid}/issue-token`. |
| `service-not-registered` | 422 | `service` in `POST /v1/incarnations` is missing in `keeper.yml → services[]` ([config.md → services](config.md#services--default_destiny_source--default_module_source)). |
| `sigil-key-not-found` | 404 | There is no Sigil signature key with this `key_id` ([ADR-026(h)](../adr/0026-sigil.md)). | `POST /v1/sigil/keys/{key_id}/primary`, `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-last-active` | 409 | The last active signature key cannot be displayed - the set must not be empty. | `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-primary` | 409 | You cannot display the primary key directly - first `POST .../primary` to another active. | `DELETE /v1/sigil/keys/{key_id}`. |
| `sigil-key-concurrent-change` | 409 | Primary installation race (one_primary index) or retired key with set-primary; retry. | `POST /v1/sigil/keys`, `POST /v1/sigil/keys/{key_id}/primary`. |
| `omen-already-exists` | 409 | `name` Omen is already occupied in the registry `omens` ([ADR-025](../adr/0025-augur.md)). | `POST /v1/augur/omens`. |
| `tempo-exceeded` | 429 | Per-AID rate-limit [Tempo](config.md#tempo) has been exceeded - the operator pulls the resolver-heavy write endpoint too often ([ADR-050](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). The response carries the header **`Retry-After`** (seconds until at least one token is replenished). Per-AID limit (by `claims.Subject`), not cluster-wide. | `POST /v1/voyages`, `POST /v1/voyages/preview`. |
| `internal-error` | 500 | Unplanned error. `detail` - generic; full diagnostics only in logs/OTel-trace, which the client can find by `traceparent`-header in the response. |

## Pagination

Applies to all list endpoints (`GET /v1/incarnations`, `GET /v1/souls`).

**Request — query params:**

| Param | Type | Default | Meaning |
|---|---|---|---|
| `offset` | `int` (≥0) | `0` | Shift from the start of the set. |
| `limit` | `int` (1..1000) | `50` | Page size. `> 1000` → `400 malformed-request`. |

**Response:**

```json
{
  "items": [ /* ... */ ],
  "offset": 0,
  "limit": 50,
  "total": 137
}
```

`total` - the total number of elements including filters (if the endpoint has filters). The client iterates, incrementing `offset` by `limit` until `offset + len(items) < total`.

We don't enter Cursor-pagination - for the admin-API offset/limit is enough (the number of incarnations and souls in MVP installations is a few thousand). Extension is possible with a separate PR without breaking change (adding the optional `cursor` parameter).

## Mapping endpoint ↔ MCP-tool ↔ permission

Pivot table. The source of truth for permissions is [rbac.md → Permissions directory](rbac.md). 1:1 mapping is ensured by construction - each name permission `<resource>.<action>` determines the MCP-tool (`keeper.<resource>.<action>`) and endpoint path/method.

Normalization of the MCP side (tool declaration format, transport, auth, input/output schemas, async-convention, error mapping) - in [mcp-tools.md](mcp-tools.md).

### Health / Meta / Docs

On the API facade itself ([`router.go`](../../keeper/internal/api/router.go)) **6 meta-routes** are mounted: `/healthz`, `/readyz`, `/openapi.yaml`, `/openapi.json`, `/docs`, `/docs/assets/*`. `/metrics` is physically **not on the facade** - it is on the dedicated metrics-listener (`listen.metrics.addr`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)); in the table below is given for completeness of the overview of the meta-surface, but is not included in the façade route count.

**Auth-boundary (mechanism A, [ADR-054 §OpenAPI-viewer](../adr/0054-openapi-code-first.md)):**

- **Public (without auth):** `/healthz`, `/readyz`, `/docs` (viewer shell framework), `/docs/assets/*` (built-in static RapiDoc). These routes carry no data and do not expose the API surface.
- **For JWT:** `/openapi.yaml` and `/openapi.json` - changed security public → required Bearer (same `RequireJWT` as `/v1`, but without `/v1` RBAC/audit/maxBody/metrics binding; mount outside `/v1`). Gain: **no anonymous reconnaissance of the full API surface**.

| Method | Path | Listener | Auth | Destination | Response |
|---|---|---|---|---|---|
| `GET` | `/healthz` | API façade | public | Liveness - the process is alive. | `200 {"status": "ok"}` |
| `GET` | `/readyz` | API façade | public | Readiness - Postgres + Vault are achievable. | `200 {"status": "ok"}` when ready; `503 {"status": "not_ready", "checks": {...}}` for not-ready. |
| `GET` | `/openapi.yaml` | API façade | **JWT (Bearer)** | Self-served OpenAPI 3.1 spec - **runtime dump of huma aggregator from code** (`servedOpenAPIHandler` → `HumaFullSpecYAML`, cache is collected once). Not embed: committed [`openapi.yaml`](openapi.yaml) - derived snapshot for UI-vendor/`soulctl`, served-spec is compiled from Go code. Without a valid JWT → `401 application/problem+json` (`unauthenticated`). | `200` (`application/yaml` according to RFC 9512, 2024 - more modern legacy `text/yaml`). |
| `GET` | `/openapi.json` | API façade | **JWT (Bearer)** | The same self-served OpenAPI 3.1 spec in JSON (`servedOpenAPIJSONHandler`) - for inline rendering by the RapiDoc viewer `/docs` (`loadSpec` accepts a parsed object). The YAML option `/openapi.yaml` remains for people and tools. Without a valid JWT → `401 application/problem+json` (`unauthenticated`). | `200 application/json`. |
| `GET` | `/docs` | API façade | public (shell) | Visual OpenAPI viewer ([RapiDoc](https://rapidocweb.com/), web-component, go:embed-assets) with built-in full-text search by endpoints (`allow-advanced-search`). Public HTML shell with Archon-JWT input field; The spec itself is loaded separately `fetch /openapi.json` from Bearer and rendered inline (`loadSpec(object)` - RapiDoc treats the string as a spec-URL and fetches it without our Bearer). The same JWT is thrown into RapiDoc "Try It" (`setApiKey(bearerAuth, …)`). Token - in `sessionStorage` (per-tab), Bearer header (not in URL). Cookies/sessions are not entered (consistent with [ADR-014](../adr/0014-operator-identity.md)). See [`docs_viewer.go`](../../keeper/internal/api/docs_viewer.go). | `200 text/html`. |
| `GET` | `/docs/assets/*` | API façade | public | Embedded static RapiDoc (one file `rapidoc-min.js`, ~843 KB, go:embed from `keeper/internal/api/docsassets/`; RapiDoc styles are in Shadow DOM, there is no separate CSS). Not CDN - air-gapped/offline installations. | `200` (Content-Type by extension). |
| `GET` | `/metrics` | metrics-listener ([ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)) | opt. basic-auth | Prometheus exhibition format. | `200` (`text/plain; version=0.0.4`). See [config.md → listen.metrics](config.md#listen). |

Health-paths selected in kubernetes-convention style (`/healthz`, `/readyz`); viewer route `/docs` - industry standard name (a deliberate exception from the Soul Stack dictionary, [naming-rules.md](../naming-rules.md)). All of them are clearly **not** under the `/v1/` prefix - independent of the major version of the Operator API.

### Operator (5) - control of Archons, [ADR-013](../adr/0013-bootstrap-archon.md) / [ADR-014](../adr/0014-operator-identity.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/operators` | `operator.create` | `keeper.operator.create` |
| `GET` | `/v1/operators` | `operator.list` | — (UI iter 2) |
| `GET` | `/v1/operators/{aid}` | `operator.list` | — (UI iter 2) |
| `POST` | `/v1/operators/{aid}/revoke` | `operator.revoke` | `keeper.operator.revoke` |
| `POST` | `/v1/operators/{aid}/issue-token` | `operator.issue-token` | `keeper.operator.issue-token` |

Read endpoints (`GET /v1/operators`, `GET /v1/operators/{aid}`) added to UI iteration 2 (placeholder `/archons-list` / `/archons/:aid`). MCP-tool symmetry is postponed until the next slice. Read-only - without audit-trail (pattern `soul.list`).

### Role (6) - RBAC-CRUD (roles / permissions / membership), [ADR-013](../adr/0013-bootstrap-archon.md) / [rbac.md](rbac.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/roles` | `role.create` | `keeper.role.create` |
| `GET` | `/v1/roles` | `role.list` | `keeper.role.list` |
| `DELETE` | `/v1/roles/{name}` | `role.delete` | `keeper.role.delete` |
| `PATCH` | `/v1/roles/{name}/permissions` | `role.update` | `keeper.role.update` |
| `POST` | `/v1/roles/{name}/operators` | `role.grant-operator` | `keeper.role.grant-operator` |
| `DELETE` | `/v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | `keeper.role.revoke-operator` |

1:1 `keeper.role.<action>` ↔ permission `role.<action>` ↔ endpoint. `role.*` - NoSelector (cluster-level operation without coven/host-scope, like `operator.*`/`synod.*`). Source of truth for semantics, bodies and error codes (`role-already-exists`, `role-builtin`, `would-lock-out-cluster`) - [rbac.md → REST `/v1/roles`](rbac.md); MCP side - [mcp-tools/roles.md](mcp-tools/roles.md). Mutating 5 routes are audited (authorization change, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `role.list` - read-only, no audit. `PATCH .../permissions` — replace semantics of the role permissions set (not merge).

### Synod (8) - managing groups of archons, [ADR-049](../adr/0049-synod.md)

Source of truth for semantics, bodies and CRUD error codes - [rbac.md → REST `/v1/synods`](rbac.md#rest-v1synods); MCP side - [mcp-tools/synods.md](mcp-tools/synods.md). `synod.*` - NoSelector.

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/synods` | `synod.create` | `keeper.synod.create` |
| `GET` | `/v1/synods` | `synod.list` | `keeper.synod.list` |
| `PATCH` | `/v1/synods/{name}` | `synod.update` | `keeper.synod.update` |
| `DELETE` | `/v1/synods/{name}` | `synod.delete` | `keeper.synod.delete` |
| `POST` | `/v1/synods/{name}/operators` | `synod.add-operator` | `keeper.synod.add-operator` |
| `DELETE` | `/v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | `keeper.synod.remove-operator` |
| `POST` | `/v1/synods/{name}/roles` | `synod.grant-role` | `keeper.synod.grant-role` |
| `DELETE` | `/v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | `keeper.synod.revoke-role` |

`PATCH /v1/synods/{name}` (ADR-049 amend) changes **ONLY `description`** groups (body `{description}`, required, 1..1024 characters); `name` (PK) immutable. Codes: `204` (success), `404 synod-not-found` (no group), `422 validation-failed` (empty `description` / limit exceeded), `400 malformed-request` (broken JSON / unknown field - including `name` in the body). builtin group is editable (`description` cosmetics, without subset/self-lockout). Audit-event `synod.updated`.

### Incarnation (17) - life cycle of runtime instances, [ADR-009](../adr/0009-scenario-dsl.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/incarnations` | `incarnation.create` | `keeper.incarnation.create` |
| `POST` | `/v1/incarnations/{name}/rerun-last` | `incarnation.rerun-last` | `keeper.incarnation.rerun-last` |
| `POST` | `/v1/incarnations/{name}/scenarios/{scenario}` | `incarnation.run` | `keeper.incarnation.run` |
| `POST` | `/v1/incarnations/{name}/scenarios/{scenario}/form-prefill` | `incarnation.get` | — (REST only) |
| `GET` | `/v1/incarnations/{name}` | `incarnation.get` | `keeper.incarnation.get` |
| `GET` | `/v1/incarnations` | `incarnation.list` | `keeper.incarnation.list` |
| `GET` | `/v1/incarnations/{name}/history` | `incarnation.history` | `keeper.incarnation.history` |
| `GET` | `/v1/incarnations/{name}/runs` | `incarnation.history` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/runs/{apply_id}` | `incarnation.history` | — (REST only) |
| `POST` | `/v1/incarnations/{name}/unlock` | `incarnation.unlock` | `keeper.incarnation.unlock` |
| `POST` | `/v1/incarnations/{name}/upgrade` | `incarnation.upgrade` | `keeper.incarnation.upgrade` |
| `POST` | `/v1/incarnations/{name}/check-drift` | `incarnation.check-drift` | `keeper.incarnation.check-drift` |
| `DELETE` | `/v1/incarnations/{name}` | `incarnation.destroy` | `keeper.incarnation.destroy` |
| `PATCH` | `/v1/incarnations/{name}/hosts` | `incarnation.update-hosts` | — (REST only) |
| `PUT` | `/v1/incarnations/{name}/traits` | `incarnation.traits-set` | `keeper.incarnation.traits-set` |
| `POST` | `/v1/incarnations/{name}/secrets/reveal` | `incarnation.view-secrets` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/secrets/revealable` | `incarnation.view-secrets` | — (REST only) |

`PATCH /v1/incarnations/{name}/hosts` edits declared `spec.hosts[]` (UI Hosts editing, [ADR-008](../adr/0008-coven-stable-tags.md)). Permission `incarnation.update-hosts` (narrowed from the previous `incarnation.update`, PM-decision 2026-06-02; backcompat-alias `incarnation.update` is canonized into `incarnation.update-hosts` on the snapshot load), scope selector `incScope` (run/upgrade/destroy parity). Audit `incarnation.hosts_updated` is written by the handler itself (payload - old/new snapshot). MCP-tool is **not yet** (REST-only; `manifest.go` does not contain `keeper.incarnation.hosts.update`).

`PUT /v1/incarnations/{name}/traits` holistically replaces the operator-set trait incarnation marks (`incarnation.traits` jsonb - source of truth, [ADR-060](../adr/0060-traits.md) R1 slice a), projected by the sync-hook into `souls.traits` member hosts. Permission `incarnation.traits-set` (scope incarnation/coven/service on path-`name`, like `update-hosts`). Audit `incarnation.traits_changed` is written by the handler itself (payload - only old/new KEYS, not values). MCP mirror - `keeper.incarnation.traits-set`. Replaces per-soul `POST /v1/souls/traits` (deprecated, see Soul). Detail - [operator-api/incarnations.md → PUT .../traits](operator-api/incarnations.md).

`POST /v1/incarnations/{name}/secrets/reveal` + discovery `GET …/secrets/revealable` — disclosure to the operator of the plaintext value of the incarnation secret declared by `revealable_secrets` service ([ADR-070](../adr/0070-secret-reveal-path.md), READ-twin [ADR-064](../adr/0064-secret-write-path.md) write-path). `reveal` `{secret_id, key}` → `{value}`: resolve `vault_ref` by literal substitution `{service}`/`{incarnation}`/`{key}` (**not CEL**; `{service}`+`{incarnation}` are required in the manifest; `key` must ∈ enumerate-array of the current `state`; `vault.ParseRef` - traversal-guard) and reads the value from Vault **only** if the resolved path is under `secret/<service>/<incarnation>/` (positive prefix-allowlist - **main guard**; system-floor `secret/keeper/`/`secret/internal/` - backstop), otherwise `404` + audit `out_of_service_scope`. `revealable` → `{items:[{secret_id, label, state_path, keys}]}` - discovery for the UI State view (an empty list is valid). Permission `incarnation.view-secrets` (scope incarnation/coven/service along path-`name`, **strictly privileged `incarnation.get`**; outside scope → **404** fail-closed, parity Get). `reveal` writes self-audit `incarnation.secret_revealed` - success (`result:"ok"`) And denied branches (`result:"denied"`+`reason`), payload `{name, secret_id, key, path, result, reason}` **WITHOUT value** (leak-guard tests); `revealable` - read, without audit. Authorized Disclosure → 200-DTO **past `MaskSecrets`**. There are **no** MCP tools (REST-only, like form-prefill). Full commit - [ADR-070](../adr/0070-secret-reveal-path.md).

`GET /v1/incarnations/{name}/runs` + `GET …/runs/{apply_id}` — read-view of incarnation runs (convolution of `apply_runs` by `apply_id`: aggregate status `applying`/`success`/`failed`/`cancelled` + per-host details with the address of the fallen task), under UI "execution status / current job". Run (apply_run) is NOT Voyage. Permission `incarnation.history` (reuse read-tier: whoever sees the history of the incarnation also sees its runs); gate - existence-`RequireAction`, per-`{name}` scope - in-handler (outside Purview-scope → `404`, parity History). Read-only, no audit, **REST-only** (no MCP tools). Detail - [operator-api/incarnations.md → GET .../runs](operator-api/incarnations.md).

`POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill` — day-2 pre-fill UI forms of the scenario from `incarnation.state` (only those declared in the `prefill_from_state`-paths, secret-exception inside; [input.md → prefill_from_state](../input.md)). Permission `incarnation.get` (reuse: whoever reads the incarnation receives a prefill of its form), per-`{name}` scope - in-handler, like Get/History. Read-resolve, without audit, **REST-only**.

### Runs (2) - global read-view of runs through all incarnations

"All Runs" UI page: convolution of `apply_runs` by `apply_id` THROUGH ALL incarnations + summary counters. Apply_run is NOT Voyage (Voyage has its own list `GET /v1/voyages`). Permission `incarnation.history` (reuse read-tier per-incarnation runs, existence-gate `RequireAction` on chi-group `/v1/runs`); narrowing of visibility by Purview ([ADR-047](../adr/0047-purview.md)) - in-handler, **fail-closed**: empty/non-resolving scope → empty list / null aggregate (`200`, not `403` - do not burn the existence of runs outside the scope; parity souls/stats). Both routes are read-only, without audit, **REST-only** (there are no MCP tools). Endpoint details - [operator-api/incarnations.md → GET /v1/runs](operator-api/incarnations.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/runs` | `incarnation.history` | — (UI All Runs) |
| `GET` | `/v1/runs/stats` | `incarnation.history` | — (UI All Runs) |

`GET /v1/runs` (operationId `listRuns`) - page of runs, newest on top (`started_at DESC`); query filters `status` (aggregate `applying`/`success`/`failed`/`cancelled`; invalid → `422`) and `incarnation` (owner incarnation name; invalid → `422`) + pagination `offset`/`limit` - **cap `limit` = 100** (not common 1000: global folding is more expensive than flat list; excess → `400`). Element - per-incarnation form `RunSummaryEntry` + field `incarnation` (run owner). `GET /v1/runs/stats` (operationId `getRunsStats`) - counters by aggregate status (`total`/`applying`/`success`/`failed`/`cancelled`) in two baskets: `all` (for all time) and `last_24h` (runs started during last 24 hours), within the same Purview-scope.

### Soul (8) - host registry

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/souls` | `soul.create` | `keeper.soul.create` |
| `POST` | `/v1/souls/coven` | `soul.coven-assign` | `keeper.soul.coven-assign` |
| `GET` | `/v1/souls` | `soul.list` | `keeper.soul.list` |
| `GET` | `/v1/souls/{sid}` | `soul.list` | — (UI detail-page) |
| `GET` | `/v1/souls/{sid}/soulprint` | `soul.list` | — (UI detail-page) |
| `GET` | `/v1/souls/{sid}/history` | `soul.list` | — (UI timeline) |
| `POST` | `/v1/souls/{sid}/issue-token` | `soul.issue-token` | `keeper.soul.issue-token` |
| `PUT` | `/v1/souls/{sid}/ssh-target` | `soul.ssh-target-update` | `keeper.soul.ssh-target.update` |

Permission `soul.list` covers reading the registry and its details: `GET /v1/souls` (list), `GET /v1/souls/{sid}` (single-soul detail), `GET /v1/souls/{sid}/soulprint` (last typed-Soulprint, [ADR-018](../adr/0018-soulprint-typed.md)), `GET /v1/souls/{sid}/history` (per-host operation timeline - scenario `apply_runs` + ad-hoc errands). Separate `soul.get` deliberately deferred ([rbac.md §Souls](rbac.md)); read endpoints use existence-gate (`RequireAction`) + handler-side InScope filter (host visibility by Purview, [ADR-047](../adr/0047-purview.md)). Read-only - no audit.

`PUT /v1/souls/{sid}/ssh-target` (`soul.ssh-target-update`, selector `host=<sid>`) updates per-host SSH push-flow details (`souls.ssh_target` jsonb: `ssh_port`/`ssh_user`/`soul_path`, [ADR-032](../adr/0032-push-orchestrator.md) amendment S7-1). 3-segment MCP-tool `keeper.soul.ssh-target.update` ↔ 2-segment permission `soul.ssh-target-update` (permission grammar is exactly `<resource>.<action>`). Audited (`soul.ssh_target_updated`).

`POST /v1/souls/{sid}/exec` (ad-hoc Errand, permission `errand.run`) included in section Errand (4), not duplicated here.

`POST /v1/souls/coven`-semantics, read/ssh-target endpoint-details - [operator-api/souls.md](operator-api/souls.md). MCP side - [mcp-tools/souls.md](mcp-tools/souls.md).

### Push (2) — [push.md](push.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/push/apply` | `push.apply` | `keeper.push.apply` |
| `GET` | `/v1/push/{apply_id}` | `push.read` | — (REST polling) |

`push.read` - separate permission (read operation does not require mutate rights `push.apply`); `GET /v1/push/{apply_id}` returns the status of the entry `push_runs` (in-flight or terminal). MCP-tool `keeper.push.cleanup` exists in manifest, but **REST route `/v1/push/cleanup` does not exist** - the declaration was removed from the spec on 2026-06-10 as dead (the route was never mounted in `router.go`); is not included in the route account (see [mcp-tools/push.md](mcp-tools/push.md)).

### Push-runs (1) - global list of push runs

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/push-runs` | `incarnation.history` | — (UI Push-runs page) |

Separate zone from `/v1/push/{apply_id}` (that one is per-id detail; this one is a list with pagination/filters). RBAC reuse `incarnation.history` (push - incarnation history, parity Voyage-list); a separate permission `push.list` is not entered until prompted by the operator. NoSelector. Connects together with the push block (`router.go`: `if pushH != nil`).

### Errand (4) — pull-ad-hoc exec outside scenario, [ADR-033](../adr/0033-errand.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/souls/{sid}/exec` | `errand.run` | `keeper.soul.errand.run` (slice E4) |
| `GET` | `/v1/errands/{errand_id}` | `errand.list` | — (REST polling) |
| `GET` | `/v1/errands` | `errand.list` | `keeper.errand.list` (slice E4) |
| `DELETE` | `/v1/errands/{errand_id}` | `errand.cancel` | `keeper.errand.cancel` (slice E5) |

Sync-primary flow (server-cap 30s), whitelist of Soul-side modules, cancel-flow (slice E5) and full request/response - in the domain file [operator-api/errands.md → Endpoint sections](operator-api/errands.md). Errand does NOT mutate `incarnation.state` (separate registry `errands`). MCP side - [mcp-tools/errands.md](mcp-tools/errands.md).

### ~~POST /v1/errand-runs (multi-target ad-hoc exec)~~ — superseded

Multi-target Errand under a single ULID. Implementation - slice E6-4.

**Superseded-by-Voyage (removed in Wave 5).** Multi-target ad-hoc exec is now `POST /v1/voyages` with `kind=command` ([ADR-043](../adr/0043-voyage.md)). Endpoint `/v1/errand-runs` and registry `errand_runs` have been removed; section retained as historical record, see [ADR-041](../adr/0041-errandrun.md).

### Voyage (5) - unified batch run, [ADR-043](../adr/0043-voyage.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/voyages` | `incarnation.run` (kind=scenario) / `errand.run` (kind=command) — RBAC-by-kind | `keeper.voyage.start` |
| `POST` | `/v1/voyages/preview` | `incarnation.run` / `errand.run` - RBAC-by-kind (as create) | — (REST only) |
| `GET` | `/v1/voyages` | `incarnation.history` | `keeper.voyage.list` |
| `GET` | `/v1/voyages/{id}` | `incarnation.history` | `keeper.voyage.get` |
| `DELETE` | `/v1/voyages/{id}` | `incarnation.run` / `errand.run` (cancel pending/scheduled) | `keeper.voyage.cancel` |

**RBAC-by-kind** ([ADR-043 §6](../adr/0043-voyage.md), fail-closed): permission is selected by `kind` from the body (visible only after the body decode, so the check lives in the handler, not in the middleware-route). Details of the target resolution / hybrid semantics of command∩Purview / Tempo limit - [operator-api/voyages.md](operator-api/voyages.md).

`GET /v1/voyages/{id}/targets` (All-runs drill, [ADR-043](../adr/0043-voyage.md) S5) - read under `incarnation.history`, **REST-only** (no MCP-tool). It is not counted in the section header "(5)" (the counter is based on the number of MCP-paired + RBAC-by-kind lines); six Voyage REST routes = five above + `/{id}/targets`.

### Cadence (8) - regular launches (scheduled/recurring Voyage), [ADR-046](../adr/0046-cadence.md) / [ADR-048](../adr/0048-conductor.md)

Schedules that spawn a regular Voyage run in time (registry `cadences`). Executes the [Conductor](conductor.md) trigger (leader-elected subsystem inside `keeper`, source-of-truth behavior). **REST-only - no MCP-tools** ([mcp-tools/cadences.md](mcp-tools/cadences.md)). All eight routes are mounted only with the Cadence registry configured; in the absence of (`router.go`: `if cadenceH != nil`), the block `/v1/cadences*` **is not mounted** → `404`. `cadence.*` - NoSelector. Endpoint details, body shapes, two-level RBAC - [operator-api/cadences.md](operator-api/cadences.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/cadences` | `cadence.create` (+ Voyage-perm by `kind`) | — (REST only) |
| `GET` | `/v1/cadences` | `cadence.list` | — (REST only) |
| `GET` | `/v1/cadences/{id}` | `cadence.list` | — (REST only) |
| `PATCH` | `/v1/cadences/{id}` | `cadence.update` (+ Voyage-perm by `kind`) | — (REST only) |
| `DELETE` | `/v1/cadences/{id}` | `cadence.delete` | — (REST only) |
| `POST` | `/v1/cadences/{id}/enable` | `cadence.enable` OR `cadence.update` | — (REST only) |
| `POST` | `/v1/cadences/{id}/disable` | `cadence.disable` OR `cadence.update` | — (REST only) |
| `GET` | `/v1/cadences/{id}/runs` | `incarnation.history` | — (REST only) |

**Two-level RBAC** (security-critical fail-closed, [ADR-046 §7](../adr/0046-cadence.md)): `cadence.create`/`cadence.update` gates middleware, but the recipe spawns Voyage on behalf of the creator - therefore, the create/patch handler additionally requires Voyage-permission by `kind` recipe (`scenario`→`incarnation.run`, `command`→`errand.run`), otherwise Cadence would become a privilege-escalation bypass of RBAC → `403`. `enable`/`disable` - OR gate `cadence.enable|disable` OR backcompat `cadence.update`. `/runs` (child Voyage) reuse `incarnation.history` (parity Voyage-list). **Floor limit:** `interval_seconds < 30` → `422` (sub-30s reaction - via Beacons, [ADR-030](../adr/0030-vigil-oracle.md)). Mutating routes (`create`/`update`/`delete`/`enable`/`disable`) are audited ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `list`/`get`/`runs` - read-only, no audit.

### Oracle (8) - Vigil / Decree registries (event-driven monitoring), [ADR-030](../adr/0030-vigil-oracle.md)

CRUD registry Beacons: Vigil (Soul-side check) and Decree (rule reactor: Portent → match → enqueue scenario). `vigil.*`/`decree.*` - NoSelector. Connect only when the Oracle registry is configured. The source of truth for semantics, bodies, error codes is [operator-api/oracle.md](operator-api/oracle.md); MCP side - [mcp-tools/oracle.md](mcp-tools/oracle.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/vigils` | `vigil.create` | `keeper.oracle.vigil.create` |
| `GET` | `/v1/vigils` | `vigil.list` | `keeper.oracle.vigil.list` |
| `GET` | `/v1/vigils/{name}` | `vigil.list` | `keeper.oracle.vigil.list` |
| `DELETE` | `/v1/vigils/{name}` | `vigil.delete` | `keeper.oracle.vigil.delete` |
| `POST` | `/v1/decrees` | `decree.create` | `keeper.oracle.decree.create` |
| `GET` | `/v1/decrees` | `decree.list` | `keeper.oracle.decree.list` |
| `GET` | `/v1/decrees/{name}` | `decree.list` | `keeper.oracle.decree.list` |
| `DELETE` | `/v1/decrees/{name}` | `decree.delete` | `keeper.oracle.decree.delete` |

4-segment MCP-tool `keeper.oracle.<resource>.<action>` ↔ 2-segment permission `<resource>.<action>` (resource `vigil`/`decree`; one permission covers list+get). Reactor flow (Portent → match Decree → enqueue) by these permissions is **NOT controlled** - this is a machine Soul-initiated path ([rbac.md §Oracle](rbac.md)). Mutating 4 routes (vigil/decree create/delete) are audited; list/get - read-only, no audit.

### Push-Provider (5) - registry env-payload params SSH push-flow plugins, [ADR-032](../adr/0032-push-orchestrator.md) amendment S7-2

CRUD registry `push_providers` (per-provider params of the SSH plugin; long-term canon instead of `keeper.yml::push.providers[]`). Sensitive params (`secret_id`/`token`/`password`/`private_key`) MUST be vault-refs. `push-provider.*` - NoSelector. Connect only when the registry is configured. The source of truth for semantics, bodies, error codes is [operator-api/push-providers.md](operator-api/push-providers.md); MCP side - [mcp-tools/push-providers.md](mcp-tools/push-providers.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/push-providers` | `push-provider.create` | `keeper.push-provider.create` |
| `GET` | `/v1/push-providers` | `push-provider.list` | `keeper.push-provider.list` |
| `GET` | `/v1/push-providers/{name}` | `push-provider.read` | `keeper.push-provider.read` |
| `PUT` | `/v1/push-providers/{name}` | `push-provider.update` | `keeper.push-provider.update` |
| `DELETE` | `/v1/push-providers/{name}` | `push-provider.delete` | `keeper.push-provider.delete` |

5 tools 1:1 `keeper.push-provider.<verb>` ↔ permission `push-provider.<verb>` ↔ REST. `read` (one entry) is separate from `list` - parallel to `operator.read`↔`operator.list`. Mutating 3 routes (`create`/`update`/`delete`) are audited; `list`/`read` - read-only, no audit. After committing the mutation - cluster-wide invalidate via Redis pub/sub `push-providers:changed`.

### Herald (5) - register of delivery channels for notifications of runs, [ADR-052](../adr/0052-herald-notifications.md)

CRUD registry `heralds` (notification delivery channel; webhook in MVP). SSRF circuit (https-only + deny private IPs) is enabled by default, disabled by per-Herald opt-out flags `config.http_allowed` / `config.allow_private`; `secret_ref` - vault-ref to signing-token (webhook signature `X-SoulStack-Signature: sha256=<hex>`, HMAC-SHA256). `herald.*` - NoSelector. Connect only when the registry is configured (`router.go`: `if heraldH != nil`). Source of truth for semantics, bodies, error codes - [operator-api/heralds.md](operator-api/heralds.md); MCP side - [mcp-tools/heralds.md](mcp-tools/heralds.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/heralds` | `herald.create` | `keeper.herald.create` |
| `GET` | `/v1/heralds` | `herald.list` | `keeper.herald.list` |
| `GET` | `/v1/heralds/{name}` | `herald.read` | `keeper.herald.read` |
| `PUT` | `/v1/heralds/{name}` | `herald.update` | `keeper.herald.update` |
| `DELETE` | `/v1/heralds/{name}` | `herald.delete` | `keeper.herald.delete` |

5 tools 1:1 `keeper.herald.<verb>` ↔ permission `herald.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/heralds*`. `read` is separate from `list` (parallel `operator.read`↔`operator.list`). Mutating 3 routes (`create`/`update`/`delete`) are audited - audit events `herald.created` / `herald.updated` / `herald.deleted` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `list`/`read` - read-only, no audit. `PUT` - replace semantics (complete replacement of mutable fields, not PATCH), like Push-Provider. After committing the mutation - cluster-wide invalidate dispatcher cache via Redis pub/sub `herald:invalidate`. Delivery terminals (`herald.delivered` / `herald.failed`) are written by workers, not CRUD routes.

### Tiding (5) - register of notification subscription rules, [ADR-052](../adr/0052-herald-notifications.md)

CRUD registry `tidings` (subscription rule: which `event_types` to respond to → which Herald to deliver). `event_types` - area-glob in the scope of runs (`scenario_run.*` / `command_run.*` / `voyage.*` / `cadence.*` + point `incarnation.drift_checked` and `incarnation.run_completed`); arbitrary wildcard is prohibited. `herald` - FK to existing Herald. Opt. selector `task` (address `register ∪ id`) subscribes to change a specific task and matches only `incarnation.run_completed` by its `changed_tasks` ([ADR-052 §l](../adr/0052-herald-notifications.md)). `tiding.*` - NoSelector. Connect only when the registry is configured. The source of truth for semantics, bodies, error codes is [operator-api/tidings.md](operator-api/tidings.md); MCP side - [mcp-tools/tidings.md](mcp-tools/tidings.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/tidings` | `tiding.create` | `keeper.tiding.create` |
| `GET` | `/v1/tidings` | `tiding.list` | `keeper.tiding.list` |
| `GET` | `/v1/tidings/{name}` | `tiding.read` | `keeper.tiding.read` |
| `PUT` | `/v1/tidings/{name}` | `tiding.update` | `keeper.tiding.update` |
| `DELETE` | `/v1/tidings/{name}` | `tiding.delete` | `keeper.tiding.delete` |

5 tools 1:1 `keeper.tiding.<verb>` ↔ permission `tiding.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/tidings*`. `read` is separate from `list`. Mutating 3 routes are audited - audit events `tiding.created` / `tiding.updated` / `tiding.deleted`; `list`/`read` - read-only, no audit. `PUT` - replace semantics (like Herald). Link to missing Herald (`herald` FK) on create/update → `404`. Demolition of the Herald channel cascades away its Tiding subscriptions (`tidings.herald ON DELETE CASCADE`). Valid `event_types` subscriptions - from the `GET /v1/event-types` directory (UI fetches, not hardcode); the same scope validates CRUD Tiding (arbitrary wildcard / type outside scope → `422`).

### Choir (6) - named topology of hosts within an incarnation, [ADR-044](../adr/0044-choir.md)

CRUD topology Choir/Voice inside incarnation (`/v1/incarnations/{name}/choirs*`). Choir belongs to incarnation → the same scope selector `incarnation`/`service`/`coven` (via path-`{name}`) as incarnation mutations. Connect only when the ChoirDB pool is configured. **REST-only - no MCP-tools** ([mcp-tools/choirs.md](mcp-tools/choirs.md)). The source of truth for semantics, bodies is [operator-api/choirs.md](operator-api/choirs.md).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/incarnations/{name}/choirs` | `choir.create` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/choirs` | `choir.list` | — (REST only) |
| `DELETE` | `/v1/incarnations/{name}/choirs/{choir}` | `choir.delete` | — (REST only) |
| `POST` | `/v1/incarnations/{name}/choirs/{choir}/voices` | `choir.add-voice` | — (REST only) |
| `GET` | `/v1/incarnations/{name}/choirs/{choir}/voices` | `choir.list` | — (REST only) |
| `DELETE` | `/v1/incarnations/{name}/choirs/{choir}/voices/{sid}` | `choir.remove-voice` | — (REST only) |

`choir.*` - incarnation-scope (via `IncarnationScopeSelector` on path-`{name}`); Voice-actions hyphenated (`add-voice`/`remove-voice`) according to the grammar `<resource>.<action>`. `list` covers both the Choir list and the Voice list. Mutating-CRUD is audited (payload is written by the handler itself - choir/voice-snapshot is available only after mutation).

### Self-describing (3) - rights directories / event-types and effective rights, [ADR-042](../adr/0042-backend-driven-ui.md)

Self-describing read routes for permission-aware UI. **Auth-only** (`RequireJWT` on `/v1/*`), **WITHOUT `RequirePermission`** - requirement of the right to read the list of rights/values ​​= chicken-egg (architect-verdict); `/me/permissions` gives EXACTLY its rights (AID from claims, not query). **REST-only - no MCP tools.** Read-only, without audit (health/meta pattern).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/permissions` | — (auth-only) | — (REST only) |
| `GET` | `/v1/event-types` | — (auth-only) | — (REST only) |
| `GET` | `/v1/me/permissions` | — (auth-only) | — (REST only) |

`GET /v1/permissions` - machine-readable RBAC-permissions directory (source - `rbac.catalog.go`), UI fetches real names to assign role rights. `GET /v1/event-types` - machine-readable directory of event-types valid for [Tiding](operator-api/tidings.md) subscription (source - `herald/eventtypes.go`, the same scope that validates CRUD Tiding); UI Tiding forms fetch valid types instead of hardcode. Body - two groups: `areas` (areas of area-glob-subscription, finished form `<area>.*` - `scenario_run.*`/`command_run.*`/`voyage.*`/`cadence.*`) + `point_events` (dot types outside area-glob - `incarnation.drift_checked`/`incarnation.run_completed`). `GET /v1/me/permissions` — effective rights of the current Archon (show/hide buttons). All three are always mounted (static from packages `rbac`/`herald` / snapshot of the enforcer, without external dependencies).

### Cloud (8) - Cloud-Provider / Cloud-Profile registries, [ADR-017](../adr/0017-keeper-side-core.md) / [cloud.md](cloud.md)

CRUD registries `providers` (cloud accounting) and `profiles` (VM-spec on top of Provider) in Postgres, managed via OpenAPI/MCP. `provider.*` / `profile.*` - NoSelector (CRUD operates on the registry itself, like `service.*` / `push-provider.*`). **Immutability:** `update`-operations **no** - changing parameters = `delete` + `create` (protection against partial mutation spec of already-living VMs), so read-visibility gates one permission `provider.read` / `profile.read`. `credentials_ref` accepts the string `vault:<mount>/<path>`; The credentials API themselves **DO NOT resolve or return** (secret hygiene). Source of truth for semantics, bodies - [cloud.md → Provider and Profile](cloud.md); MCP side - [mcp-tools.md → Cloud](mcp-tools.md#cloud-8). Routes are mounted only when the registry is configured (`Deps.ProviderSvc` / `Deps.ProfileSvc`).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/providers` | `provider.create` | `keeper.provider.create` |
| `GET` | `/v1/providers` | `provider.read` | `keeper.provider.list` |
| `GET` | `/v1/providers/{name}` | `provider.read` | `keeper.provider.get` |
| `DELETE` | `/v1/providers/{name}` | `provider.delete` | `keeper.provider.delete` |
| `POST` | `/v1/profiles` | `profile.create` | `keeper.profile.create` |
| `GET` | `/v1/profiles` | `profile.read` | `keeper.profile.list` |
| `GET` | `/v1/profiles/{name}` | `profile.read` | `keeper.profile.get` |
| `DELETE` | `/v1/profiles/{name}` | `profile.delete` | `keeper.profile.delete` |

Permission mapping: `POST`→`<resource>.create`, `GET`(list + get-`{name}`)→`<resource>.read`, `DELETE`→`<resource>.delete`. Mutating 4 routes (create/delete for each entity) are audited - audit events `provider.created` / `provider.deleted` / `profile.created` / `profile.deleted` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `provider.read`/`profile.read` (list + get) - read-only, without audit (audit Profile-create only writes `params` keys, not values). Boundary cases: `409 provider-already-exists` / `409 profile-already-exists` per double `name`; `409 provider-has-profiles` when deleting a Provider with associated Profiles (FK `ON DELETE RESTRICT`, migration 020 - first delete dependent Profiles); `422 validation-failed` to the Profile link to a non-existent Provider (FK) or broken `name`/`type`/`region`/`credentials_ref`; `404 not-found` on get/delete missing entry. 3-segment MCP-tool `keeper.<resource>.<verb>` ↔ 2-segment permission `<resource>.<verb>` (read-tool named `get`, permission verb - `read`).

### Service (9) - registry of Services (CRUD + git projections), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / [service/manifest.md](../service/manifest.md)

Registry `service_registry`: directory `services[]` is moved from static `keeper.yml` to managed-via-OpenAPI/MCP PG-table ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). `service.*` - NoSelector (CRUD operates on the registry itself). The source of truth for semantics, bodies, image invalidation is `serviceregistry.Service`; MCP side - [mcp-tools.md → Service](mcp-tools.md#service-4).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/services` | `service.register` | `keeper.service.register` |
| `GET` | `/v1/services` | `service.list` | `keeper.service.list` |
| `GET` | `/v1/services/{name}` | `service.list` | `keeper.service.list` |
| `PATCH` | `/v1/services/{name}` | `service.update` | `keeper.service.update` |
| `DELETE` | `/v1/services/{name}` | `service.deregister` | `keeper.service.deregister` |
| `GET` | `/v1/services/{name}/refs` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/scenarios` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/state-schema` | `service.list` | — (REST only) |
| `GET` | `/v1/services/{name}/dependencies` | `service.list` | — (REST only) |

Permission mapping: `POST`→`service.register`, `GET`(list + get-`{name}`)→`service.list`, `PATCH`→`service.update`, `DELETE`→`service.deregister`. Mutating 3 routes (`register`/`update`/`deregister`) are audited ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); readings - read-only, without audit. Four git projections (`/refs` - tags and branches for Upgrade-modal; `/scenarios` - dropdown Run-modal; `/state-schema` - Schema explorer; `/dependencies` - destiny/module dependencies) reuse `service.list` (projections of one Service record, without separate permission and without MCP-tools); if the external git source fails - `502`. Routes are connected only when the Service registry is configured.

**The scenario directory `GET /v1/services/{name}/scenarios`** contains for each scenario the field **`runnable: bool`** - the sign "launched by the operator from the Run-form". Marked by Keeper according to the canon of the scenario package (`IsRunnableScenario`), not from the manifest: `create` = `true`, `destroy` = `false` (special deletion flow via `DELETE /v1/incarnations/{name}`), operational scenarios (including `converge`) = `true`. The UI filters the Run-form by `runnable`, and not by the name hardcode ([ADR-042](../adr/0042-backend-driven-ui.md), [architecture.md → Service](../architecture.md)).

### Sigil-key (4) - rotation of Sigil signature keys, [ADR-026(h)](../adr/0026-sigil.md) / R3

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/sigil/keys` | `sigil.key-introduce` | `keeper.sigil.key.introduce` |
| `GET` | `/v1/sigil/keys` | `sigil.key-list` | `keeper.sigil.key.list` |
| `POST` | `/v1/sigil/keys/{key_id}/primary` | `sigil.key-set-primary` | `keeper.sigil.key.set-primary` |
| `DELETE` | `/v1/sigil/keys/{key_id}` | `sigil.key-retire` | `keeper.sigil.key.retire` |

3-segment MCP-tool `keeper.sigil.key.<verb>` ↔ 2-segment permission `sigil.key-<verb>` (resource `sigil`, hyphenated action - permission grammar exactly `<resource>.<action>`).

### Plugin (3) — Sigil allow-list plugin integrity, [ADR-026](../adr/0026-sigil.md) S4a

Admission/revocation/list of entries of the allow-list `plugin_sigils` (the plugin binary passes Sigil verification only with active admission). A separate area from Sigil-key above: that one is about the **signature keys**, this one is about the permissions of the binaries themselves. `plugin.*` - NoSelector. The source of truth for semantics, bodies and `sha256`-computation is [plugins.md → Integrity-model](plugins.md#integrity-model); MCP side - [mcp-tools.md → Plugin](mcp-tools.md#plugin-3).

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/plugins/sigils` | `plugin.allow` | `keeper.plugin.allow` |
| `GET` | `/v1/plugins/sigils` | `plugin.list` | `keeper.plugin.list` |
| `DELETE` | `/v1/plugins/sigils/{namespace}/{name}/{ref}` | `plugin.revoke` | `keeper.plugin.revoke` |

Mutating 2 routes (`allow`/`revoke`) are audited (supply-chain-mutations, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `plugin.list` - read-only, no audit. Routes are mounted only when Sigil is configured (`keeper.yml → sigil.signing_key_ref`); when Sigil is turned off, the block `/v1/plugins/sigils*` **is not mounted** - the request is caught by catch-all → `404 not-found` (`router.go`: `if sigilH != nil`).

### Module-catalog (3) - read-only module catalog for UI Run→Command, [ADR-042](../adr/0042-backend-driven-ui.md)

Machine-readable directory of core modules (doc-data from core-registry) + active plugin permissions to search for a module in UI Run→Command. **REST-only - no MCP tools.** Reuse permission `service.list` (read-only directory, new permission is not created). `form-prep` — resolver of source directories of the module UI form (`incarnation_hosts`/`choir` → live autocomplete SIDs), reuse sub-run permission `incarnation.run`.

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/modules` | `service.list` | — (REST only) |
| `GET` | `/v1/modules/{name}` | `service.list` | — (REST only) |
| `POST` | `/v1/modules/{name}/form-prep` | `incarnation.run` | — (REST only) |

All three read-only/resolve routes are without audit (pattern `service.list` / `soul.list`). Selector - NoSelector (global directory, resolve cluster-wide by `souls`).

### Augur (7) - Omen / Rite registries, [ADR-025](../adr/0025-augur.md) / [augur.md](augur.md)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `POST` | `/v1/augur/omens` | `omen.create` | `keeper.augur.omen.create` |
| `GET` | `/v1/augur/omens` | `omen.list` | `keeper.augur.omen.list` |
| `GET` | `/v1/augur/omens/{name}` | `omen.list` | `keeper.augur.omen.list` |
| `DELETE` | `/v1/augur/omens/{name}` | `omen.delete` | `keeper.augur.omen.delete` |
| `POST` | `/v1/augur/rites` | `rite.create` | `keeper.augur.rite.create` |
| `GET` | `/v1/augur/rites` | `rite.list` | `keeper.augur.rite.list` |
| `DELETE` | `/v1/augur/rites/{id}` | `rite.delete` | `keeper.augur.rite.delete` |

4-segment MCP-tool `keeper.augur.<resource>.<action>` ↔ 2-segment permission `<resource>.<action>` (resource `omen`/`rite`). Live-fetch from Soul (`AugurRequest`) is NOT controlled by these permissions ([rbac.md §Augur](rbac.md)).

### Audit (1) — read-only audit-events tape, [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)

| Method | Path | Permission | MCP-tool |
|---|---|---|---|
| `GET` | `/v1/audit` | `audit.read` | — (UI iter 2) |

Read-only event feed `audit_log` for UI iteration 2 (placeholder `/audit`). The Read-endpoint itself is NOT written to audit-trail (we avoid recursion - each GET would double the table). MCP-tool-symmetry deferred.

**Total: 4 health/meta on the API facade (`/healthz`, `/readyz`, `/openapi.yaml`, `/openapi.json`) + 132 endpoints under permissions/auth-only** (Operator 5 + Audit 1 + Role 6 + Synod 8 + Incarnation 15 + Runs 2 + Choir 6 + Soul 8 + Errand 4 + Plugin 3 + Sigil-key 4 + Service 9 + Module-catalog 3 + Self-describing 3 + Augur 7 + Oracle 8 + Push 2 + Push-runs 1 + Push-Provider 5 + Cloud 8 + Herald 5 + Tiding 5 + Voyage 6 + Cadence 8) **= 132 route in this table.** `/metrics` is **not included in this facade account** - Prometheus endpoint is placed on a separate metrics-listener (`listen.metrics.addr`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)), not mounted in `router.go` facade. (Voyage "(6)" - five MCP-paired/RBAC-by-kind lines + sixth REST route `GET /v1/voyages/{id}/targets`, read/REST-only. Augur - 7 routes: 4 omen + 3 rite. Cloud - 8 routes: 4 provider (create/list/get/delete) + 4 profile, implemented and mounted.)

> **Unmixed routes (TODO - sections have not yet been written).** In [`router.go`](../../keeper/internal/api/router.go) mounted, but not yet summarized in the tables above: `GET /v1/souls/stats` (Souls Overview unit, `soul.list`), `POST /v1/souls/traits` (bulk trait-assign, `soul.traits-assign` - **deprecated**, replaced by `PUT /v1/incarnations/{name}/traits`, ADR-060), `GET`/`PUT /v1/provisioning-policy` (`provisioning.read`/`provisioning.update`, [ADR-058](../adr/0058-operator-auth-ldap-oidc.md) Part B), `GET /v1/herald-types` (auth-only Herald channel type directory, [ADR-042](../adr/0042-backend-driven-ui.md)-pattern), `GET /v1/cluster` (HA-topology of the Keeper cluster, existence-gate `soul.list`). Login routes `/auth/ldap/login` + `/auth/oidc/{login,callback}` - outside `/v1` (public login before JWT, parity `/healthz`; ADR-058) and are not included in the `/v1` route account. Forms and semantics - in the derivative [`openapi.yaml`](openapi.yaml); These routes are not included in the counter above.

> **Tool-account vs route-account are legitimately different sets.** The section headings in [mcp-tools.md](mcp-tools.md) are considered **MCP-tools**, and here they are **REST-routes**; one domain can carry more REST routes than MCP tools. Reconciliation with code (`router.go` + `keeper/internal/mcp/manifest.go`):
>
> - **Incarnation** - REST "(15)" (router) vs MCP "(11)" (`manifest.go`). The difference is NOT an error: four REST-only routes without an MCP tool - `PATCH /v1/incarnations/{name}/hosts` (tool `keeper.incarnation.hosts.update` is missing in `manifest.go`), `POST …/scenarios/{scenario}/form-prefill` (UI-resolve), `GET …/runs` + `GET …/runs/{apply_id}` (read-view runs under UI); the remaining 11 REST routes (including `PUT .../traits` ↔ `keeper.incarnation.traits-set`, ADR-060) have MCP pairing. The mcp-tools.md header "(11)" is considered MCP-tools, parsed in [mcp-tools/incarnations.md](mcp-tools/incarnations.md). Global `GET /v1/runs` + `/v1/runs/stats` (Runs section) - also REST-only.
> - **Voyage** - REST "(5)" (without `/targets`) vs MCP "(4)" (`preview` REST-only). The sixth REST route `/targets` and `preview` are read/REST-only and do not have an MCP tool.
> - **Cadence / Choir / Self-describing** - there are **no MCP tools at all** (`manifest.go` does not contain them): the headers of these sections lead by the number of REST routes; stub files [mcp-tools/cadences.md](mcp-tools/cadences.md) / [mcp-tools/choirs.md](mcp-tools/choirs.md) record "(0)".

## Endpoint sections

Key endpoints are parsed with the full request/response schema. For the rest - a table of fields without a full JSON example (the example form is derived by analogy).

### Operator endpoints

Moved to a domain file - [operator-api/operator.md → Endpoint sections](operator-api/operator.md): `POST /v1/operators` (create Archon + first JWT), `POST /v1/operators/{aid}/revoke` (review), `POST /v1/operators/{aid}/issue-token` (re-release of JWT), `GET /v1/operators` (list), `GET /v1/operators/{aid}` (detail). MCP side - [mcp-tools/operator.md](mcp-tools/operator.md).

### Audit endpoints

Moved to a domain file - [operator-api/audit.md → Endpoint sections](operator-api/audit.md): `GET /v1/audit` (read-only event feed `audit_log`, filters `type`/`source`/`archon_aid`/`correlation_id`/`started_after`/`started_before` + pagination). MCP side - [mcp-tools/audit.md](mcp-tools/audit.md) (MCP-tool symmetry deferred).

### Incarnation endpoints

Moved to a domain file - [operator-api/incarnations.md → Endpoint sections](operator-api/incarnations.md): `POST /v1/incarnations` (create an instance - select a starting scenario via `create_scenario`, or bare-incarnation if the service does not have create scenarios), `POST …/rerun-last` (restart the last fallen scenario from `error_locked`), `POST …/scenarios/{scenario}` (custom scenario), `GET …/{name}` (spec+state+status), `GET /v1/incarnations` (list), `GET …/history` (state log), `GET …/runs` + `GET …/runs/{apply_id}` (read-view runs: list + per-host details), `POST …/unlock`, `POST …/upgrade`, `GET …/upgrade-paths` (upgrade paths: cheap - registry tags + `is_current`, `?to=` - `direction`/`mode`/`reachable`; [ADR-0068](../adr/0068-service-upgrade-v2.md)), `POST …/check-drift` (Scry), `DELETE …/{name}` (destroy), plus two superseded-by-Voyage Tide sections (historical record). There are also global `GET /v1/runs` + `GET /v1/runs/stats` (page "All Runs", § Runs (2)). MCP side - [mcp-tools/incarnations.md](mcp-tools/incarnations.md).

### Soul endpoints

Moved to a domain file - [operator-api/souls.md → Endpoint sections](operator-api/souls.md): `POST /v1/souls` (registration + bootstrap token), `POST /v1/souls/{sid}/issue-token` (bootstrap token reissue), `GET /v1/souls` (list), plus `POST /v1/souls/coven` (bulk Coven tags). MCP side - [mcp-tools/souls.md](mcp-tools/souls.md).

### Augur endpoints

Moved to a domain file - [operator-api/augur.md → Endpoint sections](operator-api/augur.md): `POST /v1/augur/omens` (create Omen), `GET /v1/augur/omens` (list), `GET /v1/augur/omens/{name}` (read), `DELETE /v1/augur/omens/{name}` (delete), `POST /v1/augur/rites` (create Rite), `GET /v1/augur/rites` (list of Omen Rites), `DELETE /v1/augur/rites/{id}` (delete). The full broker model is [augur.md](augur.md). MCP side - [mcp-tools/augur.md](mcp-tools/augur.md).

### Push endpoints

Moved to a domain file - [operator-api/push.md → Endpoint sections](operator-api/push.md): `POST /v1/push/apply` (Destiny push run via SSH), `GET /v1/push/{apply_id}` (push run state), `GET /v1/push-runs` (global list of push runs). The full push mode model is [push.md](push.md). MCP side - [mcp-tools/push.md](mcp-tools/push.md).

### Cloud endpoints

CRUD registries Cloud-Provider / Cloud-Profile - `POST/GET/DELETE /v1/providers*` + `POST/GET/DELETE /v1/profiles*` (without `PUT`/`PATCH`: Provider/Profile are immutable, changing parameters = `delete` + `create`). Provider-body - `name`/`type`/`region`/`credentials_ref` (`credentials_ref` - line `vault:<mount>/<path>`, the API secret does not resolve or return); Profile body - `name`/`provider` (FK on Provider)/`params`/`cloud_init`. Source of truth for semantics, bodies, edge cases (`409` double-`name`, `409 provider-has-profiles` FK RESTRICT, `422` reference to a non-existent Provider) and Credentials-flow - [cloud.md → Provider and Profile](cloud.md). MCP side - [mcp-tools.md → Cloud](mcp-tools.md#cloud-8).

### Voyage endpoints

Moved to a domain file - [operator-api/voyages.md → Endpoint sections](operator-api/voyages.md): `POST /v1/voyages` (create Voyage, RBAC-by-kind, target-resolve, command∩Purview, Tempo) and `POST /v1/voyages/preview` (dry-resolve scope without creation). MCP side - [mcp-tools/voyages.md](mcp-tools/voyages.md).

### Cadence endpoints

Moved to a domain file - [operator-api/cadences.md → Endpoint sections](operator-api/cadences.md): `POST /v1/cadences` (create schedule, two-level RBAC), `GET /v1/cadences` (list), `GET …/{id}` (detail), `PATCH …/{id}` (update), `DELETE …/{id}` (uncheck), `POST …/{id}/enable|disable` (toggle), `GET …/{id}/runs` (Voyage subsidiaries). Schedule executor behavior - [conductor.md](conductor.md). **MCP side missing** ([mcp-tools/cadences.md](mcp-tools/cadences.md)).

### Oracle endpoints

Moved to a domain file - [operator-api/oracle.md → Endpoint sections](operator-api/oracle.md): `POST/GET/DELETE /v1/vigils*` (Soul-side beacons checks) + `POST/GET/DELETE /v1/decrees*` (reactor rules). MCP side - [mcp-tools/oracle.md](mcp-tools/oracle.md).

### Push-Provider endpoints

Moved to a domain file - [operator-api/push-providers.md → Endpoint sections](operator-api/push-providers.md): `POST/GET/PUT/DELETE /v1/push-providers*` (env-payload params of push-flow SSH plugins; sensitive - vault-refs). MCP side - [mcp-tools/push-providers.md](mcp-tools/push-providers.md).

### Herald endpoints

Moved to a domain file - [operator-api/heralds.md → Endpoint sections](operator-api/heralds.md): `POST/GET/PUT/DELETE /v1/heralds*` (delivery channels for notifications about runs; webhook + SSRF-guard + `secret_ref` vault-ref + `X-SoulStack-Signature`). MCP side - [mcp-tools/heralds.md](mcp-tools/heralds.md).

### Tiding endpoints

Moved to a domain file - [operator-api/tidings.md → Endpoint sections](operator-api/tidings.md): `POST/GET/PUT/DELETE /v1/tidings*` (subscription rules `event_types` → Herald). MCP side - [mcp-tools/tidings.md](mcp-tools/tidings.md).

### Choir endpoints

Moved to a domain file - [operator-api/choirs.md → Endpoint sections](operator-api/choirs.md): `POST/GET/DELETE /v1/incarnations/{name}/choirs*` + voices (named topology of hosts within the incarnation). **MCP side missing** ([mcp-tools/choirs.md](mcp-tools/choirs.md)).

## Full OpenAPI YAML

Full OpenAPI 3.1 YAML spec: [`openapi.yaml`](openapi.yaml) - **derived snapshot** of Operator API forms (paths, methods, bodies, schemas), not the source of truth. The source of truth is the **huma aggregator in Go code** (`HumaFullSpecYAML` / `buildFullOpenAPISpec` in `keeper/internal/api/huma_full_spec.go`), which collects a single 3.1 spec runtime dump of huma operations of all domains ([ADR-054](../adr/0054-openapi-code-first.md)). Committed [`openapi.yaml`](openapi.yaml) is updated by `make gen-openapi` (dump entry) and is intended for UI-vendor + git-review; `make check-openapi` - drift-guard (committed byte-to-byte snapshot == huma-dump). The same dump is returned by `GET /openapi.yaml` self-serving endpoint (via `servedOpenAPIHandler`, without an embed copy). The spec is also used to generate client SDK / TS types on the UI side.

Source separation:
- **Go-types of handlers** (`keeper/internal/api/huma_*.go`) - form: paths, methods, request/response schemas, schema names, JSON fields, enum values (via native enum directory `huma_enums.go`);
- derivative [`openapi.yaml`](openapi.yaml) - a snapshot of this form for external consumers;
- this document is the standardization of transport and semantics: status codes, permissions, conventions, mapping endpoint ↔ MCP-tool ↔ permission, edge cases.

Markdown ↔ code ↔ YAML correspondence: CamelCase schema names from this document are derived by huma from handler Go-structs of the same name and end up in `components.schemas.<Name>` of the YAML derivative; snake_case JSON fields are the `json:"…"` tags of these structs. Enum values ​​in code and in JSON are short forms (`ready`) according to the rule [§ Conventions → Enum serialization](#conventions).

## See also

- [rbac.md](rbac.md) - permissions directory, selector grammar, Bootstrap of the first Archon.
- [mcp-tools.md](mcp-tools.md) - MCP side of the directory: transport, auth, tool declaration format, `_apply_id`-convention, error mapping.
- [config.md → `listen.openapi.addr`](config.md#listen) — bind address of the facade. [config.md → `auth`](config.md#auth) - JWT signature.
- [push.md](push.md) - push mode model, source of truth for `POST /v1/push/apply`.
- [cloud.md](cloud.md) - Provider/Profile semantics and Credentials-flow (REST routes `/v1/providers` / `/v1/profiles` are implemented and mounted, see § Cloud).
- [plugins.md](plugins.md) - `profile_schema` / `params_schema` plugins, used in `422 validation-failed`.
- [storage.md](storage.md) - registry `operators`, `souls`, `incarnation`, `state_history` in Postgres.
- [`../architecture.md → ADR-013`](../adr/0013-bootstrap-archon.md) - Bootstrap of the first Archon.
- [`../architecture.md → ADR-014`](../adr/0014-operator-identity.md) - identity model, JWT-claims.
- [`../architecture.md → ADR-019`](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) — state_schema migrations, upgrade cycle.
- [`../architecture.md → Incarnation`](../architecture.md) - record structure, `state_history`, `error_locked`-semantics.
- [`../naming-rules.md`](../naming-rules.md) - name dictionary (Archon, AID, KID, SID).
- [`../requirements.md`](../requirements.md) - OpenAPI and MCP as end-to-end requirements.
