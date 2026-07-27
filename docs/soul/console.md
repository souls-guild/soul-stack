# Soul — interactive console (PTY) sessions

The Soul-side half of Multi-console: a living pty on the managed host, driven by
Keeper over the existing EventStream. Implementation —
`soul/internal/runtime/consolerunner/`.

This document covers the **Soul side and the wire contract**. Keeper's session
manager and the browser transport (WebSocket `/v1/console`) are
[keeper/console.md](../keeper/console.md); the `soul.console` permission and
session recording are separate slices.

## 1. Why it is not an Errand

An [Errand](../../docs/naming-rules.md) ([ADR-033](../adr/0033-errand.md)) is one
module call answered by a single final blob capped at 64 KiB. A console is the
opposite shape: a bidirectional byte stream with no known end, where the operator
types into a real terminal. Only a pty makes `top`, `vim` and `cd` behave —
programs detect a tty and switch to full-screen, line editing and job control.
So the console gets its own runner and never reuses `errandrunner`.

## 2. Transport: only-add on the existing stream

There is **no new RPC**. The messages ride the bidi
`EventStream(stream FromSoul) returns (stream FromKeeper)`
([ADR-012](../adr/0012-keeper-soul-grpc.md)) as only-add members of the existing
`oneof payload`, defined in `proto/keeper/v1/console.proto`.

| Direction | Message | Field | Role |
|---|---|---|---|
| Keeper → Soul | `ConsoleOpen` | 13 | Start a pty session: `session_id`, `cols`, `rows`, optional `shell`. |
| Keeper → Soul | `ConsoleStdin` | 14 | Raw keystrokes written to the pty master (including `^C`/`^D`). |
| Keeper → Soul | `ConsoleResize` | 15 | New geometry → `TIOCSWINSZ` → `SIGWINCH`. |
| Keeper → Soul | `ConsoleClose` | 16 | End the session. |
| Soul → Keeper | `ConsoleOpened` | 11 | The pty is live; carries the shell `pid`. |
| Soul → Keeper | `ConsoleChunk` | 12 | Terminal output, with `seq` and `dropped_bytes`. |
| Soul → Keeper | `ConsoleExit` | 13 | The single terminal event of a session. |

Field numbers are frozen (ADR-012 forward-compat only-add) and guarded by a test
in `consolerunner/contract_test.go`. A new console message takes the next free
number; none of these is ever renumbered or reused.

Message order per `session_id` is strict:
`ConsoleOpened → ConsoleChunk* → ConsoleExit`. A session that is refused
(resource limit, bad shell, stream closing) sends **only** `ConsoleExit` — so
Keeper always gets exactly one terminal for every `session_id` it minted, and can
free its own state unconditionally.

`session_id` is a ULID minted by Keeper and is unique **per stream**: it does not
survive a reconnect (see §4). `target_sid` in `ConsoleOpen` is an echo for logs —
identity is the mTLS peer cert ([ADR-012(i)](../adr/0012-keeper-soul-grpc.md)).

### Capability

Soul announces `console` in `Hello.capabilities`
(`shared/config/soul_capability.go`, same mechanism as `passage` in
[ADR-056](../adr/0056-staged-render-passage.md)). Keeper must check it **before**
minting a session: an older binary drops `ConsoleOpen` into the default branch of
its recv-loop and never answers, which would leave the operator watching a dead
terminal until a timeout. Fail-closed, like every other capability gate.

### stdout and stderr

A pty merges the child's stdout and stderr onto one file descriptor — that is
what a terminal is. `ConsoleChunk.stream` is therefore always `STDOUT` in
practice. The enum exists so a future non-pty exec mode can split the channels
without a wire break; consumers must not wait for `STDERR`.

## 3. Flow control

The console and the apply cycle share **one** write mutex on the EventStream
(`soul/internal/grpc/client.go`). If a flooding console could block its sender, it
would hold that mutex and stall apply's `TaskEvent`/`RunResult` on the same
stream. So on the Soul side the pty reader and the stream sender are separate
goroutines joined by a bounded queue:

- the reader never blocks on the stream — when the queue is full the output is
  **dropped**, not buffered;
- the sender paces output through a token bucket (1 MiB/s sustained, 256 KiB
  burst by default);
- dropped bytes are reported in `ConsoleChunk.dropped_bytes` of the next chunk,
  and `seq` is a gap-free per-session counter from 1.

That pairing is deliberate: loss is possible under a flood, but it is always
**visible**. Keeper and the web terminal render an explicit gap marker instead of
silently splicing a corrupted ANSI stream. `soul_console_dropped_bytes_total`
exposes the same signal to monitoring.

## 4. Lifecycle and the kill-on-disconnect invariant

**A console never outlives the EventStream session that authorized it.** The
runner is bound to one stream, exactly like the Augur client; on teardown —
Keeper gone, failback swap, daemon shutdown — every pty is killed and reaped
before `handleSession` returns. A stream break that left a live root shell on the
host would be the worst failure this component could have.

Teardown escalates, because killing the shell is not enough:

1. **SIGHUP to the shell's process group.** The shell runs its `EXIT` trap and
   closes down, so the last output still reaches the operator.
2. **Hang up the pty** (close the master) **and SIGKILL the group.** Closing the
   master makes the kernel raise `SIGHUP` on the terminal's foreground process
   group and its session leader. It also unblocks the reader, which a child
   holding the slave open would otherwise keep parked.
3. **Sweep the terminal session.** `SIGKILL` to every process whose session id
   equals the shell's pid (resolved from `/proc/<pid>/stat`).

Step 3 is not belt-and-braces. A shell attached to a pty is **interactive**, so it
turns job control on and puts every job in its **own** process group — the `top`
an operator left running is not in the shell's group, and a group-only kill would
orphan it. The session is the only thing they all share, and the kernel has no
"signal a session" call. Processes that deliberately left the session (`setsid`,
`nohup`) stay out of reach — the same contract SSH gives.

`cmd.Wait` is called exactly once per session, so a console can never leave a
zombie. Guard tests cover the job-control child, fd leaks across repeated
sessions, and teardown against an already-broken stream.

## 5. Host policy and resource limits

The host has the last word on whether an interactive shell may run on it —
whatever Keeper-side RBAC permits. That policy lives in the `console:` block of
`soul.yml` ([config.md → `console:`](config.md#console)):

| Key | Default | Why |
|---|---|---|
| `enabled` | `true` | `false` forbids consoles on this host outright. Refusal is a terminal `ConsoleExit`, so Keeper drops the session at once instead of holding an operator's terminal open until a timeout. |
| `max_sessions` | 8 | Concurrent consoles **on this host** — not per operator: a wall over 10 hosts is one session on each. `1` means one console at a time. More than a handful of shells on ONE machine is a runaway UI or an attack, so exceeding it gives `ConsoleExit{LIMIT_EXCEEDED}`. |
| `rate_limit_kbps` | 1024 | Output ceiling per session (see §3). |
| `kill_grace` | 2s | Time allowed at each teardown step before escalating. |
| `shell` | `$SHELL` → `/bin/bash` → `/bin/sh` | The console program; absolute path only. |

Read at the start of each EventStream session, so a hot-reload (ADR-021) applies
on the next reconnect; a live console keeps the envelope it started with.

Chunk size (32 KiB) and queue depth (32 chunks ≈ 1 MiB in flight) stay code
constants in `consolerunner/limits.go`: they are internal flow-control tuning
with no operator-visible policy meaning. Operator-facing session policy that is
*not* host-local — idle timeout, per-operator budget, RBAC — belongs Keeper-side.

The shell is the first of `ConsoleOpen.shell` → `console.shell` → `$SHELL` →
`/bin/bash` → `/bin/sh` that exists. An explicit `shell` **must be an absolute path**: resolving
a bare name through `PATH` would let whoever can open a console pick up a shadowed
binary from the daemon's environment. `TERM=xterm-256color` is injected when
absent, since a service manager starts Soul without `TERM` and curses programs
refuse to draw without it.

The pty inherits the Soul daemon's user — typically root. That is the point of
the feature and the reason it is gated by a dedicated permission rather than
`errand.run`: an interactive shell cannot be checked against a module allow-list,
because the commands are not known in advance.

## 6. Metrics

| Metric | Meaning |
|---|---|
| `soul_console_sessions_active` | Live pty sessions. The leak signal — it must return to zero after an operator disconnects. |
| `soul_console_sessions_total{reason}` | Terminal sessions by reason (`process_exited` / `closed_by_keeper` / `open_failed` / `soul_shutdown` / `limit_exceeded`). |
| `soul_console_output_bytes_total` | Output forwarded to Keeper. |
| `soul_console_dropped_bytes_total` | Output discarded by flow control. |
