# ADR-018. Soulprint typed-schema MVP

- **Context.** [ADR-012(g)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) fixed a stub: `SoulprintReport.facts` = `google.protobuf.Struct` until the closure of [open Q №6](../architecture.md#открытые-вопросы). Currently Soulprint is used in the existing docs/examples in at least six places (essence pipeline `os/<soulprint.self.os.family>.yaml` — [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация); CEL accessors `soulprint.self.<path>` / `soulprint.hosts.where(<predicate>)` / `soulprint.where(<predicate>)` — [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов); inline in `_stack.yaml` `int(soulprint.self.memory.total_mb * 0.6)`; the text/template context `.tmpl` — `self.network.primary_ip` / `self.os.*` / `self.sid`; a probe `where:` in a scenario; the core modules `core.pkg`/`core.service` for abstraction via the native pkg-mgr/init-system) — but Soulprint has no typed schema. This ADR closes #6: a typed `SoulprintFacts` message + sub-messages, a minimal set of fields for the first E2E service, and fixing the canonical CEL form. #22 (`soulprint.collectors` in `soul.yml`, user-collectors) **remains open** — it requires a separate decision on the collector format / sandbox / execution rights / output validation (not merely a schema question).
- **Decision.**

  **(a) The typed schema in `proto/keeper/v1/soulprint.proto`.** A new field `typed_facts` (field 3) is added; the old `facts: google.protobuf.Struct` (field 2) is marked `deprecated`, but physically remains for wire-compat ([ADR-012(c)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) forward-compat only-add). Soul-side of new versions fills only `typed_facts`; Keeper is tolerant to both.

  ```protobuf
  message SoulprintReport {
    google.protobuf.Timestamp collected_at = 1;
    google.protobuf.Struct    facts        = 2 [deprecated = true]; // stub, see ADR-012(g); cannot be removed — only-add
    SoulprintFacts            typed_facts  = 3;
  }

  message SoulprintFacts {
    string         sid              = 1;  // echo for logs; authority — mTLS peer cert
    string         hostname         = 2;  // short name (without domain)
    OsFacts        os               = 3;
    KernelFacts    kernel           = 4;
    CpuFacts       cpu              = 5;
    MemoryFacts    memory           = 6;
    NetworkFacts   network          = 7;
    // field numbers 8..14 reserved for post-MVP (uptime/timezone/virtualization/cloud_provider/disks/bios)
    // 15 reserved for an optional `extra: google.protobuf.Struct` for user-collectors (an ADR candidate, see (h))
  }

  message OsFacts {
    string family      = 1;  // debian / rhel / alpine / windows / darwin
    string distro      = 2;  // ubuntu / rocky / alpine
    string version     = 3;  // "22.04" / "9.3" / "3.19"
    string codename    = 4;  // "jammy" / ""
    string arch        = 5;  // amd64 / arm64
    string pkg_mgr     = 6;  // apt / dnf / apk — for core.pkg.installed
    string init_system = 7;  // systemd / openrc / sysv — for core.service.*
  }

  message KernelFacts { string version = 1; string release = 2; }
  message CpuFacts    { int32  count = 1; string model = 2; string vendor = 3; }
  message MemoryFacts { int64  total_mb = 1; int64 available_mb = 2; int64 swap_mb = 3; }

  message NetworkFacts {
    string   primary_ip = 1;  // primary IPv4, the one bound by default (90% of use-cases)
    string   fqdn       = 2;  // FQDN (== SID, but a fact about the system)
    repeated NetworkInterface interfaces = 3;
  }

  message NetworkInterface {
    string   name = 1;
    repeated string ipv4 = 2;
    repeated string ipv6 = 3;
    string   mac  = 4;
    int32    mtu  = 5;
  }
  ```

  Precise semantic descriptions, validation, use-case examples — in [`docs/soul/soulprint.md`](../soul/soulprint.md).

  **(b) `os.pkg_mgr` and `os.init_system` are collected by the Soul agent.** Once in the agent's code (a mapping table `family+distro → pkg_mgr/init_system`), not in every core module. `core.pkg.installed` and `core.service.*` read these fields directly from `SoulprintFacts.os` — they do not duplicate the detection. Advantage: when a new OS family appears, the table is edited in one place.
  - **Amendment (2026-05-25, hybrid + a delivery mechanism).** Implemented as a **hybrid**: the soulprint fact (`os.pkg_mgr`/`os.init_system`) — the **primary** source of truth for choosing the backend in `core.pkg`/`core.service`; the runtime detection (`util.ResolvePkgMgr`/`ResolveInitSystem` → `DetectPkgMgr`/`DetectInitSystem`) — a **fallback** on an empty/`unknown` fact (not a duplicate source, but an emergency path). Removes the "no supported init system" failure on hosts/containers where the tools are not in place, and guarantees that the module and CEL `soulprint.self.os.*` (in `where:`/templates) see ONE source — otherwise a silent class of bugs (found by the acceptance nginx run, BUG-B 2026-05-25). **Delivery of the fact into the module — Variant A (in-process):** Soul injects a local snapshot (`util.HostFacts`) into the core modules via an optional internal interface `util.SoulprintAware` (NOT the public `sdk/module` contract) before `Apply`. Out-of-process custom plugins do NOT get the fact yet — Variant B (soulprint in the `ApplyRequest` proto, only-add) is reserved until the first custom plugin that needs `pkg_mgr`/`init_system`.

  **(c) `network.primary_ip` + `interfaces[]`.** A convenience string at the root of `NetworkFacts` (used in 90% of cases, including `self.network.primary_ip` in `redis.conf.tmpl`); `interfaces[]` — for multi-homed/VLAN-aware cases. The algorithm for determining `primary_ip` — Soul-side, an MVP heuristic: the interface with the default route → its primary IPv4.

  **(d) The canonical CEL form — `soulprint.self.<path>`.** The bare form `soulprint.<path>` (without `.self`) — a **validation error** in `soul-lint`. Symmetric to `register.self.*` ([destiny/tasks.md §10](../destiny/tasks.md#10-шаблонный-контекст)). The existing examples (`examples/destiny/redis/tasks/` etc.) are rewritten by a batch task to match the canon.

  **(e) `covens` is NOT in `SoulprintFacts`.** This is **Keeper-registry data** (`souls.coven[]` in Postgres, assigned by the operator via the API or `core.soul.registered` — see [`docs/keeper/modules.md`](../keeper/modules.md)), not facts collected by the Soul agent. `soulprint.self.covens` and `soulprint.hosts[].covens` in CEL — a **projection** of the Keeper-side data into the Soulprint namespace: Keeper, when resolving a CEL expression, joins `SoulprintFacts` (from Soul) + `souls.coven[]` (from Postgres) into the logical view `HostFacts`. Soul knows nothing about covens.

  **(f) `collected_at` — Soul-side, without hard validation.** Soul fills the timestamp of the moment the facts were collected. Keeper, on unmarshal, additionally sets `received_at` in the Postgres storage (not part of the wire-format, not part of the `SoulprintReport` proto). On a discrepancy `received_at - collected_at > 10 min` — a warn in the OTel trace. There is no hard validation (drop stale, reject on future) in MVP — a Soul in a private network without NTP must not break.

  **(g) The minimal set for the first E2E.** All the fields above — are needed for the existing use-cases (see the inventory in [`docs/soul/soulprint.md → section "Inventory of usage"`](../soul/soulprint.md)). If the first attempt of a real service hits a missing field — we will add it only-add (a new field number 8+ in `SoulprintFacts`).

  **(h) `extra: google.protobuf.Struct` deferred.** Field number 15 in `SoulprintFacts` is **reserved** for an optional `extra` for user-collectors, but in MVP it is NOT declared. The opening — a separate ADR upon the closure of #22 (requires decisions on: the collector format `/etc/soul/soulprint.d/*` — a binary/script; execution rights — Soul under root executes foreign code; sandbox; collect-time vs lazy; validation/sanitization of the output). Closing #22 with a single line `extra: Struct` is an under-solution, not a closure.

  **(i) The accompanying document.** The detailed spec of all fields with examples, a description of the collection algorithms, the mapping table `family→pkg_mgr/init_system` — in [`docs/soul/soulprint.md`](../soul/soulprint.md) (like `docs/templating.md` for ADR-010, like `docs/destiny/output.md` for ADR-009).

- **Consequences.**
  - `proto/keeper/v1/soulprint.proto` is augmented with new messages; `make gen` regenerates `proto/gen/go/keeper/v1/soulprint.pb.go`.
  - `docs/soul/soulprint.md` — a new file (the detailed spec).
  - `docs/naming-rules.md` is augmented with a section about the Soulprint fields.
  - The existing examples are rewritten to match `soulprint.self.<path>` by a separate batch task (`examples/destiny/redis/`, `essence/_stack.yaml` etc.).
  - `soul-lint` gets a static-checkable Soulprint schema (`docs/templating.md:97` — now `soulprint.self.*` has concrete types from the proto, not `dyn`).
  - **Open Q №6 closed.** Open Q №22 (user-collectors) — remains open.
  - ADR-012(g) is updated: the stub `facts: Struct` is marked `deprecated`, references `typed_facts`.

- **Trade-offs.**
  - `facts: google.protobuf.Struct deprecated` remains in the proto forever (forward-compat only-add). Cruft, but the price for wire-compat.
  - The Soul agent must maintain a mapping table `family+distro → pkg_mgr/init_system` for the most popular distributions. A new OS → editing the table in Soul, releasing a new version of Soul. The alternative (derived in each module) — is worse (duplication).
  - `primary_ip` as a default-route heuristic may be inaccurate in rare scenarios (multi-homed with equal metrics, IPv6-only). Accepted: 90% of cases — a typical server, the rest let them iterate `interfaces[]`.
  - `covens` via a CEL projection means that Keeper on resolve must join `SoulprintFacts` + `souls.coven[]`. An insignificant compute overhead per-resolve; the cache in Redis ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) covers it.

- **Amendment (2026-05-29, `choirs` as a stable soulprint fact — a Keeper projection).** [ADR-044](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации) (Choir) adds a stable fact `choirs[]` — the list of the names of the host's Choirs in the current incarnation, available in CEL as `soulprint.self.choirs` (and `soulprint.hosts[].choirs`). Symmetric to `covens` (point (e) above): this is **Keeper-registry data** (tables `incarnation_choirs` / `incarnation_choir_voices`, [ADR-044](0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации) point 4), **NOT** facts collected by the Soul agent — the `SoulprintFacts` proto is **not augmented** for Choir. `soulprint.self.choirs` — a **virtual projection** on CEL resolve: Keeper joins the per-host Voice records into the Soulprint namespace. Choir is a stable (declared, not volatile) fact, so it is available in `where:` without a probe (the boundary "stable in Soulprint, volatile — a probe" from [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) is respected: the declared topology is stable, the actual role after failover — is not). The corresponding edit of `docs/soul/soulprint.md` — as slice S-T4.

- **Amendment (2026-06-09, `typed_facts` on REST — byte-passthrough category D).** `GET /v1/souls/{sid}/soulprint` returns `typed_facts` as **byte-passthrough JSONB** (category D, symmetric to Augur `allow` — [ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-типы-oapi-codegen-types-only--strict)): Keeper reads the raw bytes of the `souls.soulprint_facts` column (written by the eventstream via `protojson.Marshal(SoulprintFacts)` with `UseProtoNames`) and returns them on the wire **AS-IS**, without `unmarshal→map→re-marshal`. The previous path (`map[string]any` with a re-marshal) sorted the keys lexicographically at each level; byte-passthrough returns the **PG-jsonb-normalized** key order — this is a deliberate one-time wire-change of the key order (the values are identical; the UI parses typed_facts by the proto schema `SoulprintFacts`, the key order is irrelevant to it). **Forward-compat is GUARANTEED by design:** new proto fields of `SoulprintFacts`, added by the Soul agent, reach the wire **without recompiling the Keeper** — Keeper does not parse or filter the content (previously this was a "promise" via an untyped `map`; now — a direct consequence of byte-passthrough, independent of the Go type on the Keeper side). OpenAPI: `SoulprintReadReply.typed_facts` — `x-go-type: json.RawMessage` (`type: object` in the schema for documenting the shape). The storage invariant (the jsonb column rejects invalid JSON on write) makes the previous handler-side `unmarshal` validation (and its HTTP 500 branch on "broken JSONB") redundant — it is removed. Guard-tests: byte-exact passthrough (a non-alphabetical key order is preserved) + forward-compat (an extension key outside the `SoulprintFacts` proto is present on the wire).
