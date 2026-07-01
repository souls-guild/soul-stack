// Package cert реализует keeper-side core-модуль `core.cert.registered`
// (cert-rotation Вар1, E1) — регистрирует СЕРВИСНЫЙ TLS-серт инкарнации в
// реестре Warrant (keeper/internal/cert), чтобы Reaper-правило `rotate_due_certs`
// видело срок его действия (`not_after`) и могло ротировать централизованно.
//
// Состояние:
//   - registered: декларативная форма «серт(ы) инкарнации зарегистрированы в
//     Warrant по своему сроку действия». Без этого шага Reaper слеп к ПЕРВИЧНЫМ
//     сертам (выданным оператором или в create-сценарии) — регистрация должна
//     быть частью create/rotate_tls-сценария.
//
// Механика: для каждого cert из `certs[]` модуль ЧИТАЕТ PEM из Vault по
// `vault_ref` (форма `<mount>/<path>#<field>`), парсит x509 и извлекает
// serial_number / fingerprint / not_after ИЗ САМОГО серта (автономно — не
// требует, чтобы сценарий доставал метаданные PKI). Затем регистрирует active-
// строку Warrant (supersede предыдущего active той же kind + insert нового в
// одной tx).
//
// ИДЕМПОТЕНТНОСТЬ: если active-строка с тем же fingerprint уже есть — no-op
// (changed=false). Новый fingerprint (сменился серт) → supersede+insert,
// changed=true.
//
// БЕЗОПАСНОСТЬ (ADR-010): в output/audit кладутся только НЕ-секретные метаданные
// (kind + fingerprint + serial + not_after); сам PEM / приватник наружу не идут
// (модуль читает лишь ПУБЛИЧНЫЙ серт, приватник не трогает).
package cert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.cert.registered` (base + state, как core-модули
// keeper-side); state `registered` приходит в pluginv1.ApplyRequest.state.
const Name = "core.cert"

// StateRegistered — единственное состояние модуля.
const StateRegistered = "registered"

// VaultReader — узкая поверхность vault.Client, нужная модулю: чтение KV-пути
// для извлечения PEM серта. Сужение упрощает unit-тест (fake без HTTP).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// Store — узкая транзакционная поверхность реестра Warrant: регистрация active-
// строки (supersede+insert в tx) + чтение текущего active (idempotency-чек).
// Прод — [PGStore] поверх *pgxpool.Pool.
type Store interface {
	SelectActive(ctx context.Context, incarnationID string, kind keepercert.Kind) (*keepercert.Warrant, error)
	RegisterActive(ctx context.Context, w *keepercert.Warrant) error
}

// AuditWriter — узкая зависимость для audit-event-а `cert.registered`.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module — реализация sdk/module.SoulModule. Один модуль на base-имя `core.cert`.
type Module struct {
	Vault VaultReader
	Store Store
	Audit AuditWriter
	// KID — идентификатор Keeper-инстанса, пишется в warrant.issued_by_kid
	// (audit «какой инстанс зарегистрировал серт»). Пустой → NULL.
	KID string
}

// New — wire-helper.
func New(v VaultReader, s Store, a AuditWriter, kid string) *Module {
	return &Module{Vault: v, Store: s, Audit: a, KID: kid}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != StateRegistered {
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s)", req.State, StateRegistered))
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	if _, err := util.StringParam(req.Params, "incarnation"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := parseCertTargets(req); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// certTarget — одна цель регистрации: kind + Vault-ref на PEM.
type certTarget struct {
	kind     keepercert.Kind
	vaultRef string
}

// parseCertTargets разбирает `params.certs` — непустой список
// `{kind: cert|key|ca, vault_ref: <str>}`. Пустой/отсутствующий — ошибка.
func parseCertTargets(req *pluginv1.ValidateRequest) ([]certTarget, error) {
	return parseCertTargetsFromStruct(req.Params)
}

// parseCertTargetsFromStruct — общий разбор `certs[]` для Validate/Apply (у их
// Request-типов разные обёртки, но одинаковое поле .Params → *structpb.Struct).
func parseCertTargetsFromStruct(params *structpb.Struct) ([]certTarget, error) {
	raw, err := util.ListParam(params, "certs")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("param \"certs\": empty list")
	}
	out := make([]certTarget, 0, len(raw))
	for i, item := range raw {
		sv := item.GetStructValue()
		if sv == nil {
			return nil, fmt.Errorf("param \"certs\"[%d]: expected object", i)
		}
		kindStr, kerr := util.StringParam(sv, "kind")
		if kerr != nil {
			return nil, fmt.Errorf("param \"certs\"[%d].%v", i, kerr)
		}
		kind := keepercert.Kind(kindStr)
		if kind != keepercert.KindCert && kind != keepercert.KindKey && kind != keepercert.KindCA {
			return nil, fmt.Errorf("param \"certs\"[%d].kind: invalid %q (want cert/key/ca)", i, kindStr)
		}
		ref, rerr := util.StringParam(sv, "vault_ref")
		if rerr != nil {
			return nil, fmt.Errorf("param \"certs\"[%d].%v", i, rerr)
		}
		if ref == "" {
			return nil, fmt.Errorf("param \"certs\"[%d].vault_ref: empty", i)
		}
		out = append(out, certTarget{kind: kind, vaultRef: ref})
	}
	return out, nil
}

// Apply читает PEM каждого cert из Vault, извлекает метаданные и регистрирует
// active-строку Warrant (idempotent по fingerprint). changed=true если хотя бы
// один серт был вписан (новый / сменившийся fingerprint).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != StateRegistered {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}

	incarnation, err := util.StringParam(req.Params, "incarnation")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	targets, err := parseCertTargetsApply(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Опц. pki-метаданные (для аудита происхождения серта; в предикат ротации не
	// входят). auto_rotate default true.
	pkiMount, err := util.OptStringParam(req.Params, "pki_mount")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	pkiRole, err := util.OptStringParam(req.Params, "pki_role")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	autoRotate, hasAuto, err := util.OptBoolParam(req.Params, "auto_rotate")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasAuto {
		autoRotate = true
	}

	var registered []map[string]any // не-секретные метаданные вписанных сертов
	anyChanged := false

	for _, t := range targets {
		meta, rerr := m.readCertMeta(ctx, t.vaultRef)
		if rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cert %s (%s): %v", t.kind, t.vaultRef, rerr))
		}

		// Idempotency: active-строка с тем же fingerprint → no-op.
		cur, cerr := m.Store.SelectActive(ctx, incarnation, t.kind)
		if cerr != nil && !errors.Is(cerr, keepercert.ErrNotFound) {
			return util.SendFailed(stream, fmt.Sprintf("cert %s: read active: %v", t.kind, cerr))
		}
		if cur != nil && cur.Fingerprint == meta.fingerprint {
			continue // тот же серт уже зарегистрирован
		}

		w := &keepercert.Warrant{
			IncarnationID: incarnation,
			Kind:          t.kind,
			VaultRef:      t.vaultRef,
			SerialNumber:  meta.serial,
			Fingerprint:   meta.fingerprint,
			NotAfter:      meta.notAfter,
			Status:        keepercert.StatusActive,
			AutoRotate:    autoRotate,
		}
		if pkiMount != "" {
			w.PKIMount = &pkiMount
		}
		if pkiRole != "" {
			w.PKIRole = &pkiRole
		}
		if m.KID != "" {
			w.IssuedByKID = &m.KID
		}

		if regErr := m.Store.RegisterActive(ctx, w); regErr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cert %s: register: %v", t.kind, regErr))
		}
		anyChanged = true
		registered = append(registered, map[string]any{
			"kind":          string(t.kind),
			"fingerprint":   meta.fingerprint,
			"serial_number": meta.serial,
			"not_after":     meta.notAfter.UTC().Format(time.RFC3339),
		})
	}

	if m.Audit != nil && anyChanged {
		ev := &audit.Event{
			EventType:     audit.EventCertRegistered,
			Source:        audit.SourceKeeperInternal,
			CorrelationID: incarnation,
			Payload: map[string]any{
				"incarnation": incarnation,
				"certs":       registered, // только не-секретные метаданные
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// Детерминированный output: kinds вписанных сертов (sorted), без секретов.
	kinds := make([]string, 0, len(registered))
	for _, r := range registered {
		kinds = append(kinds, r["kind"].(string))
	}
	sort.Strings(kinds)
	kindsAny := make([]any, len(kinds))
	for i, k := range kinds {
		kindsAny[i] = k
	}
	out := map[string]any{
		"registered_kinds": kindsAny,
	}
	return util.SendFinal(stream, anyChanged, out)
}

// parseCertTargetsApply — тот же разбор, что parseCertTargets, но для
// ApplyRequest (у Validate/Apply-Request разные типы, оба несут .Params).
func parseCertTargetsApply(req *pluginv1.ApplyRequest) ([]certTarget, error) {
	return parseCertTargetsFromStruct(req.Params)
}

// certMeta — извлечённые из PEM метаданные серта.
type certMeta struct {
	serial      string
	fingerprint string
	notAfter    time.Time
}

// readCertMeta читает PEM из Vault по ref (`<mount>/<path>#<field>`), парсит
// первый CERTIFICATE-блок и извлекает serial/fingerprint/not_after.
//
// kind=key ТОЖЕ ссылается на ref с сертом (у приватника нет собственного
// not_after — он живёт вместе с cert): автор кладёт в certs[key].vault_ref тот
// же серверный cert, что и в certs[cert] (парность материала), чтобы warrant-
// строка key несла корректный срок. Это допустимо: реестр хранит СРОК, а
// vault_ref key указывает, что ротировать (приватник).
func (m *Module) readCertMeta(ctx context.Context, ref string) (certMeta, error) {
	path, field, err := splitVaultRef(ref)
	if err != nil {
		return certMeta{}, err
	}
	data, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		return certMeta{}, fmt.Errorf("vault read %q: %w", path, err)
	}
	pemStr, err := selectPEMField(data, field)
	if err != nil {
		return certMeta{}, err
	}
	c, err := parseFirstCertificate([]byte(pemStr))
	if err != nil {
		return certMeta{}, err
	}
	return certMeta{
		serial:      c.SerialNumber.Text(16),
		fingerprint: keepercert.FingerprintFromCert(c),
		notAfter:    c.NotAfter.UTC(),
	}, nil
}

// splitVaultRef разбивает `<mount>/<path>#<field>` на logical-path и field.
// logical-path идёт в ReadKV как есть (без `vault:`-префикса) — симметрия с CEL
// `vault('secret/...#field')` (shared/cel splitVaultField): авторские refs
// сервисных сертов — чистый logical-path, как essence tls_cert_ref
// "secret/services/redis/tls#cert". ReadKV сам нормализует путь (relativeKVPath:
// strip mount + fail-closed guard на `..`).
func splitVaultRef(ref string) (path, field string, err error) {
	body := ref
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return "", "", fmt.Errorf("vault-ref %q: empty field after '#'", ref)
		}
	}
	if body == "" {
		return "", "", fmt.Errorf("vault-ref %q: empty path", ref)
	}
	return body, field, nil
}

// selectPEMField извлекает string-поле секрета. Без field и ровно одного поля —
// берёт его; иначе field обязателен (несколько полей — неоднозначно).
func selectPEMField(data map[string]any, field string) (string, error) {
	if field == "" {
		if len(data) != 1 {
			return "", fmt.Errorf("vault-ref without #field but secret has %d fields (specify #field)", len(data))
		}
		for _, v := range data {
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("vault secret field is not a string")
			}
			return s, nil
		}
	}
	v, ok := data[field]
	if !ok {
		return "", fmt.Errorf("field %q absent in secret", field)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("field %q is not a string", field)
	}
	return s, nil
}

// parseFirstCertificate находит первый CERTIFICATE-PEM-блок и парсит его в
// x509. leaf-серт кладётся первым в PEM-цепочке — берём его (его not_after —
// срок, по которому ротируем).
func parseFirstCertificate(pemBytes []byte) (*x509.Certificate, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("no CERTIFICATE block in PEM")
		}
		if block.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			return c, nil
		}
	}
}
