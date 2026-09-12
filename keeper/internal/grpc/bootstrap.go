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
// All fields are required: Pool — the "burn token + supersede seed +
// insert seed + flip status" transaction; VaultClient.SignCSR — CSR signing via
// Vault PKI; AuditWriter — `soul.bootstrapped` + `soul.seed-issued`.
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
//  1. Validate (SID format, token_hash format, CSR PEM non-empty).
//  2. Hash plain-token → token_hash.
//  3. Cheap token pre-check (SelectByHash, no Burn) — early-reject junk
//     BEFORE the expensive Vault round trip (M3). Anti-enum: any failure → a
//     single PermissionDenied, indistinguishable from not-found/expired/used.
//  4. Vault PKI SignCSR — issue the certificate (only once pre-check passed).
//  5. Parse cert → compute fingerprint (SHA-256 SubjectPublicKeyInfo).
//  6. Tx BEGIN.
//  7. Burn token (race-safe UPDATE with WHERE used_at IS NULL) — the
//     authoritative anti-replay check under load (the step-3 pre-check is an
//     optimization, not a replacement: the TOCTOU gap between select and burn
//     is closed by this UPDATE).
//  8. Supersede the previous active seed (no-op for a new Soul).
//  9. Insert the new active seed.
//
// 10. UpdateStatus soul: pending → connected, last_seen_by_kid = KID.
// 11. COMMIT.
// 12. Audit: `soul.bootstrapped` + `soul.seed-issued` (one correlation_id = token_id).
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
	// CSR CommonName must match the requested SID (defense-in-depth, crypto).
	// Onboarding authority is anchored on the registry fingerprint, not the CN,
	// but checking CN BEFORE Vault SignCSR keeps us from relying solely on the
	// operator's Vault PKI role config (allowed_domains could be wider than the
	// SID). An invalid CN → InvalidArgument BEFORE the PKI round trip.
	if err := validateCSRCommonName(csrPEM, sid); err != nil {
		return nil, err
	}

	// Past this line every step costs a resource the cluster shares — a pooled
	// database connection for the pre-check, then Vault, then a transaction —
	// and the caller is still unauthenticated: the token is what the pre-check
	// is about to read. The budget is therefore taken BEFORE the pool and not
	// after, so a caller over it never reaches one (NIM-839).
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

	tokenHash := bootstraptoken.HashToken(plainToken)

	// Cheap token pre-check BEFORE the Vault round trip (M3): a junk token
	// shouldn't trigger an expensive PKI sign. This is an optimization, not
	// the authority: the final anti-replay check is the Burn, under
	// FOR-UPDATE WHERE-clause semantics inside the transaction (step 7). Any
	// pre-check failure → a single PermissionDenied (anti-enum, we don't
	// distinguish not-found/expired/used).
	if err := h.precheckToken(ctx, tokenHash, sid); err != nil {
		return nil, err
	}

	// Vault PKI signing is a separate step BEFORE the transaction, but AFTER
	// the token pre-check. It's a network round trip with unpredictable latency;
	// there's no point holding a PG transaction open for it. Authoritative
	// token validation happens inside the transaction via Burn.
	signed, err := h.deps.VaultClient.SignCSR(ctx, h.deps.PKIMount, h.deps.PKIRole, string(csrPEM))
	if err != nil {
		return nil, h.mapVaultErr(err, sid)
	}
	cert, err := parseCertificatePEM(signed.CertificatePEM)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "vault returned invalid certificate: %v", err)
	}
	fingerprint := soulseed.FingerprintFromCert(cert)

	var (
		tokenID string
		seedID  string
	)
	err = pgx.BeginFunc(ctx, h.deps.Pool, func(tx pgx.Tx) error {
		tokID, burnErr := bootstraptoken.Burn(ctx, tx, tokenHash, sid, h.deps.KID)
		if burnErr != nil {
			return burnErr
		}
		tokenID = tokID

		if supErr := soulseed.SupersedeBySID(ctx, tx, sid); supErr != nil {
			return supErr
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
			return insErr
		}
		seedID = seed.SeedID

		kid := h.deps.KID
		if upErr := soul.UpdateStatus(ctx, tx, sid, soul.StatusConnected, &kid); upErr != nil {
			return upErr
		}
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

// precheckToken — cheap token check BEFORE Vault-sign (M3 early-reject).
// Reads the record by token_hash and checks (sid + not burned + not expired)
// in Go. Authority stays with Burn inside the transaction; this just filters
// out junk without a PKI round trip.
//
// Anti-enum: any failure (no record, wrong SID, expired, already used, junk
// hash format) → a single PermissionDenied, indistinguishable to the Soul by
// content or timing class. A transient DB read error (not ErrTokenNotFound)
// → Unavailable: the Soul retries, so junk still can't get through.
func (h *bootstrapHandler) precheckToken(ctx context.Context, tokenHash, sid string) error {
	rec, err := bootstraptoken.SelectByHash(ctx, h.deps.Pool, tokenHash)
	if err != nil {
		if errors.Is(err, bootstraptoken.ErrTokenNotFound) {
			return h.rejectToken(sid)
		}
		h.logger.Warn("bootstrap token pre-check read failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Unavailable, "bootstrap token pre-check failed")
	}
	if rec.SID != sid || !rec.IsActive(time.Now().UTC()) {
		return h.rejectToken(sid)
	}
	return nil
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
//   - ErrSoulNotFound       → FailedPrecondition (soul registry in an inconsistent state).
//   - everything else      → Internal, wrapping err.
func (h *bootstrapHandler) mapTxErr(err error, sid string) error {
	switch {
	case errors.Is(err, bootstraptoken.ErrTokenInvalid):
		// We don't distinguish "expired", "not found", "already used" — anti-enum
		// (same response as the step-3 pre-check).
		return h.rejectToken(sid)
	case errors.Is(err, soulseed.ErrSeedActiveExists):
		h.logger.Error("invariant violation: active seed present after Supersede",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Internal,
			"internal error: active seed already present for sid=%q", sid)
	case errors.Is(err, soul.ErrSoulNotFound):
		return status.Errorf(codes.FailedPrecondition,
			"soul %q not in registry (token Burn succeeded but UpdateStatus failed)", sid)
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
	csr, err := parseCSRPEM(csrPEM)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "csr_pem invalid: %v", err)
	}
	if csr.Subject.CommonName != sid {
		return status.Errorf(codes.InvalidArgument,
			"csr_pem common name does not match sid %q", sid)
	}
	return nil
}

// parseCSRPEM decodes the first CERTIFICATE REQUEST PEM block and parses it
// into an x509.CertificateRequest. The CSR signature isn't verified (Vault
// PKI does that during SignCSR); here we only need the Subject for CN validation.
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
