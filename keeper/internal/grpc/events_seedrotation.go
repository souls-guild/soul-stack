package grpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
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
// Doesn't fatal the stream: on any error we log + skip. The Soul-side
// rotation loop will retry on its own interval.
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
	// “hot path” of rotation, audit is written best-effort afterward.
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
