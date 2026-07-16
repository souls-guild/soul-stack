# Soul Stack template engine

Standard template engine specification for all YAML expressions (destiny, scenario, essence, keeper.yml, migration) and for file templates on the host. The solution is recorded in [architecture.md → ADR-010](adr/0010-templating.md).

Soul Stack has **two engines**, the border between them runs **by the file**:

- **CEL** (google/cel-go) - all YAML expressions: top-level expression keys (`where:`, `when:`, `changed_when:`, `failed_when:`, `until:`) and interpolation `${ … }` in string contexts.
- **Go text/template** + sprig-allowlist - rendering of files with the extension `.tmpl`, performed only by the new core module [`core.file.rendered`](#6-data-transfer-between-engines-pipeline).

There is **only one** engine running in one file. CEL is never executed inside `.tmpl`, text/template is never executed inside `.yml`. This is not an intersection, but a serial data transfer (see [§6](#6-data-transfer-between-engines-pipeline)).

## 1. Context → engine → marker

| Context | Engine | Marker |
|---|---|---|
| Top-level expression-keys (`where:`, `when:`, `changed_when:`, `failed_when:`, `until:`) | CEL (google/cel-go) | whole line = CEL, no wrapper |
| Interpolation in string contexts (`params:`, `apply: input:`, `on:` literals, `vars:`, `essence/_stack.yaml` expressions, `set:` migrations) | CEL | `${ … }` |
| Files in `templates/<path>.tmpl` | Go text/template + sprig allowlist | `{{ … }}` (Go syntax) |

The limit is strict on the file extension: `.yml` → CEL, `.tmpl` → text/template.

## 2. CEL in YAML

### 2.1. Top-level expression keys

These switches accept **a string treated entirely as a CEL expression** - without the `${ … }` wrapper. All return `bool` (except `until:`, see below).

Column **"Side"** - where the expression is calculated along the boundary [ADR-012(d)](adr/0012-keeper-soul-grpc.md) "by external access": `where:` and `assert.that[]` - Keeper (calculated in the render phase; both have access to the full scenario context, including `soulprint.hosts` - `AllowHosts=true`); `when:`/`changed_when:`/`failed_when:`/`until:` - **Soul** (flow-control: depend on `register.*` - results of previous tasks known only on Soul during the run; calculated by sandboxed cel-go sandbox `shared/cel.NewFlowControl`, see [§4](#4-yaml-processing-phase-relationship) and [§7.1](#71-cel--sandbox-by-design)).

| Key | Returns | Side | Where is it used | Source |
|---|---|---|---|---|
| `where:` | bool | Keeper | per-host script step target filter | [scenario/orchestration.md §4](scenario/orchestration.md) |
| `assert.that[]` | bool | Keeper | render-time precondition of the run (each element of the list is a separate CEL-bool; the first `false` breaks render) | [scenario/orchestration.md §2.3](scenario/orchestration.md#23-assert--render-time-precondition) |
| `when:` | bool | Soul | whether to take a step at all (gating BEFORE Apply) | [destiny/tasks.md §9](destiny/tasks.md) |
| `changed_when:` | bool | Soul | determination of the `changed` step status based on the result | [destiny/tasks.md §9](destiny/tasks.md) |
| `failed_when:` | bool | Soul | determination of the `failed` step status based on the result | [destiny/tasks.md §9](destiny/tasks.md) |
| `until:` | bool | Soul | condition for exiting the retry loop | [destiny/tasks.md §9](destiny/tasks.md) |

> **Implementation status.** Soul-sides are calculated by `when:` (gating BEFORE Apply), `changed_when:` and `failed_when:` (override `changed`/`failed` AFTER Apply, see [§4](#4-yaml-processing-phase-relationship) phase 4a) and `until:` (exit from retry loop, phase 4a - after `changed_when`/`failed_when`, [destiny/tasks.md §9](destiny/tasks.md)).
>
> **Semantics `changed_when:`/`failed_when:`** (Soul, AFTER Apply; order - first `changed_when`, then `failed_when`):
> - `changed_when:` → override `changed`: was CHANGED + predicate `false` → OK; was OK + predicate `true` → CHANGED. Does not touch `failed`. `changed_when: false` on probe - the task never `changed` (does not trigger `onchanges:`).
> - `failed_when:` → override `failed`: `true` with OK module → FAILED (artificial failure due to business condition, e.g. `failed_when: register.self.exit_code != 0`); **`failed_when: false` with a fallen module = ignore_errors** - the status is NOT FAILED (OK/CHANGED), the run is NOT stopped (fail-stop does not work), and the original module error is SAVED as informational (`register.<name>.ignored_error` + `TaskEvent.error` without FAILED status). `failed` takes precedence over `changed` (FAILED overrides CHANGED).
> - **Does NOT apply to `TIMED_OUT`:** infrastructure timeout, remains terminal fail-stop; `failed_when: false` does NOT jam it.
>
> **Semantics of `until:`** (Soul, AFTER `changed_when`/`failed_when` each retry loop attempt; full spec - [destiny/tasks.md §9](destiny/tasks.md)): `until:` - **condition for exiting retry**, not override status. `until`-true → exit, the status of the attempt remains as is (`until` DO NOT override `failed`: truthy-until on a FAILED attempt → final FAILED). `until`-false → pause `retry.delay` → next attempt; after `count` attempts with `until`-false → task FAILED (`flowcontrol.until_exhausted`), even if the attempt is OK/CHANGED. On a **TIMED_OUT** attempt, `until` is NOT calculated (the attempt is retraced if there are still attempts left). Activation is the same as for `failed_when` (`flow_context` + `register.*` previous + `register.self.*` fresh attempt with `changed`/`failed` applied).

Example:

```yaml
- name: restart master
  module: core.service.restarted
  params: { name: redis }
  where: register.role.stdout == "master"
  when: input.do_restart
```

### 2.2. Interpolating `${ … }` in string contexts

In any YAML string fields (`params:`, `vars:`, `apply: input:`, `on:` literals, `essence/_stack.yaml` expressions, migration `set:`) the CEL expression is wrapped in `${ … }`.

**Parsing rules:**

- The opening marker is **exactly** the sequence `${`. A single `$` is not a marker and does not go into further processing (`price: "$100"` remains a literal).
- The closing marker is the first `}` at the same parenthesis level of the CEL expression. The brackets `()`, `[]`, `{}` inside `${ … }` are balanced by the CEL parser, not the text scanner.
- Inside `${ … }` the CEL string literals are **single quotes** (`'primary'`) because the outer YAML wrapper takes double quotes.
- Multiline values ​​with CEL - via the YAML block `>` or `|`, or via an explicit join in `vars:`.

Example:

```yaml
params:
  name: redis
  command: "redis-cli replicaof ${ register.master.stdout } 6379"
  replicas: "${ input.replicas * 2 }"
```

### 2.3. Registered CEL functions (starting minimum)

Starting minimum - the exact list is maturing along with pilot implementations. **List expansion - via issue / ADR, not silently.** Third party custom functions - deferred (see [§11](#11-see-also)).

| Signature | Destination | Example |
|---|---|---|
| `size(x) -> int` | Size of string/list/map. | `size(input.hosts) > 0` |
| `contains(s, sub) -> bool` | A substring in a string or an element in a list. | `contains(register.role.stdout, "master")` |
| `s.matches(regex) -> bool` | Regex string matching (RE2, stdlib CEL, parity SaltStack `-E`). Member form. | `input.sid.matches("^db-[0-9]+$")` |
| `s.glob(pattern) -> bool` | Shell-glob matching (`*`/`?`/`[abc]`/`[a-z]` via [filepath.Match](https://pkg.go.dev/path/filepath#Match), parity SaltStack `-G`). Member form. Broken pattern → `false` without error (per-host predicate `target.where` should not fail on a separate host; the syntax validates `soul-lint`). Not available in migration-CEL ([ADR-019], sandbox). | `input.sid.glob("prod-*")` |
| `now() -> timestamp` | Returns the current time at the time the expression is evaluated (eval-time for each call, not the start-time of the run). | `now() - register.deployed_at > duration("1h")` |
| `duration(string) -> duration` | Constructor `duration` from a string (for example, `"1h"`, `"30s"`). Used with `now()` and time arithmetic. | `duration("30s")` |
| `vault(path) -> map / value` | **Keeper-side** reading the Vault KV secret in the CEL-render phase (phase 3, see [§4](#4-yaml-processing-phase-relationship)). Without `#field`, returns the entire map of the secret - the field is taken by CEL access `.field`; with `#field` in the path - one value directly. The real value is substituted into params and goes to Soul; masked only at the output (logs/OTel/UI). `path` is a string literal OR CEL expression from a **trusted** context (`incarnation`/`vars`, not operator-`input`); Resolved by CEL before reading Vault, there is no injection into the Vault query. See [ADR-017](adr/0017-keeper-side-core.md). | `${ vault('secret/redis/admin').password }` or `${ vault('secret/redis/admin#password') }` |
| `merge(m, m...) -> map` ∪ `merge(list(map)) -> map` | Merging maps from left to right by **top-level** key - **SHALLOW** (nested map is NOT merged deeply: the right one completely replaces the matching top key), **last-wins** (the right one overlaps the left one). Pure (no I/O/secrets/crypto). Two forms: **varargs** `merge(m, m...)` (≥1 map argument) and **`merge(list(map))`** (ONE argument-list of maps, flatten from left to right - for a collection of `.map(...)`-comprehension, which must be given to the template as a map for the sake of order determinism, see [§6](#6-data-transfer-between-engines-pipeline)). Non-map argument/element → error; empty list → empty map. Slot for translation "simple typed `input` → detailed config": merge the author's preset with passthrough-`input`-map. Not available in migration-CEL ([ADR-019], sandbox). See [ADR-010 Amendment 2026-06-22](adr/0010-templating.md). | `${ merge(essence.redis.defaults, input.redis_settings) }`; `${ merge(input.users.map(n, {n: input.users[n]})) }` |
| `default(x, y) -> dyn` | Value-or-default (parity Ansible `\| default()`): `x` available/present → `x`, otherwise `y`. **Compile-time macro** (as `vault()`): `x` is expanded to `has(x) ? x : y` BEFORE eval, so CEL greed is bypassed - a missing key does not crash the render. Abbreviates [canonical has()-guard](input.md#reading-optional-without-default-input-canonical-has-guard). **Limitation:** `x` - select-chain or identifier (`essence.tls_enable`, `input.shards`, `a.b.c`); argument-expression (`default(size(x), 0)`, `default(a+b, 0)`) → clear compile error (for calculations - ternary). Pure. Not available in migration-CEL ([ADR-019], sandbox). See [ADR-010 Amendment 2026-06-23](adr/0010-templating.md). | `${ default(essence.tls_enable, false) }`; `${ default(input.redis_settings, {}) }`; `int(default(essence.tls_port, 7379))` |
| `soulprint.self.<path>` | Soulprint fields of the current host. | `soulprint.self.os.family == "debian"` |
| `soulprint.self.traits.<key>` | Operator-set key-value of the host label ([ADR-060](adr/0060-traits.md)). **Registry-projection** (`souls.traits`, not Soul-reported), as `covens`/`choirs`; the keys are dynamic. Value is a scalar or list: comparison scalar `traits.<key> == <v>`, membership `<v> in traits.<list-key>`. | `soulprint.self.traits.namespace == 'dba-ns'`; `'alice' in soulprint.self.traits.owners` |
| `soulprint.hosts -> list` | List of run hosts with stable facts (scenario-only, see [orchestration.md §4.1](scenario/orchestration.md)). | `soulprint.hosts.size()` |
| `soulprint.where(<predicate>) -> list` | Hosts satisfying predicate-**string** (CEL over stable layer - `covens`/`os.*`/`network.*`); role - declared, available via `soulprint.hosts.where(...)` and only in bootstrap-create, see [orchestration.md §4.1](scenario/orchestration.md). A predicate is a **static string literal**, expanded at the compile phase into the built-in CEL filter-comprehension (not runtime execution of the string). Keyword-args (`coven=...`) are not supported (CEL does not have keyword-args). | `soulprint.where("'db' in covens")[0].network.primary_ip` |
| `register.<name>.<path>` | Results `register:` of previous steps; `register.self.*` is the current host in the scenario context. | `register.probe.exit_code == 0` |
| `input.<path>` | `input:` script/destiny block values. | `input.replicas` |
| `essence.<path>` | The values ​​of the collected essence. | `essence.redis.maxmemory` |
| `incarnation.<path>` | Incarnation fields (`name`, `service_version`, `spec.*`). | `incarnation.name` |
| `vars.<path>` | Local task-level and destiny-level `vars:`. | `vars.master_ip` |

> Valid predicates `soulprint.where(...)` are `covens` / `sid` / `network.*` / `os.*`. The role (`role`) in this accessor is **not available**: declared role - only through `soulprint.hosts.where(...)` (and only for bootstrap-create), volatile role - only through probe + `where:` key. See [scenario/orchestration.md §4](scenario/orchestration.md) and [ADR-008](adr/0008-coven-stable-tags.md).

> **Predicate `.where(...)` is a static string literal, not a runtime string.** It is expanded in the **compile phase** of validation: the entire expression is parsed in AST, calls to `.where("<pred>")` to `soulprint.hosts`/`soulprint.where(...)` are rewritten into the native CEL filter-comprehension (`soulprint.hosts.filter(<iter>, <pred>)`), where the predicate fields (`role`/`covens`/`os.*`/…) are qualified by the element field, and the outer context (`incarnation.*`/`input.*`/…) remains as is. The tree is compiled once. Consequences: the predicate **must** be a string literal - dynamic merging (`"'" + incarnation.name + "' in covens"`) is prohibited (understandable compile error "predicate must be a static string literal"); `.where(...)` allowed **only** on `soulprint.hosts`/`soulprint.where(...)` (generic `.where` on a custom list - validation error); nested `.where(...)` inside a predicate is not supported. The first element of the result is `[0]` (native indexing), `.first` is not entered.

> CEL comprehension macros are available inside the predicate - `exists`/`all`/`exists_one`/`map`/`filter` (for example, an idiomatic list filter `covens.exists(c, c == 'db')`), and the macro can appear next to `.where(...)` in one expression (`size(soulprint.hosts.where("role == 'replica'")) > 0 && input.xs.exists(x, x == 2)`). The Rewrite phase parses the expression without expanding macros (so that the rewritten tree is round-trip'd back into a string), and the final compilation expands `.filter`/`.exists`/… natively. the iter variables of such macros (`c`/`x`) are local: they are **not** qualified by the `.where` element field.

### 2.4. Type model

CEL compiles with known context variable types - this is the basis of static checking in `soul-lint`.

| Context | Type Source |
|---|---|
| `input.*` | block `input:` destiny/scenario ([docs/input.md](input.md)) |
| `essence.*` | essence service diagram |
| `incarnation.spec.*` | `state_schema` to `service.yml` |
| `register.<name>.*` | for core - built-in output circuits of modules; for custom - module manifest |
| `soulprint.self.*`, `soulprint.hosts[].*` | spec Soulprint (open Q No. 6 - before its closure types `dyn`, after - specific) |
| `vars.*` | type inference from an RHS expression that declared `vars:` |

If there is no type information, the node receives the type `dyn`. This is not an error: CEL continues to compile and work, but static checking for that node is lost. `soul-lint` raises the warn level.

### 2.5. Compile-cache

The CEL expression is compiled **once** per pair `(scenario, scenario_version)` and evaled multiple times with different activations (per-host, per-iteration loop, per-retry). Compile-cache is required - without it, each `where:` pays parsing + type-checking for each step of each host.

Cache key: `(scenario_id, scenario_git_ref, normalized_expr)`. Invalidation is natural when changing git ref (new version - new key).

## 3. Go text/template in `.tmpl` files

text/template - **only** for rendering files in `templates/<path>.tmpl` by the `core.file.rendered` module. Text/template is not used in YAML.

### 3.1. Strict mode

Render starts with `template.Option("missingkey=error")`. Accessing a missing field (`{{ .vars.missing }}`) - **rendering error**, the step drops normally. Strict-mode - protection against typos in field names (missing field in the context → rendering error, not an empty string). For contributions to SSTI protection, see [§7.3](#73-ssti-through-data).

### 3.2. Render context

The `.tmpl` rendering context is **isolated**, not global. text/template does not see `essence.*`, `register.*`, `soulprint.*`, `input.*` directly - only the values ​​explicitly raised by the author (`params.vars`) plus a fixed set of system fields.

**Context root** is `{ vars, self, role, essence }` plus **conditional** `input` (referring to something else in the root → strict-mode error). This root collects **Keeper-side, per-host** and delivers next to `template_content` (see [§4](#4-yaml-processing-phase-relationship) and [ADR-012(d)](adr/0012-keeper-soul-grpc.md)); Soul passes it to the engine **root**, so the template accesses `.vars.<name>`, `.self.<path>`, `.role`, `.essence.<path>` (and `.input.<name>` when the template reads it).

| Root field | Contents |
|---|---|
| `vars.*` | CEL-rendered values ​​explicitly raised by the author in `params.vars` step `core.file.rendered` ([§6](#6-data-transfer-between-engines-pipeline)). Channel "pass a DERIVATIVE value into the template" (calculated by CEL, which is not in operator-input as is). |
| `input.*` (conditional) | Resolved operator-input pass (Option B). The template reads `.input.<name>` **directly**, without a passthrough wrapper `params.vars` for each input field. Host-invariant (shared run context). **★Conditionally:** Keeper puts the `input` key **only when the template actually accesses `.input.*`** (parse-AST bypass detection `.tmpl`, not string-search - mentioning `.input` in a body comment is not considered an access). Templates on some `.vars` (for example, redis) DO NOT receive `input`, their `render_context` remains `{ vars, self, role, essence }`. ★Secret masking: the secret field in `render_context.input` is sealed according to the pass-through scheme (mechanism S-1, [§7.4](#74-secret-masking)) - also when injecting `input`. |
| `self.network.*` | host network facts (addresses, interfaces): `self.network.primary_ip`, `self.network.interfaces[]` |
| `self.os.*` | os.family, os.version, distribution; composite keys **snake_case**: `self.os.pkg_mgr`, `self.os.init_system` |
| `self.sid` | Current host SID |
| `role` | declared role from `incarnation.spec.hosts[].role` (**bootstrap-create only**: probe is not yet possible; declared role is NOT used in runtime operations, the current role is taken by probe + `register.*`, [ADR-008](adr/0008-coven-stable-tags.md)). It may be empty. |
| `essence.*` | collected essence (read-only snapshot passed to the module) |

`self` is the **same** `soulprint.self` host projection that is available in the CEL phase (`soulprint.self.<path>` in YAML ≡ `.self.<path>` in the template, single point of truth [ADR-018](adr/0018-soulprint-typed.md)). It follows from this: **keys `.self.*` are snake_case** (proto field names), not camelCase. Composite keys are written using `_`: `.self.os.pkg_mgr`, `.self.os.init_system`, `.self.network.primary_ip` - literally like in CEL `soulprint.self.os.pkg_mgr`. camelCase-form (`.self.os.pkgMgr`) - error (`map has no entry for key "pkg_mgr"` will not occur in strict-mode, but the value will not be found).

> **`soulprint.self.*` is also available in the destiny CEL pass** (ADR-009/ADR-010 amendment 2026-06-18). The symmetry of `.tmpl ↔ .yml` extends to the destiny pass: both `render_context.self` and `soulprint.self.<path>` in `.yml` destiny expressions take the same per-host stable layer of the target host. The destiny isolation boundary runs along **self vs run topology**: `soulprint.hosts`/`soulprint.where(...)` (cross-host) remain scenario-only (see [§7.1](#71-cel--sandbox-by-design)).

> **Template context is NOT host-invariant.** `self` per-host, so a self-dependent template gives each host its OWN context root - Keeper collects `render_context` for each targeted host separately ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)). This is legitimate: host invariance is required from OTHER step params, but not from `template_content`/`render_context`.

> The exact set of system fields is fixed in the `core.file.rendered` spec. Here is the standard minimum.

### 3.3. Sprig allowlist

#### Builtin Go text/template

In addition to the allowed subset of sprig, standard builtin Go text/template functions are available in templates: `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `and`, `or`, `not`, `index`, `len`, `print`, `printf`, `println`. They are not included in the sprig allowlist because they are part of the engine itself.

#### Sprig allowlist

Sprig connects **via whitelist**, not via denylist. Allowlist - closed list below. When upgrading sprig - allowlist **is revised explicitly**, new functions are **prohibited by default**.

**Allowed (starting minimum):**

- **Nil-handling:** `default`, `coalesce`, `empty`.
- **Lines:** `upper`, `lower`, `trim`, `trimAll`, `trimPrefix`, `trimSuffix`, `quote`, `squote`, `replace`, `repeat`, `split`, `splitList`, `join`.
- **Conversion:** `toString`, `int`, `int64`, `float64`, `toJson`, `fromJson`.
- **Arithmetic:** `add`, `sub`, `mul`, `div`, `mod`.
- **Base64/hash (without secret generation):** `b64enc`, `b64dec`, `sha256sum`.

**Explicitly prohibited (denylist for documentation - even if sprig is updated and adds alias):**

- **Environment access/execution/network:** `env`, `expandenv`, `exec`, `getHostByName`.
- **Crypto generation (not needed for configs, carries hidden risks):** `derivePassword`, `genCA`, `genPrivateKey`, `genSelfSignedCert`, `genSignedCert`, `buildCustomCert`.
- **Randomness (non-determinism in config rendering - bug):** `randAlphaNum`, `randAlpha`, `randAscii`, `randNumeric`, `randBytes`.
- **Metaprogramming (SSTI vector):** `tpl`, `include` (sprig variant).

Any function not included in the whitelist is not available (call = render error). Extending whitelist is a separate task, not silent.

#### Soul Stack's own functions (not sprig)

`toYaml` and `fromYaml` are **not sprig functions** (upstream sprig does not have them, these are Helm-only functions). In Soul Stack they are implemented as their own functions via goccy/go-yaml and are added to the FuncMap separately from the sprig-allowlist (`shared/tmpl/yaml_funcs.go`). They **do not affect YAML expressions in YAML sources** - this is the text/template engine for `.tmpl` files.

| Function | Signature | Behavior |
|---|---|---|
| `toYaml` | `toYaml(v any) -> string` | Serializes a value in YAML. The tail `\n` is truncated (the result is usually embedded in larger YAML). Serialization error **fails rendering** (unlike the Helm option, which swallows the error into an empty line - silently inserting garbage into the config is more dangerous than a failed step, [§10](#10-error-behavior-and-diagnostics)). |
| `fromYaml` | `fromYaml(s string) -> any` | Parses a YAML string into a structure (map/list/scalar) for further indexing in the template. A parsing error fails the render. |

Example (render of a YAML config fragment from the value passed to `vars`):

```
# templates/app.conf.tmpl
extra_settings:
{{ toYaml .vars.extra }}
```

```yaml
# scenario/destiny
- name: render app config
  module: core.file.rendered
  params:
    path: /etc/app/app.conf
    template: templates/app.conf.tmpl
    vars:
      extra: "${ essence.app.extra_settings }"
```

### 3.4. The `.tmpl` extension is required

The file sent to `core.file.rendered.params.template` must have the extension `.tmpl`. Without extension - destiny validation error on `soul-lint`. The extension serves as both a "this is a template" marker for the statement and a filter when scanning the repo.

Historical `.j2` is not used. Sweep by examples - a separate stage after ADR-010.

## 4. YAML processing phase relationship

All YAML sources (scenario, destiny, essence, keeper.yml, migrations) go through the same processing pipeline. The order is fixed, the phases do not mix.

Phase boundary - **by external access** ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)): who accesses external systems (Vault/registry/CEL-context) - Keeper; local compute without I/O - Soul. Phases 1–3 + delivery literal `.tmpl` - **Keeper-side**; phases 4 (text/template-COMPUTE) and 4a (flow-control CEL) - **Soul-side**.

1. **vault-resolve** (Keeper). All `vault:`-line-references in params are replaced with values ​​**before** CEL entry. `${ … }` in vault links themselves **disabled**: vault link is a string literal, resolves in a phase before CEL. Any `vault: "secret/foo/${ … }"` is a validation error. (This is the `vault:`-**ref** form; the CEL function `vault(...)` is a separate phase 3 mechanism, see below.)
2. **input-resolve** (Keeper). Effective operator-`input` under the destiny/scenario contract, sub-order strictly: **merge defaults + required → scoped input-vault-resolve → value-validation**. Step scoped input-vault-resolve - **separate from the author's (phase 1) limited channel**: the value of the secret field with declared `vault_scope` can be `vault:`-ref, which Keeper resolves keeper-side with the scope + hard deny-list check (`secret/keeper/*`/`secret/internal/*`), the resolve is audited (`input.vault_resolved`), value The secret is not logged. Default-deny: `vault:`-ref in a field without `vault_scope` is an error. `pattern`/`enum`/`min_length` are checked on an **already resolved** value (that's why value validation comes after vault-resolve). Full spec - [docs/input.md → "vault_scope"](input.md#vault_scope-scoped-resolve-vault-ref-in-operator-input). Channel boundary: the author's `vault:`-ref in `params:` (phase 1) and `${ vault(...) }` (phase 3) are the trusted channel of the service author, they do not need `vault_scope` and the deny-list of the operator channel does not apply to them.
3. **CEL-render** (Keeper). Top-level expression key `where:` (target resolution) and all interpolations `${ … }` (params/vars/on) are calculated. Non-string results are substituted according to the rule [§5](#5-non-string-cel-result-in-yaml). Here the **CEL function `vault(path)`** ([§2.3](#23-registered-cel-functions-starting-minimum)) is resolved - keeper-side reading of Vault KV: the real value of the secret is substituted in params and goes to Soul (it is masked only at the output - logs/OTel/UI). Unlike `vault:`-ref (phase 1, static string), `vault(...)` accepts the path as a CEL expression from the trusted context and is accessible in any `${ … }` cell. External access (Vault) - entirely Keeper-side. Flow-control keys `when:`/`changed_when:`/`failed_when:` are **NOT calculated here** - Keeper pulls them into `RenderedTask` as CEL strings (eval - phase 4a, Soul); Keeper also collects `flow_context` (see below).
   - **Template delivery + context root assembly** (Keeper, between phases 3 and 4). For step `core.file.rendered` Keeper: (1) reads the literal content of `templates/<path>.tmpl` from the snapshot (two-level resolve scenario-local→service-level, [ADR-009](adr/0009-scenario-dsl.md)) and puts it in `params.template_content`; (2) collects the **per-host** root of the text/template context `{ vars, self, role, essence }` ([§3.2](#32-render-context)) and puts it in `params.render_context`. The path-key `template` and the flat `vars`-key from params are **removed** (Soul only reads the root from `render_context`). text/template here **not executed** - Keeper delivers the template as-is. A1-option: both `template_content` and `render_context` ride inside `RenderedTask.params`, without proto changes. `render_context` host-variant (`self` per-host) - it is excluded from the per-host params host-invariance check.
   - **Build `flow_context`** (Keeper, for each task). A literal per-host snapshot of the non-register part of the CEL context of the flow-control predicates `{ input, vars, essence, incarnation, self }` (the same as for params rendering, minus `soulprint.hosts` and loop) is placed in `RenderedTask.flow_context`. `register.*` is NOT included in it - Soul builds it itself in phase 4a. Host-variant (`self` per-host), excluded from per-host-verification of host-invariance params.
4. **text/template-render** (Soul, in `core.file.rendered`). Render `template_content` (literal `.tmpl` from Keeper) with `render_context` **root** (built in phase 3) - see [§3.2](#32-render-context) and [§6](#6-data-transfer-between-engines-pipeline). This is a **local compute without I/O**: Soul only pulls `shared/tmpl` (text/template + sprig-allowlist), does not require external access (Vault/network/FS-reading). Three sandbox barriers ([§"Rationale" ADR-010](adr/0010-templating.md): strict-mode, sprig-allowlist without `exec`/`env`/FS network reading, isolated rendering context) **saved on Soul**.
4a. **flow-control CEL** (Soul, gating BEFORE Apply + override AFTER Apply). Before `module.Apply`, Soul evaluates the predicate `when:` with a stripped-down cel-go sandbox (`shared/cel.NewFlowControl`, [§7.1](#71-cel--sandbox-by-design)). Activation: `register.*` (results of previous tasks of running by register name, Soul builds itself) + `flow_context` from Keeper (`input`/`vars`/`essence`/`incarnation` top-level, `soulprint.self` ← `flow_context.self`). `when:false` → task SKIPPED (Apply is not called); connection with `onchanges:` - AND (executed only with `when && onchanges-satisfied`). **Local compute without I/O**: `vault()`/`now()`/`soulprint.hosts`/`soulprint.where` in the sandbox are not structurally available.
   - **`changed_when:`/`failed_when:` - AFTER `module.Apply`** (override `changed`/`failed` by result), the same sandbox and activation, PLUS `register.self.*` - fresh result of the current task (`changed`/`failed`/`timed_out` + `output:` fields from ApplyEvent). Order: first `changed_when` (defines `changed`), then `failed_when` (defines `failed`); `failed` takes priority (FAILED overrides CHANGED). `failed_when: false` on the failed module = ignore_errors (status NOT FAILED, the run does not break, the original error is stored in `register.<name>.ignored_error` + `TaskEvent.error`). `TIMED_OUT` is processed BEFORE this step and `failed_when` is NOT applied to it. Runtime-error CEL in `changed_when:`/`failed_when:` (for example, `register.self.<typo>`) → task FAILED ([§10](#10-error-behavior-and-diagnostics)), as in `when:`.
   - **`until:` - AFTER `changed_when`/`failed_when`** (exit from retry loop, [destiny/tasks.md §9](destiny/tasks.md)), the same sandbox and activation as `failed_when` (`register.self.*` with already applied `changed`/`failed`). `until`-true → exit (attempt status as is, without override); `until`-false → pause `retry.delay` → next attempt; exhaustion of `retry.count` with `until`-false → FAILED (`flowcontrol.until_exhausted`). On a TIMED_OUT attempt, `until` is NOT evaluated. The entire retry loop (including `until`-eval and `delay`) is Soul-side, `delay` is interrupted by canceling the run.
5. **module.Apply** (Soul). The module receives the final parameters (only if the task is not eliminated by phase 4a / onchanges).

> **Pilot constraint (flow-control host-invariance).** Flow-control predicates (`when:`/`changed_when:`/`failed_when:`) must be host-**invariant** on a multi-host target. Reason: the pilot dispatch model distributes ONE `RenderedTask` (with the `flow_context` of the first host) to the entire targeted group, so the host variable predicate would be silently calculated based on the facts of the first host for all. Protection - **two fail-closed circuits**, both temporary until per-host dispatch (separate ADR):
>
> 1. **Direct reference to `soulprint.self` in the predicate text** - regex-guard is cut off by the text `when:`/`changed_when:`/`failed_when:`. A link to `soulprint.self` is valid **only** for a single-host target; on multi-host the render fails-closed. Host-invariant links (`register.*`/`input.*`/`essence.*`/`incarnation.*`) always work. Symmetrical to constraint `loop.when` (`reLoopWhenSoulprint`).
> 2. **Derived host-variable `vars`** - first loop bypass: the value `vars`, derived from `soulprint.self` (for example `vars: { is_debian: "${ soulprint.self.os.family == 'debian' }" }` + `when: vars.is_debian`), flows into `flow_context.vars`, and the text of the soulprint predicate does not contain - regex-guard does not catch it. It is caught by checking the collected `flow_context`-**minus-`self`** between targeted hosts: `input`/`essence`/`incarnation` host-invariant by construction, `self` host-variant by nature (it is covered by circuit 1), `vars` remains - if it differs between hosts, the render fails-closed. Reconciliation is active only if there is a non-empty flow-control predicate (without it, Soul `flow_context` does not read; host-variant `vars`-in-params without `when` crashes on a separate params host-invariance check).

> **Caveat (path-injection into `vault()`).** The function path `vault(path)` must come from a **trusted** context (literal, `incarnation`, `vars`). A path concatenated from operator-`input` - for example `vault('secret/' + input.tenant + '/db')` - allows the operator to point `vault()` to an **arbitrary** KV secret, which according to the contract he should not have access to. This is a **contractual assumption, not a bug**: according to the accepted option (a), the responsibility for ensuring that the path is not derived from `input` lies with the scenario/destiny author, plus RBAC on the secret path in the Vault itself (narrow Keeper token policies). Statically prohibiting input-derived paths in `vault()` is a separate task (if security deems it necessary to strengthen it). There is no text injection into the Vault request: the path is a CEL value calculated before `ReadKV`, and not a string concatenation into the Vault protocol.

## 5. Non-string CEL result in YAML

When `${ … }` returns a non-string (int/bool/list/map/timestamp), the behavior depends on **what's around** the marker in the YAML cell:

**(a) The cell consists of exactly one `${ … }` with no accompanying text.** The result is substituted into a **native YAML type** (int → int, list → list, …).

```yaml
count: "${ input.replicas * 2 }"
# → count: 4   (int)

hosts: "${ soulprint.where(\"'db' in covens\") }"
# → hosts: [ {sid: ..., network: {...}}, ... ]   (list)

redis_config: "${ merge(essence.redis.defaults, input.redis_settings) }"
# → redis_config: { maxmemory: ..., save: ..., ... }   (map)
```

The result of `merge(...)` is **map**, so it obeys rule (a): in a single cell it is substituted by the native structure; merging a map with a string according to rule (b) is an error (you need to move it to a separate cell).

**(b) The cell has text next to `${ … }`.** The result is **stringed** and concatenated to the string.

```yaml
command: "redis-cli replicaof ${ register.master.stdout } 6379"
# → command: "redis-cli replicaof 10.0.0.5 6379"   (string)
```

Stringification is canonical: `int`/`float`/`bool` → their string representation; `timestamp` → ISO-8601; `list`/`map` → validation error (it is impossible to merge a structure with a string, you need either an explicit `toJson` analogue or moving it to a separate cell under rule (a)).

## 6. Data transfer between engines (pipeline)

The engines **do not intersect** - they transmit data sequentially (and work on different sides, [ADR-012(d)](adr/0012-keeper-soul-grpc.md)):

1. **CEL in YAML (Keeper)** calculates the `params.vars` values of the `core.file.rendered` step. CEL has a full scenario context (`input`, `essence`, `register`, `soulprint`, `vars`). Keeper delivers the literal `.tmpl` to `params.template_content`.
2. **text/template in `.tmpl` (Soul)** gets the root `{ vars, self, role, essence }` + **conditional** `input` ([§3.2](#32-render-context)): `vars` - CEL-rendered `params.vars` (derived values), `input` - resolved operator-input of the pass (Option B: the template reads `.input.<name>` directly; the key is placed by the Keeper **only when the template accesses it**, detection by parse-AST), `self`/`role`/`essence` - system fields collected by the Keeper per-host (`render_context`). The text/template context **does not** contain direct access to `register`/`soulprint.hosts` - only operator-`input`, explicitly raised by the author of `vars` and a narrow system set. Soul renders `template_content` locally, without accessing external systems.

Example (runtime operation, master is determined by a live probe):

```yaml
- name: probe actual redis role
  on: ["${ incarnation.name }"]
  module: core.exec.run
  register: redis_role
  changed_when: false
  failed_when: size(register.redis_role) < size(soulprint.hosts)
  params:
    command: "redis-cli role | head -1"

- name: capture master address
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'master'
  module: core.exec.run
  register: master_addr
  changed_when: false
  params:
    command: "hostname -i"

- name: render redis.conf on each host
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'
  module: core.file.rendered
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    vars:
      master_ip: "${ register.master_addr.stdout }"
      maxmemory: "${ essence.redis.maxmemory }"
```

Inside `templates/redis.conf.tmpl`:

```
maxmemory {{ .vars.maxmemory }}
replicaof {{ .vars.master_ip }} 6379
```

#### Bootstrap-create variant

```yaml
# Bootstrap create: redis is not running yet, probe is not possible.
# The topology is taken from the declared role through the scenario-only accessor
# soulprint.hosts and forwarded to destiny via apply: input:.
- name: configure redis on each declared host
  on: ["${ incarnation.name }"]
  apply: destiny/redis-configure
  input:
    role: "${ soulprint.hosts.where(\"sid == soulprint.self.sid\")[0].role }"
    master_ip: "${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip }"
    replicas: "${ input.replicas }"
```

Runtime operation uses probe+register (volatile role is recorded by live polling); bootstrap-create uses `soulprint.hosts.where(...)` (declared role from spec when probe is not yet possible). Don't confuse contexts - this is the footgun of the architecture level ([ADR-008](adr/0008-coven-stable-tags.md)).

One file = one engine. One step = one transition CEL → text/template (or no text/template at all, if the step is not `core.file.rendered`).

#### Collections in the template - map, not list, if the determinism of the row order is important (normative)

Go text/template `range` by **map** traverses the keys in **sorted** order (deterministically), and by **list** iterates in source iteration order, which for a collection built by CEL-comprehension `.map(...)` over map inherits the **non-deterministic** Go-map iteration order. Consequence: rendering a list from `input.<collection>.map(...)` gives file lines in random order between runs → false `changed` in `core.file.rendered` → extra `onchanges` service restart (in the rolling-restart fleet - cascading extra restart).

**Rule:** if a collection is passed to the template for which a stable order of lines is important (ACL files, lists of nodes/sentinel, any line-by-line config), pass it with a **map** (name→object), and in the template `range` with a key (`{{- range $name, $u := .vars.users }}`), and NOT a list. The collection from `.map(...)`-comprehension (CEL gives a list) is folded into a map of the form `merge(list(map))` ([§2.3](#23-registered-cel-functions-starting-minimum)): `${ merge(input.users.map(name, {name: {...}})) }`. Passing a list is only allowed where the order is specified by the author himself (literal list) and does not inherit the map iteration.

## 7. Security model

### 7.1. CEL — sandbox by design

CEL does not have syscall, file or random network access, and does not execute arbitrary code. Only our functions from [§2.3](#23-registered-cel-functions-starting-minimum) are registered (see also [§11](#11-see-also)). Third party custom functions are deferred.

The only function with I/O is `vault(path)` ([§2.3](#23-registered-cel-functions-starting-minimum)): controlled reading of Vault KV via an injected keeper-side client (not random network access - fixed Vault-endpoint from `keeper.yml`). Safe by design: path is a CEL expression from a trusted context (not operator-`input` - see caveat in [§4](#4-yaml-processing-phase-relationship)), resolved by CEL before the request (there is no injection into the Vault request); the secret value is masked in the output (logs/OTel/UI/reports), CEL processes it normally. In contexts without a Vault client (for example, isolated compile-only analysis), the function is not registered and calling it is a validation error. Identifiers with the prefix `__` are reserved for the internal mechanisms of the CEL layer (macro `vault()` is expanded to `__vault_read(path, __vault_resolver)`); an author's expression with any `__` identifier is a validation error so that the author cannot bypass macro `vault()` by directly calling the internal function.

`soulprint.where(...)`/`.where(...)` is safe by construction: the predicate is a static string literal, expanded at the compile phase into the native CEL filter-comprehension (see [§2.3](#23-registered-cel-functions-starting-minimum)), and not a string executed at runtime; injection through the predicate value is excluded constructively (dynamic gluing of the predicate is rejected during validation).

#### Two CEL-envs: Keeper (full) and Soul (flow-control sandbox)

CEL lives on two sides ([ADR-012(d)](adr/0012-keeper-soul-grpc.md)), with different envs:

- **Keeper-env** (`shared/cel.New`, + `WithVault`) - full: render params/`${ … }`, `where:`, `vault(...)`, `soulprint.hosts`/`soulprint.where`. The only side with external access (Vault). The same Keeper-env serves the **scenario pass** (`soulprint.hosts`/`soulprint.where` available) and the **destiny pass** (isolated render pass `apply: destiny`, V2 ADR-009): in the cross-host destiny pass the `soulprint.hosts`/`soulprint.where` accessors are **cut off** (`AllowHosts=false` → isolation error), and the stable self-fact `soulprint.self.*` of the target host **remains available** (ADR-009/ADR-010 amendment 2026-06-18: self is a per-host property, not a scenario-scope; the run topology comes to destiny only through `apply: input:`).
- **Soul flow-control-env** (`shared/cel.NewFlowControl`) - a stripped-down sandbox for `when:`/`changed_when:`/`failed_when:` predicates. Registers **only** functions without I/O (`size`/`contains`/`has`/`keys`/`values`/comprehensions/conversions/operators/`duration`/`glob`/`merge`/`default`) and variables `register.*` (Soul collects from the results of previous tasks by register name) + context from `flow_context` (`input`/`vars`/`essence`/`incarnation` + `soulprint.self`). Prohibited **constructively** (character not registered → compile-error "undeclared reference", sandbox-by-undeclaration, like migration-CEL [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)): `vault(...)`/`now()` (external access/non-determinism), `soulprint.hosts`/`soulprint.where` (cross-host scenario-only - isolation is forced `allowHosts=false`), any `__` identifier. Soul pulls `cel-go`, but NOT `vault`-client: There are no Vault tokens on the host, external access is keeper-only.

### 7.2. text/template - sandbox across three barriers

text/template is more dangerous (historically - vector SSTI in template engines). Sandbox is built by three simultaneous barriers:

1. **Strict mode** ([§3.1](#31-strict-mode)) - typo in field name = error not silently substituting `<no value>`.
2. **Sprig allowlist without `exec`, `env`, `tpl`** ([§3.3](#33-sprig-allowlist)) - there are no functions for executing commands, reading the environment, executing an arbitrary string as a template.
3. **Isolated render context** ([§3.2](#32-render-context)) - no global access to `register`/`soulprint.hosts` and no call to external systems; Exactly the fields of the root `{ vars, self, role, essence }` are available (+ conditional `input` of the pass when the template accesses it) + explicitly raised `vars` + narrow system set collected by Keeper per-host.

### 7.3. SSTI via data

Case: A CEL expression returns an externally controlled value (for example, `${ input.user_name }`), and it is substituted into `vars`. Threat: The value contains `{{ exec "rm -rf /" }}` and is interpreted by text/template as code.

**Closed by all three barriers at the same time:**

- Even if text/template tries to interpret the string, `exec` is missing from the allowlist (barrier 2).
- Strict mode will not allow access to a global variable (barrier 1).
- The render context does not contain sensitive data accessible through the random access-name (barrier 3).

The CEL result hitting `vars` is treated as text/template **as a text literal** and not as a template. text/template does not parse `vars` values ​​recursively - it substitutes them as `.vars.<name>`.

### 7.4. Secret masking

CEL processes values ​​with `secret: true` (from `input:`/essence-standard, [docs/input.md](input.md)) **as usual** - without special processing inside the engine. This is critical: a legitimate "pass secret to `params: { password: \"${ input.password }\" }`" case should work. CEL has no right to refuse.

Masking is applied **at the output** - at the output boundaries:

- logs (rendered-params in step log);
- OTel traces (span attributes);
- UI and API responses;
- run reports.

The value marked `secret: true` in the source schema is masked (see [docs/input.md](input.md)). Masking is the responsibility of the output layer, not the template engine.

#### Masking layers (AUGMENT / OR-union)

Output masking is done by **three declarative layers + regex-last-resort**, combined by OR (any triggered layer → value is masked). Declarative - primary; regex is the last line with an alarm for declarative space.

1. **schema** (declarative, primary) - The field is masked if its path is declared `secret: true` in the active source schema (`input_schema` / `state_schema` / manifest `InputParamDef.Secret`). Does not depend on guessing by key name. On the read-path (`GET /v1/incarnations/...` spec/state/history) Keeper materializes the service secret schema (`state_schema` + scenario input schema `create`) and masks along declared paths (`audit.MaskSecretsWithSchema`), recursively through `properties`/`items` and through **nested** `properties` under `additionalProperties`. **Limitation (TODO with a separate slice):** `secret: true` on the `additionalProperties` node ITSELF (the value of an arbitrary map secret key) read-path schema layer **NOT covered** - `SecretPathSet.IsSecret` does not substitute the `.*` segment. Such a leaf degrades to the vault-origin + regex-last-resort layers (below).

2. **vault-origin** (by content) - the string value contains `vault:<mount>/` (any mount token) → masked entirely. Masking by the CONTENT of the value (vault-ref leaked to the logs/error line), and not by the key name.

3. **seal / sealed-paths** (render-time provenance/taint, central mechanics) - Keeper in the render phase marks the path of the cell `params` **sealed** when its CEL expression reads the secret source:
   - `input.<name>`, where `<name>` is declared by `secret: true` in the active pass input circuit (scenario-input on scenario, destiny-input on destiny);
   - `vault(...)`;
   - transitive - `vars.<x>`/`compute.<x>`, whose value is itself sealed;
   - **source `render_context.input.<secret>` (mechanism S-1, Option B)** - for step `core.file.rendered` operator-input reaches the template via `render_context.input` directly, without passthrough `params.vars`; The raw `${ input.<secret> }` expression is no longer in params, so the AST crawl does not see it. Provenance is restored **declaratively**: Keeper marks the sealed path `render_context.input.<name>` for each input field declared `secret: true` (or carrying `vault_scope`) in the active pass schema. The source of the list is the input pass scheme (names of secret fields), **not** the presence of the expression in params.

Detection - **AST traversal** expressions (not single-ident match): ternary `has(input.tls_cert) ? input.tls_cert : ''` traverses all branches - any one reads the secret → cell sealed (whole-cell taint); gluing `literal + ${ secret }` → the entire result is sealed (whole-value taint, safe). A set of sealed paths (in-memory, current run) is attached to the result of the render pass and brought to the write points of the run (`status_details` / `error_summary` / logs), where masking occurs according to provenance (`audit.MaskSecretsSealed`). The metaphor is "sealed paths" (see [naming-rules.md → seal/sealed-paths](naming-rules.md#domain-entities)).

   > **Destiny Pass Restriction (S-1).** The S-1 mechanism takes secret names from the pass input schema. In the **destiny-pass** (`apply: destiny`), the destiny-input-scheme in the pilot **is not forwarded** (`renderApplyDestiny` builds an isolated pass without `Input`-scheme - the same limitation as for the AST-provenance destiny-secret-input, see spec `core.file.rendered`). Therefore, the secret-fields of destiny-input, which reached `render_context.input`, are **not sealed by S-1** - only vault-origin (if the value is `vault:`-ref) and regex-last-resort (if the key name matches `sensitiveKeyRe`) remain for them. Closing the provenance of destiny-input-secrets - **separate slice** (requires forwarding the destiny-input-scheme to the seal-source).

4. **regex-last-resort** (`sensitiveKeyRe` by key name) - NOT deleted, left as the last frontier. Catches the sensitive-by-name class not covered by a declarative (internal `bootstrap_token`/`jwt`/credentials without a schema). When the secret was caught **only** by regex (schema/vault/seal were silent along this path) - the `keeper_mask_regex_fallback_total` metric is incremented + warn-log: declarative space signal to close the class structurally, and not rely on the key name.

CEL processes sealed values ​​normally (resolves the secret, substitutes it in params, gives Soul to the real ones) - taint only marks the path for the output layer, does not change the wire value `ApplyRequest.params`.

#### Sensitive-by-construction params

In addition to `secret: true`-masking according to the source scheme, there is a category **sensitive-by-construction** - module params, which are secret by their very nature, regardless of what the operator has substituted in them. Such param is **never** logged, not put into OTel attributes, not returned to `output`/`register` and not included in `ApplyEvent` - regardless of the presence of `secret: true` on the value source.

Current list:

- **`core.url.fetched` → `headers`** (`map[string]string`). Request headers normally carry `Authorization: Bearer …` / `Cookie` / API tokens. The module can log only **keys** of headers (not values) when diagnostics are needed; the values ​​and the `headers` block itself in output are excluded by design (see [ADR-015 → `core.url`](adr/0015-core-modules-mvp.md)).
- **`core.http.probe` → `headers`** (`map[string]string`). Same category and same reason as `core.url`: probe request headers carry `Authorization`/`Cookie`/API tokens. The output contains only the list of keys of the requested headers (`headers_keys`); the values ​​and the `headers` block itself are excluded by design (see [ADR-015 → `core.http`](adr/0015-core-modules-mvp.md)). The response body (`body`) is **not** sensitive in its entirety - it goes through the usual `audit.MaskSecrets` (health endpoints return useful readable JSON).

Difference from `secret: true`: masking by scheme depends on the source markup and can be forgotten by the operator; sensitive-by-construction is hardwired into the module implementation and cannot be disabled. A new param of this category is added to the list above when entered.

#### Secure-by-default + explicit opt-out for HTTP modules

HTTP modules (`core.url`, `core.http`) go to the network and therefore form a separate supply-chain/SSRF border. Principle: **our code is secure by default** - only `https://`, SSRF-guard based on the actually resolved IP, TLS chain verification is enabled constructively. The operator sets the policy "what is allowed in this particular call" with **explicit per-call opt-out flags**:

- `allow_http` - allows `http://` (scheme only; `file://`/`ftp://` remain disabled, SSRF-guard is not weakened);
- `insecure_skip_verify` - disables TLS chain checking (self-signed / internal CA);
- `allow_private` - removes SSRF-guard (dial in metadata/loopback/RFC1918/link-local).

Each flag is `default = false`, weakens exactly one independent contour (the flags are orthogonal), and removing any one gives **warning in output `warnings` `ApplyEvent`** (the operator sees the fact of weakening in `RunResult`; only `host` is included in the warning, without the full URL and without `headers` - they are sensitive). This is not an "eternal ban on opportunities", but a safe default for an auditable opt-out (see [ADR-016](adr/0016-parity-license.md)).

> **NORMATIVE INVARIANT.** `shared/netguard` - single default-deny SSRF/https-guard; opt-out is expressed ONLY Soul-side per-call (the module chooses a different path: `ValidateFetchURL(allowHTTP)` / `NewHTTPClient` without dial-guard / `checkRedirectAllowingHTTP`), netguard functions are NOT parameterized and NOT weakened; Keeper-side Augur opt-out does NOT have - default-deny cannot be disabled (different threat-model).

**Delayed CONCERN (not in this slice):** policy level "prohibit insecure on production" (soul-lint-rule / keeper-policy, statically or centrally prohibiting setting opt-out flags outside the dev environment) - a separate approach.

## 8. Multi-line CEL and quotes

- Lines with `${ … }` must be in **double quotes** YAML or in `>`/`|` block forms. Without quotes, the YAML parser will stumble over commas/CEL brackets.
- Inside `${ … }`, CEL string literals are **single quotes** (`'primary'`). The outer YAML wrapper takes double.
- Deep nesting of quotes (`"${ soulprint.hosts.where('role == \"primary\"')[0].network.primary_ip }"` - bootstrap-create only, see §6) - famous footgun: requires `\"` escape inside a CEL literal inside a YAML literal. **Recommendation:** if deeply nested, place the expression in the `vars:` step:

```yaml
vars:
  master_ip: "${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip }"  # bootstrap-create only
params:
  command: "redis-cli replicaof ${ vars.master_ip } 6379"
```

- Multiline CEL expression - via block form:

```yaml
when: >
  input.do_restart &&
  size(soulprint.where("'db' in covens")) > 0
```

## 9. Escaping

### 9.1. Literal `${` in YAML

To insert literal `${` characters without being interpreted as a CEL marker, use a backslash:

```yaml
note: "shell-var literal: \\${HOME}"
# → note: "shell-var literal: ${HOME}"
```

The only supported method is `\${`. We don't introduce YAML techniques (anchor + alias, etc.) for escape, just one way.

### 9.2. Literal `{{` to `.tmpl`

Standard Go text/template:

```
welcome {{ "{{" }} user.name {{ "}}" }}
```

## 10. Error behavior and diagnostics

| Error class | When does | Behavior |
|---|---|---|
| **Compile-error CEL** | syntax, unknown identifier, incompatible types | **before the start of the run**, in the validation phase; coordinate(file, YAML node, position in expression) + CEL message; run does not start |
| **Runtime-error CEL** | div-by-zero, access to the `null` field, etc. | step fails, coordinate + message; standard processing via `onfail:` |
| **text/template strict-mode error** | accessing a missing field, calling a forbidden function | step rendering crashes, the same standard processing via `onfail:` |
| **Sprig-allowlist violation** | use of prohibited function | compile-error at text/template level, step drops |
| **`${` without closing `}`** | interpolation syntax error | compile-error CEL before start of run |
| **`vault:` with `${ … }`** | prohibited combination (see [§4](#4-yaml-processing-phase-relationship)) | validation error before start of run |
| **Non-string CEL result when merging** ([§5](#5-non-string-cel-result-in-yaml) case (b), list/map) | gluing a structure to a string | runtime-error |

All template engine errors go to OTel-trace as **structured events** with coordinates (file, YAML node/line `.tmpl`, position in expression), source expression and engine message. `soul-lint` uses the same coordinate forms so that the linter output and runtime errors point to the same location.

## 11. See also

- [architecture.md → ADR-010](adr/0010-templating.md) - fixing the choice of engines.
- [architecture.md → ADR-003](adr/0003-destiny-format.md#adr-003-destiny-format--yaml-with-a-typed-schema-cuejson-schema) - place of the template engine in the pipeline `render → validate → apply`.
- [scenario/orchestration.md](scenario/orchestration.md) — `where:`/`when:` in the context of scenario.
- [destiny/tasks.md §10](destiny/tasks.md) - template context inside destiny.
- [keeper/modules.md](keeper/modules.md) - keeper-side core modules (general format into which `core.file.rendered` will be built on Soul-side).
- [naming-rules.md](naming-rules.md) — `core.file.rendered`, convention `.tmpl`, marker `${ … }`.
- [docs/input.md](input.md) - `secret: true`-flag processed by masking at the output.
