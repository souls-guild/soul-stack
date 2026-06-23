package api

// NATIVE enum-каталог huma-слоя (единственный источник enum-типов контракта).
//
// ЗАЧЕМ. huma НЕ генерит enum-значения-константы (только OpenAPI-валидацию из struct-тегов),
// поэтому каталог enum-типов+констант определяется здесь явно. Enum-типы — SHARED между
// доменами (status/kind/mode встречаются в reply, request, handler), поэтому определяются ОДИН
// раз здесь (foundation), иначе домены передекларируют один тип → компиляционная коллизия.
//
// ИНВАРИАНТЫ.
//   - ★ WIRE BYTE-EXACT. Каждый enum-тип — `type <Name> string`; enum-значение на wire = строка.
//     Точные wire-байты каждой const-константы запинены в huma_enums_test.go (per-value
//     string-equality) + покрыты golden reply-тестами доменов.
//   - ★ SCHEMA-COUNT СТАБИЛЕН (159 схем). Два режима эмиссии схемы:
//       (a) INLINE-enum (огромное большинство): свойство объявляет enum INLINE
//           (`type: string` + `enum:`), huma инлайнит string-named-тип → отдельной
//           named-схемы НЕТ. Native-тип для них — ТОЛЬКО `type`+const, БЕЗ huma.SchemaProvider
//           (иначе появилась бы лишняя named-схема и schema-count поехал бы).
//       (b) NAMED-схема ($ref): только SoulStatus / SoulTransport / IncarnationStatus —
//           объявлены standalone-схемой, UI ссылается по $ref. Для них native-тип
//           реализует huma.SchemaProvider (выносит ту же named-схему с тем же enum-набором),
//           а alias-функции (aliasSoulStatusTransport / aliasIncarnationStatus) перенаправлены
//           на эти native-типы.
//
// ВНИМАНИЕ ПО SOULSTATUS. SoulStatus const-блок несёт 4 значения (connected/disconnected/
// expired/pending) — урезанный контрактный набор; named-схема же (SchemaProvider) несёт ПОЛНЫЙ
// доменный набор из internal/soul (6 значений). Это пред-существующий контент-дрейф
// (см. huma_soul_status.go), НЕ наименование. Тест сверяет const-набор (4); enum-набор
// named-схемы — отдельный инвариант SchemaProvider.

import "github.com/danielgtaylor/huma/v2"

// ───────────────────────────────────────────────────────────────────────────
// (a) INLINE-enum — `type`+const, БЕЗ SchemaProvider (huma инлайнит как legacy-генерата).
//     Значения 1:1 с oapi/types.gen.go (const-блоки «Defines values for <Name>»).
// ───────────────────────────────────────────────────────────────────────────

// AuditEventSource — источник audit-события. INLINE-enum (рукопись инлайнит на свойстве).
type AuditEventSource string

const (
	AuditEventSourceAPI            AuditEventSource = "api"
	AuditEventSourceBackground     AuditEventSource = "background"
	AuditEventSourceKeeperInternal AuditEventSource = "keeper_internal"
	AuditEventSourceMCP            AuditEventSource = "mcp"
	AuditEventSourceSignal         AuditEventSource = "signal"
	AuditEventSourceSoulGRPC       AuditEventSource = "soul_grpc"
)

// SoulHistoryItemType — тип записи истории Soul. INLINE-enum (рукопись инлайнит внутри
// SoulHistoryItem).
type SoulHistoryItemType string

const (
	SoulHistoryItemTypeErrand   SoulHistoryItemType = "errand"
	SoulHistoryItemTypeScenario SoulHistoryItemType = "scenario"
)

// OmenViewSourceType — тип источника Omen (Augur). INLINE-enum.
type OmenViewSourceType string

const (
	OmenViewSourceTypeElk        OmenViewSourceType = "elk"
	OmenViewSourceTypePrometheus OmenViewSourceType = "prometheus"
	OmenViewSourceTypeVault      OmenViewSourceType = "vault"
)

// OperatorAuthMethod — метод аутентификации оператора (ADR-014). INLINE-enum.
//
// ADR-058 (LDAP-часть принята) добавил only-add значения `ldap`/`oidc`
// (федеративная аутентификация, см. operator.AuthMethodLDAP/OIDC). Пара к этому
// расширению — OpenAPI-enum struct-тег `enum:"jwt,mtls,combined,ldap,oidc"`
// (huma_operator_op.go list-фильтр) + committed openapi.yaml: const-набор
// huma-слоя обязан совпадать с wire-enum фильтра. `oidc` заведён сразу (стадия 2
// ADR-058), чтобы дальнейшая имплементация OIDC не трогала контракт повторно.
type OperatorAuthMethod string

const (
	OperatorAuthMethodCombined OperatorAuthMethod = "combined"
	OperatorAuthMethodJWT      OperatorAuthMethod = "jwt"
	OperatorAuthMethodLDAP     OperatorAuthMethod = "ldap"
	OperatorAuthMethodMTLS     OperatorAuthMethod = "mtls"
	OperatorAuthMethodOIDC     OperatorAuthMethod = "oidc"
)

// HeraldType — тип канала уведомлений (Herald, ADR-052). INLINE-enum.
type HeraldType string

const HeraldTypeWebhook HeraldType = "webhook"

// GitRefType — тип git-ref (ADR-007). INLINE-enum.
type GitRefType string

const (
	GitRefTypeBranch GitRefType = "branch"
	GitRefTypeTag    GitRefType = "tag"
)

// ErrandResultStatus — статус результата Errand. INLINE-enum.
type ErrandResultStatus string

const (
	ErrandResultStatusCancelled        ErrandResultStatus = "cancelled"
	ErrandResultStatusFailed           ErrandResultStatus = "failed"
	ErrandResultStatusModuleNotAllowed ErrandResultStatus = "module_not_allowed"
	ErrandResultStatusRunning          ErrandResultStatus = "running"
	ErrandResultStatusSuccess          ErrandResultStatus = "success"
	ErrandResultStatusTimedOut         ErrandResultStatus = "timed_out"
)

// PushApplyViewStatus — статус push-apply (детальный вид). INLINE-enum.
type PushApplyViewStatus string

const (
	PushApplyViewStatusCancelled     PushApplyViewStatus = "cancelled"
	PushApplyViewStatusFailed        PushApplyViewStatus = "failed"
	PushApplyViewStatusPartialFailed PushApplyViewStatus = "partial_failed"
	PushApplyViewStatusPending       PushApplyViewStatus = "pending"
	PushApplyViewStatusRunning       PushApplyViewStatus = "running"
	PushApplyViewStatusSuccess       PushApplyViewStatus = "success"
)

// PushRunListEntryStatus — статус строки push-run списка. INLINE-enum.
type PushRunListEntryStatus string

const (
	PushRunListEntryStatusCancelled     PushRunListEntryStatus = "cancelled"
	PushRunListEntryStatusFailed        PushRunListEntryStatus = "failed"
	PushRunListEntryStatusPartialFailed PushRunListEntryStatus = "partial_failed"
	PushRunListEntryStatusPending       PushRunListEntryStatus = "pending"
	PushRunListEntryStatusRunning       PushRunListEntryStatus = "running"
	PushRunListEntryStatusSuccess       PushRunListEntryStatus = "success"
)

// SigilKeyIntroduceReplyStatus — статус ключа в ответе на introduce. INLINE-enum.
type SigilKeyIntroduceReplyStatus string

const (
	SigilKeyIntroduceReplyStatusActive  SigilKeyIntroduceReplyStatus = "active"
	SigilKeyIntroduceReplyStatusRetired SigilKeyIntroduceReplyStatus = "retired"
)

// SigilKeyViewStatus — статус ключа (детальный вид). INLINE-enum.
type SigilKeyViewStatus string

const (
	SigilKeyViewStatusActive  SigilKeyViewStatus = "active"
	SigilKeyViewStatusRetired SigilKeyViewStatus = "retired"
)

// VoyageKind — род Voyage (scenario/command, ADR-043). INLINE-enum.
type VoyageKind string

const (
	VoyageKindCommand  VoyageKind = "command"
	VoyageKindScenario VoyageKind = "scenario"
)

// VoyageStatus — статус Voyage. INLINE-enum.
type VoyageStatus string

const (
	VoyageStatusCancelled     VoyageStatus = "cancelled"
	VoyageStatusFailed        VoyageStatus = "failed"
	VoyageStatusPartialFailed VoyageStatus = "partial_failed"
	VoyageStatusPending       VoyageStatus = "pending"
	VoyageStatusRunning       VoyageStatus = "running"
	VoyageStatusScheduled     VoyageStatus = "scheduled"
	VoyageStatusSucceeded     VoyageStatus = "succeeded"
)

// VoyageBatchMode — режим батчинга Voyage. INLINE-enum.
type VoyageBatchMode string

const (
	VoyageBatchModeBarrier VoyageBatchMode = "barrier"
	VoyageBatchModeWindow  VoyageBatchMode = "window"
)

// VoyageOnFailure — политика при провале в Voyage. INLINE-enum.
type VoyageOnFailure string

const (
	VoyageOnFailureAbort    VoyageOnFailure = "abort"
	VoyageOnFailureContinue VoyageOnFailure = "continue"
)

// VoyageTargetEntryStatus — статус строки voyage_targets. INLINE-enum.
type VoyageTargetEntryStatus string

const (
	VoyageTargetEntryStatusAwaiting  VoyageTargetEntryStatus = "awaiting"
	VoyageTargetEntryStatusCancelled VoyageTargetEntryStatus = "cancelled"
	VoyageTargetEntryStatusFailed    VoyageTargetEntryStatus = "failed"
	VoyageTargetEntryStatusNoMatch   VoyageTargetEntryStatus = "no_match"
	VoyageTargetEntryStatusRunning   VoyageTargetEntryStatus = "running"
	VoyageTargetEntryStatusSucceeded VoyageTargetEntryStatus = "succeeded"
)

// VoyageTargetEntryTargetKind — род цели строки voyage_targets. INLINE-enum.
type VoyageTargetEntryTargetKind string

const (
	VoyageTargetEntryTargetKindIncarnation VoyageTargetEntryTargetKind = "incarnation"
	VoyageTargetEntryTargetKindSID         VoyageTargetEntryTargetKind = "sid"
)

// VoyageCreateReplyKind — род Voyage в ответе на create. INLINE-enum.
type VoyageCreateReplyKind string

const (
	VoyageCreateReplyKindCommand  VoyageCreateReplyKind = "command"
	VoyageCreateReplyKindScenario VoyageCreateReplyKind = "scenario"
)

// VoyageCreateReplyStatus — статус Voyage в ответе на create. INLINE-enum.
type VoyageCreateReplyStatus string

const (
	VoyageCreateReplyStatusPending   VoyageCreateReplyStatus = "pending"
	VoyageCreateReplyStatusScheduled VoyageCreateReplyStatus = "scheduled"
)

// VoyagePreviewReplyBatchMode — режим батчинга в preview-ответе. INLINE-enum.
type VoyagePreviewReplyBatchMode string

const (
	VoyagePreviewReplyBatchModeBarrier VoyagePreviewReplyBatchMode = "barrier"
	VoyagePreviewReplyBatchModeWindow  VoyagePreviewReplyBatchMode = "window"
)

// VoyagePreviewReplyKind — род Voyage в preview-ответе. INLINE-enum.
type VoyagePreviewReplyKind string

const (
	VoyagePreviewReplyKindCommand  VoyagePreviewReplyKind = "command"
	VoyagePreviewReplyKindScenario VoyagePreviewReplyKind = "scenario"
)

// VoyageCancelReplyStatus — статус Voyage в ответе на cancel. INLINE-enum.
type VoyageCancelReplyStatus string

const VoyageCancelReplyStatusCancelled VoyageCancelReplyStatus = "cancelled"

// ───────────────────────────────────────────────────────────────────────────
// (b) NAMED-схема ($ref) — реализуют huma.SchemaProvider; alias-цели.
//     SoulStatus / SoulTransport / IncarnationStatus. const-блок = oapi const (byte-exact),
//     SchemaProvider-enum = доменный набор (как прежние lowercase-провайдеры).
// ───────────────────────────────────────────────────────────────────────────

// SoulStatus — статус Soul в реестре. NAMED-схема "SoulStatus" ($ref). const-блок 1:1 с
// SoulStatus (4 значения рукописи); SchemaProvider несёт полный доменный набор (6, из
// internal/soul) — пред-существующий контент-дрейф рукописи (см. huma_soul_status.go).
type SoulStatus string

const (
	SoulStatusConnected    SoulStatus = "connected"
	SoulStatusDisconnected SoulStatus = "disconnected"
	SoulStatusExpired      SoulStatus = "expired"
	SoulStatusPending      SoulStatus = "pending"
)

// Schema реализует huma.SchemaProvider: регистрирует named-схему "SoulStatus" (string+enum,
// полный доменный набор) и возвращает $ref. Идемпотентна. wire-тип (строка) НЕ меняется.
func (SoulStatus) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[soulStatusSchemaName]; !ok {
		r.Map()[soulStatusSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        soulStatusEnum,
			Description: soulStatusDescription,
		}
	}
	return &huma.Schema{Ref: soulStatusSchemaRef}
}

// SoulTransport — способ доставки конфигурации. NAMED-схема "SoulTransport" ($ref). const-блок
// 1:1 с SoulTransport; SchemaProvider-enum = доменный набор (agent/ssh).
type SoulTransport string

const (
	SoulTransportAgent SoulTransport = "agent"
	SoulTransportSSH   SoulTransport = "ssh"
)

// Schema реализует huma.SchemaProvider: регистрирует named-схему "SoulTransport" и возвращает
// $ref. Идемпотентна.
func (SoulTransport) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[soulTransportSchemaName]; !ok {
		r.Map()[soulTransportSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        soulTransportEnum,
			Description: soulTransportDescription,
		}
	}
	return &huma.Schema{Ref: soulTransportSchemaRef}
}

// IncarnationStatus — статус runtime-инстанса (ADR-009/031). NAMED-схема "IncarnationStatus"
// ($ref). const-блок 1:1 с IncarnationStatus; SchemaProvider-enum = набор рукописи (8).
type IncarnationStatus string

const (
	IncarnationStatusApplying        IncarnationStatus = "applying"
	IncarnationStatusDestroyFailed   IncarnationStatus = "destroy_failed"
	IncarnationStatusDestroying      IncarnationStatus = "destroying"
	IncarnationStatusDrift           IncarnationStatus = "drift"
	IncarnationStatusErrorLocked     IncarnationStatus = "error_locked"
	IncarnationStatusMigrationFailed IncarnationStatus = "migration_failed"
	IncarnationStatusProvisioning    IncarnationStatus = "provisioning"
	IncarnationStatusReady           IncarnationStatus = "ready"
)

// Schema реализует huma.SchemaProvider: регистрирует named-схему "IncarnationStatus" и
// возвращает $ref. Идемпотентна.
func (IncarnationStatus) Schema(r huma.Registry) *huma.Schema {
	if _, ok := r.Map()[incarnationStatusSchemaName]; !ok {
		r.Map()[incarnationStatusSchemaName] = &huma.Schema{
			Type:        huma.TypeString,
			Enum:        incarnationStatusEnum,
			Description: incarnationStatusDescription,
		}
	}
	return &huma.Schema{Ref: incarnationStatusSchemaRef}
}
