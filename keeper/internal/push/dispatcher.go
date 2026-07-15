package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/protojson"
)

// SSHTarget — SSH connection details for a push host: host (= SID/FQDN), port,
// user, and the path to an already-installed soul binary.
//
// In the pilot, the soul binary is assumed to already be on the host at a
// known path (SHA-256 delivery/sync cache is the S1 slice), so SoulPath comes
// from the resolve step rather than being computed.
type SSHTarget struct {
	Host     string
	Port     int
	User     string
	SoulPath string
}

// TargetResolver resolves SSH connection details by SID. Injected as a
// dependency: the pilot plugs in a config-backed resolver, S7-1 plugs in
// PGFallbackTargetResolver.
type TargetResolver interface {
	Resolve(ctx context.Context, sid string) (SSHTarget, error)
}

// SoulLookup reads a Soul by SID — the dispatcher only needs it to check the
// transport=ssh precondition (input validation). Narrowed to a single method
// so the dispatcher can be mocked without PG. Implemented by a wrapper over
// [soul.SelectBySID].
type SoulLookup interface {
	SelectBySID(ctx context.Context, sid string) (*soul.Soul, error)
}

// Dialer opens an SSH session per DialConfig. Production uses [Dial]; tests
// use a mock function returning a fake [Session]. A function type (rather
// than an interface) keeps the wire-up trivial.
type Dialer func(ctx context.Context, cfg DialConfig) (Session, error)

// ProviderRespawner — a narrow surface for runtime re-spawn of an SshProvider
// plugin handle with updated env-payload params. Implemented by the wire-up
// in the daemon (which holds pluginhost.Host + discovered +
// PGFallbackProviderResolver).
//
// Contract: given a plugin name, the implementation resolves fresh params
// from PG/legacy-fallback, spawns a new plugin handle with an updated
// SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS env payload, and returns a pair
// (SshProvider, io.Closer) — the Closer closes the spawned plugin process
// (called by the dispatcher on the next RefreshProvider, or from its own
// Close path).
type ProviderRespawner interface {
	// RespawnProvider closes the current plugin handle (if oldCloser is
	// non-nil) and spawns a new one with updated params.
	//
	// Reentrancy invariant: the caller (SshDispatcher.RefreshProvider) holds the
	// mutex, so there won't be concurrent Respawns for the same name.
	RespawnProvider(ctx context.Context, providerName string, oldCloser io.Closer) (SshProvider, io.Closer, error)
}

// ProviderEntry — one registered SshProvider plugin in the dispatcher's map.
// Closer closes the spawned plugin process (typically
// *pluginhost.SshProviderPlugin); when swapped via RefreshProvider, the old
// Closer is closed by the respawner and the new one takes its place.
//
// A nil Closer is fine: unit tests plug in a mock Provider without spawning a
// child process.
type ProviderEntry struct {
	Provider SshProvider
	Closer   io.Closer
}

// Deps — dependencies for [SshDispatcher].
//
// Multi-provider routing (ADR-032 amendment 2026-05-27, P2 W-2): the
// dispatcher keeps a map of providers by name, `Providers
// map[string]ProviderEntry`. SendApply/Cleanup receive `providerName` from
// the caller (pushorch.PushRun resolves it via ProviderRouter) and look it up
// in the map under RLock.
type Deps struct {
	// Providers — a map of registered SshProvider plugins by name
	// (manifest.Name). An empty map is a programmer error: NewSshDispatcher
	// fails construction. Swapped at runtime via [SshDispatcher.
	// RefreshProvider] under d.mu (an atomic swap of one entry with no effect
	// on the others).
	Providers map[string]ProviderEntry
	// Respawner — runtime re-spawn of a new plugin-handle version with an
	// updated env payload. nil → [SshDispatcher.RefreshProvider] returns
	// ErrRespawnNotSupported.
	Respawner ProviderRespawner
	// Targets — resolves SSH connection details by SID.
	Targets TargetResolver
	// Souls — checks the transport=ssh precondition.
	Souls SoulLookup
	// HostAuthorities — the multi-CA set for verifying host certificates (S7-3,
	// ADR-032 amendment 2026-05-26). Must be non-empty; the handshake does an
	// OR-check across all elements via ssh.CertChecker.IsHostAuthority.
	HostAuthorities []NamedHostKeyAuthority
	// Metrics — optional multi-CA observability (a counter of matches by
	// `ca_name`). nil is a no-op (unit tests without obs.Registry / push
	// disabled).
	Metrics *Metrics
	// Dial — opens the SSH session. nil → [Dial] (production).
	Dial Dialer
	// Logger is required.
	Logger *slog.Logger
	// DialTimeout — the connect+handshake timeout. 0 → defaultDialTimeout.
	DialTimeout time.Duration
	// Deliverer — delivers the soul binary and registered modules with
	// SHA-256 dedup BEFORE the `soul apply` exec. nil → delivery is skipped
	// (BC with S0: in the pilot the binary is already on the host at SoulPath).
	Deliverer Deliverer
	// SoulSpec — what to deliver (see [SoulSpec]). Ignored when Deliverer=nil.
	SoulSpec SoulSpec
	// Cleaner — host-side artifact cleanup (`rm -rf /var/lib/soul-stack/{bin,
	// modules}/`). Used by the [SshDispatcher.Cleanup] method; SendApply
	// doesn't call it (see the Cleaner doc).
	Cleaner Cleaner
}

const defaultDialTimeout = 30 * time.Second

// SshDispatcher — the push implementation of the apply dispatcher (ADR-004,
// agentless SSH). The [SshDispatcher.SendApply] method mirrors the
// signature/semantics of pull-Outbound so it can later become an alt
// implementation of it (branching by transport at the Outbound.SendApply
// call site). Difference: push is a synchronous oneshot — it returns
// *RunResult right away (no asynchronous EventStream barrier).
//
// Multi-provider (P2 W-2): the dispatcher keeps a `Deps.Providers` map and
// looks up the provider by the name passed to SendApply/Cleanup. The routing
// logic (per-SID → coven-default → cluster-default) is out of scope for the
// dispatcher, see [ProviderRouter].
type SshDispatcher struct {
	// mu protects deps.Providers during runtime re-spawn (RefreshProvider).
	// SendApply/Cleanup on the hot path take an RLock-style snapshot reference
	// to a specific ProviderEntry and run through to the end of the session
	// without blocking. We use sync.RWMutex.
	mu   sync.RWMutex
	deps Deps
}

// NewSshDispatcher assembles the dispatcher. Providers / Targets / Souls /
// Logger are required; `HostAuthorities` must be non-empty (no CA means no
// trusted host-cert verification). Every HostAuthorities element must have a
// non-empty `Name` and a non-empty `CAPubKey` — that's the caller's
// invariant to uphold. Dial nil → production [Dial].
//
// The Providers map must be non-empty and every entry must have a non-nil
// Provider (a nil Closer is fine — unit tests).
func NewSshDispatcher(deps Deps) (*SshDispatcher, error) {
	if len(deps.Providers) == 0 {
		return nil, errors.New("push: Deps.Providers must be non-empty (multi-provider map)")
	}
	for name, entry := range deps.Providers {
		if entry.Provider == nil {
			return nil, fmt.Errorf("push: Providers[%q].Provider is nil", name)
		}
	}
	if deps.Targets == nil {
		return nil, errors.New("push: TargetResolver обязателен")
	}
	if deps.Souls == nil {
		return nil, errors.New("push: SoulLookup обязателен")
	}
	if deps.Logger == nil {
		return nil, errors.New("push: logger обязателен")
	}
	if len(deps.HostAuthorities) == 0 {
		return nil, errors.New("push: HostAuthorities обязателен непустым (CA-signed host-cert verification)")
	}
	for i, ha := range deps.HostAuthorities {
		if ha.Name == "" {
			return nil, fmt.Errorf("push: HostAuthorities[%d].Name пуст", i)
		}
		if ha.CAPubKey == nil {
			return nil, fmt.Errorf("push: HostAuthorities[%d].CAPubKey nil (CA %q)", i, ha.Name)
		}
	}
	if deps.Dial == nil {
		deps.Dial = Dial
	}
	if deps.DialTimeout == 0 {
		deps.DialTimeout = defaultDialTimeout
	}
	return &SshDispatcher{deps: deps}, nil
}

// providerEntry returns the registered ProviderEntry by name under RLock.
// SendApply/Cleanup on the hot path take a snapshot of one entry and hold it
// through the end of the run. RefreshProvider swaps the entry atomically
// under Lock — concurrent SendApply calls for other names aren't blocked.
//
// Returns (ProviderEntry{}, false) if the name isn't registered — the caller
// (SendApply/Cleanup) maps this to ErrProviderUnknown.
func (d *SshDispatcher) providerEntry(name string) (ProviderEntry, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.deps.Providers[name]
	return e, ok
}

// HasProvider — operator diagnostics: "is an SshProvider with this name
// registered". Used by tests and the invalidation listener to avoid calling
// RefreshProvider on names that aren't theirs.
func (d *SshDispatcher) HasProvider(name string) bool {
	_, ok := d.providerEntry(name)
	return ok
}

// ProviderNames — a snapshot of registered provider names (diagnostics,
// logs). Order is nondeterministic.
func (d *SshDispatcher) ProviderNames() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.deps.Providers))
	for name := range d.deps.Providers {
		out = append(out, name)
	}
	return out
}

// RefreshProvider — runtime re-spawn of an SshProvider plugin handle with
// updated env-payload params (S7-2 hot-reload, ADR-032 amendment 2026-05-26;
// extended by amendment 2026-05-27 P2 W-2 for the multi-provider map).
//
// Algorithm:
//
//  1. Empty name → mass invalidation: re-spawn ALL registered providers
//     (sequentially, under one shared Lock). Used when the mutation's origin
//     is unknown.
//  2. Name given, missing from the map → no-op without an error (not our
//     provider, the pub/sub message came from another cluster / another
//     node with a different plugin catalog).
//  3. Name given, present in the map → under Lock calls
//     respawner.RespawnProvider: it closes the old handle and spawns a new
//     one. On success it swaps the entry; on error it clears it (degraded
//     state — a subsequent SendApply for this provider will return
//     ErrProviderUnknown / nil-provider).
//
// Returns ErrRespawnNotSupported if Respawner isn't configured.
func (d *SshDispatcher) RefreshProvider(ctx context.Context, providerName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.deps.Respawner == nil {
		return ErrRespawnNotSupported
	}

	if providerName == "" {
		// Mass invalidation: iterate over all names. An error on one provider
		// doesn't stop the rest — each goes independently.
		var firstErr error
		for name := range d.deps.Providers {
			if err := d.respawnOneLocked(ctx, name); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	// Not our provider — no-op (multi-provider routing: other SshDispatchers
	// don't exist in the current layout, but let's guard against foreign
	// pub/sub messages anyway).
	if _, ok := d.deps.Providers[providerName]; !ok {
		return nil
	}
	return d.respawnOneLocked(ctx, providerName)
}

// respawnOneLocked — re-spawns one map entry. The caller must hold d.mu.Lock
// (not RLock).
func (d *SshDispatcher) respawnOneLocked(ctx context.Context, name string) error {
	old := d.deps.Providers[name]
	newProv, newCloser, err := d.deps.Respawner.RespawnProvider(ctx, name, old.Closer)
	if err != nil {
		// degraded state: remove the entry so a subsequent SendApply returns
		// ErrProviderUnknown with a clear message (instead of a nil-deref). The
		// respawner has already closed the old handle (a documented contract).
		delete(d.deps.Providers, name)
		return fmt.Errorf("push: re-spawn provider %q: %w", name, err)
	}
	d.deps.Providers[name] = ProviderEntry{Provider: newProv, Closer: newCloser}
	d.deps.Logger.Info("push: plugin re-spawned with fresh params",
		slog.String("provider", name))
	return nil
}

// ErrRespawnNotSupported — sentinel error for [SshDispatcher.RefreshProvider]:
// the dispatcher was built without a ProviderRespawner (single-instance dev /
// unit tests). The caller (daemon listener) treats it as "nothing to update"
// and carries on.
var ErrRespawnNotSupported = errors.New("push: ProviderRespawner not configured")

// ErrProviderUnknown — sentinel error for SendApply/Cleanup: providerName
// isn't registered in the map (a routing miss, or a previous RefreshProvider
// pushed the entry into degraded state due to a spawn failure).
//
// On this error, pushorch.PushRun marks the per-host status="error" with
// error_code="ssh_provider_unavailable: <name>".
var ErrProviderUnknown = errors.New("push: SshProvider not registered")

// SendApply executes a run on a push host over SSH synchronously: lookup the
// provider by name → resolve the target → check transport=ssh → ephemeral
// keypair → Authorize → Sign(pubkey) → connect (CA-host-cert verify) →
// `soul apply` with stdin=ApplyRequest → parse NDJSON stdout → RunResult.
//
// providerName — the SshProvider plugin name (resolved by
// pushorch.ProviderRouter before the call). An empty string or an
// unregistered name → ErrProviderUnknown.
//
// Returns:
//   - (*RunResult, nil) — the run reached a RunResult (its status can be
//     FAILED — that's a valid outcome, not a transport error).
//   - (nil, error) — failure BEFORE a RunResult: ErrProviderUnknown, an
//     Authorize deny, a connect/Sign failure, a cut-off before RunResult, a
//     malformed NDJSON.
func (d *SshDispatcher) SendApply(ctx context.Context, sid string, providerName string, req *keeperv1.ApplyRequest) (*keeperv1.RunResult, error) {
	if req == nil {
		return nil, errors.New("push: ApplyRequest is nil")
	}
	if providerName == "" {
		return nil, fmt.Errorf("push: providerName is empty for sid=%s", sid)
	}
	entry, ok := d.providerEntry(providerName)
	if !ok || entry.Provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderUnknown, providerName)
	}

	log := d.deps.Logger.With(
		slog.String("sid", sid),
		slog.String("apply_id", req.GetApplyId()),
		slog.String("ssh_provider", providerName),
	)

	// Precondition: the dispatcher only serves transport=ssh.
	s, err := d.deps.Souls.SelectBySID(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("push: резолв soul %s: %w", sid, err)
	}
	if s.Transport != soul.TransportSSH {
		return nil, fmt.Errorf("push: soul %s имеет transport=%q, ожидался ssh", sid, s.Transport)
	}

	target, err := d.deps.Targets.Resolve(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("push: резолв ssh-target %s: %w", sid, err)
	}

	prov := entry.Provider

	// Authorize — fail-closed: a deny stops the run before connect.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{
		Host: target.Host,
		User: target.User,
	})
	if err != nil {
		return nil, fmt.Errorf("push: Authorize %s@%s: %w", target.User, target.Host, err)
	}
	if !authReply.GetAllowed() {
		return nil, fmt.Errorf("push: Authorize отказал для %s@%s: %s", target.User, target.Host, authReply.GetReason())
	}

	// Ephemeral keypair: a Keeper-side ed25519 pair per session. The pubkey
	// goes out in SignRequest for CA providers. The private key NEVER leaves
	// Keeper.
	ephSigner, ephPubAuthorized, err := newEphemeralEd25519()
	if err != nil {
		return nil, fmt.Errorf("push: генерация ephemeral keypair %s: %w", sid, err)
	}

	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{
		Host:      target.Host,
		User:      target.User,
		PublicKey: ephPubAuthorized,
	})
	if err != nil {
		return nil, fmt.Errorf("push: Sign %s@%s: %w", target.User, target.Host, err)
	}
	auth, err := authMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return nil, fmt.Errorf("push: подготовка SSH-auth %s: %w", sid, err)
	}

	sess, err := d.deps.Dial(ctx, DialConfig{
		Host:            target.Host,
		Port:            target.Port,
		User:            target.User,
		Auth:            auth,
		HostAuthorities: d.deps.HostAuthorities,
		OnHostCAMatch:   d.onHostCAMatch,
		ProxyJump:       signReply.GetProxyJump(),
		Timeout:         d.deps.DialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("push: connect %s: %w", sid, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Warn("push: закрытие SSH-сессии с ошибкой", slog.Any("error", cerr))
		}
	}()

	if d.deps.Deliverer != nil {
		if err := d.deps.Deliverer.Deliver(ctx, sess, d.deps.SoulSpec); err != nil {
			return nil, fmt.Errorf("push: доставка артефактов %s: %w", sid, err)
		}
	}

	stdin, err := protojson.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("push: marshal ApplyRequest %s: %w", sid, err)
	}

	cmd := soulApplyCommand(target.SoulPath)
	stdout, runErr := sess.Run(ctx, cmd, stdin)

	rr, parseErr := ParseStream(strings.NewReader(stdout), func(ev *keeperv1.TaskEvent) {
		log.Debug("push: TaskEvent",
			slog.Int("task_idx", int(ev.GetTaskIdx())),
			slog.String("status", ev.GetStatus().String()))
	})
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("push: прогон %s без RunResult (exit: %v): %w", sid, runErr, parseErr)
		}
		return nil, fmt.Errorf("push: прогон %s: %w", sid, parseErr)
	}

	log.Info("push: прогон завершён", slog.String("status", rr.GetStatus().String()))
	return rr, nil
}

// Cleanup opens an SSH session to host sid and removes soul artifacts
// (`/var/lib/soul-stack/{bin,modules}/`) via [Cleaner].
//
// providerName — the same SshProvider plugin name used in the preceding
// SendApply (the caller keeps the correspondence in push_runs.summary).
func (d *SshDispatcher) Cleanup(ctx context.Context, sid string, providerName string) error {
	if d.deps.Cleaner == nil {
		return errors.New("push: Cleaner не сконфигурирован")
	}
	if providerName == "" {
		return fmt.Errorf("push: providerName is empty for sid=%s (cleanup)", sid)
	}
	entry, ok := d.providerEntry(providerName)
	if !ok || entry.Provider == nil {
		return fmt.Errorf("%w: %s (cleanup)", ErrProviderUnknown, providerName)
	}

	log := d.deps.Logger.With(
		slog.String("sid", sid),
		slog.String("op", "cleanup"),
		slog.String("ssh_provider", providerName),
	)

	s, err := d.deps.Souls.SelectBySID(ctx, sid)
	if err != nil {
		return fmt.Errorf("push: резолв soul %s: %w", sid, err)
	}
	if s.Transport != soul.TransportSSH {
		return fmt.Errorf("push: soul %s имеет transport=%q, ожидался ssh", sid, s.Transport)
	}

	target, err := d.deps.Targets.Resolve(ctx, sid)
	if err != nil {
		return fmt.Errorf("push: резолв ssh-target %s: %w", sid, err)
	}

	prov := entry.Provider

	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{
		Host: target.Host,
		User: target.User,
	})
	if err != nil {
		return fmt.Errorf("push: Authorize %s@%s: %w", target.User, target.Host, err)
	}
	if !authReply.GetAllowed() {
		return fmt.Errorf("push: Authorize отказал для %s@%s: %s", target.User, target.Host, authReply.GetReason())
	}

	ephSigner, ephPubAuthorized, err := newEphemeralEd25519()
	if err != nil {
		return fmt.Errorf("push: генерация ephemeral keypair %s: %w", sid, err)
	}

	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{
		Host:      target.Host,
		User:      target.User,
		PublicKey: ephPubAuthorized,
	})
	if err != nil {
		return fmt.Errorf("push: Sign %s@%s: %w", target.User, target.Host, err)
	}
	auth, err := authMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return fmt.Errorf("push: подготовка SSH-auth %s: %w", sid, err)
	}

	sess, err := d.deps.Dial(ctx, DialConfig{
		Host:            target.Host,
		Port:            target.Port,
		User:            target.User,
		Auth:            auth,
		HostAuthorities: d.deps.HostAuthorities,
		OnHostCAMatch:   d.onHostCAMatch,
		ProxyJump:       signReply.GetProxyJump(),
		Timeout:         d.deps.DialTimeout,
	})
	if err != nil {
		return fmt.Errorf("push: connect %s: %w", sid, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Warn("push: закрытие SSH-сессии с ошибкой", slog.Any("error", cerr))
		}
	}()

	if err := d.deps.Cleaner.Cleanup(ctx, sess); err != nil {
		return fmt.Errorf("push: cleanup %s: %w", sid, err)
	}
	log.Info("push: host-side cleanup выполнен")
	return nil
}

// authMethodsFromSign converts a SignReply into ssh.AuthMethod values.
// Supports two modes (PM-decision SSH key-ownership):
//
//   - Keeper-ephemeral (Vault SSH CA, the canonical mode for CA providers):
//     the plugin returns only a certificate, private_key="". The signer is
//     Keeper's ephemeral keypair. Cert + ephSigner → ssh.NewCertSigner.
//
//   - Static flow (soul-ssh-static): the plugin owns the key and returns a
//     ready-made pair (private_key non-empty). ephSigner is ignored.
func authMethodsFromSign(reply *pluginv1.SignReply, ephSigner ssh.Signer) ([]ssh.AuthMethod, error) {
	if reply.GetPrivateKey() != "" {
		signer, err := ssh.ParsePrivateKey([]byte(reply.GetPrivateKey()))
		if err != nil {
			return nil, fmt.Errorf("разбор private_key: %w", err)
		}
		if cert := reply.GetCertificate(); cert != "" {
			certSigner, cerr := certSignerFrom(cert, signer)
			if cerr != nil {
				return nil, cerr
			}
			return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}, nil
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	cert := reply.GetCertificate()
	if cert == "" {
		return nil, errors.New("SignReply: оба поля пусты (нужен certificate для ephemeral-режима либо private_key для static-режима)")
	}
	if ephSigner == nil {
		return nil, errors.New("ephemeral signer не передан, а private_key пуст — нечем подписать handshake")
	}
	certSigner, err := certSignerFrom(cert, ephSigner)
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}, nil
}

// certSignerFrom parses a text-form OpenSSH cert and combines it with a
// signer into an ssh.CertSigner. Used by both flows (static with cert /
// ephemeral).
func certSignerFrom(certText string, signer ssh.Signer) (ssh.Signer, error) {
	pub, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(certText))
	if perr != nil {
		return nil, fmt.Errorf("разбор certificate: %w", perr)
	}
	sshCert, ok := pub.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("certificate не является SSH-сертификатом")
	}
	certSigner, cerr := ssh.NewCertSigner(sshCert, signer)
	if cerr != nil {
		return nil, fmt.Errorf("сборка cert-signer: %w", cerr)
	}
	return certSigner, nil
}

// newEphemeralEd25519 generates a fresh ed25519 keypair per session and
// returns (signer, marshaled pubkey in OpenSSH authorized_keys format).
//
// SENSITIVE: the private key stays ONLY inside the returned signer.
func newEphemeralEd25519() (ssh.Signer, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("ed25519 GenerateKey: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("ssh signer from ed25519: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", fmt.Errorf("ssh pubkey from ed25519: %w", err)
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return signer, authorized, nil
}

// soulApplyCommand builds the command to start the oneshot applier on the
// host. stdin=protojson ApplyRequest is fed separately (Session.Run),
// stdout=NDJSON.
func soulApplyCommand(soulPath string) string {
	return soulPath + " apply"
}

// onHostCAMatch — callback from `hostCertCallback` on a host-CA match.
func (d *SshDispatcher) onHostCAMatch(caName string) {
	if d.deps.Logger != nil {
		d.deps.Logger.Debug("push: host CA matched", slog.String("ca_name", caName))
	}
	d.deps.Metrics.ObserveHostCAUsed(caName)
}
