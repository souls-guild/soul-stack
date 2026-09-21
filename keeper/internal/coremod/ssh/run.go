// Package ssh implements the keeper-side core module `core.ssh.run`: it opens
// an SSH session to each host of a list and executes an ordered list of shell
// steps on it, feeding a value to a step's stdin when the step asks for one
// (NIM-849, ADR-063 amendment 2026-09-12).
//
// ★ It is a TRANSPORT and nothing else. It knows how to reach a host that has
// no agent on it yet and how to run a command there without leaking a secret
// into argv; it does not know what the commands install, and it must never
// learn. That boundary is what NIM-834 bought: `core.bootstrap.delivered` also
// decided WHAT to put on a host, and installing a host has no portable form —
// which is how that module came to carry a second transport, then a full-install
// mode, then an init phase, then a second soul.yml port, each one a platform
// meeting the previous design and not fitting. The policy (fetch this binary,
// redeem the token with `soul init` behind the seed-cert guard, activate the
// unit with `daemon-reload && enable && start`) belongs to the service, as
// ordinary shell. The requirements on whoever writes that shell are recorded in
// the ADR-063 amendment, not here.
//
// What this module DOES owe the caller, because a service author cannot know to
// ask for it and forgetting any of it produces a host that never onboards,
// silently:
//
//   - a secret reaches the host through STDIN and never through argv. A command
//     argument is visible in `ps`, in `audit.log` and in journald ON THE HOST
//     ITSELF. `stdin_from:` names a field of the host entry and the module feeds
//     its value to the process — it is never formatted into the command. A `run:`
//     that nevertheless holds a secret is REFUSED before the connect, not
//     truncated in a log (see [guardSealedRunCells], [guardHostSecretsInArgv]);
//   - nothing a command printed leaves the module. The output is the roster and
//     its counts; stdout is not returned, not registered and not audited;
//   - fail-closed before the connect: the provider's `Authorize` (a deny stops
//     the run before an SSH session is opened), an ephemeral ed25519 keypair
//     whose private half never leaves the Keeper, and host-cert verification
//     against the host CA from Vault — an empty CA set is an error, never a
//     blind connect. All of it is [keeper/internal/push], reused rather than
//     restated;
//   - a host that was already onboarded when the preceding step ran carries no
//     per-host material at all (NIM-189, NIM-780) and is SKIPPED, not failed. It
//     is not dialed, and the `primary_ip` requirement of the direct transport is
//     settled AFTER the flag rather than before it — demanding a dial address
//     for the one host the step is about to skip is what used to fail the step
//     over it.
//
// B1-strict: a failure on any host fails the step, so the run goes to
// error_locked rather than committing state over a group that is only partly up.
//
// Transport modes come from `keeper.yml::push.transport`, NOT from the scenario:
// which way a Keeper installation reaches its hosts is a property of the
// installation, not of one task.
//
//   - direct (default): [push.Dial] by `primary_ip` — Authorize/Sign/ephemeral
//     keypair + CA-signed host-cert verify against the Vault host CA.
//   - teleport: by-name through the Teleport proxy (target = SID, never the IP).
//     Authorize/Sign are not called and a Vault host CA is not required —
//     transport, user-auth and host-verify all come from the Teleport identity
//     file.
//
// Both transports reach a machine that was created seconds ago, so on both the
// connect is a bounded retry against `join_wait_timeout` (NIM-872): a fresh VM
// joins Teleport minutes after creation, and on direct its sshd starts listening
// well after the DHCP lease the readiness predicate watches. What is retried
// differs, because what "not there yet" looks like differs — see [joinRetry].
package ssh

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the base module name without the state suffix (Registry key). The
// author form is `core.ssh.run`; the state arrives in
// pluginv1.ApplyRequest.state and is checked in Validate and Apply.
const Name = "core.ssh"

// StateRun is the only state this module has.
const StateRun = "run"

// Defaults for the optional connection parameters.
const (
	defaultSSHUser = "root"
	defaultSSHPort = 22
)

// Transport modes (Module.Transport, source: keeper.yml::push.transport). An
// empty string is TransportDirect.
const (
	// TransportDirect dials `primary_ip` with push.Dial (Authorize/Sign/
	// CA-signed host-cert).
	TransportDirect = "direct"
	// TransportTeleport dials by name through the Teleport proxy (target = SID),
	// with host-verify through the Teleport identity file.
	TransportTeleport = "teleport"
)

// Connect-retry parameters, on both transports: a machine created seconds ago
// is not reachable yet, so the first attempts legitimately answer "offline or
// does not exist" (teleport) or "connection refused" (direct).
const (
	// defaultJoinWaitTimeout is the default ceiling on the wait (param
	// `join_wait_timeout`). 15 min: the reverse-tunnel agent on a fresh VM shows
	// up considerably later than the typical 3-5 min, and a step failed over a
	// slow join is a step failed over a host that did come up. It is a ceiling
	// and not a wait — a host already listening pays nothing — so the direct
	// transport, where sshd arrives in tens of seconds rather than minutes,
	// shares it rather than naming a second number.
	//
	// ★ A wait ceiling that can exceed the effective run timeout is a dead
	// setting — the run aborts before the host ever arrives. Guarded by
	// TestProvisionTimeoutExceedsJoinWait against [DefaultJoinWaitTimeout].
	defaultJoinWaitTimeout = 15 * time.Minute
	// DefaultJoinWaitTimeout is the exported mirror of defaultJoinWaitTimeout, so
	// the static run-timeout guard checks the REAL default rather than a copy of
	// its value.
	DefaultJoinWaitTimeout = defaultJoinWaitTimeout
	// joinRetryBase is the base interval between connect attempts.
	joinRetryBase = 12 * time.Second
	// joinRetryJitter bounds the random jitter added to the interval
	// (anti-thundering-herd across a batch of VMs).
	joinRetryJitter = 4 * time.Second
)

// SshProviderHost is the narrow SSH authentication surface this module needs:
// Authorize (the Keeper's right to reach the host) + Sign (credentials for the
// session). It is exactly [push.SshProvider] — the same two-method contract
// SshDispatcher consumes, aliased rather than restated so the discovered
// plugin handle (`*pluginhost.SshProviderPlugin`) satisfies both without a
// shim.
type SshProviderHost = push.SshProvider

// AuditWriter writes the `ssh.run` event.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module implements sdk/module.SoulModule.
//
// What is required depends on Transport:
//   - direct (default): Providers + HostCAs + Dial. A gap is an explicit refusal
//     from Apply, never a connect with the check skipped.
//   - teleport: Dial alone; Providers/HostCAs are unused (Authorize/Sign are not
//     called and host-verify goes through the Teleport identity file).
//
// Audit is optional (nil → the write is skipped).
type Module struct {
	// Transport is the delivery mode: TransportDirect (also "") or
	// TransportTeleport. Source: keeper.yml::push.transport, assembled by the
	// daemon wire-up — NOT a scenario param.
	Transport string

	// Providers resolves the SSH provider named by `ssh_provider`, from the
	// discovered SshProvider plugins keyed by registration alias. Unused in
	// teleport mode.
	//
	// ★ An accessor, not a map: the provider plugins are spawned AFTER the core
	// modules are registered (setupPushDispatchers follows setupCoreModules), and
	// spawning a second copy here in order to hold a map earlier would double
	// every provider process. nil or empty at Apply is an explicit refusal.
	Providers func() map[string]SshProviderHost

	// HostCAs is the multi-CA set for verifying the target host's host-cert — the
	// same push.LoadHostCAs set SshDispatcher uses, read at Apply for the reason
	// [Module.Providers] is. It must resolve non-empty in direct mode: an empty
	// set is an error, never a blind connect. Unused in teleport mode.
	HostCAs func() []push.NamedHostKeyAuthority

	// Dial opens the SSH session. direct: push.Dial; teleport:
	// push.NewTeleportDialer; tests: a mock.
	Dial push.Dialer

	// RetryBase / RetryJitter override the connect backoff on both transports.
	// Zero → joinRetryBase / joinRetryJitter. They exist so a unit test does not
	// sleep the production interval; the wire-up leaves them unset.
	RetryBase   time.Duration
	RetryJitter time.Duration

	// Audit writes the `ssh.run` event. nil → skipped.
	Audit AuditWriter
}

// unknownState is the refusal shared by Validate and Apply.
func unknownState(state string) string {
	return fmt.Sprintf("unknown state %q (want %q)", state, StateRun)
}

func (m *Module) teleport() bool { return m.Transport == TransportTeleport }

func (m *Module) retryBackoff() (base, jitter time.Duration) {
	base, jitter = m.RetryBase, m.RetryJitter
	if base <= 0 {
		base = joinRetryBase
	}
	if jitter <= 0 {
		jitter = joinRetryJitter
	}
	return base, jitter
}

// Validate checks the params that can be judged offline. It deliberately does
// NOT carry the secret guards: those compare RENDERED values against the run's
// seal, and offline a `run:` cell is still the uninterpolated `${ … }` text.
// The guards live in Apply, before the first connect.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	if req.State != StateRun {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{unknownState(req.State)}}, nil
	}
	var errs []string
	if _, err := util.StringParam(req.Params, "ssh_provider"); err != nil {
		errs = append(errs, err.Error())
	}
	// hosts is a list of objects whose per-element shape is checked in Apply
	// (against the real values); offline only presence and type are knowable.
	if _, ok := req.Params.GetFields()["hosts"]; !ok {
		errs = append(errs, "param \"hosts\": missing")
	}
	if _, err := parseSteps(req.Params); err != nil {
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
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != StateRun {
		return util.SendFailed(stream, unknownState(req.State))
	}
	return m.applyRun(req, stream)
}

func (m *Module) applyRun(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	providerName, err := util.StringParam(req.Params, "ssh_provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	steps, err := parseSteps(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	hosts, err := parseHosts(req.Params, !m.teleport())
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// ★ The two secret guards, both BEFORE anything is dialed. A refusal here
	// costs a failed step; the alternative costs a token in `ps` and in journald
	// on a host we do not own, where nothing we do afterwards can retract it.
	if err := guardSealedRunCells(util.SealedPathsFrom(ctx), len(steps)); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if err := guardHostSecretsInArgv(hosts, steps); err != nil {
		return util.SendFailed(stream, err.Error())
	}

	sshUser, err := util.OptStringParam(req.Params, "ssh_user")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if sshUser == "" {
		sshUser = defaultSSHUser
	}
	sshPort, hasPort, err := util.OptIntParam(req.Params, "ssh_port")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasPort {
		sshPort = defaultSSHPort
	}
	joinWait, hasJoinWait, err := parseJoinWait(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasJoinWait {
		joinWait = defaultJoinWaitTimeout
	}

	// Configuration preconditions — an explicit refusal instead of a nil panic.
	if m.Dial == nil {
		return util.SendFailed(stream, "ssh run: dialer not configured (wire push.Dial / push.NewTeleportDialer in main)")
	}
	var (
		prov    SshProviderHost
		hostCAs []push.NamedHostKeyAuthority
	)
	if !m.teleport() {
		if m.HostCAs != nil {
			hostCAs = m.HostCAs()
		}
		if len(hostCAs) == 0 {
			return util.SendFailed(stream, "ssh run: host CAs not configured (set keeper.yml::push.host_ca_refs[] — CA-signed host-cert verify is required)")
		}
		providers := m.resolveProviders()
		p, ok := providers[providerName]
		if !ok || p == nil {
			return util.SendFailed(stream, fmt.Sprintf("ssh run: ssh_provider %q not registered (known: %v)", providerName, providerNames(providers)))
		}
		prov = p
	}

	results := make([]any, 0, len(hosts))
	sids := make([]any, 0, len(hosts))
	skipped, ran := 0, 0
	for _, h := range hosts {
		sids = append(sids, h.sid)
		if h.onboarded {
			// Already onboarded when the step that would have produced its
			// material ran (NIM-189, NIM-780): there is nothing to hand it and
			// nothing to dial for.
			skipped++
			results = append(results, map[string]any{"sid": h.sid, "ran": false, "skipped": true})
			continue
		}
		if err := m.runHost(ctx, prov, hostCAs, h, sshUser, int(sshPort), steps, joinWait); err != nil {
			// B1-strict. maskErr is the last barrier against a vault ref or a
			// token reaching status_details through the error text.
			return util.SendFailed(stream, fmt.Sprintf("ssh run on %q (%s): %s", h.sid, h.connectTarget(m.teleport()), maskErr(err)))
		}
		ran++
		results = append(results, map[string]any{"sid": h.sid, "ran": true, "skipped": false})
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventSSHRun,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":       StateRun,
				"ssh_provider": providerName,
				"transport":    m.transportName(),
				"count":        float64(len(hosts)),
				"skipped":      float64(skipped),
				"steps":        float64(len(steps)),
				"sids":         sids,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, "ssh run: audit write: "+maskErr(werr))
		}
	}

	// ★ NO command output. The register carries the roster and its counts and
	// nothing a command printed: a step whose job is to write a secret can echo
	// it, and this is the one channel the module controls.
	return util.SendFinal(stream, ran > 0, map[string]any{
		"hosts":   results,
		"count":   float64(len(hosts)),
		"skipped": float64(skipped),
	})
}

// runHost opens one session and executes every step on it, in order. A non-zero
// exit from any step fails the host, and with it the step (B1-strict).
func (m *Module) runHost(ctx context.Context, prov SshProviderHost, hostCAs []push.NamedHostKeyAuthority, h hostInput, user string, port int, steps []step, joinWait time.Duration) error {
	var (
		sess push.Session
		err  error
	)
	if m.teleport() {
		sess, err = m.dialTeleport(ctx, h, user, port, joinWait)
	} else {
		sess, err = m.dialDirect(ctx, prov, hostCAs, h, user, port, joinWait)
	}
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	for i, s := range steps {
		stdin, serr := s.stdinFor(h)
		if serr != nil {
			return fmt.Errorf("step %d: %w", i+1, serr)
		}
		// ★ The value goes to the process's stdin. It is never formatted into
		// cmd — that is the whole point of the module, and it is constructive
		// here rather than checked.
		if _, rerr := sess.Run(ctx, s.run, stdin); rerr != nil {
			return fmt.Errorf("step %d: %s", i+1, withoutCommand(rerr, s.run))
		}
	}
	return nil
}

// dialDirect: Authorize → ephemeral keypair + Sign → push.Dial by primary_ip
// with CA-signed host-cert verification. The same flow as
// SshDispatcher.SendApply, reused whole.
//
// ★ Only the last of those is inside the join retry (NIM-872). Authorize is
// policy and Sign is a credential mint: repeating either cannot change its
// answer, and repeating a deny for fifteen minutes would turn a refusal into a
// hang. The credential is therefore minted once and must outlive the wait — a
// provider issuing certificates shorter-lived than `join_wait_timeout` would
// hand back one that expires mid-wait, which is a provider-side TTL question and
// not something this module can detect.
func (m *Module) dialDirect(ctx context.Context, prov SshProviderHost, hostCAs []push.NamedHostKeyAuthority, h hostInput, user string, port int, joinWait time.Duration) (push.Session, error) {
	// fail-closed: a deny stops us before an SSH session is opened.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: h.primaryIP, User: user})
	if err != nil {
		return nil, fmt.Errorf("authorize %s@%s: %w", user, h.primaryIP, err)
	}
	if !authReply.GetAllowed() {
		return nil, fmt.Errorf("authorize denied for %s@%s: %s", user, h.primaryIP, authReply.GetReason())
	}

	// One ed25519 pair per host. The public half goes into SignRequest; the
	// private half exists only inside the returned signer and never leaves.
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

	sess, err := m.dialWithJoinRetry(ctx, push.DialConfig{
		Host:            h.primaryIP,
		Port:            port,
		User:            user,
		Auth:            auth,
		HostAuthorities: hostCAs,
		ProxyJump:       signReply.GetProxyJump(),
	}, joinWait, directJoinRetry)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// dialTeleport: by-name connect through the Teleport dialer, wrapped in the same
// bounded retry. Auth/HostAuthorities/ProxyJump are deliberately absent — in
// this mode they come from the identity file, and Authorize/Sign are not called
// at all.
func (m *Module) dialTeleport(ctx context.Context, h hostInput, user string, port int, joinWait time.Duration) (push.Session, error) {
	sess, err := m.dialWithJoinRetry(ctx, push.DialConfig{
		Host: h.sid, // ★ node name = SID, never primary_ip
		Port: port,
		User: user,
	}, joinWait, teleportJoinRetry)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// joinRetry is what the two transports disagree about inside one retry loop:
// which Dial failure a wait can still fix, and what to call the host that never
// arrived.
type joinRetry struct {
	// retryable decides whether to spend more of the budget on this error. False
	// ends the wait immediately and returns the error unwrapped by the deadline
	// text — the caller is not waiting for anything.
	retryable func(error) bool
	// unreached opens the deadline message: "<unreached> within
	// join_wait_timeout (…)".
	unreached string
}

var (
	// teleportJoinRetry retries every Dial error. In this mode the proxy answers
	// for a node that has not enrolled yet with an ordinary error string ("node
	// offline or does not exist") that carries no shape a classifier could read,
	// and the transport, user-auth and host-verify a direct dial could fail on
	// separately all happen behind that same one call.
	teleportJoinRetry = joinRetry{
		retryable: func(error) bool { return true },
		unreached: "node not reachable via Teleport",
	}
	// directJoinRetry retries only a failure to establish the TCP connection.
	directJoinRetry = joinRetry{
		retryable: isConnectFailure,
		unreached: "host not reachable",
	}
)

// isConnectFailure reports whether err is a failure to ESTABLISH the connection
// to the host — connection refused, host or network unreachable, a dial timeout.
//
// ★ The layer boundary is the point (NIM-872). Everything a booting VM produces
// is below it: sshd is not listening yet. Everything above it is a
// misconfiguration that waiting cannot fix — a host cert not signed by our CA or
// a public key the host rejects arrives as an SSH handshake error, and spending
// the whole join budget on one would turn a clear refusal into a fifteen-minute
// hang.
//
// It takes two shapes because the direct transport has two sub-paths, and the
// bastion one is not optional: an SshProvider returning `proxy_jump` on its
// SignReply sends [push.Dial] through `dialViaProxy` (that is what the Teleport
// provider does), and there the target connect is the PROXY's direct-tcpip
// channel rather than our own socket. The same absent sshd then arrives as the
// proxy refusing to open the channel. `ssh.Prohibited` — the bastion declining
// on policy — is deliberately not included; it is a deny, like Authorize's.
func isConnectFailure(err error) bool {
	// Our own socket: net.Dialer reports every connect failure as Op == "dial",
	// and push.Dial wraps it with %w, so the shape survives to here.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	var chErr *ssh.OpenChannelError
	return errors.As(err, &chErr) && chErr.Reason == ssh.ConnectionFailed
}

// dialWithJoinRetry repeats Dial with a fixed backoff plus jitter until a
// session opens, joinWait expires, or r rejects the error as one no wait can
// fix. The first attempt is immediate: a host that is already listening must not
// pay an interval.
func (m *Module) dialWithJoinRetry(ctx context.Context, cfg push.DialConfig, joinWait time.Duration, r joinRetry) (push.Session, error) {
	base, jitter := m.retryBackoff()
	deadline := time.Now().Add(joinWait)
	var lastErr error
	for attempt := 0; ; attempt++ {
		sess, err := m.Dial(ctx, cfg)
		if err == nil {
			return sess, nil
		}
		lastErr = err

		if !r.retryable(err) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		}
		wait := base + time.Duration(rand.Int63n(int64(jitter)+1))
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("%s within join_wait_timeout (%s, %d attempt(s)): %w", r.unreached, joinWait, attempt+1, lastErr)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		case <-t.C:
		}
	}
}

func (m *Module) transportName() string {
	if m.teleport() {
		return TransportTeleport
	}
	return TransportDirect
}

func (m *Module) resolveProviders() map[string]SshProviderHost {
	if m.Providers == nil {
		return nil
	}
	return m.Providers()
}

func providerNames(providers map[string]SshProviderHost) []string {
	out := make([]string, 0, len(providers))
	for k := range providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// step is one entry of `steps`: a shell command line plus, optionally, exactly
// one source for the process's stdin.
type step struct {
	run string
	// stdin is a literal value, the same on every host — a `${ vault(…) }` or a
	// `${ vars.* }` cell the render already resolved.
	stdin    string
	hasStdin bool
	// stdinFrom names a FIELD of the host entry. It exists because a keeper-side
	// task renders once for the whole run, not once per host: `${ host.* }` has
	// no CEL root to resolve against, and `loop:` is not supported on
	// `on: keeper` at all. Naming the field instead keeps the per-host secret out
	// of the task's params entirely.
	stdinFrom string
}

// stdinFor resolves this step's stdin for one host. A step with neither source
// gets nil (the process sees an empty stdin).
func (s step) stdinFor(h hostInput) ([]byte, error) {
	switch {
	case s.stdinFrom != "":
		v, ok := h.fields[s.stdinFrom]
		if !ok {
			return nil, fmt.Errorf("stdin_from %q: host %q carries no such string field", s.stdinFrom, h.sid)
		}
		if v == "" {
			return nil, fmt.Errorf("stdin_from %q: host %q carries it empty", s.stdinFrom, h.sid)
		}
		return []byte(v), nil
	case s.hasStdin:
		return []byte(s.stdin), nil
	default:
		return nil, nil
	}
}

// hostInput is one entry of `hosts` — in practice `${ register.<mint>.hosts }`.
//
// fields holds the entry's TOP-LEVEL string values, which is what `stdin_from:`
// addresses and what [guardHostSecretsInArgv] scans the commands for. Two
// exclusions, and both bound the guard as well as the accessor: a non-string
// value is dropped because a step feeds bytes to a process and a number or a
// bool has no unambiguous rendering to feed, and a NESTED value is dropped
// because `stdin_from:` is a field name rather than a path — so a secret buried
// at `hosts[].auth.password` is addressable by neither, and quoting it in `run:`
// is caught by the seal or not at all.
type hostInput struct {
	sid       string
	primaryIP string
	onboarded bool
	fields    map[string]string
}

// connectTarget is the address this host was reached at, for diagnostics: the
// IP on direct, the node name on teleport.
func (h hostInput) connectTarget(teleport bool) string {
	if teleport {
		return h.sid
	}
	return h.primaryIP
}

func parseHosts(params *structpb.Struct, requirePrimaryIP bool) ([]hostInput, error) {
	lv, err := util.ListParam(params, "hosts")
	if err != nil {
		return nil, err
	}
	if len(lv) == 0 {
		return nil, fmt.Errorf("param %q: empty list (no hosts to run on)", "hosts")
	}
	out := make([]hostInput, 0, len(lv))
	for i, item := range lv {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected object, got %T", "hosts", i, item.Kind)
		}
		h, herr := hostFromStruct(sv.StructValue, i, requirePrimaryIP)
		if herr != nil {
			return nil, herr
		}
		out = append(out, h)
	}
	return out, nil
}

func hostFromStruct(s *structpb.Struct, idx int, requirePrimaryIP bool) (hostInput, error) {
	sid, err := util.StringParam(s, "sid")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	ip, err := util.OptStringParam(s, "primary_ip")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	onboarded, _, err := util.OptBoolParam(s, "onboarded")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	fields := make(map[string]string, len(s.GetFields()))
	for k, v := range s.GetFields() {
		if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
			fields[k] = sv.StringValue
		}
	}
	// ★ Settled BEFORE the primary_ip requirement, not after it. That requirement
	// exists in order to dial, and a converged host is never dialed; issuance
	// emits `{sid, onboarded: true}` and nothing else (NIM-780), so asking it for
	// a dial address would fail the step over the one host it was about to skip.
	//
	// `fields` is filled either way, though nothing will read it for stdin: a
	// converged entry from an older producer may still carry material, and
	// [guardHostSecretsInArgv] must see every host's, not only the dialed ones —
	// one command line runs on ALL of them.
	if onboarded {
		return hostInput{sid: sid, primaryIP: ip, onboarded: true, fields: fields}, nil
	}
	if requirePrimaryIP && ip == "" {
		return hostInput{}, fmt.Errorf("param %q[%d].primary_ip: missing (required for the direct transport)", "hosts", idx)
	}
	return hostInput{sid: sid, primaryIP: ip, fields: fields}, nil
}

func parseSteps(params *structpb.Struct) ([]step, error) {
	lv, err := util.ListParam(params, "steps")
	if err != nil {
		return nil, err
	}
	if len(lv) == 0 {
		return nil, fmt.Errorf("param %q: empty list (nothing to run)", "steps")
	}
	out := make([]step, 0, len(lv))
	for i, item := range lv {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected object, got %T", "steps", i, item.Kind)
		}
		s := sv.StructValue
		run, rerr := util.StringParam(s, "run")
		if rerr != nil {
			return nil, fmt.Errorf("param %q[%d].%w", "steps", i, rerr)
		}
		if strings.TrimSpace(run) == "" {
			return nil, fmt.Errorf("param %q[%d].run: empty", "steps", i)
		}
		stdin, serr := util.OptStringParam(s, "stdin")
		if serr != nil {
			return nil, fmt.Errorf("param %q[%d].%w", "steps", i, serr)
		}
		_, hasStdin := s.GetFields()["stdin"]
		stdinFrom, ferr := util.OptStringParam(s, "stdin_from")
		if ferr != nil {
			return nil, fmt.Errorf("param %q[%d].%w", "steps", i, ferr)
		}
		if hasStdin && stdinFrom != "" {
			return nil, fmt.Errorf("param %q[%d]: stdin and stdin_from are two sources for one stdin — declare exactly one", "steps", i)
		}
		out = append(out, step{run: run, stdin: stdin, hasStdin: hasStdin, stdinFrom: stdinFrom})
	}
	return out, nil
}

// parseJoinWait reads the optional `join_wait_timeout` in either form: the Soul
// Stack duration-string convention (`"15m"`/`"90s"`), symmetric with
// `await_timeout` on core.soul.registered, or a plain number of seconds.
// ok=false means the param is absent and the caller substitutes the default.
// Zero is valid — an immediate deadline, i.e. one attempt with no wait.
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
		return 0, false, fmt.Errorf("param %q: expected a duration string or integer seconds, got %T", key, v.Kind)
	}
}

// guardSealedRunCells refuses a step list whose command line was rendered from a
// secret source. The seal is the run's render-time record of which params cells
// read one ([ADR-010] §7.4), threaded in on the module context by
// keeper_dispatch.
//
// ★ A refusal, not a redaction. A command argument is visible in `ps`, in
// `audit.log` and in journald ON THE HOST — masking the copy that comes back to
// the operator would hide the leak rather than prevent it, and by then the
// command has run. `stdin:`/`stdin_from:` is the way to pass a secret, and the
// message says so, because an author who hit this guard is one edit away from
// the correct form.
//
// The spellings that cover a `run:` cell are render's own (joinKey/joinIdx over
// the params root): the whole `steps` list, one whole step object, or the `run`
// cell itself. `steps[N].stdin` and `steps[N].stdin_from` deliberately do NOT
// count — stdin is the channel a secret is SUPPOSED to travel on, and a guard
// that refused it would leave no way to pass one at all.
//
// **The set is RUN-wide and carries no task index**, so it is walked against
// THIS task's own step count rather than scanned for anything `steps`-shaped —
// the same way [render.SealedValues] matches a run-wide set against one task's
// params. That bounds the blast radius to tasks with this shape; two
// `core.ssh.run` tasks in one run remain indistinguishable, and the seal has no
// index that could tell them apart. The over-refusal that leaves is safe in
// direction: if any of them sealed a `run:` cell, one of them is genuinely
// wrong and the run fails either way — only the index in the message may name
// the wrong sibling.
//
// Bound, stated because it is the reason [guardHostSecretsInArgv] exists beside
// it: the seal only knows what the RENDER knew. A value with no render-time
// provenance is not in it.
func guardSealedRunCells(sealed map[string]bool, steps int) error {
	if len(sealed) == 0 {
		return nil
	}
	hits := make([]string, 0, 1)
	if sealed["steps"] {
		hits = append(hits, "steps")
	}
	for i := 0; i < steps; i++ {
		for _, path := range []string{fmt.Sprintf("steps[%d]", i), fmt.Sprintf("steps[%d].run", i)} {
			if sealed[path] {
				hits = append(hits, path)
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Strings(hits) // an observable message must be identical for identical input
	return fmt.Errorf("ssh run: %s holds a declared secret and `run:` is a command line — argv is visible in ps, audit.log and journald on the host itself; pass it with `stdin:` or `stdin_from:` instead", strings.Join(hits, ", "))
}

// guardHostSecretsInArgv refuses a command line that quotes a secret-named field
// of one of the hosts — the token pulled out of the minting register and
// interpolated into `run:` by hand.
//
// It exists BESIDE the seal guard rather than inside it because it answers the
// case the seal cannot: `register.<mint>.hosts` is not a sealed source, so a
// cell reading it is not sealed, and the path guard sees nothing. Comparing
// values is what works there.
//
// It compares only fields whose NAME the platform already treats as sensitive
// ([audit.IsSensitiveKey]) — deliberately not every field. A sid or a hostname
// is a plausible substring of an ordinary command (`systemctl start redis`), and
// a guard that failed the whole run over that would be worse than the leak it
// prevents: B1-strict means one false positive costs the run.
//
// The message names the host, the step and the field, and never the value.
func guardHostSecretsInArgv(hosts []hostInput, steps []step) error {
	for _, h := range hosts {
		names := make([]string, 0, len(h.fields))
		for k := range h.fields {
			if h.fields[k] != "" && audit.IsSensitiveKey(k) {
				names = append(names, k)
			}
		}
		sort.Strings(names) // deterministic: two offending fields must report the same one every time
		for _, k := range names {
			for i, s := range steps {
				if strings.Contains(s.run, h.fields[k]) {
					return fmt.Errorf("ssh run: steps[%d].run carries the value of %q for host %q — argv is visible in ps, audit.log and journald on the host itself; pass it with `stdin_from: %s` instead", i, k, h.sid, k)
				}
			}
		}
	}
	return nil
}

// withoutCommand replaces the command line inside an error text with the step's
// address. `push.Session.Run` quotes the command when it cannot START it
// ("push: running %q: …"), and that text reaches `status_details` through the
// failed event — the one channel where the module otherwise never repeats a
// command.
//
// It matters because the command is the SITE's text, not the engine's: the two
// guards refuse a secret the render sealed or a secret-named host field, and
// neither can see a plaintext literal or an `${ incarnation.state.<f> }` read,
// which is unsealed while NIM-826 is open. The step index is enough to find the
// line in the scenario, and the underlying reason is preserved.
// ★ Both spellings are replaced, and the quoted one FIRST. `push` formats the
// command with %q, so a command carrying a quote, a backslash or a newline —
// `SOUL_BOOTSTRAP_TOKEN="$(cat …)"` is the canonical install step — appears in
// the text escaped and matches the raw string nowhere.
func withoutCommand(err error, cmd string) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if cmd == "" {
		return maskErrText(text)
	}
	text = strings.ReplaceAll(text, strconv.Quote(cmd), `"<run>"`)
	text = strings.ReplaceAll(text, cmd, "<run>")
	return maskErrText(text)
}

// maskErr masks a possible secret leak (a vault ref) in an error text before it
// is returned in a failed event, which reaches status_details. The same
// substring filter shared/audit applies to register output; it works on the
// VALUE's shape, so it catches a vault ref and not a resolved credential —
// redacting one of those needs the run's seal, which
// scenario.maskKeeperTaskMessage applies on the way out.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	return maskErrText(err.Error())
}

// maskErrText is maskErr over a text that has already been assembled. The key
// `_` is non-secret, so only the value filter applies.
func maskErrText(text string) string {
	masked := audit.MaskSecrets(map[string]any{"_": text})
	if s, ok := masked["_"].(string); ok {
		return s
	}
	return "***MASKED***"
}
