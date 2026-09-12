package grpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// SeedRotationDeps — wires up dependencies of the seed-rotation handler via
// EventStream (M2.5, ADR-012 / ADR-014).
//
// All fields are required:
//   - Pool — *pgxpool.Pool for a single transaction (`SupersedeBySID` + `Insert`);
//   - VaultClient — `SignCSR` to issue a new certificate;
//   - AuditWriter — `soul.seed-rotated` after the commit;
//   - Outbound — sends `SeedRotationReply` back over the same stream;
//   - KID / PKIMount / PKIRole — the same fields as [BootstrapDeps].
type SeedRotationDeps struct {
	Pool        TxBeginner
	VaultClient CSRSigner
	AuditWriter audit.Writer
	Outbound    *Outbound
	KID         string
	PKIMount    string
	PKIRole     string
}

func (d SeedRotationDeps) validate() error {
	if d.Pool == nil {
		return errors.New("grpc: SeedRotationDeps.Pool is required")
	}
	if d.VaultClient == nil {
		return errors.New("grpc: SeedRotationDeps.VaultClient is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: SeedRotationDeps.AuditWriter is required")
	}
	if d.Outbound == nil {
		return errors.New("grpc: SeedRotationDeps.Outbound is required")
	}
	if d.KID == "" {
		return errors.New("grpc: SeedRotationDeps.KID is required")
	}
	if d.PKIMount == "" {
		return errors.New("grpc: SeedRotationDeps.PKIMount is required")
	}
	if d.PKIRole == "" {
		return errors.New("grpc: SeedRotationDeps.PKIRole is required")
	}
	return nil
}

// SeedRotationLimit bounds how often a Soul may be served a new SoulSeed
// (NIM-839/NIM-840). Any field <=0 takes the matching default below.
//
// Two bounds, because one of them does not close the hole on its own.
// Per-SID is what stops the abuser or the host stuck in a retry loop — the
// handler runs inline on the stream, so serialisation already bounds its
// concurrency and only rate is left to bound. Global is what stops N hosts
// multiplying: each of them can be inside its own budget while together they
// saturate the Vault PKI mount that onboarding issues from, which would fail
// every other host's legitimate rotation as well.
type SeedRotationLimit struct {
	// PerSIDInterval is how long one SID waits per rotation once its burst is
	// spent; PerSIDBurst is how many it may take back to back.
	PerSIDInterval time.Duration
	PerSIDBurst    int

	// GlobalRate is rotations per second across every stream on this keeper;
	// GlobalBurst is the allowance on top of it.
	GlobalRate  float64
	GlobalBurst int
}

const (
	// A legitimate rotation loop runs on the certificate's lifetime — hours at
	// the least — so ten minutes between rotations is orders of magnitude of
	// headroom for it, and a hard stop for a loop that is retrying because
	// something upstream keeps failing.
	defaultSeedRotationPerSIDInterval = 10 * time.Minute
	defaultSeedRotationPerSIDBurst    = 3

	// Sized so a fleet-wide re-issue still finishes in minutes (10/s is 36k an
	// hour) while the Vault mount keeps the headroom onboarding needs.
	defaultSeedRotationGlobalRate  = 10
	defaultSeedRotationGlobalBurst = 64

	// seedRotationSweepAt — the per-SID map size at which refilled buckets are
	// dropped. The key is the mTLS-authenticated SID, so an unauthenticated
	// caller cannot grow the map at all; the sweep is only about not keeping an
	// entry per Soul that rotated once and left.
	seedRotationSweepAt = 1024

	// seedRotationSweepEvery / seedRotationGlobalLogEvery — the sweep cadence
	// and the global-refusal log throttle.
	//
	// Their own constants on purpose. Both were PerSIDInterval for one commit,
	// which quietly made an operator's per-host rate setting also decide how
	// often the fleet-wide warning appears and how often memory is reclaimed —
	// two things that have nothing to do with how fast one host may rotate, and
	// nothing in the field's name or doc to warn them.
	seedRotationSweepEvery     = 10 * time.Minute
	seedRotationGlobalLogEvery = time.Minute
)

// seedRotationLimiter is the per-SID and global pair described on
// [SeedRotationLimit]. A nil receiver allows everything — the handler is wired
// only when rotation is, and a unit build that does not exercise the limit
// should not have to construct one.
type seedRotationLimiter struct {
	interval time.Duration
	burst    int
	global   *rate.Limiter

	mu        sync.Mutex
	buckets   map[string]*sidRotationBucket
	lastSeen  time.Time
	lastSweep time.Time

	// globalEpisode throttles the log when the GLOBAL budget is what refused:
	// that refusal is about the fleet, not about the SID, and a line per SID
	// would be one per host per episode.
	globalEpisode time.Time
}

// sidRotationBucket is one SID's budget plus whether it is currently over it,
// so the log carries one line per episode instead of one per dropped request —
// a request dropped for costing too much should not then cost a disk write.
type sidRotationBucket struct {
	lim     *rate.Limiter
	limited bool
}

func newSeedRotationLimiter(cfg SeedRotationLimit) *seedRotationLimiter {
	interval := cfg.PerSIDInterval
	if interval <= 0 {
		interval = defaultSeedRotationPerSIDInterval
	}
	burst := cfg.PerSIDBurst
	if burst <= 0 {
		burst = defaultSeedRotationPerSIDBurst
	}
	globalRate := cfg.GlobalRate
	if globalRate <= 0 {
		globalRate = defaultSeedRotationGlobalRate
	}
	globalBurst := cfg.GlobalBurst
	if globalBurst <= 0 {
		globalBurst = defaultSeedRotationGlobalBurst
	}
	return &seedRotationLimiter{
		interval: interval,
		burst:    burst,
		global:   rate.NewLimiter(rate.Limit(globalRate), globalBurst),
		buckets:  make(map[string]*sidRotationBucket),
	}
}

// allow reports whether this SID may be served a rotation now, why not if not,
// and whether this is the first refusal of the current episode (the caller logs
// only then).
//
// **A refusal consumes nothing, from either budget.** Both are inspected with
// `TokensAt` — which reads without mutating — and only a request that clears
// BOTH spends a token from each. Taking the per-SID token first and then
// discovering the global budget is spent would have the global pressure eat the
// victim's own allowance: a host whose three legitimate rotations happened to
// land during a fleet-wide episode would then be refused for the next half hour
// over traffic that was never its own.
//
// The inspect-then-take is exact rather than approximate, but it rests on two
// facts worth naming because neither is obvious. `TokensAt(t)` and `AllowN(t,1)`
// run the same pure `advance(t)`, so asking twice is not a double-advance and
// the admitted set is identical — EXCEPT for a limiter with an infinite rate or
// a zero burst, which [newSeedRotationLimiter] cannot construct because it
// clamps every field positive. And both limiters are reachable only from this
// file, only under l.mu, so nothing can spend a token between the look and the
// take. Widen either — share `global`, read `Tokens()` for a metric — and the
// bug this replaced comes back silently, because `AllowN` reports refusal in a
// bool that this code has no reason to check.
func (l *seedRotationLimiter) allow(sid string, now time.Time) (allowed, global, firstRefusal bool) {
	if l == nil {
		return true, false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Clamp first: `now` reaches here from time.Now() outside this lock, so two
	// goroutines can arrive in the other order, and every use below — the sweep
	// cadence, the token arithmetic, the log throttle — wants one clock that
	// never goes backwards.
	if now.Before(l.lastSeen) {
		now = l.lastSeen
	}
	l.lastSeen = now

	b := l.buckets[sid]
	if b == nil {
		l.sweepLocked(now)
		b = &sidRotationBucket{lim: rate.NewLimiter(rate.Every(l.interval), l.burst)}
		l.buckets[sid] = b
	}

	switch {
	case b.lim.TokensAt(now) < 1:
		// This SID's own budget: one line per SID per episode.
		first := !b.limited
		b.limited = true
		return false, false, first
	case l.global.TokensAt(now) < 1:
		// The fleet's budget: the SID did nothing wrong, and a line per SID
		// would be a line per host. Throttled by wall time instead.
		first := l.globalEpisode.IsZero() || now.Sub(l.globalEpisode) >= seedRotationGlobalLogEvery
		if first {
			l.globalEpisode = now
		}
		return false, true, first
	}
	// Both were inspected above, under this same lock, against this same `now`
	// — neither can refuse here. The returns are dropped deliberately.
	b.lim.AllowN(now, 1)
	l.global.AllowN(now, 1)
	b.limited = false
	return true, false, false
}

// sweepLocked drops the SIDs whose budget has refilled — they have not rotated
// for burst×interval and their entry now says nothing a fresh one would not.
//
// Rate-limited to [seedRotationSweepEvery]: past the threshold the scan is O(n)
// under the lock every rotation serialises on, and for a fleet larger than the
// threshold it would run on every new-SID miss and delete nothing. The cost is
// that the resident set is now "the fleet, plus every SID seen since the last
// sweep" — still bounded by real authenticated hosts, just less tightly.
func (l *seedRotationLimiter) sweepLocked(now time.Time) {
	if len(l.buckets) < seedRotationSweepAt {
		return
	}
	// Never more often than a bucket can become evictable. The predicate below
	// is a FULL refill, which takes burst×interval of silence — sweeping faster
	// than that scans the whole map under the lock and deletes nothing.
	every := max(seedRotationSweepEvery, time.Duration(l.burst)*l.interval)
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < every {
		return
	}
	l.lastSweep = now
	for sid, b := range l.buckets {
		if b.lim.TokensAt(now) >= float64(l.burst) {
			delete(l.buckets, sid)
		}
	}
}

// handleSeedRotationRequest — handles an inbound `SeedRotationRequest`
// (M2.5).
//
// Flow (symmetric to [bootstrapHandler.Bootstrap], but without a burn token):
//  1. Vault PKI SignCSR — issue a new cert for the public key from the CSR.
//  2. Tx BEGIN.
//  3. SupersedeBySID(sid) — previous active → superseded.
//  4. Insert the new active seed.
//  5. COMMIT.
//  6. Outbound.SendSeedRotationReply → new cert + ca_chain + not_after.
//  7. Audit `soul.seed-rotated` (correlation_id = the new seed_id).
//
// PM-decision M2.5(4) — idempotent: if the reply isn't delivered (queue full /
// stream dropped in the window between commit and Send), the operator/Soul can
// retry through the same logic: new CSR → new Seed, the old one
// simply becomes another superseded seed.
//
// Doesn't fatal the stream: on any error we log + skip, on the design that the
// Soul-side rotation loop retries on its own interval. **That loop does not
// exist yet** — nothing under `soul/` sends a SeedRotationRequest, and
// `soul/cmd/soul` logs the reply as ignored — so "the Soul will retry" is the
// contract this handler is written to, not an observed fact. Recovery from a
// drop has no consumer to exercise it until that loop is built.
//
// Over the [SeedRotationLimit] budget the request is dropped the same way, and
// before the Vault signature and the transaction it would cost. Not answered:
// SeedRotationReply has no field for a refusal (three fields, all payload), and
// an empty one would read as a rotation that succeeded and issued nothing.
func (h *eventStreamHandler) handleSeedRotationRequest(ctx context.Context, sid, sessionID string, req *keeperv1.SeedRotationRequest) {
	deps := h.deps.SeedRotation
	if deps == nil {
		// SeedRotation disabled on this Keeper instance (separate wire-up).
		h.logger.Warn("eventstream: SeedRotationRequest received but rotation not wired up",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if req == nil {
		h.logger.Warn("eventstream: SeedRotationRequest payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	csrPEM := req.GetCsrPem()
	if len(csrPEM) == 0 {
		h.logger.Warn("eventstream: SeedRotationRequest with empty CSR",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	// The rotation CSR's CN must match the stream's SID (defense-in-depth BEFORE
	// Vault SignCSR). sid here is authoritative — taken from the mTLS peer cert, not
	// from the payload; a CSR with a foreign/empty CN is rejected, without relying on the
	// broad allowed_domains of the Vault PKI role. Doesn't fatal the stream (like other
	// errors on the rotation path): warn + skip, the Soul-side loop will retry.
	if err := validateCSRCommonName(csrPEM, sid); err != nil {
		h.logger.Warn("eventstream: seed-rotation CSR common name mismatch",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}

	// After the free checks, before the Vault round trip and the transaction:
	// a malformed request must not spend budget a legitimate rotation needs.
	if allowed, global, firstRefusal := h.seedRotation.allow(sid, time.Now()); !allowed {
		if firstRefusal {
			scope := "per-sid"
			if global {
				scope = "global"
			}
			h.logger.Warn("eventstream: seed-rotation over budget — dropping requests",
				slog.String("scope", scope),
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
			)
		}
		return
	}

	signed, err := deps.VaultClient.SignCSR(ctx, deps.PKIMount, deps.PKIRole, string(csrPEM))
	if err != nil {
		h.logger.Warn("eventstream: seed-rotation vault SignCSR failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	cert, err := parseCertificatePEM(signed.CertificatePEM)
	if err != nil {
		h.logger.Error("eventstream: seed-rotation vault returned invalid cert",
			slog.String("sid", sid), slog.Any("error", err))
		return
	}
	fingerprint := soulseed.FingerprintFromCert(cert)

	var newSeedID string
	txErr := pgx.BeginFunc(ctx, deps.Pool, func(tx pgx.Tx) error {
		if supErr := soulseed.SupersedeBySID(ctx, tx, sid); supErr != nil {
			return supErr
		}
		seed := &soulseed.SoulSeed{
			SID:          sid,
			Fingerprint:  fingerprint,
			SerialNumber: signed.SerialNumber,
			ExpiresAt:    signed.NotAfter,
			IssuedByKID:  &deps.KID,
			Status:       soulseed.StatusActive,
		}
		if insErr := soulseed.Insert(ctx, tx, seed); insErr != nil {
			return insErr
		}
		newSeedID = seed.SeedID
		return nil
	})
	if txErr != nil {
		// ErrSeedActiveExists — impossible (SupersedeBySID ran in the same tx).
		// The rest — transient / DB misbehavior; skip, the Soul will retry.
		h.logger.Warn("eventstream: seed-rotation tx failed",
			slog.String("sid", sid),
			slog.Any("error", txErr),
		)
		return
	}

	// Send reply BEFORE the audit write: the Soul is waiting for a reply — this is the
	// "hot path" of rotation, audit is written best-effort afterward.
	reply := &keeperv1.SeedRotationReply{
		CertificatePem: signed.CertificatePEM,
		CaChainPem:     signed.CAChainPEM,
		NotAfter:       timestamppb.New(signed.NotAfter),
	}
	if sendErr := deps.Outbound.SendSeedRotationReply(ctx, sid, reply); sendErr != nil {
		h.logger.Warn("eventstream: seed-rotation reply send failed (DB committed)",
			slog.String("sid", sid),
			slog.String("seed_id", newSeedID),
			slog.Any("error", sendErr),
		)
		// No return — audit still records the fact that the seed was issued.
	}

	if writeErr := deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventSoulSeedRotated,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: newSeedID,
		Payload: map[string]any{
			"sid":           sid,
			"seed_id":       newSeedID,
			"fingerprint":   fingerprint,
			"serial_number": signed.SerialNumber,
			"issued_at":     time.Now().UTC(),
			"not_after":     signed.NotAfter,
			"kid":           deps.KID,
		},
		CreatedAt: time.Now().UTC(),
	}); writeErr != nil {
		h.logger.Warn("eventstream: audit write soul.seed-rotated failed (DB committed)",
			slog.String("sid", sid),
			slog.String("seed_id", newSeedID),
			slog.Any("error", writeErr),
		)
	}

	h.logger.Info("eventstream: soul seed rotated",
		slog.String("sid", sid),
		slog.String("seed_id", newSeedID),
		slog.String("fingerprint", fingerprint),
		slog.String("session_id", sessionID),
	)
}
