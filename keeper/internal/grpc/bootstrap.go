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

// TxBeginner — узкий интерфейс над *pgxpool.Pool (только Begin для
// связки атомарной транзакции через [pgx.BeginFunc]). Позволяет
// mock-ать в unit-тестах без поднятия реального PG; production-импл —
// `*pgxpool.Pool`.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// BootstrapPool — расширение [TxBeginner] для handler-а онбординга: помимо
// Begin нужен read-доступ (через [bootstraptoken.ExecQueryRower]) для дешёвой
// pre-check токена ДО Vault-round-trip-а — early-reject мусорного токена без
// вызова PKI (M3). Production-импл — `*pgxpool.Pool` (реализует оба).
type BootstrapPool interface {
	TxBeginner
	bootstraptoken.ExecQueryRower
}

// CSRSigner — узкий интерфейс над [keepervault.Client.SignCSR]. Симметричен
// [TxBeginner]: handler зависит от метода, не от конкретного клиента.
type CSRSigner interface {
	SignCSR(ctx context.Context, mount, role, csrPEM string) (*keepervault.SignedCertificate, error)
}

// BootstrapDeps — wire-up зависимости handler-а онбординга.
//
// Все поля обязательны: pool — транзакция «burn token + supersede seed +
// insert seed + flip status»; VaultClient.SignCSR — подпись CSR через
// Vault PKI; AuditWriter — `soul.bootstrapped` + `soul.seed-issued`.
//
// KID — идентификатор keeper-инстанса; пишется в `bootstrap_tokens.used_by_kid`
// и `souls.last_seen_by_kid`, появляется в audit-payload-е.
// PKIMount / PKIRole — из `keeper.yml::vault.{pki_mount,pki_role}`.
type BootstrapDeps struct {
	Pool        BootstrapPool
	VaultClient CSRSigner
	AuditWriter audit.Writer
	KID         string
	PKIMount    string
	PKIRole     string

	// Metrics — keeper_grpc_*-collectors (ADR-024). nil → bootstrap-метрика
	// выключена (nil-safe методы [GRPCMetrics] — no-op). Должен быть тем же
	// дескриптором, что в [OutboundDeps.Metrics] / [EventStreamDeps.Metrics]
	// (один Registry).
	Metrics *GRPCMetrics

	// SigilAnchorSource — ЖИВОЙ источник набора trust-anchor-ов подписи Sigil в
	// PEM-форме (ADR-026(h), R3-S7, architect af7d). Читается при КАЖДОМ
	// Bootstrap-reply (а НЕ снимок старта): после runtime-ротации ключей подписи
	// (Introduce/SetPrimary/Retire → cluster reload R3-S6 обновляет holder) новый
	// Soul при онбординге получает АКТУАЛЬНЫЙ набор. Без этого окно между bootstrap
	// и connect отдавало бы устаревший набор — опасно при Retire (новый Soul
	// доверял бы уже выведенному ключу либо отвергал свежий primary).
	//
	// Из набора берётся и одиночный [keeperv1.BootstrapReply.SigilPubkeyPem]
	// (legacy single-anchor для старого Soul-а) — первый элемент (primary первым),
	// и полный [keeperv1.BootstrapReply.SigilPubkeyPemSet] (R3-S4 читает set>single).
	//
	// nil ИЛИ пустой набор = Sigil не настроен/выключен — оба поля reply остаются
	// пустыми, verify на Soul-е выключен (bootstrap-flow без Sigil-а как раньше).
	// Реализация в daemon — atomic-holder (trustAnchorHolder), обновляемый
	// watcher-ом `sigil:anchors-changed`.
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

// bootstrapHandler — реализация [keeperv1.KeeperServer] для Bootstrap-listener-а.
//
// EventStream вшит как Unimplemented через embedded [keeperv1.UnimplementedKeeperServer]:
// на Bootstrap-listener-е (server-only TLS) долгоживущий стрим не
// запускается, у Soul-а ещё нет клиентского сертификата.
type bootstrapHandler struct {
	keeperv1.UnimplementedKeeperServer
	deps   BootstrapDeps
	logger *slog.Logger
}

func newBootstrapHandler(deps BootstrapDeps, logger *slog.Logger) *bootstrapHandler {
	return &bootstrapHandler{deps: deps, logger: logger}
}

// Ping — health-check RPC, доступен без авторизации (server-only TLS уже
// сам по себе ограничивает caller-ов).
func (h *bootstrapHandler) Ping(_ context.Context, _ *keeperv1.PingRequest) (*keeperv1.PingReply, error) {
	return &keeperv1.PingReply{Version: h.deps.KID}, nil
}

// Bootstrap — реализация unary RPC онбординга по [docs/soul/onboarding.md].
//
// Поток:
//  1. Validate (SID format, token_hash format, CSR PEM ненулевой).
//  2. Hash plain-token → token_hash.
//  3. Cheap pre-check токена (SelectByHash, без Burn) — early-reject мусора
//     ДО дорогого Vault-round-trip-а (M3). Anti-enum: любой провал → одна
//     PermissionDenied, неотличимая от not-found/expired/used.
//  4. Vault PKI SignCSR — выпуск сертификата (только для прошедшего pre-check).
//  5. Parse cert → compute fingerprint (SHA-256 SubjectPublicKeyInfo).
//  6. Tx BEGIN.
//  7. Burn token (race-safe UPDATE с WHERE used_at IS NULL) — authoritative
//     anti-replay-чек под нагрузкой (pre-check на шаге 3 — оптимизация, не
//     замена: TOCTOU между select и burn закрыт именно этим UPDATE-ом).
//  8. Supersede предыдущий active-seed (no-op для нового Soul-а).
//  9. Insert новый active-seed.
//
// 10. UpdateStatus soul: pending → connected, last_seen_by_kid = KID.
// 11. COMMIT.
// 12. Audit: `soul.bootstrapped` + `soul.seed-issued` (один correlation_id = token_id).
//
// Все ошибки до Vault — fail-fast с rollback-ом. Vault-ошибка → tx
// rollback + Unavailable (transient — Soul retry-нет). Audit пишется
// **после** commit-а; failure аудита логируется warn-ом, но не
// отменяет онбординг (БД консистентна, audit gap — отдельный manual-fix).
func (h *bootstrapHandler) Bootstrap(ctx context.Context, req *keeperv1.BootstrapRequest) (reply *keeperv1.BootstrapReply, err error) {
	// In-process span на единицу онбординга. sid — атрибут для фильтрации
	// трейса (в metric-labels запрещён — cardinality, ADR-024 §2.2); секретов
	// (токен / CSR) не несёт. Метрика bootstrap_total фиксируется по факту
	// исхода (err==nil → ok). При OTel disabled tracer no-op — span бесплатен.
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
	// Зарезервированные sid (keeper / __run__) — синтетика прогона, не Soul (NIM-36).
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
	// CSR CommonName обязан совпадать с запрашиваемым SID (defense-in-depth,
	// crypto). Authority онбординга якорится на registry-fingerprint, не на CN,
	// но проверка CN ДО Vault SignCSR не даёт полагаться только на Vault PKI
	// role-конфиг оператора (allowed_domains мог бы быть шире SID-а). Невалидный
	// CN → InvalidArgument ДО PKI-round-trip-а.
	if err := validateCSRCommonName(csrPEM, sid); err != nil {
		return nil, err
	}
	tokenHash := bootstraptoken.HashToken(plainToken)

	// Дешёвый pre-check токена ДО Vault-round-trip-а (M3): мусорный токен
	// не должен триггерить дорогой PKI-sign. Это оптимизация, не authority:
	// финальный anti-replay-чек — Burn под FOR-UPDATE-семантикой WHERE-clause
	// внутри транзакции (шаг 7). Любой провал pre-check-а → одна
	// PermissionDenied (anti-enum, не различаем not-found/expired/used).
	if err := h.precheckToken(ctx, tokenHash, sid); err != nil {
		return nil, err
	}

	// Подписание Vault PKI — отдельным шагом ДО транзакции, но ПОСЛЕ
	// pre-check-а токена. Это сетевой round-trip с непредсказуемой latency,
	// не имеет смысла держать PG-транзакцию открытой на него. Authoritative
	// token validation выполнится внутри транзакции через Burn.
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

	// Audit — после commit. Один correlation_id = token_id связывает
	// soul.bootstrapped и soul.seed-issued (по docs/keeper/audit.md).
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

// applySigilAnchors заполняет Sigil trust-anchor-поля reply ЖИВЫМ набором из
// [TrustAnchorSource] (ADR-026(h), R3-S7): набор читается при каждом reply, не
// снимок старта — свежеонбордящийся Soul после ротации получает актуальный
// набор. Пустой набор (Sigil выключен или source nil) → оба поля остаются nil,
// bootstrap-контракт обратносовместим. Вынесено отдельным методом для unit-теста
// «после SetAnchors новый reply несёт новый набор».
func (h *bootstrapHandler) applySigilAnchors(out *keeperv1.BootstrapReply) {
	if h.deps.SigilAnchorSource == nil {
		return
	}
	anchors := h.deps.SigilAnchorSource.AnchorSetPEM()
	if len(anchors) == 0 {
		return
	}
	// Multi-anchor набор (R3-S4 читает set > single). Копию не делаем: holder
	// отдаёт read-only снимок, reply-сериализация его не мутирует.
	out.SigilPubkeyPemSet = anchors
	// Единичный legacy-якорь для старого Soul-а — первый элемент набора (primary
	// первым, см. AnchorSetPEM); тоже из живого источника.
	single := anchors[0]
	out.SigilPubkeyPem = &single
}

// precheckToken — дешёвая проверка токена ДО Vault-sign-а (M3 early-reject).
// Читает запись по token_hash и проверяет (sid + не сожжён + не истёк) в Go.
// Authority остаётся за Burn-ом в транзакции; здесь — отсев мусора без
// PKI-round-trip-а.
//
// Anti-enum: любой провал (нет записи, чужой SID, истёк, уже использован,
// мусорный hash-формат) → одна PermissionDenied, неотличимая для Soul-а от
// остальных по содержимому и timing-классу. Транзиентная ошибка чтения БД
// (не ErrTokenNotFound) → Unavailable: Soul ретрайнет, мусор так не пройдёт.
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

// rejectToken — единый anti-enum-ответ на невалидный токен (как из
// pre-check-а, так и из Burn-а через [mapTxErr]). Soul видит одну причину
// и не различает not-found / expired / used / wrong-SID по timing-у.
func (h *bootstrapHandler) rejectToken(sid string) error {
	return status.Errorf(codes.PermissionDenied,
		"bootstrap token rejected for sid=%q", sid)
}

// mapTxErr — мапит CRUD-sentinel в gRPC status:
//   - ErrTokenInvalid       → PermissionDenied (anti-enum: ничего не различаем).
//   - ErrSeedActiveExists   → Internal (нарушение инварианта SupersedeBySID).
//   - ErrSoulNotFound       → FailedPrecondition (soul-registry в неконсистентном состоянии).
//   - всё прочее            → Internal с обернутым err.
func (h *bootstrapHandler) mapTxErr(err error, sid string) error {
	switch {
	case errors.Is(err, bootstraptoken.ErrTokenInvalid):
		// Не различаем «истёк», «не найден», «уже использован» — anti-enum
		// (тот же ответ, что и pre-check на шаге 3).
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

// mapVaultErr — sign-фаза, ДО транзакции. Vault transient-failures
// (network, 5xx) → Unavailable; misconfig (bad role, bad mount) →
// FailedPrecondition; bad CSR → InvalidArgument. По sentinel-кодам
// keepervault.ErrPKI* различаем дифференцированно.
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
		// Без sentinel-а — transient by default (Soul retry-нет).
		h.logger.Warn("vault PKI sign failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Errorf(codes.Unavailable, "vault PKI sign failed: %v", err)
	}
}

// validateCSRCommonName парсит CSR из PEM и проверяет, что его
// Subject.CommonName совпадает с запрашиваемым `sid` (defense-in-depth ДО
// Vault SignCSR). Возвращает gRPC-status:
//   - невалидный/пустой PEM или непарсящийся CSR → InvalidArgument
//     (мусорный ввод, отказ ДО PKI);
//   - CN ≠ sid (включая пустой CN) → InvalidArgument с anti-enum-нейтральным
//     текстом (CN не эхо-ится в reply — не отдаём подсказку, что именно
//     запрошено).
//
// Якорь авторизации остаётся за registry-fingerprint-ом (этой проверкой не
// подменяется); она лишь не даёт онбордить cert под чужим CN, опираясь только
// на широкий allowed_domains Vault-role.
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

// parseCSRPEM декодирует первый PEM-блок CERTIFICATE REQUEST и парсит его в
// x509.CertificateRequest. Подпись CSR не проверяется (Vault PKI делает это при
// SignCSR); здесь нужен только Subject для CN-валидации.
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

// parseCertificatePEM декодирует первый PEM-блок CERTIFICATE и парсит
// его в x509.Certificate. Vault PKI выдаёт ровно один блок —
// дополнительные блоки (которых не должно быть) игнорируются.
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

// peerAddr — best-effort извлечение remote-адреса для лог-полей. Пустая
// строка если peer не передан (тестовая среда / unix-socket).
func peerAddr(ctx context.Context) string {
	if p, ok := grpcpeer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
