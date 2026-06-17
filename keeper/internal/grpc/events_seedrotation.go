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

// SeedRotationDeps — wire-up зависимости handler-а ротации seed-а через
// EventStream (M2.5, ADR-012 / ADR-014).
//
// Все поля обязательны:
//   - Pool — *pgxpool.Pool для одной транзакции (`SupersedeBySID` + `Insert`);
//   - VaultClient — `SignCSR` для выпуска нового сертификата;
//   - AuditWriter — `soul.seed-rotated` после commit-а;
//   - Outbound — Send `SeedRotationReply` обратно по тому же стриму;
//   - KID / PKIMount / PKIRole — те же поля, что у [BootstrapDeps].
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

// handleSeedRotationRequest — обработка inbound-а `SeedRotationRequest`
// (M2.5).
//
// Поток (симметричен [bootstrapHandler.Bootstrap], но без burn-токена):
//  1. Vault PKI SignCSR — выпуск нового cert на public key из CSR.
//  2. Tx BEGIN.
//  3. SupersedeBySID(sid) — предыдущий active → superseded.
//  4. Insert новый active-seed.
//  5. COMMIT.
//  6. Outbound.SendSeedRotationReply → новый cert + ca_chain + not_after.
//  7. Audit `soul.seed-rotated` (correlation_id = новый seed_id).
//
// PM-decision M2.5(4) — idempotent: если reply не доставлен (queue full /
// stream упал в окне между commit-ом и Send-ом), оператор/Soul могут
// повторить через ту же логику: новый CSR → новый Seed, старый-новый
// просто станет ещё одним superseded.
//
// Не fatal-ит стрим: на любую ошибку логируем + пропускаем. Soul-side
// rotation-loop повторит через свой интервал.
func (h *eventStreamHandler) handleSeedRotationRequest(ctx context.Context, sid, sessionID string, req *keeperv1.SeedRotationRequest) {
	deps := h.deps.SeedRotation
	if deps == nil {
		// SeedRotation disabled на этом Keeper-инстансе (отдельный wire-up).
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
	// CN ротационного CSR обязан совпадать с SID стрима (defense-in-depth ДО
	// Vault SignCSR). sid тут авторитетен — взят из mTLS peer-cert, не из
	// payload; CSR с чужим/пустым CN отвергаем, не полагаясь на широкий
	// allowed_domains Vault PKI role. Не fatal-ит стрим (как и прочие ошибки
	// rotation-пути): warn + skip, Soul-side loop повторит.
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
		// ErrSeedActiveExists — невозможен (SupersedeBySID отработал в той же tx).
		// Остальные — transient / DB-misbehavior; пропускаем, Soul retry-нет.
		h.logger.Warn("eventstream: seed-rotation tx failed",
			slog.String("sid", sid),
			slog.Any("error", txErr),
		)
		return
	}

	// Send reply ПЕРЕД audit-write-ом: Soul ждёт ответ — это «горячий
	// путь» рoтации, audit пишется best-effort после.
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
		// Не return — audit всё равно фиксирует факт выпуска seed-а.
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
