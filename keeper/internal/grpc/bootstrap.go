package grpc

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TxBeginner — narrow interface over *pgxpool.Pool (just Begin, for
// wiring an atomic transaction through [pgx.BeginFunc]). Lets us
// mock it in unit tests without a real PG; the production impl is
// `*pgxpool.Pool`.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// BootstrapPool — extends [TxBeginner] for the onboarding handler: besides
// Begin it needs read access (via [bootstraptoken.ExecQueryRower]) for a cheap
// token pre-check BEFORE the Vault round trip — early-reject a junk token without
// calling PKI (M3). Production impl is `*pgxpool.Pool` (satisfies both).
type BootstrapPool interface {
	TxBeginner
	bootstraptoken.ExecQueryRower
}

// CSRSigner — narrow interface over [keepervault.Client.SignCSR]. Symmetric to
// [TxBeginner]: the handler depends on the method, not a concrete client.
type CSRSigner interface {
	SignCSR(ctx context.Context, mount, role, csrPEM string) (*keepervault.SignedCertificate, error)
}

// BootstrapDeps — wire-up dependencies for the onboarding handler.
//
// All fields are required: Pool — the "authorize the token + write the seed"
// transaction; VaultClient.SignCSR — CSR signing via Vault PKI; AuditWriter —
// `soul.bootstrapped` + `soul.seed-issued`.
//
// KID — the keeper instance identifier; written to `bootstrap_tokens.used_by_kid`
// and `souls.last_seen_by_kid`, and shows up in the audit payload.
// PKIMount / PKIRole — from `keeper.yml::vault.{pki_mount,pki_role}`.
type BootstrapDeps struct {
	Pool        BootstrapPool
	VaultClient CSRSigner
	AuditWriter audit.Writer
	KID         string
	PKIMount    string
	PKIRole     string

	// Metrics — keeper_grpc_*-collectors (ADR-024). nil → bootstrap metrics
	// are disabled (nil-safe [GRPCMetrics] methods — no-op). Must be the same
	// descriptor as [OutboundDeps.Metrics] / [EventStreamDeps.Metrics]
	// (one Registry).
	Metrics *GRPCMetrics

	// SigilAnchorSource — LIVE source of the Sigil signing trust-anchor set in
	// PEM form (ADR-026(h), R3-S7, architect af7d). Read on EVERY Bootstrap reply
	// (not a startup snapshot): after a runtime signing-key rotation
	// (Introduce/SetPrimary/Retire → cluster reload R3-S6 updates the holder), a
	// new Soul onboarding gets the CURRENT set. Without this, the window between
	// bootstrap and connect would hand out a stale set — dangerous on Retire (the
	// new Soul would trust an already-retired key, or reject the fresh primary).
	//
	// The set feeds both the single-anchor legacy field
	// [keeperv1.BootstrapReply.SigilPubkeyPem] (for old Souls) — its first element
	// (primary first) — and the full [keeperv1.BootstrapReply.SigilPubkeyPemSet]
	// (R3-S4 reads set > single).
	//
	// nil OR an empty set = Sigil not configured/disabled — both reply fields stay
	// empty, verify on the Soul side stays off (bootstrap flow behaves as before
	// Sigil existed). Implemented in the daemon as an atomic holder
	// (trustAnchorHolder), updated by the `sigil:anchors-changed` watcher.
	SigilAnchorSource TrustAnchorSource

	// KeeperVersion — the build version of this instance, returned by the Ping
	// RPC (ADR-0076(h)). Empty → Ping answers with an empty version, which is
	// exactly what a pre-ADR-0076 keeper looked like on the wire.
	KeeperVersion string

	// MaxConns bounds concurrent TCP connections on the listener; a listener
	// knob rather than a handler one, but this is the only struct
	// [NewBootstrapServer] takes that is not the config schema. <=0 →
	// [defaultBootstrapMaxConns].
	MaxConns int

	// PreauthConcurrency bounds how many Bootstrap RPCs may be holding a
	// database connection at once (NIM-839). <=0 →
	// [defaultBootstrapPreauthConcurrency].
	PreauthConcurrency int

	// PreauthWait is how long a caller waits for a slot in that budget before
	// being refused. <=0 → [bootstrapPreauthWait].
	PreauthWait time.Duration
}

func (d BootstrapDeps) validate() error {
	if d.Pool == nil {
		return errors.New("grpc: BootstrapDeps.Pool is required")
	}
	if d.VaultClient == nil {
		return errors.New("grpc: BootstrapDeps.VaultClient is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: BootstrapDeps.AuditWriter is required")
	}
	if d.KID == "" {
		return errors.New("grpc: BootstrapDeps.KID is required")
	}
	if d.PKIMount == "" {
		return errors.New("grpc: BootstrapDeps.PKIMount is required")
	}
	if d.PKIRole == "" {
		return errors.New("grpc: BootstrapDeps.PKIRole is required")
	}
	return nil
}

// bootstrapHandler — implements [keeperv1.KeeperServer] for the Bootstrap listener.
//
// EventStream is wired as Unimplemented via the embedded [keeperv1.UnimplementedKeeperServer]:
// the Bootstrap listener (server-only TLS) never starts the long-lived stream —
// the Soul doesn't have a client certificate yet.
type bootstrapHandler struct {
	keeperv1.UnimplementedKeeperServer
	deps   BootstrapDeps
	logger *slog.Logger

	// preauth is the admission budget for the part of Bootstrap that costs a
	// pooled database connection; preauthWait is how long a caller may wait for
	// a slot before being refused. See [bootstrapHandler.acquirePreauth].
	preauth     chan struct{}
	preauthWait time.Duration

	// lastRefusalLog throttles the "budget spent" warning; see
	// [bootstrapHandler.notePreauthRefusal].
	refusalMu      sync.Mutex
	lastRefusalLog time.Time
}

const (
	// defaultBootstrapPreauthConcurrency — how many Bootstrap RPCs may be past
	// the free checks at once (NIM-839).
	//
	// A ceiling on pre-auth work in flight, not on request rate: the value is
	// only meaningful against the pool the listener is given, which is its own
	// ([pg.NewBootstrapPool]) and small. What it buys is that a caller over the
	// budget waits WITHOUT a pooled connection, instead of queueing on
	// `Acquire` and holding one.
	defaultBootstrapPreauthConcurrency = 16

	// bootstrapPreauthWait — how long a caller waits for a slot before being
	// refused. Sized off the work a slot holds: one Vault PKI signature plus
	// one short transaction, so 16 slots turn over tens of times a second and a
	// burst of a few hundred hosts clears well inside this. A flood never
	// drains, which is what makes the wait a refusal rather than a queue.
	bootstrapPreauthWait = 5 * time.Second

	// bootstrapPreauthWaitShare — the DIVISOR on the caller's remaining time
	// bounding how much of it queueing may take: 2 means at most one half, and
	// at least one half is always left for the work.
	//
	// A ratio and not a duration on purpose. The caller's budget is
	// `keeper.retry.handshake_timeout` in `soul.yml`, which this server cannot
	// read and which operators are told to lower for faster failover; any fixed
	// number subtracted from it silently becomes "no wait at all" below that
	// number. See [bootstrapHandler.acquirePreauth].
	//
	// Typed, so it cannot be mistaken for a duration: untyped, every use site
	// would infer time.Duration and a future `remaining * share` or
	// `slog.Duration(…, share)` would silently mean two nanoseconds.
	bootstrapPreauthWaitShare time.Duration = 2

	// bootstrapRefusalLogInterval — the floor between two "budget spent"
	// warnings. See [bootstrapHandler.notePreauthRefusal].
	bootstrapRefusalLogInterval = 30 * time.Second

	// bootstrapMaxTokenLen — the ceiling on the presented token, in bytes. A
	// real one is 32 random bytes as base64url, 43 characters
	// ([bootstraptoken.Generate]); the cap is two orders of magnitude of
	// headroom and exists so the free checks stay free, which they are not if a
	// padded token can be parked for the length of the wait.
	bootstrapMaxTokenLen = 256
)

func newBootstrapHandler(deps BootstrapDeps, logger *slog.Logger) *bootstrapHandler {
	limit := deps.PreauthConcurrency
	if limit <= 0 {
		limit = defaultBootstrapPreauthConcurrency
	}
	wait := deps.PreauthWait
	if wait <= 0 {
		wait = bootstrapPreauthWait
	}
	return &bootstrapHandler{
		deps:        deps,
		logger:      logger,
		preauth:     make(chan struct{}, limit),
		preauthWait: wait,
	}
}

// errPreauthBudgetSpent — the budget refused: no slot came free inside the
// wait, or the caller had too little time left to be worth admitting.
var errPreauthBudgetSpent = errors.New("grpc: bootstrap pre-auth budget spent")

// errPreauthCallerGone — the caller's context ended while it waited. Distinct
// from a spent budget: it is not a capacity signal, must not be logged as one,
// and the caller should hear its own reason back.
var errPreauthCallerGone = errors.New("grpc: bootstrap caller gave up while waiting")

// acquirePreauth takes a slot in the pre-auth budget, waiting for one, and
// returns nil if it got one.
//
// The wait is the difference between bounding the pool and breaking onboarding.
// A caller waiting here holds a goroutine, a stream and its (16 KiB-capped)
// request, and holds NO pooled connection — which is the property the budget
// exists for. Refusing outright would be an outage for the legitimate case:
// `soul bootstrap` tries each endpoint exactly once and does not retry
// ([soul/internal/bootstrap.Run]), so a mass onboarding that briefly exceeds
// the budget would fail on the hosts that lost the race with nothing to pick
// them back up.
//
// **The guarantee is proportional: queueing never takes more than 1 in
// [bootstrapPreauthWaitShare] of the caller's remaining time**, so at least the
// same proportion is still there when the slot is won. That matters because the
// Soul wraps dial, handshake and RPC in ONE deadline and writes its private key
// only after a successful reply, while the server burns the token
// mid-transaction: time this queue spends is time the work does not get.
//
// A share rather than a fixed reserve. The caller's budget is
// `keeper.retry.handshake_timeout` in `soul.yml` — an operator knob this server
// cannot see, documented with advice to lower it. Any fixed number subtracted
// from it is a cliff: set the timeout at or under that number and every
// contended caller takes an immediate refusal, which is the NIM-839 onboarding
// outage restored by a setting in a different file.
//
// **What this does NOT promise, deliberately:** that an admitted caller has
// enough time to finish. The server cannot know — the cost is dominated by a
// Vault round trip of "unpredictable latency", and an UNCONTENDED caller with
// the same short deadline faces exactly the same risk without ever touching
// this function. An absolute floor would only be that same cliff at a different
// value: it would refuse callers whose deadline is short but whose Vault is
// fast, which is a working deployment today. The residual risk — a commit that
// lands after the reply can be delivered, burning a token for a host that never
// learns it onboarded — belongs to the burn itself and is NIM-865, not to how
// long anyone queued.
//
// release is never nil, on any path, so a caller that defers it without
// checking the error cannot panic. waited is what was actually spent queueing,
// for the refusal log.
func (h *bootstrapHandler) acquirePreauth(ctx context.Context) (release func(), waited time.Duration, err error) {
	noop := func() {}
	take := func() { <-h.preauth }
	if h.preauth == nil {
		return noop, 0, nil
	}
	// A caller that is already gone gets nothing, contended or not: the fast
	// path would otherwise hand it a slot and a pooled connection.
	// A caller that is already gone gets nothing, contended or not: the fast
	// path would otherwise hand it a slot and a pooled connection.
	if ctx.Err() != nil {
		return noop, 0, errPreauthCallerGone
	}
	select {
	case h.preauth <- struct{}{}:
		return take, 0, nil
	default:
	}

	wait := h.preauthWait
	if remaining, bounded := timeLeft(ctx); bounded {
		if share := remaining / bootstrapPreauthWaitShare; share < wait {
			wait = share
		}
	}
	if wait <= 0 {
		// Only reachable once the deadline has already passed — the share of a
		// positive remainder is positive. That is a caller that is gone, not a
		// keeper that is full, and must not be counted as capacity.
		return noop, 0, errPreauthCallerGone
	}

	start := time.Now()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case h.preauth <- struct{}{}:
	case <-timer.C:
		return noop, time.Since(start), errPreauthBudgetSpent
	case <-ctx.Done():
		return noop, time.Since(start), errPreauthCallerGone
	}

	// A select with two ready cases picks pseudo-randomly, so winning the slot
	// is not evidence the caller is still there. Check before spending its
	// share of the pool on a peer that has gone.
	if ctx.Err() != nil {
		take()
		return noop, time.Since(start), errPreauthCallerGone
	}
	return take, time.Since(start), nil
}

// timeLeft reports how long ctx has, and whether it is bounded at all. A
// context with no deadline is the unit-test case — every real Bootstrap client
// is `soul bootstrap`, which always sets one. It is not treated as infinite
// headroom by accident; it simply has no budget to protect.
func timeLeft(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}

// notePreauthRefusal reports a spent budget at most once per
// [bootstrapRefusalLogInterval]. A refusal is what a flood produces, so one
// line per refused attempt would be a second way to spend the host — but with
// no line at all the budget is undiagnosable, because [GRPCMetrics.ObserveBootstrap]
// has only `ok` and `failed` and cannot tell "at capacity" from a Vault outage
// or a bad token.
func (h *bootstrapHandler) notePreauthRefusal(now time.Time, waited time.Duration) {
	h.refusalMu.Lock()
	due := h.lastRefusalLog.IsZero() || now.Sub(h.lastRefusalLog) >= bootstrapRefusalLogInterval
	if due {
		h.lastRefusalLog = now
	}
	h.refusalMu.Unlock()

	// Outside the lock: this runs on the refusal path of a flood, and a slow
	// log writer holding it would put every refused caller in a convoy behind
	// the line describing the flood.
	if due {
		// `waited` is MEASURED, not predicted. It differs from the configured
		// wait whenever the caller's own deadline was the smaller of the two,
		// and reporting the configured value there would tell the operator to
		// add capacity when the answer is `keeper.retry.handshake_timeout`.
		h.logger.Warn("bootstrap: pre-auth budget spent — refusing onboarding attempts",
			slog.Int("budget", cap(h.preauth)),
			slog.Duration("waited", waited),
			slog.Duration("wait_configured", h.preauthWait),
		)
	}
}

// Ping — health-check RPC, available without authorization (server-only TLS
// already restricts callers on its own).
//
// Version carries the keeper BUILD version (ADR-0076(h)). It used to return the
// KID — a field-name/value drift: the KID identifies the instance, not its
// version, and it is already delivered by HelloReply.kid. Nothing in-repo read
// the field, so correcting it breaks no consumer.
func (h *bootstrapHandler) Ping(_ context.Context, _ *keeperv1.PingRequest) (*keeperv1.PingReply, error) {
	return &keeperv1.PingReply{Version: h.deps.KeeperVersion}, nil
}

// Bootstrap — implements the unary onboarding RPC per [docs/soul/onboarding.md].
//
// Flow:
//  1. Free checks (SID format, reserved SID, token present and bounded,
//     CSR PEM non-empty).
//  2. Take the pre-auth budget — everything below costs a shared resource.
//  3. Parse the CSR: CN must equal the SID, and the self-signature must verify
//     (possession of the key being bound). Its SubjectPublicKeyInfo hash is the
//     fingerprint the seed decisions below are made on.
//  4. Hash plain-token → token_hash.
//  5. Cheap token pre-check (SelectByHash, no Burn) — early-reject junk
//     BEFORE the expensive Vault round trip (M3), and decide which of the two
//     authorization paths this is. Anti-enum: any failure → a single
//     PermissionDenied, indistinguishable from not-found/expired/used.
//  6. Vault PKI SignCSR — issue the certificate (only once pre-check passed).
//  7. Tx BEGIN.
//  8. Authorize the token: [bootstraptoken.Burn] for a first presentation —
//     the race-safe UPDATE that is the authoritative anti-replay check under
//     load (step 5 is an optimization, not a replacement: the TOCTOU gap
//     between select and burn is closed by this UPDATE) — or
//     [bootstraptoken.RedeemAgain] for a retry after a lost reply.
//  9. Write the seed ([bootstrapHandler.writeSeed]).
//
// 10. COMMIT.
// 11. Audit: `soul.bootstrapped` + `soul.seed-issued` (one correlation_id = token_id).
//
// **Step 9 chooses insert-vs-re-stamp from the REGISTRY, not from step 8.** A
// key `soul_seeds` has never seen is inserted after superseding whatever was
// active; a key already recorded for this SID has its row re-stamped in place,
// because the globally unique fingerprint index forbids a second row and the
// fingerprint IS the identity, which did not move. Both authorization paths can
// arrive at either branch — a fresh token redeemed by a host that kept its key
// is a FIRST presentation that must re-stamp.
//
// **The recovery path** (step 8's second arm) is admitted only when the token
// was burned by a Soul, the host has never held a stream, and the CSR carries
// the key that burn bound. What must not be repeated is binding a SID to a key,
// not presenting the token; a reply the network ate is not a second binding.
// See docs/adr/0090-bootstrap-reply-loss-recovery.md (NIM-865).
//
// **`souls.status` is not written here, on either path.** It stays `pending` —
// "token issued, Soul not yet connected", which is exactly true — and reaches
// `connected` from the EventStream handshake ([soul.MarkConnected]), the event
// the value is defined by.
//
// All errors before Vault are fail-fast with a rollback. A Vault error → tx
// rollback + Unavailable (transient — the Soul retries). Audit is written
// **after** commit; an audit failure is logged as a warning but doesn't
// abort onboarding (the DB is consistent; the audit gap is a separate manual fix).
func (h *bootstrapHandler) Bootstrap(ctx context.Context, req *keeperv1.BootstrapRequest) (reply *keeperv1.BootstrapReply, err error) {
	// In-process span for one onboarding attempt. sid is an attribute for trace
	// filtering (forbidden in metric labels — cardinality, ADR-024 §2.2); it
	// carries no secrets (token / CSR). The bootstrap_total metric is recorded
	// based on the outcome (err==nil → ok). With OTel disabled the tracer is a
	// no-op — the span is free.
	ctx, span := tracer.Start(ctx, "grpc.bootstrap",
		trace.WithAttributes(attribute.String("sid", req.GetSid())),
	)
	defer func() {
		h.deps.Metrics.ObserveBootstrap(err)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, "bootstrap failed")
		}
		span.End()
	}()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	sid := req.GetSid()
	if !soul.ValidSID(sid) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid sid %q", sid)
	}
	// Reserved sids (keeper / __run__) — run synthetics, not a Soul (NIM-36).
	if soul.IsReservedSID(sid) {
		return nil, status.Errorf(codes.InvalidArgument, "reserved sid %q", sid)
	}
	plainToken := req.GetBootstrapToken()
	if strings.TrimSpace(plainToken) == "" {
		return nil, status.Error(codes.InvalidArgument, "bootstrap_token is empty")
	}
	// Length is not a secret and discloses nothing a caller does not already
	// know, so this stays outside the anti-enum single-answer rule below.
	if len(plainToken) > bootstrapMaxTokenLen {
		return nil, status.Error(codes.InvalidArgument, "bootstrap_token is too long")
	}
	csrPEM := req.GetCsrPem()
	if len(csrPEM) == 0 {
		return nil, status.Error(codes.InvalidArgument, "csr_pem is empty")
	}
	// Past this line every step costs a resource the cluster shares — CPU for the
	// CSR's signature check, a pooled database connection for the pre-check, then
	// Vault, then a transaction — and the caller is still unauthenticated: the
	// token is what the pre-check is about to read. The budget is therefore taken
	// BEFORE any of it, so a caller over it never reaches one (NIM-839).
	//
	// The CSR parse used to sit above this line, as a free check. It stopped
	// being free when NIM-865 made it verify the self-signature: that is
	// public-key work whose cost the caller chooses, by choosing the modulus. It
	// is one `defer release()` away from the refusal path either way.
	release, waited, acqErr := h.acquirePreauth(ctx)
	defer release()
	switch {
	case errors.Is(acqErr, errPreauthCallerGone):
		// Its own reason back, not a capacity signal: a client that hung up is
		// not evidence the keeper is full, and counting it as such is how the
		// one diagnostic an operator has stops meaning anything. The nil guard
		// is for a future second return site: FromContextError(nil) yields a
		// nil *Status whose Err() is nil, which would make this RPC answer
		// (nil, nil) and be recorded as a success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		return nil, status.Error(codes.Canceled, "caller left before onboarding started")
	case acqErr != nil:
		h.notePreauthRefusal(time.Now(), waited)
		return nil, status.Error(codes.ResourceExhausted, "bootstrap is at capacity")
	}

	// CSR CommonName must match the requested SID, and the self-signature must
	// verify (defense-in-depth, crypto). Onboarding authority is anchored on the
	// registry fingerprint, not the CN, but checking both BEFORE Vault SignCSR
	// keeps us from relying solely on the operator's Vault PKI role config
	// (allowed_domains could be wider than the SID). Either failure →
	// InvalidArgument BEFORE the PKI round trip.
	//
	// The parsed CSR is kept: its SubjectPublicKeyInfo is the key the request is
	// asking to bind, and the recovery path below is decided by comparing it
	// against the key the registry already bound (NIM-865).
	csr, err := parseValidatedCSR(csrPEM, sid)
	if err != nil {
		return nil, err
	}
	csrFingerprint := soulseed.FingerprintFromCSR(csr)

	tokenHash := bootstraptoken.HashToken(plainToken)

	// Cheap token pre-check BEFORE the Vault round trip (M3): a junk token
	// shouldn't trigger an expensive PKI sign. This is an optimization, not the
	// authority: the authoritative check is the WHERE clause of the Burn — or,
	// for an already-burned token being retried by the host that burned it, of
	// the RedeemAgain — inside the transaction below. Any pre-check failure → a
	// single PermissionDenied (anti-enum, we don't distinguish
	// not-found/expired/used/not-recoverable).
	recovery, err := h.precheckToken(ctx, tokenHash, sid, csrFingerprint)
	if err != nil {
		return nil, err
	}

	// Vault PKI signing is a separate step BEFORE the transaction, but AFTER
	// the token pre-check. It's a network round trip with unpredictable latency;
	// there's no point holding a PG transaction open for it. Authoritative
	// token validation happens inside the transaction via Burn / RedeemAgain.
	signed, err := h.deps.VaultClient.SignCSR(ctx, h.deps.PKIMount, h.deps.PKIRole, string(csrPEM))
	if err != nil {
		return nil, h.mapVaultErr(err, sid)
	}
	cert, err := parseCertificatePEM(signed.CertificatePEM)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "vault returned invalid certificate: %v", err)
	}
	fingerprint := soulseed.FingerprintFromCert(cert)
	// Vault signed a CSR whose key is not the one that was authorized. Nothing
	// downstream would notice — the recovery path matches the seed row on the
	// pre-check's value — so the mismatch is caught here rather than written.
	if fingerprint != csrFingerprint {
		h.logger.Error("vault signed a certificate for a different public key than the CSR carried",
			slog.String("sid", sid))
		return nil, status.Error(codes.Internal, "vault certificate does not match the presented CSR")
	}

	var (
		tokenID string
		seedID  string
	)
	err = pgx.BeginFunc(ctx, h.deps.Pool, func(tx pgx.Tx) error {
		// Authorize. The two paths differ only here: a first presentation burns,
		// a recovery re-authorizes delivery to the binding that burn created.
		// RedeemAgain carries its whole authorization in one statement — see its
		// godoc for the five conditions and why none is evaluated in Go first.
		var (
			tokID   string
			authErr error
		)
		if recovery {
			tokID, authErr = bootstraptoken.RedeemAgain(ctx, tx, tokenHash, sid, h.deps.KID, csrFingerprint)
		} else {
			tokID, authErr = bootstraptoken.Burn(ctx, tx, tokenHash, sid, h.deps.KID)
		}
		if authErr != nil {
			return authErr
		}
		tokenID = tokID

		sID, seedErr := h.writeSeed(ctx, tx, sid, fingerprint, signed)
		if seedErr != nil {
			return seedErr
		}
		seedID = sID

		// The row stays `pending`, and NOT because nothing needs saying: a
		// certificate has been signed, but no stream has ever existed and
		// `connected` claims one does ([soul.Status]). `pending` — "token
		// issued, Soul not yet connected" — is true here and stays true until
		// [soul.MarkConnected] fires on the EventStream handshake. The status
		// this handler used to write was never corrected by anything, because
		// the Reaper's disconnect sweep cannot match a NULL `last_seen_at`
		// (migration 043), so a host that never appeared read as `connected`
		// forever — and `core.bootstrap.issued` treats that as "already
		// onboarded, nothing to do" and declines to re-arm it (NIM-865).
		//
		// Registry membership was checked by the pre-check's own read and is
		// enforced under the transaction by `soul_seeds_sid_fk`.
		return nil
	})
	if err != nil {
		return nil, h.mapTxErr(err, sid)
	}

	// Audit — after commit. One correlation_id = token_id links
	// soul.bootstrapped and soul.seed-issued (per docs/keeper/audit.md).
	correlationID := tokenID
	notAfter := signed.NotAfter
	if writeErr := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventSoulBootstrapped,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: correlationID,
		Payload: map[string]any{
			"sid":         sid,
			"token_id":    tokenID,
			"seed_id":     seedID,
			"fingerprint": fingerprint,
			"not_after":   notAfter,
			"kid":         h.deps.KID,
			// Why one token has more than one bootstrapped event. Without it an
			// auditor sees a token apparently redeemed twice and no way to tell
			// a recovery from the replay the burn is supposed to prevent.
			"recovered_lost_reply": recovery,
		},
	}); writeErr != nil {
		h.logger.Warn("audit write soul.bootstrapped failed (DB committed)",
			slog.String("sid", sid),
			slog.String("seed_id", seedID),
			slog.Any("error", writeErr),
		)
	}
	if writeErr := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventSoulSeedIssued,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: correlationID,
		Payload: map[string]any{
			"sid":           sid,
			"seed_id":       seedID,
			"fingerprint":   fingerprint,
			"serial_number": signed.SerialNumber,
			"issued_at":     time.Now().UTC(),
			"not_after":     notAfter,
			"kid":           h.deps.KID,
		},
	}); writeErr != nil {
		h.logger.Warn("audit write soul.seed-issued failed (DB committed)",
			slog.String("sid", sid),
			slog.String("seed_id", seedID),
			slog.Any("error", writeErr),
		)
	}

	h.logger.Info("soul bootstrapped",
		slog.String("sid", sid),
		slog.String("seed_id", seedID),
		slog.String("fingerprint", fingerprint),
		slog.String("kid", h.deps.KID),
		slog.String("peer", peerAddr(ctx)),
		// The one field that separates a first onboarding from a host
		// recovering a reply it never received. Without it the operator sees
		// two identical lines and no reason a certificate was signed twice.
		slog.Bool("recovered_lost_reply", recovery),
	)

	out := &keeperv1.BootstrapReply{
		CertificatePem: signed.CertificatePEM,
		CaChainPem:     signed.CAChainPEM,
		NotAfter:       timestamppb.New(notAfter),
		Kid:            h.deps.KID,
	}
	h.applySigilAnchors(out)
	return out, nil
}

// applySigilAnchors fills the reply's Sigil trust-anchor fields with the LIVE
// set from [TrustAnchorSource] (ADR-026(h), R3-S7): the set is read on every
// reply, not a startup snapshot — a Soul onboarding right after a rotation gets
// the current set. An empty set (Sigil disabled or source nil) → both fields
// stay nil, keeping the bootstrap contract backward-compatible. Split into its
// own method for the unit test "after SetAnchors the next reply carries the new set."
func (h *bootstrapHandler) applySigilAnchors(out *keeperv1.BootstrapReply) {
	if h.deps.SigilAnchorSource == nil {
		return
	}
	anchors := h.deps.SigilAnchorSource.AnchorSetPEM()
	if len(anchors) == 0 {
		return
	}
	// Multi-anchor set (R3-S4 reads set > single). No copy needed: the holder
	// hands back a read-only snapshot, and reply serialization doesn't mutate it.
	out.SigilPubkeyPemSet = anchors
	// Single legacy anchor for old Souls — the set's first element (primary
	// first, see AnchorSetPEM); also from the live source.
	single := anchors[0]
	out.SigilPubkeyPem = &single
}

// errSeedKeyTaken — the presented key already has a row in `soul_seeds` that
// this onboarding may not write to. Mapped to the same PermissionDenied as a
// rejected token: either the key belongs to another SID, or it is one this host
// retired, and neither is something to explain to an unauthenticated caller.
var errSeedKeyTaken = errors.New("grpc: the presented key is already bound elsewhere")

// writeSeed records the freshly signed certificate, choosing between inserting
// a new seed and re-stamping an existing one **from the registry** — not from
// which token path authorized the call.
//
// That distinction is the whole point. `soul_seeds_fingerprint_idx` is globally
// unique and the fingerprint is the public key, so "does this key already have
// a row" is a fact about the key, and any path that can present a key the
// registry has already seen has to answer it. Since NIM-865 two can: a recovery
// re-presents the key deliberately, and so does a FRESH token redeemed by a
// host that kept its key from an attempt whose reply was lost — which is the
// ordinary operator recovery (`issue-token --force`, or a repeat of
// `core.bootstrap.issued`). Deciding by path would have left that second one
// hitting the unique index and failing as Internal with the new token burned.
func (h *bootstrapHandler) writeSeed(ctx context.Context, tx pgx.Tx, sid, fingerprint string, signed *keepervault.SignedCertificate) (string, error) {
	existing, err := soulseed.SelectByFingerprint(ctx, tx, fingerprint)
	switch {
	case errors.Is(err, soulseed.ErrSeedNotFound):
		// A key the registry has never seen: the ordinary first onboarding, and
		// a rotation-by-bootstrap. Supersede whatever was active, insert.
		if supErr := soulseed.SupersedeBySID(ctx, tx, sid); supErr != nil {
			return "", supErr
		}
		seed := &soulseed.SoulSeed{
			SID:          sid,
			Fingerprint:  fingerprint,
			SerialNumber: signed.SerialNumber,
			ExpiresAt:    signed.NotAfter,
			IssuedByKID:  &h.deps.KID,
			Status:       soulseed.StatusActive,
		}
		if insErr := soulseed.Insert(ctx, tx, seed); insErr != nil {
			return "", insErr
		}
		return seed.SeedID, nil

	case err != nil:
		return "", err

	// The next two arms do NOT implement the refusal — [soulseed.RefreshCertificate]
	// does, by matching `sid` and `status='active'` in its own WHERE, and falling
	// through to it would refuse both cases with the same PermissionDenied. They
	// exist to NAME the reason in the log, because the wire answer is deliberately
	// one undistinguished refusal and an operator otherwise has nothing to go on.
	// Delete them and every test still passes; that is the point, not an oversight.
	case existing.SID != sid:
		// Another host's identity. A public key is public, so presenting one is
		// free — what it must not buy is a certificate.
		h.logger.Warn("bootstrap: presented key is bound to another sid",
			slog.String("sid", sid), slog.String("bound_to", existing.SID))
		return "", errSeedKeyTaken

	case existing.Status != soulseed.StatusActive:
		// This host's own key, but retired — superseded by a rotation, or
		// revoked. Re-issuing against it would undo that decision.
		h.logger.Warn("bootstrap: presented key is a retired seed of this sid",
			slog.String("sid", sid), slog.String("status", string(existing.Status)))
		return "", errSeedKeyTaken

	default:
		// The identity is already recorded and still current; only the
		// certificate is missing. Re-stamp in place — the fingerprint does not
		// move, so there is nothing to supersede and nothing to insert.
		return soulseed.RefreshCertificate(ctx, tx, sid, fingerprint,
			signed.SerialNumber, signed.NotAfter, &h.deps.KID)
	}
}

// precheckToken — cheap token check BEFORE Vault-sign (M3 early-reject).
// Reads the record by token_hash and checks (sid + not expired) in Go, then
// decides which of the two paths the request is on. Authority stays with Burn /
// RedeemAgain inside the transaction; this just filters out junk without a PKI
// round trip.
//
// recovery reports that the token is already burned and this presentation is
// the same host retrying after a lost reply (NIM-865). The three extra
// conditions that admit it are re-evaluated atomically by
// [bootstraptoken.RedeemAgain]; checking them here as well keeps a doomed
// retry from spending a Vault signature, which is the same bargain the
// not-burned pre-check has always made.
//
// Anti-enum: any failure (no record, wrong SID, expired, burned and not
// recoverable, junk hash format) → a single PermissionDenied, indistinguishable
// to the Soul by content or timing class. A transient DB read error (not a
// not-found) → Unavailable: the Soul retries, so junk still can't get through.
func (h *bootstrapHandler) precheckToken(ctx context.Context, tokenHash, sid, csrFingerprint string) (recovery bool, err error) {
	rec, err := bootstraptoken.SelectByHash(ctx, h.deps.Pool, tokenHash)
	if err != nil {
		if errors.Is(err, bootstraptoken.ErrTokenNotFound) {
			return false, h.rejectToken(sid)
		}
		h.logger.Warn("bootstrap token pre-check read failed",
			slog.String("sid", sid), slog.Any("error", err))
		return false, status.Errorf(codes.Unavailable, "bootstrap token pre-check failed")
	}
	now := time.Now().UTC()
	if rec.SID != sid || !rec.ExpiresAt.After(now) {
		return false, h.rejectToken(sid)
	}
	if rec.UsedAt == nil {
		return false, nil
	}
	ok, err := h.precheckRecovery(ctx, rec, sid, csrFingerprint)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, h.rejectToken(sid)
	}
	return true, nil
}

// precheckRecovery reports whether an already-burned token may be presented
// again by the host that burned it. It answers the question the burn cannot:
// a burn records that a certificate was PRODUCED, and the Soul's deadline can
// expire between that and the reply arriving, leaving the token spent and the
// host with no credential (NIM-865).
//
// Three conditions, each closing something specific — see
// [bootstraptoken.RedeemAgain] for the authoritative form:
//
//   - the burn was a Soul's, not a marker written by force-reissue or a
//     cascade. Those record an invalidation with no reply to lose.
//   - the host has never held a stream. Once it has, the credential provably
//     arrived and the token is finished, so a leaked plaintext cannot be
//     turned against a host that is up and working.
//   - the active seed is bound to the key being presented now. This is the
//     one-shot property, stated where it belongs: what may not be repeated is
//     binding the SID to a key, not presenting the token. An attacker's own
//     CSR carries an attacker's own key and is refused here exactly as a
//     second Burn refuses it today.
//
// A read error is surfaced, not swallowed into a refusal: answering "no" on an
// unreadable registry would make a Postgres blip look to the operator like a
// rejected token.
func (h *bootstrapHandler) precheckRecovery(ctx context.Context, rec *bootstraptoken.Record, sid, csrFingerprint string) (bool, error) {
	// Coalesced exactly as [bootstraptoken.redeemAgainSQL] does, so the
	// optimization and the authority cannot disagree about what a kid-less burn
	// means. A nil here is not a refusal of its own: `used_at` already says the
	// token was burned, and that clause is the one that carries the decision.
	usedByKID := ""
	if rec.UsedByKID != nil {
		usedByKID = *rec.UsedByKID
	}
	for _, marker := range bootstraptoken.SystemKIDs() {
		if usedByKID == marker {
			return false, nil
		}
	}

	s, err := soul.SelectBySID(ctx, h.deps.Pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return false, nil
		}
		h.logger.Warn("bootstrap recovery pre-check: soul read failed",
			slog.String("sid", sid), slog.Any("error", err))
		return false, status.Errorf(codes.Unavailable, "bootstrap token pre-check failed")
	}
	if s.LastSeenAt != nil {
		return false, nil
	}

	seed, err := soulseed.SelectActiveBySID(ctx, h.deps.Pool, sid)
	if err != nil {
		if errors.Is(err, soulseed.ErrSeedNotFound) {
			return false, nil
		}
		h.logger.Warn("bootstrap recovery pre-check: seed read failed",
			slog.String("sid", sid), slog.Any("error", err))
		return false, status.Errorf(codes.Unavailable, "bootstrap token pre-check failed")
	}
	return seed.Fingerprint == csrFingerprint, nil
}

// rejectToken — the single anti-enum response for an invalid token (whether
// from pre-check or from Burn via [mapTxErr]). The Soul sees one reason and
// can't distinguish not-found / expired / used / wrong-SID by timing.
func (h *bootstrapHandler) rejectToken(sid string) error {
	return status.Errorf(codes.PermissionDenied,
		"bootstrap token rejected for sid=%q", sid)
}

// mapTxErr — maps a CRUD sentinel to a gRPC status:
//   - ErrTokenInvalid       → PermissionDenied (anti-enum: no distinctions made).
//   - ErrSeedActiveExists   → Internal (SupersedeBySID invariant violated).
//   - ErrSeedNotFound       → PermissionDenied (recovery raced the identity, see below).
//   - ErrSeedSoulNotFound   → FailedPrecondition (soul registry in an inconsistent state).
//   - everything else      → Internal, wrapping err.
func (h *bootstrapHandler) mapTxErr(err error, sid string) error {
	switch {
	case errors.Is(err, bootstraptoken.ErrTokenInvalid):
		// We don't distinguish "expired", "not found", "already used", or a
		// re-presentation that failed any of its conditions — anti-enum (same
		// response as the step-3 pre-check).
		return h.rejectToken(sid)
	case errors.Is(err, soulseed.ErrSeedActiveExists):
		h.logger.Error("invariant violation: active seed present after Supersede",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Internal,
			"internal error: active seed already present for sid=%q", sid)
	case errors.Is(err, soulseed.ErrSeedNotFound):
		// Recovery only: the (sid, fingerprint) row the pre-check saw is no
		// longer active — rotated, revoked or superseded between the two reads.
		// The same refusal as a rejected token, deliberately: the binding this
		// presentation was authorized against is gone, so it has no more right
		// to a certificate than any other stale one, and saying which of the
		// two it was would hand a prober the distinction the whole path avoids.
		h.logger.Warn("bootstrap recovery lost its seed between pre-check and commit",
			slog.String("sid", sid))
		return h.rejectToken(sid)
	case errors.Is(err, errSeedKeyTaken):
		return h.rejectToken(sid)
	case errors.Is(err, soulseed.ErrSeedSoulNotFound):
		return status.Errorf(codes.FailedPrecondition,
			"soul %q not in registry (token burned but the seed write failed)", sid)
	default:
		h.logger.Error("bootstrap tx failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Internal, "bootstrap failed: %v", err)
	}
}

// mapVaultErr — sign phase, BEFORE the transaction. Vault transient failures
// (network, 5xx) → Unavailable; misconfig (bad role, bad mount) →
// FailedPrecondition; bad CSR → InvalidArgument. Differentiated by the
// keepervault.ErrPKI* sentinel codes.
func (h *bootstrapHandler) mapVaultErr(err error, sid string) error {
	switch {
	case errors.Is(err, keepervault.ErrPKIMountEmpty),
		errors.Is(err, keepervault.ErrPKIRoleEmpty):
		return status.Errorf(codes.FailedPrecondition,
			"vault PKI misconfigured: %v", err)
	case errors.Is(err, keepervault.ErrPKICSREmpty):
		return status.Error(codes.InvalidArgument, "csr_pem is empty")
	case errors.Is(err, keepervault.ErrPKIResponseInvalid):
		h.logger.Error("vault returned malformed PKI sign response",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Internal, "vault PKI response invalid: %v", err)
	default:
		// No sentinel — transient by default (the Soul retries).
		h.logger.Warn("vault PKI sign failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Unavailable, "vault PKI sign failed: %v", err)
	}
}

// validateCSRCommonName parses the CSR from PEM and checks that its
// Subject.CommonName matches the requested `sid` (defense-in-depth BEFORE
// Vault SignCSR). Returns a gRPC status:
//   - invalid/empty PEM or an unparsable CSR → InvalidArgument
//     (junk input, rejected BEFORE PKI);
//   - CN ≠ sid (including an empty CN) → InvalidArgument with an
//     anti-enum-neutral message (the CN isn't echoed back in the reply — we
//     don't hint at what was actually requested).
//
// The authorization anchor remains the registry fingerprint (this check
// doesn't replace it); it only keeps a cert from being onboarded under the
// wrong CN by relying solely on a broad Vault role allowed_domains.
func validateCSRCommonName(csrPEM []byte, sid string) error {
	_, err := parseValidatedCSR(csrPEM, sid)
	return err
}

// parseValidatedCSR is [validateCSRCommonName] for a caller that needs the CSR
// itself afterwards — the onboarding handler reads its SubjectPublicKeyInfo to
// recognize a retry of an onboarding already bound to that key (NIM-865).
// Splitting it this way keeps one copy of the CN rule: two would be a check
// that can be tightened on one path and left open on the other.
func parseValidatedCSR(csrPEM []byte, sid string) (*x509.CertificateRequest, error) {
	csr, err := parseCSRPEM(csrPEM)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr_pem invalid: %v", err)
	}
	if csr.Subject.CommonName != sid {
		return nil, status.Errorf(codes.InvalidArgument,
			"csr_pem common name does not match sid %q", sid)
	}
	return csr, nil
}

// parseCSRPEM decodes the first CERTIFICATE REQUEST PEM block, parses it into
// an x509.CertificateRequest, and verifies its self-signature.
//
// **The signature check is an authorization input, not hygiene** (NIM-865). A
// CSR's SubjectPublicKeyInfo is what the onboarding binds the SID to, and what
// a recovery is matched on — so without this, a caller could present a public
// key it does not hold the private half of, and the "an attacker's replay
// carries an attacker's own key" argument the anti-replay rule rests on would
// be untrue. Verifying proves possession, which is exactly the claim the
// fingerprint is taken to stand for. It also keeps a CSR carrying another
// host's key from reaching Vault at all: that used to be refused only after
// signing, leaving a real certificate issued against a rolled-back transaction
// and recorded in no `soul_seeds` row, hence revocable through no registry.
//
// Not a substitute for Vault's own validation — this is the same
// defense-in-depth stance as the CN check, which likewise does not rely on the
// operator's PKI role being narrow.
func parseCSRPEM(csrPEM []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, errors.New("pem.Decode returned nil block")
	}
	if block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("unexpected pem block type %q (want CERTIFICATE REQUEST)", block.Type)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("x509.ParseCertificateRequest: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	return csr, nil
}

// parseCertificatePEM decodes the first CERTIFICATE PEM block and parses
// it into an x509.Certificate. Vault PKI issues exactly one block —
// any extra blocks (which shouldn't be there) are ignored.
func parseCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("pem.Decode returned nil block")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("unexpected pem block type %q (want CERTIFICATE)", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("x509.ParseCertificate: %w", err)
	}
	return cert, nil
}

// peerAddr — best-effort extraction of the remote address for log fields.
// Empty string if no peer is present (test environment / unix socket).
func peerAddr(ctx context.Context) string {
	if p, ok := grpcpeer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
