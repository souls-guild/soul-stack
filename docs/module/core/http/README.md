# core.http

Read-probe HTTP endpoint (health-check / API-readiness / read version).
**Soul-side**, statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/http/http.go`](../../../../soul/internal/coremod/http/http.go)
(dispatcher + params validation) and
[`soul/internal/coremod/http/probe.go`](../../../../soul/internal/coremod/http/probe.go)
(verb `probe`). The idea is borrowed from Ansible `uri`, but deliberately narrowed down to
**readings**: "doing well" instead of muddy permissiveness.

`probe` is a **verb-form**, not a declarative-state: it does not result in anything
state, and returns facts about the endpoint in `register`. Mutating HTTP
(POST/PUT/PATCH/DELETE) deliberately postponed post-MVP by a separate ADR extension
(probably `core.http.request`) - then the changed contract for
mutations. `probe` remains strictly read-only.

## Read-only: doesn't change anything

`changed = false` **always**, constructive and non-configurable - read-probe not
changes the state of the host. Use case - `core.exec.run`: the module gives facts, but
interprets them `changed_when:` at the scenario level. Idempotency -
by nature (no-op for state).

## States

| State (verb) | Destination | `changed` |
|---|---|---|
| `probe` | One GET/HEAD request to `url`; response (status/body/elapsed_ms/headers_keys) is returned in `register`. | `false` always (read-only). |

## probe — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `url` | string | required | Target URL. **By default, only `https://`** (remove before `http(s)` - `allow_http`; `file://` is always disabled). See "Security". |
| `method` | string | optional (default `GET`) | HTTP method. Only read-only `GET` / `HEAD` are allowed (the comparison is case-insensitive, `get` → `GET`). Mutating methods are rejected on `Validate`. `HEAD` does not read the body. |
| `headers` | map | optional | Request headers. **Sensitive-by-construction** ([ADR-010 §7.4](../../../templating.md)): values ​​are never logged and do not end up in output - only the list of **keys** (`headers_keys`) is given in output. |
| `status_codes` | list of ints | optional (default `[200]`) | A set of expected status codes. Actual status out of set → step `failed` (but with output attached for diagnostics). |
| `timeout` | string (duration) | optional (default `30s`) | Request timeout in convention `duration` Soul Stack (`time.ParseDuration` + suffix `<N>d`). Must be positive. Shorter than `core.url` (health-check, not download). |
| `allow_private` | bool | optional (default `false`) | Removes **SSRF-guard** for legitimate internal health-check (see "Security"). |
| `allow_http` | bool | optional (default `false`) | Removes **https-only**: allows `http://` (downgrade redirect https→http is also allowed). `file://` remains prohibited. **Not** opens SSRF - dial-guard lives separately (orthogonal to `allow_private`). |
| `insecure_skip_verify` | bool | optional (default `false`) | Disables **TLS verification** (self-signed / internal CA). MITM risk - platoon only for a trusted internal endpoint. Orthogonal to other flags. |

All three flags are **secure-by-default + explicit opt-out**: each weakens a separate one
independent circuit, removing one does not affect the others. When cocking any
of these, probe puts `warnings` in the output field (see "Output / register").

## Security

[ADR-016](../../../adr/0016-parity-license.md) "safety first":

All loops **secure-by-default**; each is filmed separately
opt-out-param, and removing one **doesn't** weaken the others (the flags are orthogonal).

- **https-only** (default) - `http://` and `file://` are rejected
(`util.ValidateFetchURL`). Remove to `http(s)` - `allow_http: true` (`file://`
remains disabled even with `allow_http`).
- **TLS** - system trust store (default). Disable verification for
self-signed / internal CA - `insecure_skip_verify: true`. MITM risk:
Arm only for a trusted internal endpoint.
- **Downgrade-redirect protection** (default) - redirect to non-https is blocked.
With `allow_http: true` downgrade-hop `https→http` is allowed (paired with
resolved http); redirect to a non-`http(s)` scheme is always blocked.
- **SSRF-guard** (default) - probe in metadata / loopback / RFC1918 / link-local
blocked by actually resolved IP (closes direct SSRF to
cloud-metadata IAM `169.254.169.254` and DNS-rebind). Remove for legitimate
internal health-check — `allow_private: true`. **`allow_http` SSRF not
opens** - dial-guard lives separately (http still won't reach
metadata/loopback without `allow_private`).
- **Warning when guard is removed** - when any opt-out flag is armed, the probe is placed
in the output field `warnings` (one line per captured contour): the operator sees
fact of weakening security. Only **host** is included in the warning (NOT the full URL -
it can carry `path`/`query` with sensitive data, and NOT headers). Formulations:
  `TLS verification disabled (insecure_skip_verify) for <host>` /
  `plaintext http allowed (allow_http) for <host>` /
  `SSRF-guard disabled (allow_private) for <host>`.
- **Headers** — sensitive-by-construction (values are not logged, in output —
keys only).
- **Body Cap** - response reads no more than `64 KiB` (OOM protection); over the limit
the body is discarded, `truncated: true` is put in output (the border is cut according to
full UTF-8 rune).

## Capabilities / side-effects

- **Does not execute subprocesses.** Pure in-memory HTTP client
([`probe.go`](../../../../soul/internal/coremod/http/probe.go) - `doer.Do`); in
manifest ([`http.yaml`](../../../../shared/coremanifest/http.yaml)) declared
only [`network_outbound`](../../../naming-rules.md#required_capabilities-enum)
(outgoing request), **without** `exec_subprocess` and `fs_write_root`.
- **Read-only, does not write anything.** Unlike [`core.url`](../url/README.md) (that
downloads the file and writes to FS → declares `fs_write_root`), `probe` does not touch
the file system in general: the response is read into memory and returned to `register`.
`changed = false` is constructive (see "Read-only: doesn't change anything").
- **Body Cap - `64 KiB`** (`maxBodyBytes`,
[`probe.go`](../../../../soul/internal/coremod/http/probe.go)): OOM protection on
great answer. Above the limit, the body is discarded (`truncated: true`), border
is cut using the full UTF-8 rune.
- **SSRF-guard, downgrade- and TLS-protection** on a network call (see "Security"):
default client blocks metadata/loopback/RFC1918/link-local by resolved IP
(`allow_private`), rejects the downgrade redirect (`allow_http`) and verifies
TLS chain (`insecure_skip_verify`). The HTTP client is built **per-call** from these
three orthogonal flags (`util.NewHTTPClient(util.HTTPClientOpts{…})`), not
is selected from pre-assembled instances.

## Output / register

`{ status, body, truncated, elapsed_ms, changed: false }`; `headers_keys`
is added only if `headers` (sorted list of keys,
without values). `warnings` (list of strings) is added only if it was armed
at least one opt-out flag (`allow_private` / `allow_http` / `insecure_skip_verify`)
- one line per captured contour, with `host` (without the full URL and headers).

The body (`body`) is given as is - sensitive - the whole thing is **not** considered
(the health endpoint normally returns `{"status":"ok"}`, which is why the probe is needed).
**only** vault-ref substrings are masked from the body (`vault:…` → `***MASKED***`),
including ref inside JSON. An arbitrary plaintext secret in the body is **not** masked:
the body is semi-trusted, and the operator should not put something in the probe endpoint that it should not
glow. Binary/broken body bytes are converted to valid UTF-8 (replaced with
U+FFFD) so that the probe returns a clean result rather than dropping a step.

If the status is outside `status_codes`, the step is `failed`, but with the same output (actual
status/body are needed for diagnostics). Transport error (DNS/TLS/timeout/
blocked downgrade redirect) → `failed` without output.

## Example

```yaml
- name: Wait until the service answers HTTP 200
  module: core.http.probe
  register: health
  retry: { count: 5, delay: 3s }
  params:
    url: https://service.internal:8443/healthz
    method: GET
    status_codes: [200]
    allow_private: true
```

(minimum valid example - there are no tasks for `core.http` in `examples/` yet)

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/url/README.md](../url/README.md) - downloading a file via URL (`fetched`, also https-only).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-016](../../../adr/0016-parity-license.md) - "security comes first" (https-only, SSRF-guard).
- [templating.md](../../../templating.md) - secret-masking and sensitive-by-construction (§7.4).
