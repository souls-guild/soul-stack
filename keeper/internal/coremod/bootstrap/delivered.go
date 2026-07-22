// Package bootstrap implements the keeper-side core module `core.bootstrap.delivered`
// (ADR-063, docs/keeper/modules.md) — thin delivery of the per-VM bootstrap token
// over SSH to freshly-created cloud-init VMs.
//
// Closes BUG#2 cloud-provision: previously the scenario carried the placeholder
// address `keeper.push.applied`, which keeper-dispatch rejected as an unknown
// module — the created VM never received a token and never onboarded.
//
// Design A1 ("thin delivery", + init phase per the ADR-063 amendment): cloud-init
// (B-flat, ADR-017(h)) has already put the soul binary + CA + systemd unit on the
// VM. The module writes the token, redeems it (`soul init`, seed-guard-idempotent)
// and optionally activates the unit. Per-host flow (sequential): Authorize
// (deny → fail-closed) → ephemeral keypair + Sign → Dial (CA-signed host-cert
// verify) → write the token to `token_path` (★token over STDIN, not argv) →
// soul init (see initSoulCmdFmt) → optional daemon-reload + enable + start.
// A failure on any host aborts the step (B1-strict): state isn't committed, the
// run goes to error_locked.
//
// install mode (optional param `install: true`, transport=teleport only, ADR-063
// amendment "full-install over SSH"): a platform without cloud-init userdata (e.g.
// a namespace with `ci_user_data` disabled) doesn't get set up before delivery — the
// module itself lays down the ENTIRE install blueprint over the same Teleport
// session BEFORE the token. The install steps come from a single source,
// soulinstall.RenderInstallScript (directories → keeper-ca.pem(STDIN) →
// soul.yml(STDIN) → soul.service(STDIN) → curl binary); blueprint parameters are
// resolved from the same keeper.yml::cloud_init + Vault as cloud-init userdata
// (config-reuse). token-write and `systemctl start` are NOT part of
// RenderInstallScript — this module adds them after the install steps. Any
// non-zero exit from an install step fails the host (B1-strict, same as the rest
// of the flow). install=true in direct mode is a Validate error (there, setup
// comes from cloud-init, install isn't needed).
//
// Transport modes (ADR-063 amendment "Teleport by-name transport"):
//   - direct (default): generic push.Dial by primary_ip — Authorize/Sign/
//     ephemeral + CA-signed host-cert verify (host CA from Vault), C1.
//   - teleport: delivery via Teleport Proxy BY-NAME (target = SID/FQDN, NOT
//     primary_ip). Transport + user-auth + host-verify entirely through the
//     Teleport identity file (keeper-side Teleport-Dialer, push.NewTeleportDialer):
//     Authorize/Sign/ephemeral are NOT called, Vault host-CA is NOT required
//     (host-verify via Teleport CA, C1 not applicable). A fresh VM shows up in
//     Teleport only ~3-5min after creation → connect is wrapped in
//     retry-with-backoff up to `join_wait_timeout` (past the deadline — failed,
//     B1-strict).
//
// MVP boundaries (ADR-063): one key-based SshProvider, token-only delivery,
// hosts processed sequentially. Cloud-init CA-signed host-key (C1) is
// required-for-live, the next slice: without it Dial rejects a fresh VM's
// host-cert (a bare host-key with no CA signature), and live-e2e won't pass.
//
// Symmetric with keeper/internal/coremod/cloud: the same sdk/module interface,
// the same Registry pattern, the same audit secret-masking.
package bootstrap

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cloudinit"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the base module name without state suffix (Registry key). Author form
// of task address is `core.bootstrap.delivered` (base + state `delivered`); state
// arrives in pluginv1.ApplyRequest.state and is validated in Apply.
const Name = "core.bootstrap"

// StateDelivered is the only state of this module.
const StateDelivered = "delivered"

// Default values for optional parameters.
const (
	defaultTokenPath = "/etc/soul/token"
	defaultSSHUser   = "root"
	defaultSSHPort   = 22
)

// Transport modes for delivery (Module.Transport field, source: keeper.yml::push.transport).
// Empty string is treated as TransportDirect (backward-compat with existing generic delivery).
const (
	// TransportDirect is generic push.Dial by primary_ip (Authorize/Sign/CA-host-cert).
	TransportDirect = "direct"
	// TransportTeleport is by-name delivery via Teleport Proxy (target = SID), without
	// Authorize/Sign, host-verify through Teleport identity-file.
	TransportTeleport = "teleport"
)

// Retry-with-backoff parameters for connect in teleport mode (fresh VM joins Teleport
// around 3-5 minutes after creation). In direct mode, retry is not applied (host already
// exists at step time).
const (
	// defaultJoinWaitTimeout is the default ceiling for Teleport-join wait (optional param
	// `join_wait_timeout`). 15 min: reverse-tunnel agent on fresh cloud-VM appears in Teleport
	// significantly later than typical 3-5 min (slow join → step failed needlessly, though VM
	// eventually came up), so large margin. Invariant: provision-aware run-timeout floor
	// (ceiling+deployBudget) must exceed this value — TestProvisionTimeoutExceedsJoinWait.
	defaultJoinWaitTimeout = 15 * time.Minute
	// DefaultJoinWaitTimeout is the exported mirror of defaultJoinWaitTimeout:
	// makes default Teleport-join time-budget part of observable contract,
	// so static guard-invariant (provision-aware effective run-timeout scenario must exceed
	// joinWait — else setting is "dead": onboarding barrier stalls prematurely) checks the
	// REAL value, not its duplicate. Value and behavior of delivered.go unchanged.
	DefaultJoinWaitTimeout = defaultJoinWaitTimeout
	// joinRetryBase is the base interval between connect attempts.
	joinRetryBase = 12 * time.Second
	// joinRetryJitter is the upper bound of random jitter added to interval
	// (anti-thundering-herd for batch delivery to N VMs).
	joinRetryJitter = 4 * time.Second
)

// deliverScriptFmt is a shell command to write token to VM. Token is passed via STDIN
// (`cat > path`), NOT in argv: argv would leak to `ps`/audit.log/journald on the VM.
// `install -d -m 0700 /etc/soul` creates directory with private permissions (umask 077
// additionally protects from race window between cat and chmod), `chmod 0400` makes it
// read-only to owner. token_path is substituted directly: source is scenario render
// (trusted keeper-side input, not Soul-reported), shell-escape not needed.
const deliverScriptFmt = "install -d -m 0700 /etc/soul && umask 077 && cat > %s && chmod 0400 %s"

// initSoulCmdFmt is the token redeem command after writing (5th wall ADR-063): seed creates
// ONLY `soul init` (soul-side has no token-file "pickup"), without init the delivered token
// is dead weight; soul run fails with "SoulSeed not found". %s placeholders: seed-cert-guard
// (SeedCertPath — token is single-use, repeat init after successful redeem would break host
// on step retry) / token_path / binary / soul.yml. ★ Literal $(cat …) is expanded by subshell
// on VM — token is NOT in keeper's argv; exact parity with cloud-init.tmpl self-onboard phase.
const initSoulCmdFmt = `test -e %s || SOUL_BOOTSTRAP_TOKEN="$(cat %s)" %s init --config %s`

// startSoulCmd activates soul agent after init (start_soul: true): parity with cloud-init.tmpl
// runcmd — daemon-reload picks up freshly written unit (install mode), enable survives VM reboot;
// both are idempotent.
const startSoulCmd = "systemctl daemon-reload && systemctl enable soul && systemctl start soul"

// SshProviderHost is the narrow SSH authentication surface needed by this module:
// Authorize (Keeper's right to reach host) + Sign (issue SSH credentials for session).
// This is exactly [push.SshProvider] — same host-side consumer contract of two methods
// used by SshDispatcher.
//
// Reused (not duplicated) intentionally: production implementation is discovered
// SshProvider plugin (`*pluginhost.SshProviderPlugin`), which already implements
// Authorize/Sign and is provided to SshDispatcher with the same type. Provider map
// is assembled by daemon wire-up from same spawned plugins; in module unit-tests
// mocked by struct. nil-map/empty → module not registered (see registry).
type SshProviderHost = push.SshProvider

// AuditWriter is for audit-event `bootstrap.delivered`.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// InstallResolver resolves keeper.yml cloud_init block into [cloudinit.Config] —
// source for [soulinstall.Blueprint] for install mode. Narrow surface (not the
// *cloudinit.Resolver itself) keeps bootstrap package independent from config.Store:
// production wrapper in daemon reads current KeeperConfig snapshot (hot-reload) and
// calls Resolver.Resolve — same pattern as cloudInitProvider for cloud-create.
//
// Returned Config already carries resolved CA-PEM (from Vault) + endpoint/URL.
// nil-resolver on Module means build without install support: install=true
// then returns explicit error "install mode not configured".
type InstallResolver interface {
	Resolve(ctx context.Context) (cloudinit.Config, error)
}

// Module implements sdk/module.SoulModule.
//
// Requirement depends on transport (Transport field):
//   - direct (default): Providers + HostCAs + Dial are required (nil/empty →
//     explicit error from Apply or non-registration in Registry).
//   - teleport: Dial (Teleport-Dialer) suffices; Providers/HostCAs not used
//     (Authorize/Sign not called, host-verify via Teleport).
//
// Audit is optional (nil → write skipped).
type Module struct {
	// Transport is the delivery mode: TransportDirect (default if "") or
	// TransportTeleport. Source: keeper.yml::push.transport (daemon wire-up),
	// NOT scenario-param: mode is property of keeper installation, not individual task.
	Transport string

	// Providers resolves SSH provider by name from param `ssh_provider`. MVP is
	// single-provider: module holds map assembled by wire-up from discovered
	// SshProvider plugins (by manifest.Name). Not used in teleport mode
	// (Authorize/Sign not called).
	Providers map[string]SshProviderHost

	// HostCAs is multi-CA set for verifying host-cert of target VMs (same
	// push.LoadHostCAs as SshDispatcher). Non-empty in direct mode; empty →
	// Apply returns explicit error (CA-signed host-cert verify is required,
	// fail-closed). Not used in teleport mode (host-verify via Teleport
	// identity-file, C1 not applicable).
	HostCAs []push.NamedHostKeyAuthority

	// Dial opens SSH session. direct: push.Dial; teleport:
	// push.NewTeleportDialer; test: mock-Dialer.
	Dial push.Dialer

	// RetryBase / RetryJitter are backoff parameters for teleport-connect-retry.
	// 0 → defaults joinRetryBase / joinRetryJitter. Fields exist for unit-tests
	// (short backoff so retry-until-join doesn't sleep actual ~12s); production
	// wire-up doesn't set them.
	RetryBase   time.Duration
	RetryJitter time.Duration

	// Install resolves blueprint parameters for install mode (param `install: true`,
	// teleport only). nil → install mode not configured: task with `install: true`
	// returns explicit error. Production wrapper reads keeper.yml::cloud_init
	// snapshot + Vault (cloudinit.Resolver), wire-up in daemon.
	Install InstallResolver

	// Audit is audit-writer (`bootstrap.delivered`). nil → write skipped.
	Audit AuditWriter
}

// retryBackoff returns (base, jitter) with default substitution for zero values.
func (m *Module) retryBackoff() (base, jitter time.Duration) {
	base = m.RetryBase
	if base <= 0 {
		base = joinRetryBase
	}
	jitter = m.RetryJitter
	if jitter <= 0 {
		jitter = joinRetryJitter
	}
	return base, jitter
}

// teleport reports whether module operates in by-name Teleport mode.
func (m *Module) teleport() bool { return m.Transport == TransportTeleport }

// hostInput is one VM from param `hosts` (= register.<provision>.hosts from
// core.cloud.created): SID, IP for connection, and plain bootstrap token.
type hostInput struct {
	sid       string
	primaryIP string
	token     string
}

// connectTarget is the connection address for diagnostics (direct → primary_ip; teleport
// → SID/node-name). Used in failed-event text so operator sees exactly what was
// being delivered to.
func (h hostInput) connectTarget(teleport bool) string {
	if teleport {
		return h.sid
	}
	return h.primaryIP
}

// Validate checks params without transport dependency (mode is property of keeper
// installation, not task). ★ `ssh_provider` is required in both modes, but in
// transport: teleport it does NOT determine transport: Authorize/Sign not called,
// name goes ONLY into audit-payload `bootstrap.delivered`. Changing required-status
// per transport is post-MVP optional.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != StateDelivered {
		errs = append(errs, fmt.Sprintf("unknown state %q (want %q)", req.State, StateDelivered))
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	if _, err := util.StringParam(req.Params, "ssh_provider"); err != nil {
		errs = append(errs, err.Error())
	}
	// hosts is validated for structure in Apply (per-element list-of-objects);
	// here we only check presence and type via accessor.
	if _, ok := req.Params.GetFields()["hosts"]; !ok {
		errs = append(errs, "param \"hosts\": missing")
	}
	if _, err := util.OptStringParam(req.Params, "token_path"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringParam(req.Params, "ssh_user"); err != nil {
		errs = append(errs, err.Error())
	}
	if n, ok, err := util.OptIntParam(req.Params, "ssh_port"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && (n < 1 || n > 65535) {
		errs = append(errs, "param \"ssh_port\": must be in 1..65535")
	}
	if _, _, err := parseJoinWait(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	if _, _, err := util.OptBoolParam(req.Params, "start_soul"); err != nil {
		errs = append(errs, err.Error())
	}
	// install: full-install over SSH (ADR-063 amendment). Valid ONLY in
	// transport=teleport (MVP): in direct setup cloud-init userdata is used, install
	// not needed. install=true+direct is explicit error (not silent no-op).
	if install, ok, err := util.OptBoolParam(req.Params, "install"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && install && !m.teleport() {
		errs = append(errs, "param \"install\": install mode only for teleport transport (direct setup uses cloud-init)")
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != StateDelivered {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return m.applyDelivered(req, stream)
}

// applyDelivered implements state=delivered. See package doc-comment.
func (m *Module) applyDelivered(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	providerName, err := util.StringParam(req.Params, "ssh_provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	hosts, err := parseHosts(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	tokenPath, err := util.OptStringParam(req.Params, "token_path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if tokenPath == "" {
		tokenPath = defaultTokenPath
	}
	sshUser, err := util.OptStringParam(req.Params, "ssh_user")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if sshUser == "" {
		sshUser = defaultSSHUser
	}
	sshPort, ok, err := util.OptIntParam(req.Params, "ssh_port")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !ok {
		sshPort = defaultSSHPort
	}
	startSoul, hasStart, err := util.OptBoolParam(req.Params, "start_soul")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasStart {
		startSoul = true // default: start soul after token delivery
	}
	joinWait, hasJoinWait, err := parseJoinWait(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasJoinWait {
		joinWait = defaultJoinWaitTimeout
	}
	install, _, err := util.OptBoolParam(req.Params, "install")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if install && !m.teleport() {
		// Duplicates Validate gate: defense-in-depth if Apply called bypassing
		// Validate (direct ApplyRequest construction in tests/tools).
		return util.SendFailed(stream, "bootstrap delivered: install mode only for teleport transport")
	}

	// Configuration preconditions — explicit error instead of cryptic nil-panic.
	if m.Dial == nil {
		return util.SendFailed(stream, "bootstrap delivered: dialer not configured (wire push.Dial / push.NewTeleportDialer in main)")
	}

	// install-steps are resolved ONCE per step (blueprint shared by all VMs:
	// endpoint/CA/URL taken from keeper.yml::cloud_init, not per-host). Resolve
	// reads Vault (CA-PEM) — no need to call per-host. Empty slice when
	// install=false → deliverHost executes no install-steps (token-only).
	var installSteps []soulinstall.InstallStep
	if install {
		steps, ierr := m.resolveInstallSteps(ctx)
		if ierr != nil {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap delivered: install blueprint: %s", maskErr(ierr)))
		}
		installSteps = steps
	}

	// prov is resolved only for direct mode: teleport doesn't call Authorize/Sign
	// (transport+auth entirely through identity-file), Providers map is empty.
	var prov SshProviderHost
	if !m.teleport() {
		if len(m.HostCAs) == 0 {
			return util.SendFailed(stream, "bootstrap delivered: host CAs not configured (set keeper.yml::push.host_ca_refs[] — CA-signed host-cert verify required)")
		}
		p, ok := m.Providers[providerName]
		if !ok || p == nil {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap delivered: ssh_provider %q not registered (known: %v)", providerName, m.providerNames()))
		}
		prov = p
	}

	script := fmt.Sprintf(deliverScriptFmt, tokenPath, tokenPath)
	initCmd := fmt.Sprintf(initSoulCmdFmt, soulinstall.SeedCertPath, tokenPath, soulinstall.SoulBinaryPath, soulinstall.SoulConfigPath)

	results := make([]any, 0, len(hosts))
	sids := make([]any, 0, len(hosts))
	for _, h := range hosts {
		started, err := m.deliverHost(ctx, prov, h, sshUser, int(sshPort), script, initCmd, installSteps, startSoul, joinWait)
		if err != nil {
			// B1-strict: error of any host fails entire step. maskErr protects against
			// vault-ref/token leak into error text (failed-event goes to status_details —
			// observable channel).
			return util.SendFailed(stream, fmt.Sprintf("deliver token to %q (%s): %s", h.sid, h.connectTarget(m.teleport()), maskErr(err)))
		}
		results = append(results, map[string]any{
			"sid":       h.sid,
			"delivered": true,
			"started":   started,
		})
		sids = append(sids, h.sid)
	}

	if m.Audit != nil {
		// Audit-payload — WITHOUT tokens (like cloud.provisioned masking): only
		// count + sids. Plain token visible only in previous step's register and
		// masked on all its outputs; not present here at all.
		ev := &audit.Event{
			EventType: audit.EventBootstrapDelivered,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":       StateDelivered,
				"ssh_provider": providerName,
				"count":        float64(len(hosts)),
				"sids":         sids,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// ★ WITHOUT token in output: register.<name>.hosts[] carries only {sid, delivered,
	// started}. count is number of successfully processed hosts (== len(hosts), else
	// step would have failed).
	return util.SendFinal(stream, true, map[string]any{
		"hosts": results,
		"count": float64(len(hosts)),
	})
}

// deliverHost processes one host: open SSH session (transport-dependent) →
// optional install-blueprint → write token (STDIN) → soul init (redeem token,
// seed-guard-idempotent) → optional unit activation. Returns (started, error).
//
// Session opening diverges by transport:
//   - direct: Authorize (fail-closed) → ephemeral keypair + Sign → Dial by
//     primary_ip (CA-signed host-cert verify) — reuses push infrastructure
//     (newEphemeralEd25519/authMethodsFromSign) exactly as SshDispatcher.SendApply,
//     ephemeral private key never leaves Keeper.
//   - teleport: retry-Dial by SID (node-name) — without Authorize/Sign/ephemeral;
//     transport+auth+host-verify inside Teleport-Dialer via identity-file.
//
// installSteps (non-empty only in install mode, teleport) execute on same
// session BEFORE token — VM without cloud-init gets setup (binary/CA/soul.yml/unit)
// right here. Any non-zero exit from install-step fails host (B1-strict).
//
// Tail (token write + soul init + activation) is common for both modes.
func (m *Module) deliverHost(ctx context.Context, prov SshProviderHost, h hostInput, user string, port int, script, initCmd string, installSteps []soulinstall.InstallStep, startSoul bool, joinWait time.Duration) (started bool, err error) {
	var sess push.Session
	if m.teleport() {
		sess, err = m.dialTeleport(ctx, h, user, port, joinWait)
	} else {
		sess, err = m.dialDirect(ctx, prov, h, user, port)
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = sess.Close() }()

	// install BEFORE token: VM without cloud-init userdata has no setup until this
	// point. ★ Secrets in install-steps (CA-PEM) go via step.Stdin, not argv
	// (RenderInstallScript is single source, secret-write floor). Non-zero
	// exit/err from any step → fail (B1-strict): partially configured VM doesn't get token.
	for i, step := range installSteps {
		if _, rerr := sess.Run(ctx, step.Cmd, step.Stdin); rerr != nil {
			return false, fmt.Errorf("install step %d: %w", i+1, rerr)
		}
	}

	// ★ Token is in STDIN (script does `cat > token_path`), NOT in argv.
	if _, rerr := sess.Run(ctx, script, []byte(h.token)); rerr != nil {
		return false, fmt.Errorf("write token: %w", rerr)
	}

	// soul init — see initSoulCmdFmt (5th wall: without redeem token is useless).
	if _, rerr := sess.Run(ctx, initCmd, nil); rerr != nil {
		return false, fmt.Errorf("soul init: %w", rerr)
	}

	if startSoul {
		if _, rerr := sess.Run(ctx, startSoulCmd, nil); rerr != nil {
			return false, fmt.Errorf("start soul: %w", rerr)
		}
		started = true
	}
	return started, nil
}

// dialDirect is generic mode: Authorize → ephemeral keypair + Sign → push.Dial
// by primary_ip (CA-signed host-cert verify). Unchanged from original A1-flow.
func (m *Module) dialDirect(ctx context.Context, prov SshProviderHost, h hostInput, user string, port int) (push.Session, error) {
	// Authorize is fail-closed: deny stops delivery before connect.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: h.primaryIP, User: user})
	if err != nil {
		return nil, fmt.Errorf("authorize %s@%s: %w", user, h.primaryIP, err)
	}
	if !authReply.GetAllowed() {
		return nil, fmt.Errorf("authorize denied for %s@%s: %s", user, h.primaryIP, authReply.GetReason())
	}

	// Ephemeral keypair: Keeper-side ed25519 pair per-host. Pubkey goes into
	// SignRequest for CA providers; private key NEVER leaves Keeper.
	ephSigner, ephPub, err := push.NewEphemeralEd25519()
	if err != nil {
		return nil, fmt.Errorf("ephemeral keypair: %w", err)
	}
	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{Host: h.primaryIP, User: user, PublicKey: ephPub})
	if err != nil {
		return nil, fmt.Errorf("sign %s@%s: %w", user, h.primaryIP, err)
	}
	auth, err := push.AuthMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return nil, fmt.Errorf("ssh auth: %w", err)
	}

	sess, err := m.Dial(ctx, push.DialConfig{
		Host:            h.primaryIP,
		Port:            port,
		User:            user,
		Auth:            auth,
		HostAuthorities: m.HostCAs,
		ProxyJump:       signReply.GetProxyJump(),
	})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// dialTeleport is by-name mode: Dial by SID (node-name) via Teleport-Dialer
// wrapped in retry-with-backoff until joinWait. Fresh VM appears in Teleport
// only ~3-5 min after creation, so first DialHost attempts return
// 'offline or does not exist' — expected, not valid reason to fail step immediately.
//
// ★ DialConfig carries ONLY Host=SID/Port/User/Timeout: Auth/HostAuthorities/
// ProxyJump are ignored by Teleport-Dialer in teleport mode (auth+host-verify
// from identity-file). Authorize/Sign/ephemeral not called at all here.
func (m *Module) dialTeleport(ctx context.Context, h hostInput, user string, port int, joinWait time.Duration) (push.Session, error) {
	cfg := push.DialConfig{
		Host: h.sid, // ★ node-name = SID, NOT primary_ip
		Port: port,
		User: user,
	}
	sess, err := m.dialWithJoinRetry(ctx, cfg, joinWait)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// dialWithJoinRetry repeats m.Dial(cfg) with fixed backoff + jitter until
// session is received or joinWait expires (or ctx). On deadline returns
// last Dial error — caller moves step to failed (B1-strict, error_locked).
//
// First attempt is immediate (no wait): if node already online (re-run /
// slow provision), delivery doesn't pay extra interval.
func (m *Module) dialWithJoinRetry(ctx context.Context, cfg push.DialConfig, joinWait time.Duration) (push.Session, error) {
	base, jitter := m.retryBackoff()
	deadline := time.Now().Add(joinWait)
	var lastErr error
	for attempt := 0; ; attempt++ {
		sess, err := m.Dial(ctx, cfg)
		if err == nil {
			return sess, nil
		}
		lastErr = err

		// Context cancelled (run interrupted) — exit immediately with ctx-error.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("teleport join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		}
		// Wait budget exhausted — VM never appeared in Teleport.
		wait := base + time.Duration(rand.Int63n(int64(jitter)+1))
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("node not reachable via Teleport within join_wait_timeout (%s, %d attempt(s)): %w", joinWait, attempt+1, lastErr)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("teleport join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		case <-t.C:
		}
	}
}

// resolveInstallSteps gathers install-blueprint once per step: resolves
// keeper.yml cloud_init block into Config (Vault for CA-PEM) → maps via
// cloudinit.Config.Blueprint() (single mapper, shared with cloud-init userdata) →
// renders install-sequence. RenderInstallScript validates blueprint inside
// (endpoint host:port / PEM CA / https-URL).
func (m *Module) resolveInstallSteps(ctx context.Context) ([]soulinstall.InstallStep, error) {
	if m.Install == nil {
		return nil, fmt.Errorf("install resolver not configured (wire keeper.yml::cloud_init resolver in main)")
	}
	cfg, err := m.Install.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	// Config→Blueprint mapping via cloudinit.Config.Blueprint() (same
	// single mapper as cloud-init userdata): new Blueprint-field
	// auto-picked up here, no install-path desync.
	return soulinstall.RenderInstallScript(cfg.Blueprint())
}

func (m *Module) providerNames() []string {
	out := make([]string, 0, len(m.Providers))
	for k := range m.Providers {
		out = append(out, k)
	}
	return out
}

// parseHosts extracts list {sid, primary_ip, bootstrap_token} from param
// `hosts`. In practice arrives as CEL expression `${ register.<provision>.hosts }`
// (output of core.cloud.created). Empty list / missing required fields is
// error (nothing to deliver / nowhere / nothing to write).
func parseHosts(params *structpb.Struct) ([]hostInput, error) {
	lv, err := util.ListParam(params, "hosts")
	if err != nil {
		return nil, err
	}
	if len(lv) == 0 {
		return nil, fmt.Errorf("param %q: empty list (no hosts to deliver to)", "hosts")
	}
	out := make([]hostInput, 0, len(lv))
	for i, item := range lv {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected object, got %T", "hosts", i, item.Kind)
		}
		h, herr := hostFromStruct(sv.StructValue, i)
		if herr != nil {
			return nil, herr
		}
		out = append(out, h)
	}
	return out, nil
}

// parseJoinWait reads optional param `join_wait_timeout` in two forms:
//   - duration string convention `duration` Soul Stack (`"15m"`/`"90s"`/`"1d"`) —
//     symmetric to `await_timeout` in core.soul.registered; essence sets it
//     (`provision_join_wait_timeout`) via provision scenario;
//   - number of seconds (float64) — back-compat with ADR-063 (int, seconds).
//
// Returns (dur, ok, err): ok=false when param absent (caller substitutes
// defaultJoinWaitTimeout). Negative value / invalid string is error
// (wait ceiling cannot be negative). 0 is valid (immediate
// deadline: one attempt without wait).
func parseJoinWait(params *structpb.Struct) (time.Duration, bool, error) {
	const key = "join_wait_timeout"
	v, ok := params.GetFields()[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch kind := v.Kind.(type) {
	case *structpb.Value_StringValue:
		d, err := config.ParseDuration(kind.StringValue)
		if err != nil {
			return 0, false, fmt.Errorf("param %q: invalid duration %q", key, kind.StringValue)
		}
		if d < 0 {
			return 0, false, fmt.Errorf("param %q: must be >= 0", key)
		}
		return d, true, nil
	case *structpb.Value_NumberValue:
		f := kind.NumberValue
		if f != float64(int64(f)) {
			return 0, false, fmt.Errorf("param %q: expected integer seconds, got %v", key, f)
		}
		if f < 0 {
			return 0, false, fmt.Errorf("param %q: must be >= 0 (seconds)", key)
		}
		return time.Duration(int64(f)) * time.Second, true, nil
	default:
		return 0, false, fmt.Errorf("param %q: expected duration string or integer seconds, got %T", key, v.Kind)
	}
}

func hostFromStruct(s *structpb.Struct, idx int) (hostInput, error) {
	sid, err := util.StringParam(s, "sid")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	ip, err := util.StringParam(s, "primary_ip")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	tok, err := util.StringParam(s, "bootstrap_token")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	return hostInput{sid: sid, primaryIP: ip, token: tok}, nil
}

// maskErr masks possible secret leak (vault-ref / token) in error text
// before returning in failed-event. Same substring filter as shared/audit, which
// cleans register-output (`token` fragment + vault-ref). Key `_` is non-secret.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"_": err.Error()})
	if s, ok := masked["_"].(string); ok {
		return s
	}
	return "***MASKED***"
}
