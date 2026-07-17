package cert

import (
	"fmt"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyIssued реализует state `core.cert.issued` (NIM-99 Slice C): Keeper САМ
// выпускает серверный TLS-серт инкарнации — генерит keypair+CSR, подписывает
// через Vault PKI ролью ИЗ МАНИФЕСТА (не из params), кладёт cert+key в Vault по
// E3-путям и регистрирует active-строки Warrant (cert + key-спутник).
//
// БЕЗОПАСНОСТЬ (R2-инвариант): приватник генерится Keeper-ом и живёт только в
// Vault — в output/audit/текст ошибки НЕ попадает (certissue уже это гарантирует,
// мы его тоже наружу не кладём).
func applyIssued(m *Module, req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if m.Signer == nil || m.VaultWriter == nil || m.Policy == nil || m.CSRGen == nil || m.PKIMount == nil {
		return util.SendFailed(stream, "core.cert.issued: модуль не сконфигурирован (нет PKI-signer/policy)")
	}

	incarnation, err := util.StringParam(req.Params, "incarnation")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// auto_rotate — флаг enroll в авто-ротацию Reaper; default да.
	autoRotate, hasAuto, err := util.OptBoolParam(req.Params, "auto_rotate")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasAuto {
		autoRotate = true
	}

	pol, err := m.Policy.Resolve(ctx, incarnation)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !pol.Enabled {
		return util.SendFailed(stream, "certificate_rotation отсутствует/выключен для сервиса — выпуск невозможен")
	}
	if pol.PKIRole == "" {
		return util.SendFailed(stream, "certificate_rotation: pki_role не задан в манифесте — выпуск невозможен")
	}
	// Fail-fast на несуществующий сценарий (зеркало reaper-проверки членства): иначе
	// серт энроллится с auto_rotate=true, а ротатор молча его скипает → тихое истечение.
	if pol.Scenario == "" || !scenarioKnown(pol.Scenario, pol.KnownScenarios) {
		return util.SendFailed(stream, fmt.Sprintf("core.cert.issued: сценарий ротации %q не найден в сервисе (объяви scenario/%s/ или поправь certificate_rotation.scenario)", pol.Scenario, pol.Scenario))
	}

	mount := m.PKIMount()

	// Роль подписи — pol.PKIRole (МАНИФЕСТ), пути Vault — по service из политики.
	mat, err := certissue.Issue(ctx, m.Signer, m.VaultWriter, m.CSRGen, certissue.Params{
		CommonName: incarnation + ".tls",
		DNSNames:   []string{incarnation + ".tls", incarnation},
		Mount:      mount,
		Role:       pol.PKIRole,
		CertPath:   certissue.VaultPath(pol.Service, incarnation, keepercert.KindCert),
		KeyPath:    certissue.VaultPath(pol.Service, incarnation, keepercert.KindKey),
	})
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	certWarrant := &keepercert.Warrant{
		IncarnationID: incarnation,
		Kind:          keepercert.KindCert,
		VaultRef:      mat.CertRef,
		SerialNumber:  mat.SerialNumber,
		Fingerprint:   mat.Fingerprint,
		NotAfter:      mat.NotAfter,
		Status:        keepercert.StatusActive,
		AutoRotate:    autoRotate,
		PKIMount:      &mount,
		PKIRole:       &pol.PKIRole,
	}
	if m.KID != "" {
		certWarrant.IssuedByKID = &m.KID
	}
	// cert+key в разных tx; reaper драйвит от cert-строки, рассинхрон самолечится на повторе (review M3)
	if regErr := m.Store.RegisterActive(ctx, certWarrant); regErr != nil {
		return util.SendFailed(stream, fmt.Sprintf("register cert: %v", regErr))
	}

	// key-спутник: приватник обновился вместе с cert; сам НЕ драйвер ротации
	// (auto_rotate=false — его не сканирует Reaper). Тот же fingerprint/serial/
	// not_after (одна пара).
	keyWarrant := &keepercert.Warrant{
		IncarnationID: incarnation,
		Kind:          keepercert.KindKey,
		VaultRef:      mat.KeyRef,
		SerialNumber:  mat.SerialNumber,
		Fingerprint:   mat.Fingerprint,
		NotAfter:      mat.NotAfter,
		Status:        keepercert.StatusActive,
		AutoRotate:    false,
	}
	if m.KID != "" {
		keyWarrant.IssuedByKID = &m.KID
	}
	if regErr := m.Store.RegisterActive(ctx, keyWarrant); regErr != nil {
		return util.SendFailed(stream, fmt.Sprintf("register key: %v", regErr))
	}

	// Audit best-effort (nil-audit ок); только НЕ-секретные метаданные.
	if m.Audit != nil {
		ev := &audit.Event{
			EventType:     audit.EventCertIssued,
			Source:        audit.SourceKeeperInternal,
			CorrelationID: incarnation,
			Payload: map[string]any{
				"incarnation":   incarnation,
				"kind":          string(keepercert.KindCert),
				"fingerprint":   mat.Fingerprint,
				"serial_number": mat.SerialNumber,
				"not_after":     mat.NotAfter.UTC().Format(time.RFC3339),
			},
		}
		_ = m.Audit.Write(ctx, ev)
	}

	out := map[string]any{
		"registered_kinds": []any{string(keepercert.KindCert), string(keepercert.KindKey)},
		"fingerprint":      mat.Fingerprint,
		"serial_number":    mat.SerialNumber,
		"not_after":        mat.NotAfter.UTC().Format(time.RFC3339),
	}
	return util.SendFinal(stream, true, out)
}

// scenarioKnown — членство сценария среди scenario/ снапшота (зеркало reaper-проверки).
func scenarioKnown(scenario string, known []string) bool {
	for _, s := range known {
		if s == scenario {
			return true
		}
	}
	return false
}
