# core-beacon

Built-in **core-beacon** - body [Vigil](../../../naming-rules.md)
(Soul-side event-driven monitoring, [ADR-030](../../../adr/0030-vigil-oracle.md)).
Beacon observes the state of the host and when it changes (**edge-triggered**) it raises
[Portent](../../../naming-rules.md) Soul → Keeper.

**Read-only by design** - the beacon observes, but **DOES NOT mutate** the host (invariant
[ADR-030](../../../adr/0030-vigil-oracle.md)).
This distinguishes beacons from core modules (`core.<module>.<state>` leads the host to
condition). The Beacon is addressed as `core.beacon.<name>` in the `VigilDef.check` field.

Implementation - [`soul/internal/beacon/`](../../../../soul/internal/beacon/); register
built-in beacon collects `beacon.Default()`
([`beacon.go`](../../../../soul/internal/beacon/beacon.go)). Plugin-beacon
(kind `soul_beacon`, ADR-030 V5-2) - see section [Custom soul_beacon plugins](#custom-soul_beacon-plugins)
below; their registry on top of pluginhost collects `beacon.NewPluginRegistry`,
connection to core - `beacon.NewCompositeRegistry`.

## Typed PortentPayload (V5-1)

With V5-1 ([ADR-030 amendment 2026-05-26](../../../adr/0030-vigil-oracle.md))
built-in core-beacons expose **typed payload** to `PortentEvent.payload`
(oneof) in parallel with legacy `PortentEvent.data` (Struct). Each built-in
beacon corresponds to typed-message:

| Beacon | Typed-message | Where-CEL access |
|---|---|---|
| `core.beacon.file_changed` | `FileChangedPortent` | `event.file_changed.<field>` |
| `core.beacon.service_down` | `ServiceDownPortent` | `event.service_down.<field>` |
| `core.beacon.port_closed` | `PortClosedPortent` | `event.port_closed.<field>` |
| `core.beacon.disk_full` | `DiskFullPortent` | `event.disk_full.<field>` |
| `core.beacon.process_absent` | `ProcessAbsentPortent` | `event.process_absent.<field>` |
| `core.beacon.http_unhealthy` | `HttpUnhealthyPortent` | `event.http_unhealthy.<field>` |
| `core.beacon.inotify` | `InotifyPortent` | `event.inotify.<field>` |
| plugin-beacon (V5-2, `soul_beacon.*`) | `Struct` to `event.custom` | `event.custom.<field>` |

The exact shapes are the "**Typed-payload**" tables in the sections below.

### Deprecation `PortentEvent.data` (Struct)

The `data` field is marked `[deprecated = true]` in proto. Transition plan
(**1-release WARN → hard-cut**, parity with push S7-decision):

1. **V5-1 (now) - hand-off period.** Soul-side emitter-mapper fills
**BOTH** branches: typed `payload` + legacy `data`. One WARN log per process
at the first issue. Where-CEL works in both styles:
`event.data.<field>` and `event.<typed_branch>.<field>` are equivalent.
2. **V5-2…V5-3.** Same hand-off; new where-CEL - only typed form.
3. **S5-final (one production release later).** Hard-cut: `data` removed
from proto-scheme, Soul-side emits only typed `payload`, where-CEL
`event.data.*` stops compiling (compile error in Decree).

Type-mismatch (where-CEL is expecting `event.file_changed`, arrived `service_down`)
→ fail-safe **no-match** (default-deny): missing branch gives
no-such-key, cel-go returns runtime-error, Oracle interprets it as "not matched".

Implementation contract - [`soul/internal/beacon/typed_payload.go`](../../../../soul/internal/beacon/typed_payload.go)
(`fillTypedPayload`); CEL activation on the Keeper side -
[`keeper/internal/oracle/where.go`](../../../../keeper/internal/oracle/where.go)
(`buildEventActivation`).

## Contract `State`

`Check` returns `State` - **meaningful host status string**. Scheduler
compares it with the previous value (edge-triggered): change `State` → one
`Portent`. The semantics of the string is at the discretion of the specific beacon. Beacon **not**
emits the event itself and **doesn't** store the baseline - the scheduler does that.

`data` - `Struct` with details for `PortentEvent.data` (no secrets/bodies/headers -
beacon does not show payload in Portent/logs/OTel). Invalid params → **error**
`Check` (scheduler skips a tick, baseline is not touched, Portent is not issued:
validation error ≠ host state change). "Inaccessible from the observer's point of view"
(refused/timeout/no init system) is a **valid state**, not an error.

## Built-in beacons

| Beacon | State | Destination |
|---|---|---|
| `core.beacon.service_down` | `up` / `down` | Service activity (poll is-active, without start/stop). Pilot - see [`service_down.go`](../../../../soul/internal/beacon/service_down.go). |
| `core.beacon.file_changed` | hash SHA-256 / `missing` | Changing file contents (editing/rotating/deleting). Pilot - see [`file_changed.go`](../../../../soul/internal/beacon/file_changed.go). |
| `core.beacon.port_closed` | `open` / `closed` | Availability of TCP port (one dial, no data sending). |
| `core.beacon.disk_full` | `ok` / `full` | Filling the file system by threshold (statfs). |
| `core.beacon.process_absent` | `present` / `absent` | Availability of a process according to the pattern (`pgrep`). |
| `core.beacon.http_unhealthy` | `healthy` / `unhealthy` | HTTP endpoint health by status code (one GET). |
| `core.beacon.inotify` | `quiet` / `events` | Kernel-level FS events via inotify (Linux-only). Pilot - see [`inotify_linux.go`](../../../../soul/internal/beacon/inotify_linux.go). |

---

## core.beacon.service_down

Observes service activity. Status poll only (`is-active`/equivalent),
without `start`/`stop` (read-only). Activity detection and backend-detection logic
reuses from [`core.service`](../service/README.md) via the common `util.Runner`
/ `util.DetectInitSystem` (systemd / OpenRC / SysV). Implementation -
[`service_down.go`](../../../../soul/internal/beacon/service_down.go).

**State:** `up` - service is active; `down` - service stopped **or** init system
cannot be determined (from the observer's point of view, the service is unavailable - this is
event of interest, not error `Check`).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `service` | string | required | Unit name (as in `core.service`). |

**data:** `{ service, active, init_system }`. `active` — bool poll result;
`init_system` - specific init system (`systemd` / `openrc` / `sysv` /
`unknown`). When the init system is undefined, `active=false`, `init_system=unknown`.

**Typed-payload:** `ServiceDownPortent { service, active, init_system }` (V5-1,
ADR-030 amendment 2026-05-26). Where-CEL is `event.service_down.service`, etc.

---

## core.beacon.file_changed

Observes a change in the contents of the file. Counts SHA-256 content streamed
(`io.Copy`, without loading the entire file into memory - the observed file may be large),
without writing (read-only). Implementation -
[`file_changed.go`](../../../../soul/internal/beacon/file_changed.go).

**State:** SHA-256 hex hash of the file contents; `missing` - no file. Change State
(edit / rotate / appear / delete) edge-triggered → `Portent`. Appearance
and file disappearance is just as edge-triggered as changing content (transition
hash↔`missing`).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | The absolute path to the observed file. |

**data:** `{ path, sha256 }` for existing file; `{ path, state: "missing" }`
for absent (the `sha256` field is then not set).

**Typed-payload:** `FileChangedPortent { path, sha256 }` (V5-1, ADR-030 amendment
2026-05-26; `path` string, `sha256` empty if there is no file). Where-CEL reads
`event.file_changed.path` / `event.file_changed.sha256`.

---

## core.beacon.port_closed

Observes the availability of a TCP port. One `dial` without sending data to the socket
(read-only). Implementation - [`port_closed.go`](../../../../soul/internal/beacon/port_closed.go).

**State:** `open` - connection established; `closed` - port not accepted
(connection refused / timeout / host unavailable). From the observer's point of view
unreachable port = `closed` (event of interest, not error `Check`).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `port` | int | required | TCP port `1..65535`. Accepted as a number or string (in case of `${…}` interpolation). |
| `host` | string | optional (default `127.0.0.1`) | Target Host/IP. Default is a local daemon on its own port. |
| `timeout` | string (duration) | optional (default `3s`) | Dial timeout (convention `duration`: `time.ParseDuration` + suffix `<N>d`). A hanging dial is an already observed "unavailable". |

**data:** `{ host, port }`.

**Typed-payload:** `PortClosedPortent { host, port }` (V5-1, ADR-030 amendment
2026-05-26). Where-CEL is `event.port_closed.host` / `event.port_closed.port`.

---

## core.beacon.disk_full

Watches the file system become full. One `statfs` call (read-only syscall),
without parsing the output of `df` - more precisely and without dependence on the locale/format of the utility.
Implementation - [`disk_full.go`](../../../../soul/internal/beacon/disk_full.go).

**State:** `full` - use of FS `≥ threshold_percent` (border
**inclusive**); otherwise `ok`.

`used_percent` is counted as `(Blocks - Bavail) / Blocks`, where `Bavail` are blocks,
available to **unprivileged** process: root-reserved (`~5%` by default
ext-family) is counted as busy, just like in a regular `df`. Calculation via
`Bfree` would overestimate used against `df` and cock `full` falsely early.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Mount point or any path within the monitored file system. |
| `threshold_percent` | int | optional (default `90`) | Threshold `full`, `1..100`. `full` when using `≥` threshold. |

**data:** `{ path, used_percent, threshold }`.

**Typed-payload:** `DiskFullPortent { path, used_percent, threshold }` (V5-1,
ADR-030 amendment 2026-05-26). Where-CEL is `event.disk_full.used_percent`, etc.

---

## core.beacon.process_absent

Observes the presence of a process. Poll via `pgrep` (no kill/signal). `pgrep` selected
instead of scanning `/proc`: OS-agnostic (Linux/BSD) and mockable in unit tests via
`util.Runner` (as `core.service` / `core.beacon.service_down`). Implementation -
[`process_absent.go`](../../../../soul/internal/beacon/process_absent.go).

**State:** `present` - `pgrep` found a match (exit 0); `absent` - matches
no (exit 1). Error `pgrep` itself (broken pattern / no binary, exit ≥2) →
error `Check`.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `pattern` | string | required | Process name/ERE pattern (matches against process name, like `pgrep <pattern>`). |

**data:** `{ pattern }`.

**Typed-payload:** `ProcessAbsentPortent { pattern }` (V5-1, ADR-030 amendment
2026-05-26). Where-CEL is `event.process_absent.pattern`.

---

## core.beacon.http_unhealthy

Monitors the health of the HTTP endpoint. One `GET`, **without body reading** (read-only).
Security is reused from [`core.http`](../http/README.md) (same pattern
opt-out security-vs-flexibility): `util.ValidateFetchURL` + `util.NewHTTPClient`
(SSRF-guard on the dial phase, downgrade-redirect protection, system TLS trust store).
Implementation - [`http_unhealthy.go`](../../../../soul/internal/beacon/http_unhealthy.go).

**State:** `healthy` - the status code is included in `status_codes`; `unhealthy` - code out
dial **or** transport error (DNS/TLS/timeout/unreachable/blocked
SSRF-guard dial, `status` = 0). Unreachable endpoint = `unhealthy` (event
of interest, not the error `Check`).

**Default is as secure as possible** (https + SSRF-guard + TLS verification). For
internal health-check (`https://127.0.0.1:8443/health`, RFC1918) - where
secure-default gives false `unhealthy` (dial blocked by netguard) - operator
explicitly raises the opt-out flags in `VigilDef.params`. warn when removing guard here
**not** emitted (unlike apply modules): beacon - read-probe on schedule
without an output-warnings channel, an explicit flag in `Vigil.params` is the operator's consent.

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `url` | string | required | Target endpoint. **`https://`** default; `http://` - only with `allow_http`; other schemes (`file://` ...) are always rejected on `Check`. |
| `status_codes` | list of ints | optional (default `[200]`) | "Healthy" status codes. Code out of set → `unhealthy`. |
| `timeout` | string (duration) | optional (default `30s`) | Request timeout (convention `duration`). Must be positive. |
| `allow_http` | bool | optional (default `false`) | Accept `http://` (removes https-only and downgrade protection for redirects). **Not** opens SSRF - dial-guard lives separately. |
| `insecure_skip_verify` | bool | optional (default `false`) | Do not verify the TLS certificate (self-signed / internal CA). MITM risk. |
| `allow_private` | bool | optional (default `false`) | Remove SSRF dial-guard - allow dial in loopback / RFC1918 (internal endpoint). |

The three opt-out loops are orthogonal (`allow_http` does not open SSRF, etc.); everyone
is cleared only by an explicit flag.

**data:** `{ url, status }` - **only** URL and status code. Response body and headers
**doesn't** go here: sensitive-by-construction
([ADR-010 §7.4](../../../templating.md)) - beacon does not show payload in Portent.
`status` = 0 means transport error (endpoint unavailable or dial
blocked by SSRF guard at `allow_private:false`).

**Typed-payload:** `HttpUnhealthyPortent { url, status }` (V5-1, ADR-030
amendment 2026-05-26). Where-CEL is `event.http_unhealthy.url` /
`event.http_unhealthy.status`.

---

## core.beacon.inotify

Observes FS events via **kernel inotify** syscall - without polling and hash,
beacon wakes up the scheduler only when FS is actually active. **Linux-only**: on
non-Linux platforms the beacon itself gives the error `platform not supported`, registry
this does not crash (the constant address is available everywhere for the sake of the common keeper-enum /
soul-registry). Implementation - [`inotify_linux.go`](../../../../soul/internal/beacon/inotify_linux.go);
stub — [`inotify_other.go`](../../../../soul/internal/beacon/inotify_other.go).

**Fold-adapter** (V5-3, ADR-030 amendment 2026-05-26): background-goroutine
reads inotify-fd between scheduler ticks and accumulates events into a buffer;
Check on each tick returns a "window" of events for the interval. State `events`
is armed if there are ≥ 1 events in the window, otherwise `quiet`. Comparison of state edge-triggered
(quiet → events / events → quiet) - one Portent for each "activity occurrence"
and one for "fading", and not an avalanche of Portents, one per event.

Unlike `core.beacon.file_changed` (polling + SHA-256 content) -
`inotify` does not calculate the hash and does not open files: the kernel sends an event to any
metadata/content change, beacon only projects it to Portent. This
cheaper in terms of CPU/IO on large directories, but does not catch "phantom" edits
without kernel event (NFS / snapshots without inotify forward).

**State:** `events` - there are ≥ 1 events in the window (`InotifyPortent.count > 0`);
`quiet` - the window is empty (there was a scheduler tick, but the kernel did not send anything).

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | The absolute path to the file or directory. Watch on the directory catches events inside (one level - without recursion); watch on a file - events of the file itself. |
| `events` | list of strings | optional (default - all 5) | Event type filter: `created` / `modified` / `deleted` / `moved` / `attrib`. Converted to a kernel mask (IN_CREATE / IN_MODIFY / IN_DELETE / IN_MOVED_* / IN_ATTRIB); The kernel itself filters - the filter does not reach the beacon. The unknown element is ignored (forward-compat). |
| `recursive` | bool | optional (default `false`) | **MVP only accepts `false`.** `true` → beacon rejects Vigil with an error (potential source of bugs with walk-mount-point / symlink - deferred until explicitly requested). |
| `throttle` | string (duration) | optional | Accepted by the forward-compat grammar, **ignored** in MVP; all events are issued as is. Throttle is planned as a separate slice. |

**data:** `{ path, count, events: [{type, file, at}, …] }` for `events`-state;
`{ path, count: 0 }` for `quiet`. `file` - name in the directory (for directory-watch);
is empty if the watch is on a separate file (the kernel does not set the name). `at` —
Soul-side unix-seconds registration (NOT kernel-time - inotify does not give time
events).

**Typed-payload:** `InotifyPortent { path, events: [{type, file, at}], count }`
(V5-3, ADR-030 amendment 2026-05-26). Where-CEL -
`event.inotify.path == "/etc/audit"` or
`event.inotify.events.exists(e, e.type == "created")` (CEL `exists` by
repeated-field is designed as list-of-maps on activation).

### Edge cases

- **`max_user_watches` exhausted** (`fs.inotify.max_user_watches` sysctl). Kernel
returns `ENOSPC` to `inotify_add_watch`. beacon converts to understandable
Vigil error to operator; scheduler logs and skips the tick (baseline does not
will be installed, Portent will not be issued). The solution is to raise sysctl
  `fs.inotify.max_user_watches`.
- **Missing path.** `inotify_add_watch` will return `ENOENT`; beacon gives away
error (scheduler skips tick). After creating the path at the next tick
watch will be installed.
- **Permission denied.** If the Soul agent does not have read access to watch-target -
`EACCES`; same semantics (error → missed tick, scheduler logs).
- **Watch for a separate file vs directory.** In the first case, `events[].file` is empty
(kernel does not send name), in the second it contains a relative name.
- **Deleting an observed target.** Kernel sends `IN_DELETE_SELF` → beacon
projects to `type=deleted` and **watch automatically stops** (kernel
removes wd). Re-add behavior after re-create is deferred until explicitly requested
operator.

### Example Vigil + Decree

```yaml
# vigils-registry (managed OpenAPI/MCP, ADR-030):
- name: audit-log-tamper
  check: core.beacon.inotify
  interval: "5s"
  params:
    path: /var/log/audit
    events: [modified, deleted, moved]
  coven: [prod]

# decree-registry:
- name: alert-on-audit-tamper
  on_vigil: audit-log-tamper
  where_cel: "event.inotify.count > 0"
  action:
    scenario: notify-soc
    args:
      reason: "audit log tamper"
```

### Lifecycle and known trade-offs MVP

- **Singleton semantics.** One instance of `InotifyBeacon` serves everything
Process Vigils; per-path watches are stored in a map inside a beacon. Several
Vigils with different `path` - independent kernel-fd and independent buffers.
- **Fd-leak when deleting Vigil.** Scheduler does not signal the beacon about
removing Vigil (ReplaceAll), so kernel-fd for the disappeared path
remain open until the process is completed (the kernel itself will release).
Limited leak (many unique paths of course); explicit lifecycle
hook on interface `Beacon` - deferred.

## Custom `soul_beacon` plugins

In addition to the built-in `core.beacon.*`, the operator can add his own beacon plugins
([ADR-030 V5-2](../../../adr/0030-vigil-oracle.md)).
4th kind in plugin-infra (parity with `soul_module` / `cloud_driver` / `ssh_provider`):

- binary `soul-beacon-<name>`, manifest [`kind: soul_beacon`](../../../keeper/plugins.md);
- SDK - [`sdk/beacon`](../../../../sdk/beacon/beacon.go), `Beacon` interface with two RPCs:
  - `Validate(params) → ok+errors[]` — runtime checks `params` Vigil (what is not expressed in the JSON Schema manifest);
  - `Check(params, state_cookie) → state + payload + state_cookie + error` - one poll tick.
- Vigil addressing - `<namespace>.<name>` (for example `community.zfs-degraded`); The Soul-side dispatcher distinguishes built-in `core.beacon.*` from plugin-beacon by namespace.
- lifecycle — **one-shot per Spawn** ([ADR-020(d)](../../../adr/0020-plugin-infrastructure.md)): scheduler makes Spawn → Check → Close on every tick; for frequent ticks, the plugin can save in-memory state via `state_cookie` (passback).
- security - fail-closed Sigil-verify before Spawn ([ADR-026](../../../adr/0026-sigil.md)): without active permission (`keeper.plugin.allow ns=<ns> name=<name> ref=<ref>`) the plugin will NOT run.

Payload plugin-beacon - `PortentEvent.payload.custom` (Struct, oneof-branch). Where-CEL Decree reads `event.custom.<field>`. The exact shape of `payload` is at the discretion of the author (plugin-specific).

For an example of a minimal plugin - see [`docs/keeper/plugins.md#kind-soul_beacon-zfs-pool-health-adr-030-v5-2`](../../../keeper/plugins.md#kind-soul_beacon-zfs-pool-health-adr-030-v5-2).

## See also

- [ADR-030](../../../adr/0030-vigil-oracle.md) - Vigil / Oracle / event-driven monitoring (beacons + reactor).
- [naming-rules.md → Domain Entities](../../../naming-rules.md) - Vigil / Portent / Oracle / Decree.
- [core/http/README.md](../http/README.md) - read-probe HTTP (from where https-only + SSRF-guard is reused).
- [core/service/README.md](../service/README.md) - service management (from where `core.beacon.service_down` gets activity detection).
- [keeper/plugins.md → `kind: soul_beacon`](../../../keeper/plugins.md) — plugin-beacon manifest schema.
