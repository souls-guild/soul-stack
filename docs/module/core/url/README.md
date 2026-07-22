# core.url

Uploading a file via URL (a URL-fetch capability reworked into a secure MVP).
**Soul-side**, statically built into the `soul` binary. Implementation -
[`soul/internal/coremod/url/url.go`](../../../../soul/internal/coremod/url/url.go) (contract
and validation) and [`soul/internal/coremod/url/fetched.go`](../../../../soul/internal/coremod/url/fetched.go)
(download, verify, idempotency).

The module is focused on supply-chain security and works according to the principle
**secure-by-default + explicit opt-out**: default `https://`, SSRF-guard,
checking the TLS chain and checksum verification **before** the file appears in the target
paths - see Security below. Each of these restrictions
the operator can clear a separate flag (`allow_http` / `allow_private` /
`insecure_skip_verify`), removing exactly one contour; withdrawal -> line in output
`warnings` (the operator sees apply as a result). Without shell.

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `fetched` | The file at `url` is materialized into `path` with `mode`/`owner`/`group` specified. | See branch table below - behavior depends on whether `checksum` is set. |

Behavior of `fetched` by branches:

| Condition | Action | `changed` |
|---|---|---|
| `checksum` is specified + the file exists and hash matches | content **does not download**; `mode`/`owner`/`group` are brought to the declaration (convergence) | `true` only if the attribute |
| `checksum` specified, no file / hash did not match | download in temp → verify by `checksum` → atomic rename; mismatch → `failed`, temp is deleted, the target path is not touched | `true` |
| `checksum` **not** specified + file exists and matches SHA-256 | download to temp → compare SHA-256 with existing → no entry; `mode`/`owner`/`group` converge | `true` only if the attribute |
| `checksum` **not** specified + contents different / no file | download in temp → atomic rename | `true` |

## fetched — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `url` | string | required | Download address. By default, **only `https://`** - `http://` and `file://` are rejected in `Validate` (downgrade and reading local FS). `http://` is only allowed with `allow_http: true`. |
| `path` | string | required | Target file path. The download temp file is created in the same directory (for atomic rename). |
| `checksum` | string | optional | The expected hash is in the form `<algo>:<hex>`. `sha256` and `sha1` are supported (`md5` is deliberately **not** supported - weak for supply-chain). Hex is checked for length and alphabet. If set, verify before publishing; if not, idempotency uses SHA-256. |
| `mode` | string | optional | Rights in octal form (`"0644"`, `"0755"`). When writing, applies to temp before rename; in a no-op branch, it is checked/corrected only when `mode` is specified (an empty `mode` does not impose a default on an existing file). |
| `owner` | string | optional | Owner (username). Resolved via `/etc/passwd`. |
| `group` | string | optional | Group (name). Resolved via `/etc/group`. |
| `headers` | map | optional | HTTP request headers (for example `Authorization`). **Sensitive-by-construction** ([ADR-010](../../../adr/0010-templating.md) §7.4): Values ​​are never logged or sent to output/register. `If-None-Match` / `If-Modified-Since` gives a conditional GET here - see [304 conditional-GET](#304-conditional-get). |
| `timeout` | string | optional (default `300s`) | Request timeout in `duration` Soul Stack form (`time.ParseDuration` + suffix `<N>d`). Must be positive. |
| `allow_http` | bool | optional (default `false`) | Removes the `http://` prohibition: the operator allows the plaintext channel. **DOES NOT open SSRF** — dial-guard continues to block private addresses (separate flag `allow_private`). Removal → line in output `warnings`. Case: internal artefact-mirror without TLS. |
| `insecure_skip_verify` | bool | optional (default `false`) | Disables TLS chain checking (self-signed / internal CA). **MITM risk** - cocked only consciously. Removal → line in output `warnings`. Case: loading from an internal service under its own CA, not added to the system trust store. |
| `allow_private` | bool | optional (default `false`) | Removes SSRF-guard: allows dial in metadata / loopback / RFC1918 / link-local. Removal → line in output `warnings`. Case: legitimate internal endpoint (internal repo mirror in RFC1918). |

## 304 conditional-GET

If the operator passes a cache validator to `headers` (`If-None-Match: "<etag>"` or
`If-Modified-Since: <date>`), the server can respond with **304 Not Modified** - the body is not
is transmitted. The module interprets this normally; a separate param is not entered (validator
is placed through the usual `headers`):

| Situation | Behavior |
|---|---|
| 304 + local file by `path` **exists** | content is relevant → no-op: `mode`/`owner`/`group` are brought to the declaration (convergence), `output.sha256`/`size` - based on an existing file. `changed=true` only if the attribute was edited. |
| 304 + local file **no** | **`failed`** (fail-fast): the server sent "no change", but there is no cache - this is stale `If-None-Match` without a local copy. There is nothing to download, body 304 does not carry payload. Message: `server returned 304 but no local file at <path>: stale If-None-Match without cache`. |

Works with both `https://` and `http://` (with `allow_http`). 304 checked
**before** the general 2xx check, so does not fall into `unexpected status`.

## Security

The module is the main supply-chain boundary for downloading files, so by default
the restrictions are strict. This is **secure-by-default**: each contour is removed separately
opt-out flag (see params table), the flags are orthogonal, removing any one puts
line in output `warnings` (the operator sees apply as a result; only `host`, without
full URL and without `headers` - query/path and headers may contain secrets).
([ADR-016](../../../adr/0016-parity-license.md)
"safety first"):

- **By default only `https://`** (`util.ValidateFetchURL(url, allow_http)`).
The scheme is checked through `url.Parse` case-insensitively and truly -
`http://`, `file://` and tricks like `https://\nhttp://evil` are rejected.
`allow_http: true` allows `http://`, but `file://`/`ftp://` remain
are prohibited and SSRF-guard is not weakened.
- **SSRF-guard.** Dial in metadata (`169.254.169.254`), loopback, RFC1918,
link-local/CGNAT/site-local is blocked by **actually resolved IP**. This
closes direct SSRF (theft of cloud-metadata IAM credentials) and DNS-rebind (dial goes
using an already verified IP, without a second name resolution). Host with a pair of A records
"public + metadata" is blocked entirely. Can be removed **only** with a flag
`allow_private: true` (legitimate internal endpoint); `allow_http` it is not
touches.
- **TLS is the default system trust store**. Chain check is removed
flag `insecure_skip_verify: true` (self-signed / internal CA, MITM risk).
- **Redirects**: `CheckRedirect` rejects downgrade `https→http`. When
`allow_http: true` downgrade-hop is allowed (paired with https-only downgrade), but
redirect to non-`http(s)` scheme is still blocked, as is dial to private
addresses on hop (if `allow_private` is not removed).
- **Checksum verify BEFORE publishing.** Downloading always goes to a temporary file in
directory `path`, the hash is calculated on the fly. Verify against `checksum` occurs before
`rename`: if there is a mismatch, the temp is removed, the target path is not touched - **invalid
hash never materializes**.
- **`headers` are not disclosed** anywhere: neither in the logs, nor in output/register, nor in
echo field `url`.

## Capabilities / side-effects

- **Network access:** HTTPS GET via `url` (with SSRF-guard).
- **Changes the file system:** creates/overwrites `path` via atomic rename
from temp file; rules by mode and owner. Requires permissions for system paths.
- **Does not perform subprocesses** - download, hash and write in-process,
without shell.

## Output / register

`{ path, url, sha256, size, changed, fetched: true }`, where `sha256` is SHA-256
written/matched content (always SHA-256 for output, even if
`checksum` was set by sha1), `size` - size in bytes, `url` - echo without `headers`.

## Example

`fetched` with checksum verification - download the release from GitHub Releases:

```yaml
- name: Fetch node_exporter tarball
  module: core.url.fetched
  params:
    url: "${ input.base_url + '/v' + input.version + '/node_exporter-' + input.version + '.linux-' + soulprint.self.os.arch + '.tar.gz' }"
    path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
    checksum: "${ input.sha256 }"
    mode: "0644"
```

(from [`examples/destiny/node-exporter/tasks/install.yml`](../../../../examples/destiny/node-exporter/tasks/install.yml) —
triple "download → unpack → decompose binary." `base_url` — input with default
GitHub Releases (overridden on the mirror), `arch` - from `soulprint.self.os.arch`)

## See also

- [README.md](../../README.md) - directory of core modules.
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-010](../../../adr/0010-templating.md) - sensitive-by-construction `headers`.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
- [ADR-016](../../../adr/0016-parity-license.md) - "safety comes first."
