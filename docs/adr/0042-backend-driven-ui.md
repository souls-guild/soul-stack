# ADR-042. Backend-driven dynamic data in the UI — the UI does not hardcode dynamic catalogs.

**Context.** The companion UI [`soul-stack-web`](https://github.com/co-cy/soul-stack-web) ([ADR-035](0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui)) consumed some of the dynamic catalogs as hardcode — for example, the list of RBAC permissions was baked into TS. This spawned a class of bugs: the backend added/renamed a permission (`soul.read`), the UI did not know about it → a request with `unknown_permission`, a desync of UI ↔ backend catalog. The module catalog, meanwhile, was already served by an endpoint (`GET /v1/modules`) — the pattern is inconsistent. A common principle is needed: what may be hardcoded in the UI and what the backend must serve.

**Decision** (variant A2, propose-and-wait passed with architect 2026-05-29).

1. **The UI does NOT hardcode dynamic catalogs.** Any catalog that the backend centrally validates or that is extended without a UI release is served via an OpenAPI catalog endpoint; the UI fetches it at runtime.

2. **The backend serves identifiers + machine metadata**, not human text: `resource`/`action` for a permission, `selector_keys`, enum values, `required`/`secret` flags, etc. Human labels and translation are on the UI side (i18n) with a **graceful fallback to the identifier**: no label → the identifier itself is shown, the UI does not crash.

3. **Boundary** (the core of the ADR):
   - **Backend-driven (the UI must fetch):** RBAC permission catalog, module catalog, enums of run / incarnation / errand-run / tide statuses, keys and types of targeting selectors, and any closed catalog that **(a)** the backend centrally validates or **(b)** is extended without a UI release.
   - **Allowed in the UI:** markup / layout / icons / color tokens, i18n strings and human labels, local user preferences. **Criterion:** "does not affect whether the backend accepts a request and does not grow backend-side".

**Consequences.**
- A new permission / module / status in the backend is visible in the UI **without a UI release** — at minimum as an identifier.
- The desync of UI hardcode with the backend catalog is eliminated as a class of bugs.
- The cost — the UI makes an additional fetch of catalogs (cacheable).

**Instances of the principle.**
- `GET /v1/modules` — module catalog (already exists).
- `GET /v1/permissions` — RBAC permission catalog (introduced by this ADR).
- `GET /v1/event-types` — event-type catalog for Tiding subscription (source [`herald/eventtypes.go`](https://github.com/co-cy/soul-stack/blob/main/keeper/internal/herald/eventtypes.go), ADR-052).

The cross-cutting requirement for the web component is recorded in [requirements.md](../requirements.md); OpenAPI remains the sole contract between core ↔ web ([ADR-035](0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui)).
