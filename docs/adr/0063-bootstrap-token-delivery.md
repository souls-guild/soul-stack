# ADR-063. core.bootstrap.issued / delivered — keeper-side bootstrap tokens and delivery

> **Status: active for `issued` and for `core.ssh.run`; `delivered` is REMOVED (amendment 2026-09-09, NIM-834).** Everything below the header describing `core.bootstrap.delivered` — design A1, its parameters, its transports, install mode — is **history**: the module is gone, and WHAT to install on a host is the site's own job. What it guaranteed is not history. Read the last two amendments first, in order: [2026-09-09](#amendment-2026-09-09--delivered-is-removed-installing-a-host-is-site-specific-nim-834) restates the guarantees as **requirements on whoever installs the host**, and [2026-09-12](#amendment-2026-09-12--the-transport-comes-back-without-the-policy-nim-849) brings the **transport** back as `core.ssh.run` — the engine executes the site's commands on an agentless host and enforces the secret floor, without owning the policy. Read the rest for the reasoning behind each requirement, which is where the live runs that paid for them are recorded.
>
> architect's design (A1 "thin delivery"), public names `core.bootstrap.delivered` and `core.bootstrap.issued` confirmed by the user. The canon is fixed docs-first BEFORE code; this ADR **amends [ADR-017](0017-keeper-side-core.md), [ADR-061](0061-onboarding-await-and-midrun-reresolve.md), [ADR-015](0015-core-modules-mvp.md)**.
>
> **Implementation progress.** Pilot slice implemented: module + conditional registration + Deps + scenario-swap (`keeper.push.applied` stub → `core.bootstrap.delivered`) + unit tests. **C1 (cloud-init CA-signed host-key) and live-e2e — the next slice, NOT this one** (see §MVP Boundaries). Before C1 a live run of direct mode will break: `push.Dial` rejects the host-cert of a fresh VM whose cloud-init installed a bare (not CA-signed) host-key.
>
> **Amendment (Teleport by-name transport) — implemented (pilot).** A second transport mode `transport: teleport` (by-name via Teleport Proxy, host-verify via Teleport identity-file, C1 not applicable) + keeper-side Teleport-Dialer + retry-until-join + wire-up daemon (teleport mode) + guard tests. See §Amendment below. The direct mode of the bootstrap module is not yet wired into the daemon (BootstrapDial=nil → not registered) — that is a generic-live slice.
>
> **Amendment (full-install mode for platforms without cloud-init userdata) — Slice 1/3 implemented.** `core.bootstrap.delivered` gains a second operating mode — **full-install** over Teleport SSH (installs the ENTIRE setup, not just the token) for platforms without cloud-init userdata (e.g. a namespace with `ci_user_data` disabled). The install-blueprint is extracted into the shared package [keeper/internal/soulinstall](../../keeper/internal/soulinstall) — the single source of truth (canonical `Blueprint`), reused by both onboarding paths. Slice 1 (blueprint extraction: `Blueprint`/`RenderCloudInitYAML`/`RenderInstallScript`/`InstallStep` + switching the `cloudinit` package to shared + tests) is **done**; Slice 2 (install mode in the delivered module itself) and Slice 3 (scenario `generate_userdata:false`+`install:true`+live) are next. See §Amendment (full-install mode) below.
>
> **Amendment (init phase + unit activation + `event_stream_port`) — implemented, proven by live workarounds.** A live run of the push-install-flow hit two walls: (5) the token was delivered but nobody redeemed it — no soul-side "pickup" of the token file exists, the seed is created ONLY by `soul init`, and soul run kept crashing in a restart loop "SoulSeed not found"; (6) the blueprint derived BOTH soul.yml ports from a single `bootstrap_endpoint` — soul dialed EventStream on the Bootstrap port ("Unimplemented: method EventStream"). Plus a hole: push-install did only `systemctl start` without `daemon-reload`/`enable` — after a VM reboot the unit did not come up. See §Amendment (init phase) below.
>
> **Amendment (`core.bootstrap.issued`, NIM-596) — implemented.** Transactional batch issuance for ready-made VM SIDs, explicit reissue/expiry and identity-takeover refusal, separate audit event, plus optional `primary_ip` in Teleport delivery. See the 2026-08-08 amendment below.

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
5. `session.Run("test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN=\"$(cat <token_path>)\" /usr/local/bin/soul init --config /etc/soul/soul.yml", nil)` — **token redeem** (CSR→Bootstrap-RPC→SoulSeed; §Amendment init phase). The guard on the seed-cert = idempotency (the token is single-use; since NIM-865 a same-key retry is also admitted server-side — see the amendment at the end); the literal `$(cat …)` is expanded by the subshell on the VM — the token is not in the keeper's argv.
6. if `start_soul` — `session.Run("systemctl daemon-reload && systemctl enable soul && systemctl start soul", nil)` (parity with cloud-init runcmd; enable survives a VM reboot).

**B1-strict.** A failure of any host (Authorize-deny / connect-fail / write-fail / init-fail / start-fail) → step `failed` → state is not committed → `error_locked`. There is no partial delivery.

## Addressing and side

- Namespace `core`, module `bootstrap`, state `delivered`. The registry key is the base `core.bootstrap`; the state comes from the address suffix via `config.SplitModuleAddr` (like all keeper-side cores).
- Full task name: `module: core.bootstrap.delivered`. Side **Keeper-side**, the step **must** carry `on: keeper`.
- Implementation — `keeper/internal/coremod/bootstrap/delivered.go`, deleted in NIM-834.

## Parameters (`params:`)

| Parameter | Type | Req. | Default | Semantics |
|---|---|---|---|---|
| `hosts` | array of object `{sid, primary_ip, bootstrap_token}` | required | — | List of VMs. In practice comes as the CEL expression `${ register.<provision>.hosts }` (output of `core.cloud.created`). An empty list → `failed`. An entry marked `onboarded: true` carries no token and is skipped — see the 2026-07-26 amendment. |
| `ssh_provider` | string | required | — | Name of the SshProvider plugin (`keeper.yml::plugins.ssh_providers[].name`) for SSH authentication. **★ In `transport: teleport` it does NOT determine the transport** (Authorize/Sign are not called) — the operator passes the name, but it goes ONLY into the audit payload. Dropping the required status per transport is post-MVP optional. |
| `token_path` | string | — | `/etc/soul/token` | Path of the token file on the VM. |
| `ssh_user` | string | — | `root` | SSH user. |
| `ssh_port` | int (1..65535) | — | `22` | TCP port of sshd. |
| `start_soul` | bool | — | `true` | Unit activation after init: `systemctl daemon-reload && systemctl enable soul && systemctl start soul`. `soul init` (step 5) runs independently of this flag. |

## Output contract (module `output:`)

`register.<name>.*`: `hosts[] = {sid, delivered, started}` + `count` (number of processed hosts) + `skipped` (hosts that were already onboarded and needed no delivery, see the 2026-07-26 amendment). Plus the standard `.changed` (always `true` on success) / `.failed` of the DSL core.

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
- **★ C1 — cloud-init CA-signed host-key (required-for-live, NEXT slice).** `push.Dial` trusts only a host-cert signed by the host-CA (`HostAuthorities`), not a bare host-key (rejection of TOFU). A fresh VM after cloud-init has its own host-key — it **must** be CA-signed by the same host-CA, otherwise the handshake is rejected and delivery fails at the connect. cloud-init (B-flat userdata) must generate the host-key and sign it with the host-CA from `keeper.yml::cloud_init` (or place a pre-signed host-cert). Without C1 the module is valid on render (L0 Trial) and passes unit tests, but live-e2e will not pass. C1 + live cloud validation is a separate slice.

## Amendment (Teleport by-name transport)

The module gains a second transport mode `transport: teleport` (vs the default `direct`=generic push.Dial). In teleport mode delivery goes through the Teleport proxy by-name (target=SID/FQDN, NOT primary_ip): the keeper-side Teleport-Dialer ([keeper/internal/push/dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) does transport+user-auth+host-verify entirely through the Teleport identity-file (`creds.SSHClientConfig()`). Deviations from A1: (1) Authorize/Sign/ephemeral-keypair are NOT called; (2) a Vault host-CA is NOT required for teleport — host-verify goes through the Teleport CA (C1 is not applicable to teleport mode); (3) a retry-with-backoff until Teleport-join (~3-5 min) is added. The direct mode (Vault/static, CA-signed host-cert, C1) is unchanged. Teleport creds — the keeper.yml push block (`push.transport` + `push.teleport.{proxy_addr,identity_file,cluster}`), the soul-ssh-teleport plugin does not participate in this flow.

A new scenario parameter `join_wait_timeout` (int, seconds; default 360) — the ceiling for waiting for Teleport-join, relevant only in teleport mode; on expiry the step is `failed` (B1-strict, `error_locked`). Registration of the module in teleport mode requires only the dialer (`BootstrapDial`), providers/host-CA are not needed (see the gate in `coremod.Default`). **Superseded in two places:** the default is 15m, and the parameter is no longer teleport-only — see the [2026-09-14 amendment](#amendment-2026-09-14--the-bounded-wait-applies-to-direct-too-nim-872).

### Amendment 2026-06-30 — Teleport proxy behind an L7-TLS load balancer (`use_system_trust` + `alpn_upgrade`)

**Problem.** The Teleport-Dialer ([dial_teleport.go](../../keeper/internal/push/dial_teleport.go)) verifies the proxy server cert through the identity-CA-pool (`creds.TLSConfig()`) + a forced sentinel ServerName `teleport.cluster.local` (Teleport API client). This is valid ONLY when the proxy presents a Teleport-issued cert. If the Teleport proxy sits BEHIND a public L7-TLS load balancer (e.g. wildcard `*.teleport.example.com`, SAN `*.teleport.example.com, teleport.example.com, www.teleport.example.com` — **no** `teleport.cluster.local`), the gRPC handshake (the `DialHost` path via `credentials.NewTLS`) fails on an x509 DNSName mismatch: `certificate is valid for *.teleport.example.com, not teleport.cluster.local`. `proxy.ClientConfig.InsecureSkipVerify` does not help — it affects only the ALPN-conn-upgrade wrapper, whereas the gRPC handshake goes through our `TLSConfigFunc` with a forced ServerName.

**Solution — the optional field `push.teleport.use_system_trust` (bool, default false).** When `true`, `TLSConfigFunc`, after `creds.TLSConfig()`, adjusts the returned `*tls.Config`: `RootCAs = nil` (Go takes the system trust store → verifies the public balancer cert) + `ServerName = host(proxy_addr)` (removes the sentinel `teleport.cluster.local`). `Certificates`/`GetClientCertificate` (the mTLS client cert for auth on the proxy) are preserved. When `false` (the default) — behavior is bit-for-bit as before (identity-CA-pool + sentinel ServerName); existing installations with a Teleport-issued proxy cert are not affected.

**Security rationale.** This is **not** `InsecureSkipVerify` (trusting any cert — that would open MITM): `RootCAs=nil` gives the same unblock but PRESERVES verification of the public cert against the system trust. The proxy server cert is **not a Soul Stack trust boundary**: authentication of target nodes goes through the client-mTLS-cert (auth on the proxy) + the SSH host-CA from the identity-file (host-verify through the Teleport CA). The system trust verifies only the public balancer cert; trust in the nodes is not weakened. The host from `proxy_addr` is split off by `net.SplitHostPort` at dialer startup (a malformed `proxy_addr` without `:port` → a constructor error, fail-closed, not a late Dial).

**The second half of the same case — `push.teleport.alpn_upgrade` (bool, default false).** `use_system_trust` fixes the INTERNAL gRPC-mTLS handshake (cert mismatch), but behind the L7-TLS load balancer a second barrier remains: the LB terminates TLS and **does not proxy the raw gRPC/SSH stream** (`DialHost` fails already after TLS — on `403 Forbidden; transport: received unexpected content-type "text/plain"`; the 403 is returned by the LB web layer, this is **not** Teleport-RBAC). When `alpn_upgrade: true`, `ALPNConnUpgradeRequired: true` is set in `proxy.ClientConfig` — Teleport wraps the stream in an ALPN-conn-upgrade (a WebSocket tunnel on `/webapi/connectionupgrade`), which the L7-LB passes through as ordinary HTTP. Teleport enables `WithALPNConnUpgradePing` itself inside `newDialerForGRPCClient`. We do not touch other fields: `TLSRoutingEnabled` affects only the path to Auth (not `DialHost`), `InsecureSkipVerify` would propagate into the OUTER TLS to the LB and disable verification of the public cert (a MITM hole) — categorically no.

**Both flags — a pair for proxy-behind-L7-LB.** `use_system_trust` fixes the internal gRPC-TLS over the tunnel, `alpn_upgrade` breaks through the tunnel itself via the L7-LB; the layers are orthogonal, but for this topology they are enabled together. Behind the transport, authentication of target nodes on the identity does not change: the role's access to nodes (`ssh` logins) remains Teleport-RBAC on the identity-file — this is configured on the Teleport side (the bot's `tctl` role), not by our code.

## Amendment 2026-06-30 — full-install mode (platforms without cloud-init userdata)

**Problem.** Design A1 assumes that cloud-init (B-flat, [ADR-017(h)](0017-keeper-side-core.md)) has already installed the soul binary + CA + systemd-unit on the VM, while `core.bootstrap.delivered` places ONLY the per-VM token. This requires the provider to accept userdata at Create (`generate_userdata: true`). Some platforms **do not accept** userdata — for example a namespace with `ci_user_data` disabled: the VM comes up "bare", cloud-init does not run on it, and delivering a single token is meaningless (there is neither a binary, nor a config, nor a unit that the token is meant to complement). For such platforms `generate_userdata` is **not the only** onboarding path ([ADR-017(h) amendment](0017-keeper-side-core.md)): the entire setup must be installed by another channel.

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

- **Idempotency is mandatory:** the guard on the seed-cert — the token is single-use, a retry of the step after a successful redeem without a guard would fail the host. (Since NIM-865 an unguarded retry carrying the SAME key no longer fails once the host has connected — it is refused, which is the same outcome for this step; the guard stays mandatory. See the amendment at the end.) The guard path is fixed by the constant `soulinstall.SeedCertPath` + a sync-guard test against the layout constants of `soul/internal/seed` (`currentLink`/`CertFile`) and `paths.seed` of the generated soul.yml (`TestSeedCertPath_SyncWithSoulSeedLayout`).
- **Secret-floor preserved:** the command carries the literal unexpanded `$(cat <token_path>)`, expanded by the subshell on the VM — the token is NOT in the keeper's argv; STDIN is empty. Bit-for-bit symmetry with the self-onboard phase of cloud-init.tmpl.
- **token-write remains:** init reads the token from the file — there is no second transfer of the secret, the `token_path` contract is additive.
- `soul init` runs independently of `start_soul` (redeem is the essence of delivery, start is a separate option).

**(Hole, major) No `daemon-reload` + `enable`.** push-install did only `systemctl start soul` — the cloud-init.tmpl runcmd does `daemon-reload` (picking up the freshly written unit) + `enable` (the unit survives a reboot) + `start`. Without enable, after a VM reboot soul.service did not come up. The fix: `start_soul` now runs the chain `systemctl daemon-reload && systemctl enable soul && systemctl start soul` (all three are idempotent, safe in both modes).

**(6th wall, blocker) `event_stream_port` in soul.yml = the bootstrap port.** `keeper.yml::cloud_init` carried only `bootstrap_endpoint` (`host:port`), and the blueprint derived BOTH soul.yml ports from it — soul run dialed EventStream on the Bootstrap-only listener (`Unimplemented: method EventStream not implemented`; EventStream lives on a separate mTLS port, ADR-012(b)). The assumption "behind the LB the port is single" does not hold on live topologies. The fix: a new optional field **`cloud_init.event_stream_port`** (int) → `cloudinit.Config.EventStreamPort` → `soulinstall.Blueprint.EventStreamPort` → `event_stream_port` in soul.yml in both renderers (cloud-init userdata and install-script); `bootstrap_port` still comes from `bootstrap_endpoint`. `0`/omitted → a back-compat fallback to the bootstrap port (single-port LB, previous behavior bit-for-bit).

- **Closes BUG#2 cloud-provision** (the `keeper.push.applied` keeper-side stub did not exist).
- **The name `keeper.push.applied` is rejected** as a keeper-side core address: `push.applied` is the audit-event type of an operator-initiated Destiny push run (`POST /v1/push/apply`), not a keeper-side token-delivery module. The coincidence was an illustrative scenario stub that was misleading.
- **A separate bin-doc** — [docs/keeper/modules.md → `core.bootstrap.delivered`](../keeper/modules.md#corebootstrapdelivered--removed-nim-834), now the removal note.

## Amendment 2026-07-26 — a host that was already onboarded is skipped, not failed (NIM-189)

Follows from [ADR-017 amendment 2026-07-26](0017-keeper-side-core.md): a re-`create` over an incarnation whose hosts are already up passes those hosts through and issues **no** bootstrap token for them (a token is a one-time capability, and a host holding a seed has nothing to redeem it against). `core.cloud.created` marks such entries **`onboarded: true`** in `hosts[]`.

Without a matching rule here the delivery step would die on them: `hosts[i].bootstrap_token` was unconditionally required, so a missing token was a parse-level hard error and the whole re-run failed at delivery.

- **`onboarded: true` → skip.** The host is not dialed at all — nothing to write, and `soul init` would be a no-op behind the seed-guard (`test -e <SeedCertPath>`) anyway. It is still REPORTED: `hosts[] = {sid, delivered: false, started: false, onboarded: true}`.
- **The flag is the only exemption.** A host with neither a token nor `onboarded: true` is still a hard error, exactly as before. Skipping it silently would leave the incarnation short of a member with nothing in the output to show for it.
- **Output gains `skipped`** — how many hosts needed no delivery (`0` on a clean run). `count` keeps its meaning: the total number of hosts in the list. B1-strict is untouched for every host that IS delivered to.
- Audit `bootstrap.delivered` still carries `{action, ssh_provider, count, sids}` with skipped hosts included in `sids` — they are part of the roster this step accounted for.

## Amendment 2026-08-08 — `core.bootstrap.issued` for ready-made VM onboarding (NIM-596)

**Problem.** The delivery contract was coupled by its producer shape to
`core.cloud.created`: it could deliver a token, but there was no keeper-side DSL
operation that produced tokens for VMs which already existed outside the
CloudDriver lifecycle. A service accepting a list of ready-made Teleport nodes
therefore had to leave the scenario DSL, call the operator API per host, and
manually reconstruct `hosts[]`. Besides being operationally awkward, a failure
halfway through that loop left a usable partial group with no run-level outcome.

**Decision: add the public state `core.bootstrap.issued`.** Input `sids` is a
required non-empty unique list of canonical FQDN/SIDs. In one Postgres
transaction Keeper creates a missing Soul as `pending`, `transport=agent`, or
locks and checks an existing not-yet-onboarded agent Soul, invalidates any
previous unused token, and inserts a fresh standard-TTL token. The output is
`hosts[]={sid,bootstrap_token,expires_at,created,reissued}` plus aggregate
counts. This shape is directly consumable by `core.bootstrap.delivered` and is
deliberately independent of `core.cloud.created`.

**Identity boundary.** Only `pending` and `expired` agent records are eligible.
`expired` means onboarding never completed and is re-armed to `pending` with a
fresh `requested_at`. `connected` and `disconnected` both already own a
SoulSeed, so issuing another bootstrap capability would create an identity
takeover path; both are refused fail-closed. `revoked`, `destroyed`, and
`transport=ssh` are refused as well. The Soul row is locked during issuance,
closing the race with Bootstrap redeem: the transaction either observes the
onboarded status and refuses, or invalidates the preceding token before the new
one is committed.

**Repeat and expiry semantics.** Every successful repeat over an eligible batch
returns fresh plaintext and invalidates each previous unused token, whether or
not its `expires_at` has passed. A repeat is therefore the explicit recovery
operation after an interrupted or expired delivery. The special marker
`system-bootstrap-issued-reissue` distinguishes this from operator
`force=true` and cloud reprovision. The whole list is a single transaction: an
error names its SID and rolls back all earlier hosts in the batch, so no partial
set remains usable without output.

**Secret boundary and audit.** Plaintext is never persisted in the Souls/token
registries; only SHA-256 is stored. It is revealed exactly once in the current
run register under `bootstrap_token`, so a following delivery step can address
each host. That key is covered by the common secret masker and is excluded from
incarnation state, task/audit payloads, OTel, SSE and logs. New audit event
`bootstrap.issued` carries only `{action,count,created,reissued,sids}`. Delivery
keeps its separate `bootstrap.delivered` event, also without tokens.

**Teleport addressing correction.** `core.bootstrap.delivered` in Teleport mode
dials `hosts[].sid` and never reads the IP. `primary_ip` is therefore optional in
that path, making `issued → delivered(install=true)` a real contract rather than
one that requires invented data. Direct transport continues to require a
non-empty `primary_ip` and fails before dialing without it.

The canonical ready-made VM flow is:

`core.bootstrap.issued → core.bootstrap.delivered(install=true, teleport) → core.soul.registered(await_online)`.

## Amendment 2026-09-04 — issuance converges over a host this run already onboarded (NIM-780)

**Problem, found on a live run.** The fail-closed rule above made a `create` that
died *after* onboarding unrepeatable. The sequence: `create` builds the VMs,
mints, delivers and onboards them — the Souls are `connected` — and the run then
fails later, on the rollout or on cluster assembly. The operator repeats
`create`; the cloud plugin idempotently hands back the same machines (it scans by
its run label, NIM-16) and therefore the same SIDs; issuance refuses every one of
them as an identity takeover and rolls the whole batch back. The run could never
be completed without deleting Soul rows by hand.

It could not be worked around in the service either. A keeper task has no roster
to narrow the list with — `keeperVars` sets no `Soulprint`, so `soulprint.hosts`
does not even compile there — and an empty `sids` list is refused too, so
"mint only for the new ones" has no expressible form.

**Why it became reachable only now.** `core.cloud.created` carried the exemption:
under the [2026-07-26 amendment](#amendment-2026-07-26--a-host-that-was-already-onboarded-is-skipped-not-failed-nim-189)
it passed an already-up host through as `onboarded: true`, and delivery skipped
it. Under [epic NIM-757](0017-keeper-side-core.md) the cloud driver becomes an
ordinary plugin, which knows nothing about Souls and cannot produce that flag —
and issuance never had an exemption of its own.

**Decision: the same exemption, on the side that mints.** A `connected` /
`disconnected` agent Soul that is **this run's own** is passed through instead of
refused: no token, no write of any kind, `hosts[] = {sid, onboarded: true}`.

- **Ownership is the same predicate, not a new one.** A row is this run's own
  when it is a member of the run's incarnation or of no incarnation at all —
  `EnsureProvisionable`'s rule, now shared as `keepersoul.OwnedByRun` rather than
  restated. Unbound counts as own for the reason it always did, and it is load-
  bearing here: a run that died between delivery and `core.soul.registered` left
  its hosts up but not yet bound, and that is precisely a run needing repair.
- **A host of another incarnation is still an identity takeover** and is still
  refused, still rolling the whole batch back. An empty incarnation means
  *unknown*, never *no owner*, so a caller outside a run can claim only unbound
  rows.
- **Nothing is written for a converged host.** No token — a bootstrap token is a
  one-time capability and this host authenticates with a seed. No `pending`
  refresh — re-arming a live host would wipe its presence and expose it to the
  Reaper's pending sweep while its stream is up.
- **The SID keeps its slot in `hosts[]`, in order.** Dropping it would be the
  same dead end from the other side: delivery refuses an empty `hosts` list, so a
  fully converged re-run would then fail there instead.
- **Delivery needs no address for it, on either transport.** The entry is
  `{sid, onboarded: true}` and nothing more — issuance knows no IPs. The
  `primary_ip` requirement of `direct` is therefore settled AFTER the `onboarded`
  flag rather than before it; the old order only ever worked because
  `core.cloud.created`'s pass-through entries carried an IP from the VM record,
  so `direct` never met one without. Demanding a dial address for the one host
  the step is about to skip failed the step over it.
- **Output and audit gain `skipped`**, symmetric with `core.bootstrap.delivered`.
  `count` keeps its meaning — every requested SID — and every SID stays in the
  audit `sids`: a host that needed no token is a fact about the run, not the
  absence of one.

`revoked`, `destroyed` and `transport=ssh` are untouched: still refused for every
caller, ownership irrelevant.

A scenario that re-maps `register.<issue>.hosts` into delivery's `hosts` must
carry `onboarded` through. Dropping it turns a converged entry into a host with
no `bootstrap_token`, which delivery is right to reject.

## Amendment 2026-09-09 — `delivered` is removed, installing a host is site-specific (NIM-834)

**Decision.** `core.bootstrap.delivered` is **removed**. `core.bootstrap.issued`
**stays**: minting is an INSERT of a pending Soul plus the hash of a fresh
one-time token in one Postgres transaction, and nothing outside the Keeper can
do that.

**Why the delivery half had to go.** The module did two unrelated things — it
put the Soul binary on the host and it handed the host its token — and only the
second is ours. Installation is different at every site: a prepared image, a
cloud-init cycle, somebody's own shell script, somebody else's fleet manager.
The evidence is in this ADR's own history: the module accreted a second
transport, then a full-install mode, then an init phase, then a second soul.yml
port, each one a platform meeting the previous design and not fitting. There is
no right way to install a host from the engine, so the engine should not have
one. The two paths it grew — `RenderCloudInitYAML` and `RenderInstallScript` in
[keeper/internal/soulinstall](../../keeper/internal/soulinstall) — are kept as
the **normative description of the result**, not as an implementation: they
describe the files, paths, permissions, `soul.yml` and systemd unit an installed
host must end up with. Neither has a caller any more.

**The engine's remaining half of onboarding** is `core.bootstrap.issued →
(install, off-engine) → core.soul.registered(await_online)`. The middle step is
outside the scenario, and a scenario that mints a token and never has it
redeemed will block at the barrier until the run timeout. That is the honest
shape of it today; naming it is the point of this amendment.

### ★ Requirements on whoever installs the host

Each of these was paid for by a live run, and **none of them is reproduced by
writing a file with the token in it**. An installer that misses one produces a
host that never onboards, silently.

**1. The token must be REDEEMED, not merely placed.** There is no soul-side
pickup of a token file — the seed is created ONLY by `soul init`. The live run
that found this delivered the token, nobody redeemed it, and `soul run` sat in a
restart loop on "SoulSeed not found" (see the [init-phase
amendment](#amendment-2026-07-02--init-phase-in-the-a1-flow-unit-activation-event_stream_port)).
The redeem command, and its guard:

```
test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" /usr/local/bin/soul init --config /etc/soul/soul.yml
```

The seed-cert guard is **mandatory**, not tidiness: a bootstrap token is
single-use, so an unguarded retry after a successful redeem fails the host on
the installer's second run. The guard path is `soulinstall.SeedCertPath`, pinned
against soul's own seed layout by `TestSeedCertPath_SyncWithSoulSeedLayout` —
an installer that hardcodes the path instead of taking it from there is one
refactor away from a guard that never matches.

**2. The token travels in STDIN, never in argv.** A command argument is visible
in `ps`, in `audit.log` and in journald **on the host itself**, so the write is
`cat > <token_path>` fed from stdin, and the redeem carries the literal
unexpanded `$(cat <token_path>)`, which the subshell expands on the host —
keeping the token out of the caller's argv too. The same floor applies to every
other secret in the install (the CA PEM, `soul.yml`): body in stdin, path in the
command. `RenderInstallScript` guarantees this constructively, and the
ARGV-LEAK-GUARD test in `soulinstall` is what keeps it true.

**3. Unit activation is `daemon-reload && enable && start`, not `start`.** Also
a live finding: push-install did a bare `systemctl start soul`, and after a VM
reboot the unit did not come up. `daemon-reload` picks up the freshly written
unit, `enable` survives the reboot; all three are idempotent.

### What the module gave for free and an installer must now provide itself

- **`Authorize` before the connect (fail-closed).** A provider deny aborted
  delivery before an SSH session was opened, rather than after.
- **An ephemeral ed25519 keypair per host, private key never leaving the
  Keeper.**
- **CA-signed host-cert verification** against the host CA from Vault. TOFU was
  refused outright: an empty host-CA set was an error, never a blind connect.
- **No token in the step output.** The plaintext is revealed exactly once, in
  the register of the minting step, where the common secret masker redacts it by
  key; the delivery step's own output carried `{sid, delivered, started}` and
  nothing else.
- **B1-strict, per host.** Any host failing anything failed the whole step, so
  the run went to `error_locked` rather than committing state over a group that
  was only partly up.
- **A wait for the host to become reachable, bounded and named.** A fresh VM
  appeared in Teleport only ~3-5 minutes after creation, so the connect was a
  retry with backoff against `join_wait_timeout`. Whatever waits now needs the
  same invariant against the run timeout: a wait ceiling that can exceed the
  effective run timeout is a dead setting, and the run aborts before the host
  ever arrives.

### What is kept, and what stopped having a consumer

- **Kept as reference:** [keeper/internal/soulinstall](../../keeper/internal/soulinstall)
  (the blueprint plus the two guards above) and
  [keeper/internal/cloudinit](../../keeper/internal/cloudinit) (the resolver over
  it). **Kept as machinery:** `keeper/internal/push/dial_teleport.go` with
  `keeper.yml::push.transport` / `push.teleport`, because the environment
  constraints [ADR-066](0066-teleport-onboarding-profile.md) proved live — a bot
  identity without `pin_source_ip`, `alpn_upgrade` behind an L7-TLS balancer, an
  active external IP before enroll — are what a site installer reaching a VM
  through Teleport has to satisfy, and they are cheaper to keep than to
  rediscover.
- **Inert config:** `push.transport` and `cloud_init` are still parsed and
  validated, and now nothing reads them. Removing them is a config-contract
  change and belongs to the ticket that writes the installer.
- **Retired audit event:** `bootstrap.delivered` is emitted by nothing. The
  constant stays and its wire value stays **reserved** — `audit_log` holds rows
  written under it, and handing the id to a different event would relabel that
  history (`TestRetiredEventIDsAreNotReused`). `bootstrap.issued` is unchanged.
- **`core.bootstrap.delivered` is refused, not ignored**, on both sides:
  soul-lint does not resolve the address against the core catalog, and the
  keeper-side module answers `unknown state "delivered"`. Silence would be the
  dangerous outcome — a run that skips installation reaches `await_online` and
  fails there, minutes away and one subject away from the cause.

## Amendment 2026-09-12 — the transport comes back, without the policy (NIM-849)

**What the 2026-09-09 amendment did not foresee.** Removing `delivered` removed
the policy, which was right, and the **transport**, which was not. Verified
2026-09-12: `keeper/internal/push` had no caller outside its own tests, and with
it went the engine's only way to execute anything on a host that has no agent —
`core.exec.run` and `core.file.present` are Soul-side and a fresh VM has no
Soul. The amendment above describes the middle step as "off-engine", but there
was no channel for a site installer to be off-engine *in*: a `create` minted
tokens nobody could redeem and hung at `await_online`. That is an MVP blocker,
not a documented boundary.

**Decision: a keeper-side core module `core.ssh.run`.** It takes a list of hosts
and an ordered list of shell steps, opens one SSH session per host and runs the
steps on it. What the commands do is the service author's business; the module
carries the transport and the secret floor and nothing else.

**The name is the boundary.** `core` for the namespace, `ssh` for the module —
the transport — and `run` for the state, the verb, symmetric with Soul-side
`core.exec.run`. `core.bootstrap.installed` was the alternative and was
rejected: a name that says *bootstrap* invites policy back in, and this ADR's own
history is what that costs — a second transport, then a full-install mode, then
an init phase, then a second `soul.yml` port, each one a platform meeting the
previous design and not fitting. A module named for its transport has nowhere to
put an opinion about what it transports. `core.push.run` was rejected too: in
this repository "push" already names an operator-initiated Destiny push run, and
[the 2026-07-02 amendment](#amendment-2026-07-02--init-phase-in-the-a1-flow-unit-activation-event_stream_port)
already refused `keeper.push.applied` for exactly that confusion.

### Parameters

`hosts` (required), `steps` (required), `ssh_provider` (required), `ssh_user`
(default `root`), `ssh_port` (default `22`), `join_wait_timeout` (default 15m).
Transport comes from `keeper.yml::push.transport` — `direct` or `teleport` —
and is deliberately not a scenario param: which way a Keeper installation
reaches its hosts is a property of the installation, and a task that could pick
one would be a task that has to know the site's topology. That key and the
`push.teleport` block, inert between NIM-834 and this ticket, have a consumer
again.

Each step carries `run:` (a shell command line) and at most one stdin source:

- `stdin:` — a literal value, the same on every host (`${ vault(…) }`,
  `${ vars.* }`);
- `stdin_from:` — the NAME of a field of the host entry, whose value the module
  feeds to the process.

**Why `stdin_from:` rather than `${ host.bootstrap_token }`.** There is no
`host.*` CEL root, and there cannot be one here: a keeper-side task is rendered
ONCE for the whole run, not once per host (`keeperVars` binds no roster, and the
seal's host-invariance is itself guarded by
`TestRender_SealedSetDoesNotMoveWithTheRoster`), so a per-host value has no
expression that could reach a params cell. `loop:` is not an escape either —
it is not supported on `on: keeper` at all. Naming the field is what remains,
and it is the better shape anyway: the per-host secret never enters the task's
params, so keeping it out of argv is constructive rather than checked.

### The three guarantees, and how each is kept

**1. STDIN, never argv — a refusal, not a redaction.** The value of `stdin:` /
`stdin_from:` is passed to `session.Run` as the process's stdin and is formatted
into a command line nowhere in the module. A `run:` that nevertheless holds a
secret is refused **before the connect**, by two independent checks, because
neither covers the other's case:

- a `run:` cell the render SEALED ([ADR-010] §7.4 — it read `${ vault(…) }`, a
  vault-backed `vars:` name, a secret `input:`, a `loop:` bind) is refused by
  PATH. The run's sealed set already reached `dispatchKeeperTasks`, for masking
  on the way out; it now also travels into the module on the module context, the
  same channel `WithIncarnation`/`WithService` use, because masking a message
  after the command has run is too late;
- a `run:` that quotes the VALUE of a secret-named field of one of the hosts —
  the token pulled out of `register.<mint>.hosts` and interpolated by hand — is
  refused by value. The seal cannot see this one: a register is not a sealed
  source unless the module declared the output `secret: true`, which
  `core.bootstrap.issued` does not. Only fields whose NAME the platform already
  treats as sensitive are compared: a sid is a plausible substring of an ordinary
  command, and under B1-strict one false positive costs the whole run.

Masking downstream is the wrong instrument here. Argv is visible in `ps`, in
`audit.log` and in journald **on the host itself**, which is a machine we do not
own yet; redacting the operator's copy would hide the leak rather than prevent
it.

**2. No token in the output.** The step's own output is `hosts[] = {sid, ran,
skipped}` + `count` + `skipped`. Command stdout is not captured, not registered
and not audited — a step whose job is to write a secret can echo it, and
`accumulateKeeperRegister` writes the register row verbatim on purpose. The
audit event `ssh.run` carries `{action, ssh_provider, transport, count, skipped,
steps, sids}`: counts and addressing, never the command text, which is the
site's own policy and nothing the module can vouch for.

**3. Fail-closed before the connect**, reusing `keeper/internal/push` rather
than restating it: `Authorize` (a deny stops the run before a session is
opened), an ephemeral ed25519 keypair per host whose private half never leaves
the Keeper (`push.NewEphemeralEd25519` + `push.AuthMethodsFromSign`, the two
wrappers `export.go` exists for), and `push.Dial` with the host CA from Vault —
an empty CA set is an error, never a blind connect. In teleport mode
Authorize/Sign are not called and host-verify comes from the identity file, as
before; the bounded wait for a fresh VM to become reachable is
`join_wait_timeout` (on both transports since the
[2026-09-14 amendment](#amendment-2026-09-14--the-bounded-wait-applies-to-direct-too-nim-872)),
and the invariant that the effective run timeout must exceed it is guarded again
by `TestProvisionTimeoutExceedsJoinWait`.

**The `onboarded` branch survives.** A host flagged `onboarded: true` carries no
per-host material at all — reaching for `bootstrap_token` on it in CEL is an
error, not an empty string — so it is skipped and not dialed, and the
`primary_ip` requirement of the direct transport is settled AFTER the flag (the
[2026-09-04 amendment](#amendment-2026-09-04--issuance-converges-over-a-host-this-run-already-onboarded-nim-780)'s
lesson). The skip lives inside the module because a scenario cannot do it:
`when:` on an `on: keeper` task is only half-evaluated — a static predicate
works, one reading `register.*` is silently ignored.

### What is still the service author's, and still binding

`core.ssh.run` does not know what a host should end up with. The
[requirements above](#-requirements-on-whoever-installs-the-host) are unchanged
and now bind the author of the `steps:` list rather than a hypothetical
off-engine installer — redeem the token with `soul init` behind the
`soulinstall.SeedCertPath` guard rather than merely writing the file, keep the
token in stdin (which the module now enforces), and activate with
`daemon-reload && enable && start` rather than a bare `start`. The module
refuses the argv mistake; it cannot refuse a missing redeem or a missing
`enable`, and both still produce a host that never onboards, silently. The
canonical description of the result remains
[keeper/internal/soulinstall](../../keeper/internal/soulinstall), which still has
no caller — it describes an outcome, not a procedure.

The ready-made VM chain is therefore whole again:
`core.bootstrap.issued` → `core.ssh.run` → `core.soul.registered(await_online)`,
with the middle step's CONTENT authored per site.

## Amendment 2026-09-14 — the bounded wait applies to `direct` too (NIM-872)

`join_wait_timeout` did nothing on the `direct` transport. `dialTeleport` was a
bounded retry; `dialDirect` was `Authorize` → `Sign` → `push.Dial`, and the first
error went straight up. A step advertising a wait ceiling that half its
transports ignore is worse than one with no ceiling at all — the setting reads as
a guarantee.

Found by a live `create` on `vmlocal` (NIM-867): the VM was created, the lease
arrived, and the run failed about ten seconds later with `dial tcp
192.168.122.173:22: connect: connection refused`, because sshd starts well into
the boot.

**This is not a `vmlocal` defect.** The readiness predicate — a DHCP lease
carrying an address and a name — is the cloud contract's, and `vmlocal`
reproduces it verbatim. The surviving cloud driver has the same race; it is
invisible there only because that site dials over Teleport, which already
retried.

**What changed.** One retry loop serves both transports. What differs between
them is which failure a wait can still fix, so that is the only parameter:

- **`teleport` retries every `Dial` error**, as before. The proxy answers for an
  unenrolled node with an ordinary error string ("node offline or does not
  exist") that carries no shape to classify, and transport, user-auth and
  host-verify all happen behind that one call.
- **`direct` retries only a failure to ESTABLISH the connection** — connection
  refused, host or network unreachable, a dial timeout. Nothing is matched on
  error text; the classifier reads the error's SHAPE, and there are two, because
  the direct transport has two sub-paths. Over our own socket `net.Dialer`
  reports each failure as a `*net.OpError` with `Op == "dial"`, wrapped by
  `push.Dial` with `%w`. When the SshProvider returns `proxy_jump` on its
  `SignReply`, `push.Dial` goes through `dialViaProxy` instead and the target
  connect is the bastion's `direct-tcpip` channel: the identical absent sshd then
  arrives as an `*ssh.OpenChannelError` with `Reason == ConnectionFailed` and no
  `net.OpError` under it at all. A classifier reading only the socket shape would
  have left the bastion half of `direct` exactly as broken as before the fix —
  which is how it was first written here, and what the review caught.
  `ssh.Prohibited` (the bastion declining on policy) is excluded: that is a deny,
  like `Authorize`'s.

**What is deliberately NOT retried**, because repeating it cannot change the
answer and would turn a clear refusal into a fifteen-minute hang:

- an **`Authorize` deny** — policy, and upstream of the loop entirely;
- a **`Sign` failure** — the credential mint refusing, also upstream;
- an **SSH handshake rejection** — a host cert not signed by our CA, a public key
  the host will not take. This is the layer boundary the classifier draws, and it
  is the whole of the design: everything a booting machine produces is below it;
- a **non-zero exit from a step** — the command reached the host and answered.
  Redialing would replay the earlier steps of the list on a machine that already
  has them. A scenario-level `retry:` on the task is still legitimate and still
  the author's call — it is a different thing from a retried dial.

Each of the five is a guard test in `keeper/internal/coremod/ssh/run_test.go`,
mutated in the form of real code (drop the retry; give `direct` teleport's
predicate; wrap `runHost` rather than the dial).

**Proven against a real machine**, because a mock dialer cannot: every failure
those guards hand the dialer is a value a test wrote, so a classifier reading a
shape the live path never produces would leave all five green and the fix inert.
The `libvirt`-tagged lane `keeper/internal/coremod/ssh/live_vmlocal_test.go`
provisions a VM through the `vmlocal` artifact and waits out a real refused
window: four refusals, connected on the fifth attempt 58s after the lease, the
steps then running on the guest, and `Authorize`/`Sign` called once each across
the whole wait. The refusal arrived as `*net.OpError{Op: "dial"}` — the shape the
classifier reads, measured rather than assumed. With the retry mutated out the
same lane dies on the first refusal with this ticket's error verbatim.

★ **The width of the window belongs to the image and the host, not to the
engine.** Measured on a workstation with no artificial delay: `vmlocal` reported
the lease 15s after the request and the FIRST dial connected — on a Debian-12
cloud image here, sshd is already listening when the lease lands. The race is
real (this ticket was filed off a live run that hit it, with a stale lease
widening it) but it is not reproducible on demand, so the live lane holds sshd
down for a fixed period instead of hoping: the machine, the kernel's refusal and
the sshd that finally answers are all real, and only the moment it answers is
deterministic. A test that exercises its subject only sometimes is not a test —
the first version of that lane asserted a natural window, found none, and failed
rather than passing vacuously.

The credential is minted once and must outlive the wait: a provider issuing
certificates shorter-lived than `join_wait_timeout` would hand back one that
expires mid-wait, and the expiry would then surface as a handshake rejection —
which is not retried, so the step fails at once, minutes into the budget. That is
a provider-side TTL question the module cannot detect, and the alternative —
re-signing per attempt — burns an issuance on every refusal. It is stated for
operators in the module README rather than only here, because the person who sets
the Vault SSH role TTL reads that page.

Two bounds worth recording rather than fixing. `DialConfig.Timeout` is unset (as
before this ticket), so the deadline is tested BETWEEN attempts and a single
attempt that hangs on a dropped SYN can overrun `join_wait_timeout` by the OS TCP
timeout. And the deadline is per host, so N hosts can each spend the budget,
while `TestProvisionTimeoutExceedsJoinWait` guards the run timeout against one —
pre-existing on teleport, newly reachable on the default transport, and in
practice amortised because hosts boot in parallel.

The default stays one number for both. 15m is a ceiling, not a wait: a host
already listening pays nothing, so the direct transport, where sshd arrives in
tens of seconds rather than minutes, shares it rather than naming a second
setting. `TestProvisionTimeoutExceedsJoinWait` is unchanged and still guards it
against the effective run timeout.

## Amendment 2026-09-19 (NIM-865, [ADR-0090](0090-bootstrap-reply-loss-recovery.md)): the seed-cert guard is now honoured by the server too

`test -e /var/lib/soul-stack/seed/current/cert.pem || … soul init` is called idempotent above, and the client half always was: the guard correctly distinguishes "I onboarded" from "I did not". The server half was not. The Keeper burned the token when it **signed**, so a reply lost in flight — a slow Vault PKI inside the client's deadline is enough — left the guard seeing no certificate, re-running `soul init` on every boot, and taking `PermissionDenied` forever.

A retry is now recognized as a retry: `soul init` reuses the key of the unfinished attempt (`paths.seed/pending-key.pem`) and the Keeper admits a re-presentation that carries the key the burn already bound, on a host that has never held a stream. The command in step 5 is unchanged — what changed is that repeating it can now succeed. One-time still holds where it matters: the keypair a token may issue for is fixed at the first presentation, and the token dies the moment the host connects.
