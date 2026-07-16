# Threat-model Soul Stack

Fixed threat model for the Keeper cluster + Souls fleet. Reference for the operator and development team: what is considered an asset, what actors and surfaces are considered, what is covered by the mechanism and what remains a residual risk. **Not marketing and not a tutorial** - a regulatory fixation of security boundaries on the path to beta.

The document was recorded based on the results of the security audit 2026-06-12 (verdict PASS). He does not introduce new solutions - he describes already implemented mechanisms; Each item refers to a code/ADR source. The discrepancy between this document and the actual behavior of the code is a bug (create it as a security-issue, and do not edit the document for the code).

See also: [`../keeper/rbac.md`](../keeper/rbac.md) (RBAC/Purview/least-privilege), [`../operations/bootstrap-rbac.md`](../operations/bootstrap-rbac.md) (Archon Bootstrap), [`../keeper/prod-setup.md`](../keeper/prod-setup.md) (prod-Vault: AppRole/auto-unseal/signing-key), [ADR-026 → Sigil](../adr/0026-sigil.md), [ADR-047 → Purview](../adr/0047-purview.md).

## Assets

What we protect and why compromise is critical.

| Active | Where does he live | Why is it critical |
|---|---|---|
| **Vault-secrets** (Essence/passwords) | Vault KV, resolved in the CEL rendering phase on Keeper | Host config parameters, database/service passwords. Leak = compromise of managed services. Should never fall into observable channels (see invariant below). |
| **mTLS-CA** (SoulSeed Trust Root) | Vault PKI root | Every SoulSeed subscribes. CA compromise = the ability to issue a valid client certificate and impersonate any Soul or join EventStream. |
| **JWT signing-key** | Vault KV `secret/keeper/jwt-signing-key` ([ADR-014](../adr/0014-operator-identity.md)) | Signs the JWT of all Archons. Compromise = issuing a token with any roles (full RBAC bypass). |
| **Sigil trust-anchor** (ed25519 plugin-signing) | private - Vault KV `secret/keeper/sigil-keys/<key_id>`; public recruitment goes to Soul in `BootstrapReply` ([ADR-026(d)/(h)](../adr/0026-sigil.md)) | Signs accepted plugin digests. Compromise = ability to sign an arbitrary plugin binary (RCE on the fleet via a forged `soul-mod-*`). |
| **Souls Fleet** | `soul`-daemons on hosts | RCE surface: modules are executed on the host. Compromise of Keeper management = execution of arbitrary code throughout the fleet. |
| **Postgres** | registries (`souls`/`operators`/`voyages`/…) + audit | Cold state of the cluster and audit log. Compromise = substitution of registries, erasing traces. |
| **Redis** | presence/lease/leader | Hot layer: presence Souls, SID-lease, leader-election Reaper/Conductor. Compromise = false presence, lease hijacking, split-brain. |

## Actors, surfaces and boundaries (which is closed)

For each actor: entry point (surface), closing mechanism and **residual risk** (what is not deliberately closed).

### External operator (semi-trusted)

- **Surface:** Operator API - OpenAPI (HTTP/JSON) / MCP / CLI as a thin wrapper. Including the served spec (`GET /openapi.yaml` + `GET /openapi.json`) and the visual viewer `GET /docs` (RapiDoc) - see below for their access.
- **Closed:**
  - JWT authentication (claims `iss`/`sub`/`exp`/`roles`, JWT signing-key from Vault, [ADR-014](../adr/0014-operator-identity.md)). Applies to `/v1/*` **and** to the service spec `/openapi.yaml`/`/openapi.json` (same `RequireJWT`, see below).
  - RBAC **default-deny** - each endpoint requires explicit permission ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), [`../keeper/rbac.md`](../keeper/rbac.md)).
  - **Purview-scope** — the visibility of nodes is limited by the scope of the role (coven/regex/soulprint/state-selector); fail-closed (empty Purview → empty list, not the entire fleet) ([ADR-047](../adr/0047-purview.md)).
  - **Least-privilege subset** — Archon cannot issue a role with rights greater than his own (`keeper/internal/rbac`, subset-invariant takes into account roles via Synod, [ADR-049 §f](../adr/0049-synod.md)).
  - Closed: **privilege-escalation** (via subset check), **bypass via MCP** (MCP-tools go through the same enforcer as REST), **self-lockout** (you cannot remove the last statement with `*`-permission, [ADR-013](../adr/0013-bootstrap-archon.md)).
- **API surface disclosure (served spec and viewer, ADR-054 code-first):**
  - **Spec is generated from code (huma-aggregator), not from a committed manuscript.** `/openapi.yaml` and `/openapi.json` give a runtime dump of one source-of-truth (huma-spec, OpenAPI 3.1) - "truth in code". Committed `docs/keeper/openapi.yaml` is a derived generator for UI-vendor (`make gen-openapi`), it is NOT served. Drift code↔spec is caught by the test `TestFullSpec_CoversAllRoutes` (`keeper/internal/api/huma_full_spec_test.go`: set of spec routes == set of real chi-routes).
  - **`/openapi.yaml` + `/openapi.json` - FOR JWT.** Both require Bearer (same `RequireJWT` as `/v1`), but mounted OUTSIDE `/v1` and without `/v1`-binding (maxBody/metrics/audit/RBAC): the sinter is static. Previously, the service spec was public - it revealed the full API surface anonymously; anonymous access to the list of endpoints is now closed (`keeper/internal/api/router.go`).
  - **`/docs` (RapiDoc) - public shell without API disclosure (mechanism A, ADR-054 doc-viewer).** `/docs` itself and static `/docs/assets/*` are public and do not carry data/API description - this is an empty HTML page + web-component RapiDoc. The sensitive (full spec) comes only AFTER the operator has entered the JWT: the page fetches `/openapi.json` with `Authorization: Bearer` and renders the object inline. The token is kept in the tab's `sessionStorage` (XSS hygiene, does not survive tab closing). RapiDoc assets are embedded via `go:embed` (`docsassets`) - no CDN, offline-render, no loading of third-party JS.
- **Residual risk:** not highlighted above what is covered above; the main one is the insider operator (see below).

### Compromised Soul

- **Surface:** long-lived bidi `EventStream` over mTLS ([ADR-012](../adr/0012-keeper-soul-grpc.md)).
- **Closed:**
  - Authentication **fingerprint → SID** by peer certificate: SID is taken from mTLS peer cert (`keeper/internal/grpc/peer.go`), **not from payload** - echo SID in messages is only for logs, authority is the certificate.
  - **Seed-rotation of only your own SID** - Soul cannot rotate someone else's SoulSeed.
  - **Augur default-deny** — external access of Soul (Vault/Prometheus/ELK) is allowed only by an explicit grant (`rites`, [ADR-025](../adr/0025-augur.md)); default is nothing.
- **Residual risk (by-design):** modules are executed on the host with service-user rights. This is a property of the pull model (Soul applies Destiny locally) - Keeper does not trust the host more than the rights of this process. The Blast radius of compromising one Soul is limited to that host; There is no cross-host escalation via EventStream (SID-auth + Augur default-deny).

### Network attacker (MITM / SSRF)

- **Surface:** Keeper↔Soul transport and **outgoing** Keeper connections to untrusted targets (Herald webhook delivery, `core.url`/`core.http` via Augur delegation).
- **Closed:**
  - **TLS 1.3 + `RequireAndVerifyClientCert`** on EventStream (`shared/tlsx`, `keeper/internal/grpc/auth.go`) - no downgrade and no skip-verify in production.
  - **SSRF-guard** on all outgoing to untrusted ones: `shared/netguard` - **resolve-then-dial, rebind-safe** (`ValidateEndpoint` → `GuardedDialContext` connects via an already verified IP, there is no second resolve between the check and dial; `NewCheckRedirect` gates redirect hops). Blocks direct access to private IPs and DNS rebind.
- **Residual risk:** Correctness depends on correctly configured Vault PKI role (`enforce_hostnames`/`allowed_domains`) - see environment requirements.

### Insider Operator

- **Surface:** legitimate Archon with issued rights.
- **Closed:**
  - Limited by **RBAC-scope + least-privilege** (sees and touches only nodes of its PurView, no more rights than issued).
  - **Audit with masked-payload** - each write action is written to `audit_log`; parameters are masked at the output (`shared/audit/mask.go`), secrets are not included in the log. The completeness of audit coverage of write routes is maintained by the aggregate structural guard (see the invariant below about anti-S6).
- **Residual risk (by-design):** `cluster-admin` (`*`-permission) has full access - this is a required role (bootstrap, recovery). Minimizing risk: keep a minimum of `*` operators, self-lockout protects against accidental deletion of the last admin, short JWT-TTL limits the window for compromising a stolen token.

## Residual low-risks (backlog)

Low-priority risks not deliberately closed. Not beta blockers; candidates for defense-in-depth.

- **CSR CN/SAN is not validated until `SignCSR`.** Onboarding signs the CSR without checking the CN/SAN against the declared SID. Not critical: authentication is anchored on the **registry-fingerprint** (and not on the certificate's CN), so spoofing the CN does not give privileges - but CN/SAN validation would add defense-in-depth (early cutting off of junk CSRs).
- **Name-based secret-masking is fragile to new code-paths.** Secret masking in observable channels works by field names (`shared/audit/mask.go`). A new path code that logs `params` directly (bypassing the masker) may leak the secret. Protection - invariant below + review on each new logging path; A candidate for elimination is taint-tracking of secrets instead of name-based.

## External audit status / pentest

External independent pentest at the time of the closed small beta **was not carried out**. By decision dated 2026-06-15, the **internal security-gate**, which includes:

- **Deep IS audit 2026-06-12** — verdict PASS, **0 critical / 0 high** (this document is its fixation).
- **Threat-model** - this document: assets, actors, closed surfaces and residual risks.
- **`govulncheck` is clean for all modules** - supply-chain scan is embedded in `make check` (see environment requirements, target `make check-vuln` for all go.work modules).
- **Security revalidation of OpenAPI pivot - PASS** - 0 blockers; audit-completeness of write routes (anti-S6), RBAC default-deny + Purview-scope and JWT-enforcement are proven (see Insider operator and environment requirements).

External independent pentest planned **post-beta / before GA** - closed small beta (single operators, fleet of up to hundreds of hosts) provides a limited surface area in which the internal gate is commensurate with the risk; On the path to GA as the audience grows, external verification is mandatory. The guarantee limit for a beta tester is fixed at [`../known-limitations.md`](../known-limitations.md).

## Operator environment requirements

Invariants that **are not automatically provided by the code** and must be maintained during deployment. Violation of any of them weakens the model.

- **Vault PKI role with `enforce_hostnames` + `allowed_domains`.** SSRF-guard and mTLS validation rely on the PKI role not issuing certificates for arbitrary domains. Role configuration - on the operator ([`../keeper/prod-setup.md`](../keeper/prod-setup.md), [`../operations/infra.md`](../operations/infra.md)).
- **Invariant: `RenderedTask.Params` never gets into observable channels.** Rendered task parameters (potentially containing secrets after Vault CEL resolution) should not leak to audit / OTel / SSE / plaintext logs. Masking - output (`shared/audit/mask.go`); Each new path code touching `Params` must pass the masker. This is a regulatory requirement for the code (caught by the review), not a config option.
- **Invariant: every mutating `/v1`-route writes an audit-event (anti-S6).** A set of write-routes ⊆ a set of audit-covered routes. Maintained by a two-level gate in the code, no review: aggregate structural-guard (`keeper/internal/api/audit_completeness_guard_test.go`) - a declarative registry of write routes from the `buildRouter` topology, a new write route without writing to the registry makes the test red; per-domain `*_RecordsOnSuccess` tests - prove that event is actually written on 2xx (lesson S6: "middleware is hung" ≠ "audit writes" - the bridge intercepted the ResponseWriter BEFORE the recorder, the recording was silently lost). This is a regulatory requirement for code.
- **Regular `govulncheck`.** Supply-chain scan of dependencies is embedded in `make check` through the target `make check-vuln` (running `govulncheck ./...` across all 5 go.work modules; offline-graceful - `SKIP_VULNCHECK=1` passes without a network). At the time of fixation, govulncheck is clear in all modules. Pre-release launch - part of `make check`; periodic repeat for fresh vuln-DB - operational hygiene.
