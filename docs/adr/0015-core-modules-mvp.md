# ADR-015. Core modules MVP: exact list

- **Context.** Open Q #5 contained the sub-question "the exact set of core modules in MVP". The [module model](../architecture.md#модель-модулей) and [addressing](../architecture.md#адресация-модулей) already fix the three-level shape `<namespace>.<module>.<state>`, the core namespace, and the precedents `core.pkg.installed`, `core.file.present`/`core.file.rendered`. This ADR fixes the minimal list of Soul-side and Keeper-side core modules without which the first service does not work end-to-end. The "MVP" criterion — enough for the typical validation trio: Redis HA / PostgreSQL standalone / a simple web app.
- **Decision.**

  **Soul-side core MVP (17 modules):**

  | Module | State forms | Purpose |
  |---|---|---|
  | `core.pkg` | `installed` / `absent` / `latest` | OS packages, abstraction over the native pkg-mgr (apt/yum/dnf/pacman/apk), detection via Soulprint. |
  | `core.file` | `present` / `absent` / `rendered` / `directory` | File exists with literal content (`present`) / absent (`absent`) / rendered from `.tmpl` (`rendered`, see [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) / directory exists with owner/group/mode (`directory`, see Amendment 2026-06-18 below). |
  | `core.service` | `running` / `stopped` / `restarted` / `enabled` | Service, abstraction over systemd/openrc/sysv. |
  | `core.user` | `present` / `absent` | Local OS users. |
  | `core.group` | `present` / `absent` | Local groups. |
  | `core.exec` | `run` (verb) | An arbitrary command, exec(). The probe idiom [ADR-008](0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) is tied to it. |
  | `core.cmd` | `shell` (verb) | A shell command (pipes, redirects). The difference from `exec.run` is shell interpretation. |
  | `core.cron` | `present` / `absent` | Cron jobs in crontab format. |
  | `core.mount` | `present` / `absent` / `mounted` / `unmounted` | Mount points, /etc/fstab. |
  | `core.git` | `cloned` / `pulled` | Cloning/updating a git repository on the host. |
  | `core.archive` | `extracted` | Extracting archives (tar/zip/gz/bz2). |
  | `core.sysctl` | `present` | Kernel parameters (`vm.overcommit_memory`, `kernel.shmmax`, etc.). |
  | `core.url` | `fetched` | Download a file by URL (analog of Ansible `get_url`). `https` by default; `http` / insecure-TLS / private IPs — an explicit per-call opt-out (`allow_http`/`insecure_skip_verify`/`allow_private`, each defaulting to `false`, the opt-out is logged as a warn in the output `warnings`); idempotency via `checksum` (`sha256`/`sha1`) or SHA-256 comparison; atomic verify-then-rename; `headers` sensitive-by-construction ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) §7.4). |
  | `core.line` | `present` / `absent` | In-place per-line editing of an existing file (lineinfile equivalent). The first core module that does not overwrite the file as a whole. **A trimmed safe MVP** (see below): `present`+`regexp` replaces the FIRST matching line (+warning when >1), `present` without `regexp` adds the exact line at `insertafter`/`insertbefore` (literal/EOF/BOF), `absent` removes all matches. Writing is atomic (temp+rename). |
  | `core.repo` | `present` / `absent` | A package repository (apt/dnf/yum/apk; idea from ansible `apt_repository`/`yum_repository`). Backend by `util.DetectPkgMgr`: apt → `/etc/apt/sources.list.d/<name>.list` + a key in `/etc/apt/keyrings/<name>.gpg` with a `signed-by=` reference (modern format, NOT `apt-key`); dnf/yum → `/etc/yum.repos.d/<name>.repo` (ini); apk → a line in `/etc/apk/repositories`. Idempotency: file + content + key match → `changed=false`. Writing via `util.AtomicWritePreserving`. **Security:** `gpg_key` is critical (supply-chain) — if set, the key is really materialized/verified; `gpg_check=false` is ALLOWED (opt-out) with a mandatory warning; `http://` is PERMITTED (an internal mirror, unlike the https-only `core.url`) with a mandatory warning. |
  | `core.firewall` | `present` / `absent` | ONE firewall rule (idea from ansible `ufw`/`firewalld`). Backend by the new `util.DetectFirewall` (by the installed management binary, NOT by Soulprint): MVP — ufw and firewalld, iptables deferred. Idempotency: parsing `ufw status` / `firewall-cmd --list-...` (fragile across versions — covered by strict unit tests on fixed samples). **CRITICAL SECURITY INVARIANT:** the module NEVER touches the default policy and NEVER enables the firewall as a whole (`ufw enable` / `systemctl start firewalld`) — otherwise on a remote host it would cut off SSH and we would lose control. Only add/delete of a specific rule (fixed by a comment in code and a unit test). |
  | `core.http` | `probe` (verb) | **Read-probe HTTP** (health-check / API-readiness / reading a version; idea from ansible `uri`, deliberately narrowed to reading). Object `http`, verb `probe` — **read-only**. `method` enum `{GET, HEAD}`, default `GET` (NOT "any method" — narrowing the mutability of ansible). `https` by default; `http` / insecure-TLS / private IPs — an explicit per-call opt-out (`allow_http`/`insecure_skip_verify`/`allow_private`, each defaulting to `false`, the opt-out is logged as a warn in the output `warnings`); reuse of `util.ValidateFetchURL` + `util.CheckRedirect` downgrade-block (like `core.url`). `status_codes` (default `[200]`; mismatch → `failed` with the output attached for diagnostics). **`changed=false` ALWAYS** — by construction and non-configurable: a probe does not change host state; interpretation of the result is `changed_when:` at the scenario level (the `core.exec.run` precedent). Output/register: `status` / `body` (cap 64 KiB by bytes with a **rune-aware** fallback to the last full rune + a `truncated` flag; the body is sanitized to valid UTF-8 — the probe is read-only and the body may be binary, it must not crash Apply; only **vault-ref substrings** `vault:…` inside the body are masked, NOT the whole thing as sensitive — for the sake of health responses like `{"status":"ok"}`. **Limitation:** arbitrary plaintext secrets (`password: hunter2`) in the body are NOT masked — the body is semi-trusted, the operator must not put into a probe endpoint anything that must not be exposed) / `elapsed_ms` / `headers_keys` (keys only) / `changed=false`. `headers` — sensitive-by-construction ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) §7.4). Mutating HTTP (POST/PUT/PATCH/DELETE, likely `core.http.request`) — **deferred post-MVP** by a separate ADR extension (and then the changed-contract of mutations too; do NOT copy ansible "changed=true if 2xx"). |

  **`core.template` is NOT split out** as a separate module — file rendering is done by `core.file.rendered` ([ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)). The old mention of `template` in the [module model](../architecture.md#модель-модулей) and [addressing](../architecture.md#адресация-модулей) is historical drift, corrected by this ADR. **`core.copy` is NOT split out** — it is covered by `core.file.present` with inline content.

  **`core.line` (lineinfile equivalent) — accepted (2026-05)** in a trimmed safe form: a request came in, and a predictable MVP was chosen instead of ansible-style permissiveness (exactly the reason it was deferred: "regex matches not what you think"). Deliberate MVP limitations, extensible later without a breaking change: **backrefs are NOT supported** (substitution of regexp groups into `line`); **insertafter/insertbefore — literal or EOF/BOF only, NOT regexp**; `present`+`regexp` replaces only the FIRST match (the rest are untouched + a warning). This is a **pilot of the new pattern** "in-place per-line editing" — the sample for in-place core is fixed on it; `core.repo` (writing repo files via `util.AtomicWritePreserving`) and `core.firewall` are implemented following it.

  **`core.http` (read-probe HTTP) — accepted (2026-05)** on a real request, as a separate slice after `core.repo`/`core.firewall`. The scope is deliberately narrowed to **read-only** (verb `probe`, methods `GET`/`HEAD`): 90% of "HTTP in destiny" cases are health-check / API-readiness / reading a version, they are pure and `changed=false` by nature. This is a departure from ansible-style permissiveness of `uri` (any method + a murky "changed=true if 2xx"). The object `http` is set up **separately from `core.url`** deliberately: the boundary is "`url` puts bytes on disk, `http` returns a response into a register"; this leaves a clean place for a future mutating `core.http.request`. The HTTP infrastructure (https-only validation, redirect downgrade-block, client constructor) is factored into `util` (`util.ValidateURL`/`util.CheckRedirect`/`util.NewHTTPClient`/`util.HTTPDoer`) and reused by both modules — a single supply-chain-protection pattern. Mutating HTTP is deferred post-MVP by a separate ADR.

  **Preserve-by-default for in-place core modules (normative).** In-place core modules that edit an *existing* file (`core.line` present/absent and future in-place core) **preserve the mode/owner/group of the existing file by default**: if `mode`/`owner`/`group` are not set in params — the current permissions and owner are restored after the atomic rename (rename creates the temp with the process's permissions, so preserve is explicit). Explicitly set `mode`/`owner`/`group` **override**. Creating a *new* file (e.g. `core.line` `create:true`) uses defaults (`mode` → 0644, owner — the current process). Implemented by the shared brick `util.AtomicWritePreserving` — an inherited point for all in-place core.

  **`core.hostname` — optional**, more often solved by cloud-init. We will add it if a scenario without cloud-init appears.

  **Keeper-side core (dispatcher `on: keeper`, see [`docs/keeper/modules.md`](../keeper/modules.md)):**

  | Module | Status | Purpose |
  |---|---|---|
  | `core.soul.registered` | already fixed | Binding a SID to the coven labels of the souls registry. |
  | `core.cloud.provisioned` | **introduced** | A CloudDriver call from a scenario (cloud-create). Replaces the earlier pattern "destiny `cloud-provision` with `on: keeper`" — see [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read). |
  | `core.vault.kv-read` | **introduced** | Reading a secret from Vault on the keeper side at render time. Formalizes the "vault-resolve phase" of [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) as an explicit module step. See [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read). |

  **Not part of MVP:**
  - `core.essence.read` — implicit access to `essence.*` in the template context covers it; an explicit module is not needed.
  - `core.incarnation.commit-state` — the commit is done by the keeper implicitly on a successful apply ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).
- **Consequences.**
  - In `docs/naming-rules.md` the "Concrete core modules" section is extended with the full list.
  - In the [module model](../architecture.md#модель-модулей) and [module addressing](../architecture.md#адресация-модулей) `template` is removed from the core examples (replaced by `core.file.rendered`).
  - Open Q #5 in the part "the exact set of core modules" is closed; the remaining sub-questions (the module registry in the Keeper, the manifest format, the handshake protocol version, the module versioning policy, the optional `required_modules`) remain open.
  - These 17 Soul-side + 3 Keeper-side modules are the mandatory implementation of phase 0; the implementation plan is a separate task (probably a pilot of one module → a batch after review of the pattern, see [CLAUDE.md → Mass operations and batching](../../CLAUDE.md)).
  - `core.url.fetched` was added post-hoc (after the first 12) as a safe declarative replacement for the workaround via `core.cmd.shell creates:` for downloading releases/binaries. In the exporter destiny examples the pattern `core.cmd.shell` (curl/wget with `creates:`) is replaced by `core.url.fetched` — as a separate slice after review of the module itself (the batching stop rule).
  - Besides the 17 MVP core modules above, the `soul` binary provides an **infrastructure** core module `core.module.installed` for delivering custom modules to the host (Keeper → server-streaming RPC `FetchModule` → the catalog cache `paths.modules`, Sigil-verify). This is **not counted among the 17**: `core.module.installed` is a function of the Soul daemon itself, implemented as a core module for uniformity with the Destiny DSL (the operator writes an explicit step `module: core.module.installed` with `params: {name: <ns>.<name>}` in their Destiny if they want a custom module). The behavior specification and input schema are **fixed by [ADR-065](0065-core-module-installed.md)** (transport `FetchModule`, the registry `plugins.soul_modules[]`, Sigil verification, hot-register).
- **Trade-offs.**
  - 17 Soul-side modules — more than the bare minimum (it could be squeezed to 6, replacing the rest via `core.exec.run`), but poor DX and idempotency cannot be verified. The base 12 are the necessary minimum for a production-ready first service; `core.url` was added on top as a safe replacement for the `core.cmd.shell` workaround for download, `core.line` — post-hoc on a real request (a trimmed safe MVP), `core.repo`/`core.firewall` — as the next slice on a real request, `core.http` (read-probe) — as a separate slice on a real request.
  - `core.line` is **accepted** (a trimmed safe MVP, without backrefs, replace of the first match) — a pilot of the in-place per-line editing pattern. `core.repo`/`core.firewall` are **accepted** (2026-05) as one slice following the pilot: both reuse `util.AtomicWritePreserving`/`util.DetectPkgMgr`/the new `util.DetectFirewall`, both `present`/`absent`. `core.http` (read-probe, verb `probe`, `changed=false`) is **accepted** (2026-05) as a separate slice: it reuses the HTTP infrastructure factored into `util` (`util.ValidateURL`/`util.CheckRedirect`/`util.NewHTTPClient`/`util.HTTPDoer`) jointly with `core.url`; mutating HTTP is deferred post-MVP. The early candidate name `core.uri` was rejected (uri/url confusion) — the object `http` was chosen.
  - `core.cloud.provisioned` and `core.vault.kv-read` are a re-framing of existing implicit things; migration of the examples in `service.yml` is a separate task.
- **Amendment (2026-06-18, new state `directory` in `core.file`).** In `core.file`
  a state `directory` (`core.file.directory`) was added — declarative creation of a
  directory instead of the imperative `core.exec.run install -d`. Params: `path`
  (required), `owner`, `group`, `mode` (as in `present`), `parents` (bool, default
  `false` — the semantics of `mkdir -p`: create intermediate directories). `recurse`
  (recursive setting of permissions on the contents) is deliberately NOT implemented in MVP —
  only the directory itself is managed; it is added later without a breaking change on a
  real request. Idempotency (parity with `present`): the directory exists and
  `owner`/`group`/`mode` match → `changed=false`; the directory does not exist → create →
  `changed=true`; attributes drift → fix (`chmod`/`chown`) → `changed=true`;
  the path is occupied by a file (not a directory) → error without overwriting. Supported by Plan/Scry
  ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)):
  `planDirectory` — pure-read drift (the same `changed` that Apply would have produced, without
  mutating the host). Implementation — [`soul/internal/coremod/file/directory.go`](../../soul/internal/coremod/file/directory.go)
  (`applyDirectory`/`planDirectory` + `case "directory"` branches in Apply/Plan/Validate),
  reuses `util.ParseMode`/`util.ApplyOwnership`/`util.OwnershipDrift`.
  The author manifest — the `states.directory` block in
  [`shared/coremanifest/file.yaml`](../../shared/coremanifest/file.yaml) (additive,
  only-add; `soul-lint` validates it automatically). Additive and backward-compatible:
  existing `core.file` tasks are not affected. Documentation —
  [`docs/module/core/file/README.md`](../module/core/file/README.md).
- **Amendment (2026-06-24, optional param `src` on `core.file.present`).** To the state
  `present` an optional param `src` was added — **an absolute path of a regular file on
  the Soul host** (typically the result of `core.archive.extracted`); `present` copies the
  CONTENT of `src` into `path`. `src` sets **content only**, not the attributes of the
  source — `mode`/`owner`/`group` of the target file are taken from the explicit `present`
  params as before. `src` and `content` are **mutually exclusive** (checked by the presence
  of the KEY in params, not by the emptiness of the string — so `content: ""` + `src` is also
  caught as the conflict `content and src are mutually exclusive`); if neither is set →
  **the legacy behavior is preserved** (an empty file), this is backward compatibility, not
  an error. **MVP — regular file only**: directory/symlink/device/socket/fifo are
  explicitly rejected; the type is checked via `os.Lstat` + `IsRegular()` (specifically Lstat —
  a symlink is rejected, not followed: protection against source substitution), absence →
  `read src %s: no such file`, non-regular → `src %s is not a regular file`, relative →
  `src must be absolute`. Idempotency by `sha256(src-bytes)`: src is read into
  memory once, the same buffer is hashed and written (without a double read — TOCTOU).
  **Atomicity asymmetry**: the src branch writes atomically (`util.AtomicWrite`, temp +
  rename), the content branch stays on `os.WriteFile` (unchanged). Supported by
  Plan/Scry ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)):
  `planPresent` with `src` takes the desired hash = `sha256(readSrc(src))`, then a
  pure-read comparison without writing; if src is absent/unreadable during Plan → `PlanFailed`
  (NOT false-clean). Implementation — a new helper `util.ReadRegularFile` (next to
  `util.AtomicWrite`) + branches in `applyPresent`/`planPresent`/`Validate`
  ([`soul/internal/coremod/file/file.go`](../../soul/internal/coremod/file/file.go)).
  The author manifest — the param `src` in `states.present.input`
  ([`shared/coremanifest/file.yaml`](../../shared/coremanifest/file.yaml)); the manifest DSL
  does not express mutual exclusion (there is no `oneof`) — the XOR lives in `Module.Validate`, the
  manifest has only descriptions. **Additive, proto is untouched, no breaking**: `core.file.present`
  tasks without `src` are not affected. Documentation —
  [`docs/module/core/file/README.md`](../module/core/file/README.md).
- **Amendment (2026-06-24, new state `applied` in `core.sysctl`).** In `core.sysctl`
  a state `applied` (`core.sysctl.applied`) was added — bulk application of a SET of
  kernel parameters in one drop-in, in addition to the per-key `present`. Motivation:
  host-tuning sets (Redis/ES/Kafka etc.) carry ~a dozen parameters; a separate
  `core.sysctl.present` for each bloats the plan and shifts indices. Params:
  `settings` (map `name→value`, required), `filename` (string, required — the name of the
  drop-in in `/etc/sysctl.d/`, e.g. `30-redis`; the `.conf` suffix is added),
  `reload` (enum `auto|always|never`, default `auto` — **reuse of the enum dictionary from
  `core.service` `daemon_reload`**, util.DaemonReloadMode), `ignore_failures` (bool,
  default `false` → `sysctl -e -p`, silences read-only/non-existent keys in
  containers). Idempotency: the drop-in content is **deterministic** (keys are
  SORTED, format `key = value`) → comparison with the existing file, `changed=true`
  only on a diff (atomic write via `util.AtomicWritePreserving`); reload
  (`sysctl -p <file>` PRECISELY by the drop-in, NOT the whole `--system`) is gated: `never` →
  never (opt-out), `always` → unconditionally, `auto` → only on a file change (like
  `daemon_reload:auto`); the reload itself does NOT mark `changed`. Supported by Plan/Scry
  ([ADR-031](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)):
  `planApplied` — pure-read drift (comparison of content without writing/reload).
  **A deliberate exception to the boundary with `core.file`.** Unlike per-key `present`
  (where the persist file is a byproduct) and the general rule "files are rendered by
  `core.file.rendered`", the state `applied` **owns the drop-in itself**: the module builds
  the content from the map, writes it and manages reload + idempotency in one step. This is a
  deliberate Ansible model (the `sysctl` module owns the file+reload), not drift:
  the drop-in content is trivial (`key = value`, sorted) and does not require text/template, and
  the bundle "file↔reload↔idempotency" is atomic at the module level. Implementation —
  [`soul/internal/coremod/sysctl/applied.go`](../../soul/internal/coremod/sysctl/applied.go)
  (`applyApplied`/`planApplied` + `case "applied"` branches in Apply/Plan/Validate),
  reuses `util.AtomicWritePreserving`/`util.DaemonReloadMode`. The author manifest
  — the `states.applied` block in
  [`shared/coremanifest/sysctl.yaml`](../../shared/coremanifest/sysctl.yaml) (additive,
  only-add). Additive and backward-compatible: `core.sysctl.present` tasks are not affected.
  Documentation — [`docs/module/core/sysctl/README.md`](../module/core/sysctl/README.md).
- **Amendment (2026-06-18, centralized daemon-reload in `core.service`).** `core.service` (systemd backend) before mutating actions (`running` / `restarted` / `enabled`) checks the systemd flag `NeedDaemonReload` and, on a desync of the unit file with the loaded definition, runs `systemctl daemon-reload` BEFORE start/restart/enable. Closes a bug: after editing a unit file without a reload, `systemctl restart` silently restarts with the OLD definition (exit 0, only a warning). The behavior is controlled by the optional parameter `daemon_reload` (string enum `auto` | `always` | `never`, **default `auto`**, declared in `shared/coremanifest/service.yaml` on the states `running`/`restarted`/`enabled`; on `stopped` it is NOT declared — a reload is not needed there): `auto` — reload only when `NeedDaemonReload=yes` (gated, idempotent); `always` — reload unconditionally; `never` — an explicit opt-out. The check mechanism — `systemctl show <unit> --property=NeedDaemonReload --value` (`yes`/`no`); on the first install of a new unit the flag = `no` (systemd will pick up the definition on start), a reload is not needed. **The reload does NOT mark the step as `changed`** (changed remains a function only of start/restart/enable) — on an actually performed reload a diagnostic `reloaded: true` is added to `output`. `openrc`/`sysv` — a **no-op** (they have no daemon-reload). Implementation — the helper `util.EnsureDaemonReloaded` next to `util.ServiceActive` (the same mock-able Runner, without D-Bus/go-systemd); the enum is validated in `core.service.Validate` (an unknown value → a validation error, not silently). Additive and backward-compatible: existing tasks without `daemon_reload` get `auto`.
