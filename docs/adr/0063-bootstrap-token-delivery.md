# ADR-063. core.bootstrap.delivered — keeper-side bootstrap-token delivery over SSH

> **Status: active.** architect's design (A1 "thin delivery"), name `core.bootstrap.delivered` via propose-and-wait (confirmed by the user). The canon is fixed docs-first BEFORE code; this ADR **amends [ADR-017](0017-keeper-side-core.md), [ADR-061](0061-onboarding-await-and-midrun-reresolve.md), [ADR-015](0015-core-modules-mvp.md)**.
>
> **Implementation progress.** Pilot slice implemented: module + conditional registration + Deps + scenario-swap (`keeper.push.applied` stub → `core.bootstrap.delivered`) + unit tests. **C1 (cloud-init CA-signed host-key) and live-e2e — the next slice, NOT this one** (see §MVP Boundaries). Before C1 a live run of direct mode will break: `push.Dial` rejects the host-cert of a fresh VM whose cloud-init installed a bare (not CA-signed) host-key.
>
> **Amendment (Teleport by-name transport) — implemented (pilot).** A second transport mode `transport: teleport` (by-name via Teleport Proxy, host-verify via Teleport identity-file, C1 not applicable) + keeper-side Teleport-Dialer + retry-until-join + wire-up daemon (teleport mode) + guard tests. See §Amendment below. The direct mode of the bootstrap module is not yet wired into the daemon (BootstrapDial=nil → not registered) — that is a generic-live slice.
>
> **Amendment (full-install mode for platforms without cloud-init userdata) — Slice 1/3 implemented.** `core.bootstrap.delivered` gains a second operating mode — **full-install** over Teleport SSH (installs the ENTIRE setup, not just the token) for platforms without cloud-init userdata (e.g. WB namespace without `ci_user_data`). The install-blueprint is extracted into the shared package [keeper/internal/soulinstall](../../keeper/internal/soulinstall) — the single source of truth (canonical `Blueprint`), reused by both onboarding paths. Slice 1 (blueprint extraction: `Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep` + switching the `cloudinit` package to shared + tests) is **done**; Slice 2 (install mode in the delivered module itself) and Slice 3 (scenario `generate_userdata:false`+`install:true`+live) are next. See §Amendment (full-install mode) below.
>
> **Amendment (init phase + unit activation + `event_stream_port`) — implemented, proven by live workarounds.** A live run of the push-install-flow hit two walls: (5) the token was delivered but nobody redeemed it — no soul-side "pickup" of the token file exists, the seed is created ONLY by `soul init`, and soul run kept crashing in a restart loop "SoulSeed not found"; (6) the blueprint derived BOTH soul.yml ports from a single `bootstrap_endpoint` — soul dialed EventStream on the Bootstrap port ("Unimplemented: method EventStream"). Plus a hole: push-install did only `systemctl start` without `daemon-reload`/`enable` — after a VM reboot the unit did not come up. See §Amendment (init phase) below.

**Context.** [ADR-061](0061-onboarding-await-and-midrun-reresolve.md) introduced a unified create run provision→onboarding→role: `core.cloud.created` creates N VMs, the register-output carries their `sid` + plain bootstrap tokens, then `core.soul.registered` with `await_online` blockingly waits for onboarding. Between "VM created" and "`soul` agent online" the VM must receive its bootstrap token — without it CSR onboarding ([docs/soul/onboarding.md](../soul/onboarding.md)) does not start.

cloud-init (B-flat, [ADR-017(h)](0017-keeper-side-core.md)) installs the soul binary + CA + systemd-unit on the VM, but **deliberately does NOT carry the token** (userdata is logged by the cloud provider — a secret must not be put there). The token is issued AFTER Create and must be delivered by a separate channel. Before this ADR the scenario carried the stub address **`keeper.push.applied`**, which keeper-dispatch rejected as an unknown module (there is no such keeper-side core) — the created VM never received the token, the `await_online` barrier never gathered presence, and the run went to `error_locked`. This is **BUG#2 cloud-provision**.

**Solution.** A new keeper-side core module **`core.bootstrap.delivered`** (dispatcher `on: keeper`) — thin delivery of the per-VM bootstrap token over SSH. Replaces the `keeper.push.applied` stub.

## Design A1 — "thin delivery"

The module places on the VM **ONLY the token** (everything else — the soul binary, CA, unit — was already installed by cloud-init) and optionally starts the soul agent. This is not a Destiny push run (that one carries `ApplyRequest`), but a single operation "deliver the secret + trigger start". It reuses the existing SSH push infrastructure ([keeper/internal/push](../../keeper/internal/push)), the same path as `SshDispatcher.SendApply`.

**Per-host flow (sequential):**

1. `SshProvider.Authorize(host, user)` — a deny aborts delivery before the connect (**fail-closed**).
2. ephemeral ed25519 keypair + `SshProvider.Sign(pubkey)` → `ssh.AuthMethod`s (reuses `push.NewEphemeralEd25519` + `push.AuthMethodsFromSign`). The private key **NEVER** leaves the Keeper.
3. `push.Dial(DialConfig{Host: primary_ip, HostAuthorities: <host-CA from Vault>, …})` → `Session` (CA-signed host-cert verify — the same as push).
4. `session.Run("install -d -m 0700 /etc/soul && umask 077 && cat > <token_path> && chmod 0400 <token_path>", tokenBytes)` — **★ token in STDIN, NOT in argv** (otherwise it leaks into `ps`/audit.log/journald on the VM itself).
5. `session.Run("test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN=\"$(cat <token_path>)\" /usr/local/bin/soul init --config /etc/soul/soul.yml", nil)` — **token redeem** (CSR→Bootstrap-RPC→SoulSeed; §Amendment init phase). The guard on the seed-cert = idempotency (the token is single-use); the literal `$(cat …)` is expanded by the subshell on the VM — the token is not in the keeper's argv.
6. if `start_soul` — `session.Run("systemctl daemon-reload && systemctl enable soul && systemctl start soul", nil)` (parity with cloud-init runcmd; enable survives a VM reboot).

**B1-strict.** A failure of any host (Authorize-deny / connect-fail / write-fail / init-fail / start-fail) → step `failed` → state is not committed → `error_locked`. There is no partial delivery.

## Addressing and side

- Namespace `core`, module `bootstrap`, state `delivered`. The registry key is the base `core.bootstrap`; the state comes from the address suffix via `config.SplitModuleAddr` (like all keeper-side cores).
- Full task name: `module: core.bootstrap.delivered`. Side **Keeper-side**, the step **must** carry `on: keeper`.
- Implementation — [`keeper/internal/coremod/bootstrap/delivered.go`](../../keeper/internal/coremod/bootstrap/delivered.go).

## Parameters (`params:`)

| Parameter | Type | Req. | Default | Semantics |
|---|---|---|---|---|
| `hosts` | array of object `{sid, primary_ip, bootstrap_token}` | required | — | List of VMs. In practice comes as the CEL expression `${ register.<provision>.hosts }` (output of `core.cloud.created`). An empty list → `failed`. |
| `ssh_provider` | string | required | — | Name of the SshProvider plugin (`keeper.yml::plugins.ssh_providers[].name`) for SSH authentication. **★ In `transport: teleport` it does NOT determine the transport** (Authorize/Sign are not called) — the operator passes the name, but it goes ONLY into the audit payload. Dropping the required status per transport is post-MVP optional. |
| `token_path` | string | — | `/etc/soul/token` | Path of the token file on the VM. |
| `ssh_user` | string | — | `root` | SSH user. |
| `ssh_port` | int (1..65535) | — | `22` | TCP port of sshd. |
| `start_soul` | bool | — | `true` | Unit activation after init: `systemctl daemon-reload && systemctl enable soul && systemctl start soul`. `soul init` (step 5) runs independently of this flag. |

## Output contract (module `output:`)

`register.<name>.*`: `hosts[] = {sid, delivered, started}` + `count` (number of processed hosts). Plus the standard `.changed` (always `true` on success) / `.failed` of the DSL core.

**★ NO token in output.** The plain token itself is visible only in the register of the previous step (`core.cloud.created`, key `bootstrap_token`, masked by `audit.MaskSecrets` on all outputs) — in the `core.bootstrap.delivered` output it is absent entirely.

## Security

- **Token in STDIN, not in argv** (§A1 step 4): the process argv is visible in `ps` and ends up in audit.log/journald on the VM itself.
- **Audit payload without tokens** (event `bootstrap.delivered`, `source: keeper_internal`): `{action, ssh_provider, count, sids}` — a parallel to the cloud.provisioned masking.
- **The error text is masked** before being emitted into the `failed` event (`maskErr` → `audit.MaskSecrets`): the vault-ref / token do not leak into `status_details`.
- **CA-signed host-cert verify is mandatory** (fail-closed): an empty host-CA set → Apply returns an explicit error, does not connect "blindly" (like `push.Dial`, `InsecureIgnoreHostKey` is forbidden).
- **fail-closed Authorize**: a deny aborts delivery before the SSH session is opened.

## Dependencies and registration

`coremod.Deps` is extended with three fields (assembled by the wire-up from the same push infrastructure as `SshDispatcher`):

- `BootstrapProviders map[string]bootstrap.SshProviderHost` — discovered SshProvider plugins keyed by `manifest.Name` (type `SshProviderHost` = `push.SshProvider`, the same as the dispatcher's; the pluginhost wrapper for Sign/Authorize is `*pluginhost.SshProviderPlugin`).
- `BootstrapHostCAs []push.NamedHostKeyAuthority` — host-CA from Vault (`push.LoadHostCAs`).
- `BootstrapDial push.Dialer` — `push.Dial` (mocked in tests).

Registration in `coremod.Default` is **conditional** (like `core.choir` with `ChoirStore`): the module is wired only when `BootstrapProviders` is non-empty AND `BootstrapHostCAs` is non-empty AND `BootstrapDial` is set. Any gap — a build without push access (pull-only / no host-CA): a step with this address will fail with "unknown keeper-side module" (a clear "not configured" refusal).

## MVP Boundaries

- **One key-based SshProvider mode.** The SignReply contract covers ephemeral-cert (Vault SSH CA) and static-key; multi-provider routing within one step is not introduced (`ssh_provider` — one name).
- **Token only.** The module does not deliver the binary/modules/config (that is cloud-init B-flat). Not to be confused with `SshDispatcher`/a Destiny push run.
- **Hosts sequentially.** Parallel delivery across N VMs is a possible extension without a breaking change (per-host operations are independent).
- **★ C1 — cloud-init CA-signed host-key (required-for-live, NEXT slice).** `push.Dial` trusts only a host-cert signed by the host-CA (`HostAuthorities`), not a bare host-key (rejection of TOFU). A fresh VM after cloud-init has its own host-key — it **must** be CA-signed by the same host-CA, otherwise the handshake is rejected and delivery fails at the connect. cloud-init (B-flat userdata) must generate the host-key and sign it with the host-CA from `keeper.yml::cloud_init` (or place a pre-signed host-cert). Without C1 the module is valid on render (L0 Trial) and passes unit tests, but live-e2e will not pass. C1 + live validation on WB cloud is a separate slice.

## Amendment (Teleport by-name transport)

The module gains a second transport mode `transport: teleport` (vs the default `direct`=generic push.Dial). In teleport mode delivery goes through the Teleport proxy by-name (target=SID/FQDN, NOT primary_ip): the keeper-side Teleport-Dialer ([keeper/internal/push/dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) does transport+user-auth+host-verify entirely through the Teleport identity-file (`creds.SSHClientConfig()`). Deviations from A1: (1) Authorize/Sign/ephemeral-keypair are NOT called; (2) a Vault host-CA is NOT required for teleport — host-verify goes through the Teleport CA (C1 is not applicable to teleport mode); (3) a retry-with-backoff until Teleport-join (~3-5 min) is added. The direct mode (Vault/static, CA-signed host-cert, C1) is unchanged. Teleport creds — the keeper.yml push block (`push.transport` + `push.teleport.{proxy_addr,identity_file,cluster}`), the soul-ssh-teleport plugin does not participate in this flow.

A new scenario parameter `join_wait_timeout` (int, seconds; default 360) — the ceiling for waiting for Teleport-join, relevant only in teleport mode; on expiry the step is `failed` (B1-strict, `error_locked`). Registration of the module in teleport mode requires only the dialer (`BootstrapDial`), providers/host-CA are not needed (see the gate in `coremod.Default`).

### Amendment 2026-06-30 — Teleport proxy behind an L7-TLS load balancer (`use_system_trust` + `alpn_upgrade`)

**Problem.** The Teleport-Dialer ([dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) verifies the proxy server cert through the identity-CA-pool (`creds.TLSConfig()`) + a forced sentinel ServerName `teleport.cluster.local` (Teleport API client). This is valid ONLY when the proxy presents a Teleport-issued cert. If the Teleport proxy sits BEHIND a public L7-TLS load balancer (WB: wildcard `*.tp.rwb.ru`, SAN `*.tp.rwb.ru, tp.rwb.ru, www.tp.rwb.ru` — **no** `teleport.cluster.local`), the gRPC handshake (the `DialHost` path via `credentials.NewTLS`) fails on an x509 DNSName mismatch: `certificate is valid for *.tp.rwb.ru, not teleport.cluster.local`. `proxy.ClientConfig.InsecureSkipVerify` does not help — it affects only the ALPN-conn-upgrade wrapper, whereas the gRPC handshake goes through our `TLSConfigFunc` with a forced ServerName.

**Solution — the optional field `push.teleport.use_system_trust` (bool, default false).** When `true`, `TLSConfigFunc`, after `creds.TLSConfig()`, adjusts the returned `*tls.Config`: `RootCAs = nil` (Go takes the system trust store → verifies the public balancer cert) + `ServerName = host(proxy_addr)` (removes the sentinel `teleport.cluster.local`). `Certificates`/`GetClientCertificate` (the mTLS client cert for auth on the proxy) are preserved. When `false` (the default) — behavior is bit-for-bit as before (identity-CA-pool + sentinel ServerName); existing installations with a Teleport-issued proxy cert are not affected.

**Security rationale.** This is **not** `InsecureSkipVerify` (trusting any cert — that would open MITM): `RootCAs=nil` gives the same unblock but PRESERVES verification of the public cert against the system trust. The proxy server cert is **not a Soul Stack trust boundary**: authentication of target nodes goes through the client-mTLS-cert (auth on the proxy) + the SSH host-CA from the identity-file (host-verify through the Teleport CA). The system trust verifies only the public balancer cert; trust in the nodes is not weakened. The host from `proxy_addr` is split off by `net.SplitHostPort` at dialer startup (a malformed `proxy_addr` without `:port` → a constructor error, fail-closed, not a late Dial).

**The second half of the same case — `push.teleport.alpn_upgrade` (bool, default false).** `use_system_trust` fixes the INTERNAL gRPC-mTLS handshake (cert mismatch), but behind the L7-TLS load balancer a second barrier remains: the LB terminates TLS and **does not proxy the raw gRPC/SSH stream** (`DialHost` fails already after TLS — on `403 Forbidden; transport: received unexpected content-type "text/plain"`; the 403 is returned by the LB web layer, this is **not** Teleport-RBAC). When `alpn_upgrade: true`, `ALPNConnUpgradeRequired: true` is set in `proxy.ClientConfig` — Teleport wraps the stream in an ALPN-conn-upgrade (a WebSocket tunnel on `/webapi/connectionupgrade`), which the L7-LB passes through as ordinary HTTP. Teleport enables `WithALPNConnUpgradePing` itself inside `newDialerForGRPCClient`. We do not touch other fields: `TLSRoutingEnabled` affects only the path to Auth (not `DialHost`), `InsecureSkipVerify` would propagate into the OUTER TLS to the LB and disable verification of the public cert (a MITM hole) — categorically no.

**Both flags — a pair for proxy-behind-L7-LB.** `use_system_trust` fixes the internal gRPC-TLS over the tunnel, `alpn_upgrade` breaks through the tunnel itself via the L7-LB; the layers are orthogonal, but for this topology they are enabled together. Behind the transport, authentication of target nodes on the identity does not change: the role's access to nodes (`ssh` logins) remains Teleport-RBAC on the identity-file — this is configured on the Teleport side (the bot's `tctl` role), not by our code.

## Amendment 2026-06-30 — full-install mode (platforms without cloud-init userdata)

**Problem.** Design A1 assumes that cloud-init (B-flat, [ADR-017(h)](0017-keeper-side-core.md)) has already installed the soul binary + CA + systemd-unit on the VM, while `core.bootstrap.delivered` places ONLY the per-VM token. This requires the provider to accept userdata at Create (`generate_userdata: true`). Some platforms **do not accept** userdata — for example a WB namespace without `ci_user_data`: the VM comes up "bare", cloud-init does not run on it, and delivering a single token is meaningless (there is neither a binary, nor a config, nor a unit that the token is meant to complement). For such platforms `generate_userdata` is **not the only** onboarding path ([ADR-017(h) amendment](0017-keeper-side-core.md)): the entire setup must be installed by another channel.

**Solution — two modes of `core.bootstrap.delivered`:**

- **token-only** (current behavior, A1): cloud-init installed the setup, the module delivers only the token. Unchanged.
- **full-install**: the module installs the **ENTIRE** setup over Teleport SSH — the same files at the same paths with the same permissions that cloud-init would have placed (keeper-ca.pem → soul.yml → soul.service → curl the soul binary), then the token and optionally `systemctl start soul`. For platforms without userdata.

**Single source of the install-blueprint (DRY).** So that both onboarding paths (cloud-init userdata and full-install over SSH) install an **identical** result — the same paths, permissions, soul.yml and systemd-unit — the install-blueprint is extracted into the shared package [`keeper/internal/soulinstall`](../../keeper/internal/soulinstall):

- `Blueprint` — the canonical resolved parameters of the install result (paths/permissions — package constants: `KeeperCAPath`/`SoulConfigPath`/`SoulServicePath`/`SoulBinaryPath` + modes).
- `RenderCloudInitYAML(Blueprint) (string, error)` — the cloud-config YAML for the userdata path (now called by `cloudinit.GenerateUserdata` as a thin wrapper).
- `RenderInstallScript(Blueprint) ([]InstallStep, error)` — the sequence of SSH steps for the full-install path (`InstallStep{Cmd, Stdin}`). FOR NOW a foundation: it will be called by install mode in Slice 2.

Drift between the two renderers is constructively impossible: the body of soul.yml and the systemd-unit are defined by the functions `SoulConfigYAML`/`SystemdUnit`, and the cloud-init template renders them via `{{ .SoulConfigYAMLIndented }}`/`{{ .SystemdUnitIndented }}` (with YAML indent under `content: |`) — there is no textual copy of this material in the template, both paths physically take a single source.

**The only intentional permission divergence between the paths** — `keeper-ca.pem`: `0600` for full-install over SSH (a stricter floor) vs `0644` in cloud-init userdata (the CA is public). The rest of the setup is identical — the same paths, soul.yml and systemd-unit from a single source.

**Blueprint source = `keeper.yml::cloud_init` (config-reuse).** Full-install reads the same config block `keeper.yml::cloud_init` as the userdata path (`bootstrap_endpoint`/`tls_ca_ref`/`soul_binary_url`/`soul_binary_ca`). The block name remains `cloud_init` despite the non-cloud-init mode: it is the single source of install parameters for both paths, and duplicating it under a second name would be drift. A clarification in the bin-doc, not a new ADR.

**The security invariant is preserved in both modes.** Secret-write goes through SSH **stdin, not argv** (§A1 step 4 for the token; in full-install the CA-PEM and soul.yml are written the same way — `cat > path` with stdin, not `echo` in argv). `RenderInstallScript` guarantees this constructively (the PEM body in `InstallStep.Stdin`, `.Cmd` carries only the write path); covered by the ARGV-LEAK-GUARD test. The per-VM token still ends up in neither the userdata nor the blueprint — a separate step (the token-only part).

**Implementation slices:**

1. **blueprint extraction (done)** — `soulinstall.Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep`, switching `keeper/internal/cloudinit` to the shared renderer (the external contract `Config`/`Resolver`/`GenerateUserdata` preserved), tests of both renderers + anti-drift + ARGV-LEAK-GUARD.
2. install mode in `core.bootstrap.delivered` — executing the `RenderInstallScript` steps over Teleport SSH before token-write, under a scenario flag.
3. scenario integration — `core.cloud.created` with `generate_userdata: false` + `core.bootstrap.delivered` with `install: true`; live validation on a platform without userdata.

**Cross-ref:** [ADR-017(h)](0017-keeper-side-core.md) — `generate_userdata` is NOT the only onboarding path (full-install over SSH is an alternative for platforms without userdata).

## Amendment 2026-07-02 — init phase in the A1 flow, unit activation, `event_stream_port` in cloud_init

Three defects of the push-install-flow, each proven by a live workaround (after a manual workaround the soul reached CONNECTED):

**(5th wall, blocker) No init phase — the delivered token was redeemed by nobody.** A1 ended at "token in `token_path` + `systemctl start soul`", silently assuming that the soul agent would pick up the token file itself. No such mechanism **exists**: `/etc/soul/token` was known only to the delivered module and the docs, there is no consumer in `soul/`. SoulSeed is created ONLY by `soul init` (the token from `--token` > env `SOUL_BOOTSTRAP_TOKEN`, STDIN is not read; CSR→Bootstrap-RPC→seed in `<paths.seed>/current/`), and `soul run` kept crashing in a restart loop "SoulSeed not found — run soul init --token first".

The fix — a new step 5 of the A1 flow between token-write and activation (both modes, token-only and install):

```
test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" /usr/local/bin/soul init --config /etc/soul/soul.yml
```

- **Idempotency is mandatory:** the guard on the seed-cert — the token is single-use, a retry of the step after a successful redeem without a guard would fail the host. The guard path is fixed by the constant `soulinstall.SeedCertPath` + a sync-guard test against the layout constants of `soul/internal/seed` (`currentLink`/`CertFile`) and `paths.seed` of the generated soul.yml (`TestSeedCertPath_SyncWithSoulSeedLayout`).
- **Secret-floor preserved:** the command carries the literal unexpanded `$(cat <token_path>)`, expanded by the subshell on the VM — the token is NOT in the keeper's argv; STDIN is empty. Bit-for-bit symmetry with the self-onboard phase of cloud-init.tmpl.
- **token-write remains:** init reads the token from the file — there is no second transfer of the secret, the `token_path` contract is additive.
- `soul init` runs independently of `start_soul` (redeem is the essence of delivery, start is a separate option).

**(Hole, major) No `daemon-reload` + `enable`.** push-install did only `systemctl start soul` — the cloud-init.tmpl runcmd does `daemon-reload` (picking up the freshly written unit) + `enable` (the unit survives a reboot) + `start`. Without enable, after a VM reboot soul.service did not come up. The fix: `start_soul` now runs the chain `systemctl daemon-reload && systemctl enable soul && systemctl start soul` (all three are idempotent, safe in both modes).

**(6th wall, blocker) `event_stream_port` in soul.yml = the bootstrap port.** `keeper.yml::cloud_init` carried only `bootstrap_endpoint` (`host:port`), and the blueprint derived BOTH soul.yml ports from it — soul run dialed EventStream on the Bootstrap-only listener (`Unimplemented: method EventStream not implemented`; EventStream lives on a separate mTLS port, ADR-012(b)). The assumption "behind the LB the port is single" does not hold on live topologies. The fix: a new optional field **`cloud_init.event_stream_port`** (int) → `cloudinit.Config.EventStreamPort` → `soulinstall.Blueprint.EventStreamPort` → `event_stream_port` in soul.yml in both renderers (cloud-init userdata and install-script); `bootstrap_port` still comes from `bootstrap_endpoint`. `0`/omitted → a back-compat fallback to the bootstrap port (single-port LB, previous behavior bit-for-bit).

- **Closes BUG#2 cloud-provision** (the `keeper.push.applied` keeper-side stub did not exist).
- **The name `keeper.push.applied` is rejected** as a keeper-side core address: `push.applied` is the audit-event type of an operator-initiated Destiny push run (`POST /v1/push/apply`), not a keeper-side token-delivery module. The coincidence was an illustrative scenario stub that was misleading.
- **A separate bin-doc** — [docs/keeper/modules.md → `core.bootstrap.delivered`](../keeper/modules.md#corebootstrapdelivered).
