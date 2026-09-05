# ADR-011. Go code layout: go.work with per-side modules

- **Context.** Soul Stack is three binaries (`keeper`, `soul`, `soul-lint`, [ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)) plus plugins as separate repositories (`soul-mod-*`, `soul-cloud-*`, `soul-ssh-*` — see [Plugin infrastructure](../architecture.md#plugin-infrastructure)). Open Q #9 "Go code layout" remained open: a mono-module with three `cmd/` vs go.work with several modules. The decision must address (1) isolation of `soul` from Keeper's server-side code (minimal Soul surface, a security requirement of [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)/[ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)), (2) shared packages for cross-cutting requirements (metrics, OTel, hot-reload, log rotation, the Vault client, CEL/text/template — [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)), (3) a public SDK for plugin authors without having to pull in all the main code.
- **Decision.** **Variant B — go.work with per-side modules** is adopted. The top-level structure:

  ```
  soul-stack/                                 # repo root, NOT a module
  ├── go.work                                 # use ./keeper ./soul ./soul-lint ./shared ./sdk ./proto ./proto/plugin
  ├── proto/                  # go.mod | internal Keeper↔Soul (keeper/v1) + gen/go/  (Operator API — OpenAPI spec + oapi-codegen, ADR-051; operator/v1 abolished)
  ├── proto/plugin/           # go.mod | NESTED module for plugin contracts (soulmodule/clouddriver/sshprovider) + gen/go/
  ├── shared/                 # go.mod | obs/ log/ config/ vault/ tlsx/ cel/ tmpl/  (vault/ — client part only)
  ├── sdk/                    # go.mod | public SDK: module/ clouddriver/ sshprovider/ handshake/
  ├── keeper/                 # go.mod | cmd/keeper + cmd/soul-trial + internal/{apiserver,mcpserver,rbac,store,reaper,push,pluginhost,cloud,vault,coremod,render,scenario}
  │                           #          (soul-trial — the offline Trial runner, ADR-023; asserts internal/render+scenario)
  ├── soul/                   # go.mod | cmd/soul + internal/{agent,connection,modules,coremod,cache}  — does NOT require keeper
  ├── soul-lint/              # go.mod | cmd/soul-lint + internal/  — does NOT require keeper/soul/sdk
  ├── examples/               # non-Go artifacts only (YAML etc.)
  ├── docs/
  └── tools/                  # helper tools (without their own Go logic)
  ```

  Key properties:
  - **Soul isolation is guaranteed by the Go compiler.** `soul/go.mod` does not contain `require .../keeper`; an attempt to import Keeper's server code from Soul physically will not compile. This raises the security invariant of [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)/[ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) from the level of a CI linter to the level of a module boundary.
  - **A shared contract for core/custom modules.** Core modules on both sides (`keeper/internal/coremod/`, `soul/internal/coremod/`) implement the same interface from `sdk/module/` as custom modules in separate repositories.
  - **The plugin-proto submodule.** `proto/plugin/` is a separate nested go.mod; plugin authors pull only the three service contracts + handshake, without Keeper↔Soul or the Operator API. A minimal dependency.
  - **The Vault client is isolated.** `shared/vault/` is the client part only (Soul-safe: token lookup, transit decrypt). Server-side Vault operations (the CA for SoulSeed, reading Essence, the PKI engine) live in `keeper/internal/vault/` and are **not exported** through `shared/`.
  - **The module path placeholder** = `github.com/soul-stack/soul-stack/<module>` during local development. On push to a remote host — a sed replacement across all `go.mod` files.
  - **Versioning policy — joint tags.** One git tag on the root repo = one logical version of all modules (`keeper/`, `soul/`, `soul-lint/`, `shared/`, `sdk/`, `proto/`, `proto/plugin/` all get `v0.1.0` on `git tag v0.1.0`). Plugin authors need one number, not seven.
  - **Generated Go code from proto is committed** in `proto/gen/go/` and `proto/plugin/gen/go/` (reproducible builds, no protoc on the developer's machine). CI checks the idempotency of `make gen`.
  - **`cmd/` inside modules** is kept (`keeper/cmd/keeper/main.go`) even with one binary per module — it will be useful for future utilities (`keeper-migrate` etc.). The first such case is **`keeper/cmd/soul-trial`** (the second binary artifact of the `keeper` module): the offline Trial runner ([ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper), [ADR-023](0023-trial-test-runner.md#adr-023-test-runner-trial-soul-trial-and-dsl-coverage)). It lives in the keeper module because it asserts `keeper/internal/render`/`scenario`/the migration engine directly (variant B — moving it to `shared/` — would break the invariant "`shared/vault/` is the client part only", since render pulls server-side Vault/topology/incarnation). `go.work` does not need editing: the `./keeper` module is already in the use list.
  - **`shared/` is one module** at the start, with subpackages; split it as it grows.
  - **`examples/` — non-Go artifacts only** (YAML, configs, templates). Runnable Go examples go in `tools/` or separate repos. This closes the risk of "accidentally creating a Go module in examples".
- **Consequences.**
  - The first code commit creates 7 `go.mod` files + `go.work` + empty `cmd/<binary>/main.go` for the three binaries + a placeholder `proto/keeper/v1/keeper.proto` with a single RPC `Ping` to verify generation.
  - Plugin authors (`soul-mod-*`, `soul-cloud-*`, `soul-ssh-*`) in their repositories write `require github.com/soul-stack/soul-stack/proto/plugin v0.X.Y` and `require github.com/soul-stack/soul-stack/sdk v0.X.Y` — without a dependency on keeper/soul/shared.
  - External integrators (for the Operator API client SDK) pull `require github.com/soul-stack/soul-stack/proto v0.X.Y` or the future `sdk/api` module (a separate task after the first Operator API release).
  - A change to the Keeper↔Soul proto contract is edited in `proto/keeper/v1/`, regenerate, edit the host side — all in one PR in one repo. **The shape of the Operator API** is edited not in proto but in the **OpenAPI spec** ([`docs/keeper/openapi.yaml`](../keeper/openapi.yaml)) → `make gen-api` (oapi-codegen → `keeper/internal/api/oapi/`, [ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict)); the markdown normalization of transport and the endpoint ↔ MCP-tool ↔ permission mapping — [`docs/keeper/operator-api.md`](../keeper/operator-api.md). `proto/operator/v1` is abolished (see the Amendment below).
  - A change to the plugin contract is edited in `proto/plugin/v1/`, regenerate `proto/plugin/gen/go/`, the version of `proto/plugin/` is bumped (via the shared repo tag); updating dependent plugin repos is a separate synchronization.
  - The version of Go modules inside the main repo is a semver tag of the root repo. This is an **explicitly articulated exception in [ADR-007](0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field)** for Go modules (`go.mod` `require` requires a semver tag).
  - **Server-side runtime dependencies go in `<binary>/internal/`, not in `shared/`.** Heavy server-side drivers (Postgres `pgx`, server-side Vault, server-side Redis operations, the server-side OTel collector) live in `<binary>/internal/<subsystem>/`, not in `shared/`. Allowed in `shared/`: interfaces, clients (Soul-safe), helpers without a heavy init graph, data types and enums. Precedent: `shared/vault/` (client-only) vs `keeper/internal/vault/` (server-side); by the same logic `shared/audit/` holds the `Writer` interface + types + masking + a ULID helper, while the pgx implementation is in `keeper/internal/auditpg/`. Generally: any import of a server-side driver into `shared/` drags package-level init (pgtype registration, `sync.Pool`, etc.) into **every** binary, including Soul, which breaks the compiler-enforced Soul isolation above.
  - Open Q #9 is closed; open Q #5 and #2 remain open and may be refined without editing this ADR.
- **Trade-offs.**
  - Seven `go.mod` files at the start — more boilerplate than the mono-module variant. Mitigated by the fact that Soul isolation is guaranteed by the compiler, not a CI linter (a security requirement).
  - The IDE and `go build ./...` traverse the workspace — a small overhead in IDE responsiveness and build time. Not critical with seven modules.
  - The SDK as a Go library is versioned by a semver tag — an explicitly articulated exception in [ADR-007](0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field) for Go modules (differs from "artifact version = git ref").
  - If the plugin ecosystem grows to dozens of external authors and a separate SDK repo is needed — the B→C migration (moving `sdk/` + `proto/plugin/` to `github.com/soul-stack/soul-stack-sdk`) is mechanical and does not block the decision now.

- **Amendment (OpenAPI epic, 2026-06-09): abolition of `proto/operator/v1`.** The Operator API transport is **HTTP/JSON** ([ADR-004](0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)), not gRPC; there is no gRPC service over `operator/v1`. `proto/operator/v1` (6 `.proto`) was a **parasitic second model of the shape** alongside the OpenAPI spec — hand-written DTOs silently diverged from the spec (request drift). The source of truth for the **shape** of the Operator API → the **OpenAPI spec** ([`docs/keeper/openapi.yaml`](../keeper/openapi.yaml)), and Go types from it are generated by **oapi-codegen** into the package **`keeper/internal/api/oapi/`** ([ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict)).
  - `proto/operator/v1` and its generated Go are **removed** (as a separate slice). The Makefile gen target loses its operator-proto section.
  - **`go.work` and the module structure do NOT change:** the `proto/` module remains — it hosts `proto/keeper/v1/` (the Keeper↔Soul contract). The isolation invariant of `proto/plugin/` (plugin authors pull a minimal dependency) is not affected.
  - All mentions of "Operator API (operator/v1)" / "messages for N HTTP endpoints" in the text above are to be read as **"Operator API — OpenAPI spec + oapi-codegen" ([ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict))**.

- **Amendment (pivot to code-first, 2026-06-12): the huma operations registry vs the `oapi` package — a transitional coexistence.** [ADR-054](0054-openapi-code-first.md) reversed the source of the Operator API shape: **Go types → OpenAPI** (huma v2 + humachi) instead of **OpenAPI → Go types** (oapi-codegen, ADR-051). Once the rollout is complete, the package **`keeper/internal/api/oapi/`** (committed `types.gen.go`/`server.gen.go`) and the bridge **`oapi_strict.go`** **die**, the shape is carried by the Go structs of the huma operations (`keeper/internal/api/huma_*.go`), and the spec is derived (a huma dump). **Transitional state (pilot + rollout):** both frameworks COEXIST under a single `chi.Mux` — some routes on huma (pilot: `POST /v1/cadences`), the rest on oapi-strict. `go.work` and the module structure do NOT change: the huma operations live in `keeper/internal/api/` next to the existing handlers; a new direct require `github.com/danielgtaylor/huma/v2` in `keeper/go.mod`. Tearing down the `oapi` package + the Makefile targets `gen-api`/`check-gen-api` is a separate slice AFTER the rollout is complete.
  - **Finalization (2026-06-13, HEAD `fde65bf`): teardown DONE.** The rollout is complete; the package **`keeper/internal/api/oapi/`** (committed `types.gen.go`/`server.gen.go`), the bridge **`oapi_strict.go`**/`strictServer`, the package `keeper/internal/api/meta/` (the embed source of the spec) and the Makefile targets `gen-api`/`check-gen-api`/`sync-openapi` are **removed** (replaced by `gen-openapi`/`check-openapi`). The shape of the Operator API is carried entirely by the Go structs of the huma operations (`keeper/internal/api/huma_*.go` + the aggregator `huma_full_spec.go`, the native enum catalog `huma_enums.go`); `docs/keeper/openapi.yaml` is a derived snapshot. The comment tree at the top of the file (the stub reference `Operator API — OpenAPI spec + oapi-codegen, ADR-051; operator/v1 abolished`) is to be read as **"Operator API — huma code-first, ADR-054; oapi-codegen and operator/v1 abolished"**.

## Amendment 2026-09-01 (NIM-757): `proto/plugin/` loses a service contract and `sdk/` loses a subpackage

★ **Implemented (NIM-761, 2026-09-04).** `proto/plugin/v1/clouddriver.proto` and its generated
stubs are deleted, and `sdk/clouddriver` is gone — its contract half with it, its retry / wait /
confirm-destroy / error-classification half moved to **`sdk/cloudutil`**. The layout above lists
one subpackage that no longer exists. Written under NIM-759, flipped under NIM-761.

The user's decision of 2026-09-01 removes the separate **CloudDriver** contract — a cloud driver
becomes an ordinary SoulModule plugin declaring `side: keeper`
([ADR-017 amendment 2026-09-01](0017-keeper-side-core.md#amendment-2026-09-01-nim-757-the-clouddriver-contract-is-removed--a-cloud-driver-is-an-ordinary-plugin),
[ADR-020 amendment 2026-09-01](0020-plugin-infrastructure.md#amendment-2026-09-01-nim-757-cloud_driver-is-removed-and-side-keeper-is-what-replaces-it)).
Two lines of the layout above change, and neither `go.work` nor the module count moves.

**⚠ First, a factual correction to this ADR, not a new decision.** The tree comment at `proto/plugin/`
and the "plugin-proto submodule" bullet both say **three** service contracts —
`soulmodule` / `clouddriver` / `sshprovider`. `proto/plugin/v1/` in fact carries **four** services
today: `SoulModule` (`soulmodule.proto:17`), `CloudDriver` (`clouddriver.proto:17`), `SshProvider`
(`sshprovider.proto:15`) and **`SoulBeacon`** (`beacon.proto:28`), the last added by the
[ADR-030 amendment 2026-05-26 (S5 closure)](0030-vigil-oracle.md#amendment-2026-05-26-s5-closure)
as V5-2 — the fourth plugin kind — which neither this ADR nor [ADR-020](0020-plugin-infrastructure.md)'s
Context ever absorbed. So the count after the cut is **three, not two**. Stated plainly so a reader does not
"fix" it back down by subtracting one from a number that was already wrong.

**`proto/plugin/`** — `clouddriver.proto` and its committed generated Go
(`proto/plugin/gen/go/v1/clouddriver.pb.go`, `clouddriver_grpc.pb.go`) are deleted, and
`KIND_CLOUD_DRIVER = 2` in `common.proto` becomes `reserved`. The submodule itself is unchanged:
it is still a separate nested `go.mod` that plugin authors pull on its own, still versioned by the
shared root tag, and its generated Go is still committed. ⚠ The "CI checks the idempotency of
`make gen`" property in the Consequences above **does not cover this deletion**: `Makefile:22`
enumerates the inputs with `find` over `proto/plugin/v1` and protoc does not remove stale outputs,
so a `.proto` deleted
without its `.pb.go` leaves a green `check-gen` over an orphaned generated file. The two files go
by hand, in the same commit.

**`sdk/`** — the subpackage list `module/ clouddriver/ sshprovider/ handshake/` loses its
`clouddriver/` entry. ⚠ Same shape of drift as the contract count above: that list is itself stale
independently of this change — `sdk/` also carries `beacon/`, `schema/` and `cmd/` today, none of
which the Decision names — so the post-cut list is not `module/ sshprovider/ handshake/`, it is
whatever the tree holds minus `clouddriver/`. Recorded, not silently rewritten. `sdk/clouddriver/` held the shared driver skeleton
(`Classify` / `Retry` / `WaitUntilReady` / `ConfirmDestroy`,
[ADR-017(f)](0017-keeper-side-core.md)); with the contract gone there is nothing for it to be the
skeleton of. This is the one place where a **module directory** disappears rather than a package
inside one, so for a plugin author it reads as a separate breakage from the proto one: a rebuild
against a newer tag loses `github.com/souls-guild/soul-stack/sdk/clouddriver` as well as
`pluginv1.RegisterCloudDriverServer`. `sdk/schema/` — which is where the closed `kind:` enum
actually lives, not `proto` — loses `KindCloudDriver` and its `profile_schema` validation rules.

**`keeper/`** — `internal/` sheds `provider/`, `profile/`, `coremod/cloud/` and
`pluginhost/clouddriver.go`. The `internal/{…}` enumeration in the tree above lists `pluginhost`
and `cloud` as subsystems; `pluginhost` stays, `cloud` goes. The Soul-isolation invariant, the
`shared/` vs `<binary>/internal/` rule and the joint-tag versioning policy are all untouched —
this removes leaves, not boundaries.
