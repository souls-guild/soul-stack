package grpc

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
}

func newBootstrapHandler(deps BootstrapDeps, logger *slog.Logger) *bootstrapHandler {
	return &bootstrapHandler{deps: deps, logger: logger}
}

// Ping — health-check RPC, available without authorization (server-only TLS
// already restricts callers on its own).
func (h *bootstrapHandler) Ping(_ context.Context, _ *keeperv1.PingRequest) (*keeperv1.PingReply, error) {
	return &keeperv1.PingReply{Version: h.deps.KID}, nil
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
