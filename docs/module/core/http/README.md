# core.http

Soul-side HTTP API module built into the `soul` binary. It returns endpoint
responses to `register`; it never writes response bytes to disk.

Implementation:

- [`http.go`](../../../../soul/internal/coremod/http/http.go) — dispatch and validation;
- [`probe.go`](../../../../soul/internal/coremod/http/probe.go) — shared transport/response contract and read-only `probe`;
- [`request.go`](../../../../soul/internal/coremod/http/request.go) — mutating `request`.

## Verbs

| Verb | Methods | Semantics |
|---|---|---|
| `probe` | `GET` / `HEAD` (default `GET`) | Strictly read-only; `changed=false` always. Backward-compatible with the original core.http contract. |
| `request` | `POST` / `PUT` / `PATCH` / `DELETE` (required) | Exactly one mutating API call; an expected response reports `changed=true`. GET/HEAD are rejected. |

`request` does not hide retries. API idempotency is the caller's contract, and
additional attempts must be declared with scenario `retry`/`until` (or guarded
by a prior probe/`when`).

## Parameters

Both verbs accept:

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `url` | string | required | Target URL. HTTPS only unless `allow_http: true`; only HTTP(S) schemes are accepted. |
| `headers` | map of string | optional | Request headers. Every value is sensitive-by-construction; only keys can appear in output. |
| `status_codes` | list of int | optional, `[200]` | Accepted response statuses. A mismatch fails the step but keeps response diagnostics. An empty list also falls back to `[200]`. |
| `timeout` | duration string | optional, `30s` | Positive Soul Stack duration (`250ms`, `30s`, `2m`, `<N>d`). |
| `allow_private` | bool | optional, `false` | Lift the resolved-IP SSRF guard for an explicitly trusted private/loopback target. |
| `allow_http` | bool | optional, `false` | Allow a plaintext `http://` target. For probe it also permits HTTP redirect hops; request never follows redirects. Does not lift the private-IP guard. |
| `insecure_skip_verify` | bool | optional, `false` | Disable TLS certificate verification. Does not lift the other guards. |

`probe` additionally accepts optional `method` (`GET` or `HEAD`, default
`GET`). `request` instead requires an explicit `method` from
`POST|PUT|PATCH|DELETE` and accepts:

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `body` | string | optional, empty | Request body. It is never echoed; `output.body` is the response body. |
| `content_type` | string | optional | Sets `Content-Type`. When both forms are present, this value overrides `headers.Content-Type`. |

## Response and failure contract

Success returns:

```text
status, body, truncated, elapsed_ms, headers_keys, changed, warnings?
```

- Response `body` is capped at 64 KiB by bytes. The read stops at the cap, and
  the final text is capped again after UTF-8 sanitation and secret redaction —
  both can expand bytes. A cut rolls back to the last complete UTF-8 rune and
  sets `truncated=true`.
- A truncated `body` also drops a trailing fragment that is a prefix of an
  effective header value. A cut precedes redaction, so a value straddling it
  would otherwise survive as a plaintext prefix that the whole-value masker
  cannot see. There are **three** cuts and every one of them is repaired: the
  read cap, the roll-back to the last complete rune immediately after it, and
  the output cap that masking can push the text back over. The first two are
  repaired on the raw bytes, while the fragment is still byte-exact —
  afterwards UTF-8 repair rewrites a cut rune into `U+FFFD` and makes the
  fragment unrecognisable. Both descend together over one fragment table
  computed once: the roll-back removes a single byte per step, and the
  response body is chosen by the endpoint, so rebuilding the table per step
  would hand it a CPU sink. Only an *incomplete* value is removed: one that
  ends whole at the cut is left for the masker, since a value that ends with
  its own prefix (`secretsecret`) would otherwise be cut in half and leak the
  remainder. Removal repeats until the cut is clean, because dropping one
  fragment can uncover another. Intact bodies are never trimmed this way.
- Any echoed effective request-header values are masked, then vault-ref
  substrings. Header values go first so that a value which itself carries an
  unresolved `${ vault(...) }` ref is still matched as a whole rather than
  rewritten in the middle and left with its leading bytes exposed.
  Masking works on **byte coverage**, not on replacing one occurrence at a
  time: every occurrence of every sensitive value marks its bytes, and each
  maximal covered run collapses into a single `***MASKED***`. Two occurrences
  can overlap — two values abutting on a shared byte, one value overlapping
  itself when its first byte equals its last, or a cut landing mid-repeat and
  manufacturing the shape — and a matcher that consumes left to right has
  already moved past the second occurrence's start, leaving up to
  `len(value)-1` plaintext bytes of a credential in the body. A side effect
  worth knowing: adjacent occurrences merge into one mask, so the number of
  masks in the output does not count the echoes.
  The whole response body is not treated as secret, so an endpoint must not
  return unrelated arbitrary plaintext secrets.
- The request `body` param is deliberately **not** masked in the response.
  Unlike a header value it is ordinary payload — usually the very document the
  operator wants to read back — and masking it would blank out every echo of
  it, down to a body as common as `{}`. Put secrets in headers, not in the
  body, if they must not come back in `output.body`.
- `headers_keys` is the sorted set of effective request-header names. Values
  and the raw `headers` map are never returned. `Content-Type` supplied via
  `content_type` is included.
- `probe` success has `changed=false`; `request` success has `changed=true`.
- A status outside `status_codes` returns `failed` with the same diagnostic
  response and `changed=false`.
- DNS/TLS/timeout failures return `failed` without response output. Probe also
  reports a blocked/invalid redirect this way. Request never follows a
  redirect: its first 3xx is handled like any other status against
  `status_codes`.
- Every lifted guard adds a host-only `warnings` entry. URL path/query, header
  values, request body and tokens never enter warnings.

## Security and capabilities

The three guards are independent and armed by default:

- scheme: HTTPS-only, lifted only by `allow_http`;
- resolved-address SSRF guard: metadata, loopback, RFC1918 and link-local are
  blocked, lifted only by `allow_private`;
- TLS chain verification, lifted only by `insecure_skip_verify`.

Probe redirects reuse the same scheme and resolved-IP policy. Request stops at
the first response, so a 307/308 cannot replay a mutating method/body; scenario
retry remains the only source of another mutation. Header values are
sensitive-by-construction, including `Authorization`, cookies and
`X-Consul-Token`; they are transmitted but redacted even if an endpoint or
transport echoes them, and are excluded from module output, diagnostic
messages and logs/audit events.

One `Apply` opens exactly one `Do` call, and no redirect can expand it. Note
one transport-level exception the module deliberately does not intercept: Go's
`net/http` treats a request carrying `Idempotency-Key` or `X-Idempotency-Key`
as replayable and may resend it once if a pooled connection dies before any
response byte arrives. That is opt-in — the author has to set a header whose
entire meaning is "this request is safe to repeat" — and stripping it would
silently contradict the author. Scenario `retry` remains the only module-level
source of another mutation.

The manifest grants exactly `network_outbound`. The module executes no
subprocess and performs no filesystem writes.

Only exact state `core.http.probe` is admitted by the Errand runner.
`core.http.request` is mutating and therefore default-deny on that ad-hoc
contour.

## Consul Agent examples

Consul Agent on `127.0.0.1:8500` requires both explicit opt-outs:
`allow_http: true` for plaintext HTTP and `allow_private: true` for loopback.
Scenario retry controls any repeated mutation.

Register a Redis exporter:

```yaml
- name: Register Redis exporter in the local Consul Agent
  module: core.http.request
  params:
    url: http://127.0.0.1:8500/v1/agent/service/register?replace-existing-checks=true
    method: PUT
    allow_http: true
    allow_private: true
    content_type: application/json
    headers:
      X-Consul-Token: "${ input.consul_token }"
    body: >-
      {"Name":"redis-exporter","ID":"redis-exporter","Address":"127.0.0.1","Port":9121}
    status_codes: [200]
```

Enable maintenance mode without losing the Service ID:

```yaml
- name: Put Redis exporter in maintenance
  module: core.http.request
  params:
    url: http://127.0.0.1:8500/v1/agent/service/maintenance/redis-exporter?enable=true
    method: PUT
    allow_http: true
    allow_private: true
    headers: { X-Consul-Token: "${ input.consul_token }" }
```

Deregister on destroy:

```yaml
- name: Deregister Redis exporter
  module: core.http.request
  params:
    url: http://127.0.0.1:8500/v1/agent/service/deregister/redis-exporter
    method: PUT
    allow_http: true
    allow_private: true
    headers: { X-Consul-Token: "${ input.consul_token }" }
```

These are contract examples only; downstream service wiring is intentionally
outside NIM-608.

## See also

- [ADR-015](../../../adr/0015-core-modules-mvp.md) — core.http contract and NIM-608 amendment.
- [ADR-016](../../../adr/0016-parity-license.md) — secure-by-default HTTP policy.
- [ADR-033](../../../adr/0033-errand.md) — exact-state Errand boundary.
- [templating.md](../../../templating.md) — sensitive-by-construction parameters.
- [core.url](../url/README.md) — download-to-filesystem module.
