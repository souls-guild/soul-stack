package api

// HUMA-NATIVE reply-DTO VOYAGE-домена (Teardown T5b, финал — группа 4, паттерн pilot T5a
// huma_incarnation_reply.go). Reply/output Body huma-операций voyage — native Go-struct в
// пакете api, НЕ генерёный legacy-генерата. Граница/инварианты — см. шапку huma_incarnation_reply.go.
//
// ★ ДЕДУП VOYAGE+CADENCE СИНХРОННО. voyage-схемы (Voyage/VoyageSummary/VoyageListReply +
// shared VoyageTarget) дублируются между voyage-доменом и cadence-runs (GET /v1/cadences/
// {id}/runs возвращает ТЕ ЖЕ типы). TestFullSpec_NoSchemaCollision дедуплицирует одноимённые
// схемы ТОЛЬКО при byte-identical теле. Поэтому voyage И cadence-runs ОБЯЗАНЫ ссылаться на
// ОДИН native-набор api.Voyage/api.VoyageListReply: voyage-домен — прямым native-Body,
// cadence-runs — alias-ом generic-envelope на native api.VoyageListReply (huma_cadence_
// envelope.go, registerCadenceEnvelopes). Тело Voyage идентично by construction → коллизии нет.
//
// ENUM (architect minor — сверено с meta/openapi.yaml :7654-7851). ВСЕ voyage reply-enum
// ОБЪЯВЛЕНЫ INLINE (`type: string` + `enum:` прямо на свойстве, БЕЗ standalone-схемы $ref):
//   - Voyage.kind/status/batch_mode/on_failure       → VoyageKind/Status/BatchMode/OnFailure;
//   - VoyageTargetEntry.target_kind/status           → VoyageTargetEntryTargetKind/Status;
//   - VoyageCreateReply.kind/status                  → VoyageCreateReplyKind/Status;
//   - VoyagePreviewReply.kind/batch_mode             → VoyagePreviewReplyKind/BatchMode;
//   - VoyageCancelReply.status                       → VoyageCancelReplyStatus.
// Standalone-схемы для них рукопись НЕ объявляет → enum-alias-механизм (как aliasIncarnation-
// Status) НЕ применяется. Поля — NATIVE enum-тип (huma_enums.go, T5d-2c-full Phase 1) — huma
// инлайнит их как `type: string` с enum-набором (byte-exact с прежним legacy-генерата-Body; native value
// идентичен oapi value). Конвертер кастует legacy-генерата → native (value напрямую, pointer — helper).
//
// SHARED VoyageTarget (КЛАСС A). Voyage.target — РЕЮЗ существующего native api.VoyageTarget
// (huma_voyage_target.go), той же схемы, что input voyage/cadence. НЕ дублируем тип.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). voyage_id — машинно ULID (audit.NewULID,
// handlers/voyage.go:988); started_by_aid ← operator.AIDPattern. Чисто формат для
// клиент-кодогена; pattern не влияет на json.Marshal (golden цел). VoyageTarget.sids
// НЕ тегируется: тип shared input↔output, на INPUT pattern стал бы рантайм-422.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === nested-листья (форма 1:1 с legacy-генерата) ===

// VoyageSummary — native агрегаты прогона (форма 1:1 с VoyageSummary, types.gen.go :3973):
// total/succeeded/failed/cancelled — int БЕЗ omitempty (required, рукопись :7710); no_match —
// *int С omitempty (0/nil → ключ опущен, как handler noMatchPtr). Имя структуры = контрактное
// имя схемы.
type VoyageSummary struct {
	Cancelled int  `json:"cancelled"`
	Failed    int  `json:"failed"`
	NoMatch   *int `json:"no_match,omitempty"`
	Succeeded int  `json:"succeeded"`
	Total     int  `json:"total"`
}

// VoyageTargetEntry — native строка voyage_targets (All-runs drill; форма 1:1 с
// VoyageTargetEntry, types.gen.go :4001): apply_id/errand_id/finished_at — *-С omitempty
// (взаимоисключающие back-link-и kind=scenario/command → ключ опущен); status/target_kind —
// inline oapi-enum (huma инлайнит `type: string`); batch_index — int; target_id — string.
type VoyageTargetEntry struct {
	ApplyID    *string                     `json:"apply_id,omitempty"`
	BatchIndex int                         `json:"batch_index"`
	ErrandID   *string                     `json:"errand_id,omitempty"`
	FinishedAt *time.Time                  `json:"finished_at,omitempty"`
	Status     VoyageTargetEntryStatus     `json:"status"`
	TargetID   string                      `json:"target_id"`
	TargetKind VoyageTargetEntryTargetKind `json:"target_kind"`
}

// === Voyage (детальный snapshot) — форма 1:1 с Voyage ===

// Voyage — native snapshot Voyage-прогона (GET detail / list item; форма 1:1 с Voyage,
// types.gen.go :3787). Поля required-по-рукописи (:7789) — без omitempty (attempt/current_
// batch_index/dry_run/scope_size/total_batches/started_by_aid/voyage_id/kind/status/created_at);
// pointer-optional С omitempty — все nullable-поля (batch_*/concurrency/fail_threshold/finished_
// at/module/on_failure/require_alive/scenario_name/schedule_at/started_at/summary/target). enum
// kind/status/batch_mode/on_failure — inline oapi-enum (huma `type: string`). Target — РЕЮЗ
// shared api.VoyageTarget (КЛАСС A; та же схема, что input). Summary — native VoyageSummary.
// date-time — наносекундный wire (handler присваивает голый time.Time БЕЗ .UTC()/Truncate).
// Имя структуры = контрактное имя схемы.
type Voyage struct {
	Attempt           int              `json:"attempt"`
	BatchMode         *VoyageBatchMode `json:"batch_mode,omitempty"`
	BatchPercent      *int             `json:"batch_percent,omitempty"`
	BatchSize         *int             `json:"batch_size,omitempty"`
	Concurrency       *int             `json:"concurrency,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	CurrentBatchIndex int              `json:"current_batch_index"`
	DryRun            bool             `json:"dry_run"`
	FailThreshold     *int             `json:"fail_threshold,omitempty"`
	FinishedAt        *time.Time       `json:"finished_at,omitempty"`
	Kind              VoyageKind       `json:"kind"`
	Module            *string          `json:"module,omitempty"`
	OnFailure         *VoyageOnFailure `json:"on_failure,omitempty"`
	RequireAlive      *bool            `json:"require_alive,omitempty"`
	ScenarioName      *string          `json:"scenario_name,omitempty"`
	ScheduleAt        *time.Time       `json:"schedule_at,omitempty"`
	ScopeSize         int              `json:"scope_size"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	StartedByAID      string           `json:"started_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status            VoyageStatus     `json:"status"`
	Summary           *VoyageSummary   `json:"summary,omitempty"`
	Target            *VoyageTarget    `json:"target,omitempty"`
	TotalBatches      int              `json:"total_batches"`
	VoyageID          string           `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// === обёртки (форма 1:1 с legacy-генерата) ===

// VoyageListReply — native 200-envelope GET /v1/voyages (форма 1:1 с VoyageListReply,
// types.gen.go :3923): items/offset/limit/total (offset/limit/total — int, parity legacy-генерата);
// items.$ref на native Voyage. ★ ОБЩИЙ тип для voyage-list И cadence-runs (последний — через
// alias generic-envelope → api.VoyageListReply, huma_cadence_envelope.go) → одна named-схема
// VoyageListReply byte-identical в обоих доменах. Имя структуры = контрактное имя схемы.
type VoyageListReply struct {
	Items  []Voyage `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// VoyageTargetsReply — native 200-тело GET /v1/voyages/{id}/targets (форма 1:1 с
// VoyageTargetsReply, types.gen.go :4021): voyage_id + targets[] (native VoyageTargetEntry),
// оба required. Имя структуры = контрактное имя схемы.
type VoyageTargetsReply struct {
	Targets  []VoyageTargetEntry `json:"targets"`
	VoyageID string              `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// VoyageCreateReply — native 202-тело POST /v1/voyages (форма 1:1 с VoyageCreateReply,
// types.gen.go :3839): voyage_id/kind/scope_size/status/location (все required). kind/status —
// inline oapi-enum (huma `type: string`). Имя структуры = контрактное имя схемы.
type VoyageCreateReply struct {
	Kind      VoyageCreateReplyKind   `json:"kind"`
	Location  string                  `json:"location"`
	ScopeSize int                     `json:"scope_size"`
	Status    VoyageCreateReplyStatus `json:"status"`
	VoyageID  string                  `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// VoyagePreviewReply — native 200-тело POST /v1/voyages/preview (форма 1:1 с
// VoyagePreviewReply, types.gen.go :3951): kind/scope_size/total_batches/batch_mode
// (required) + effective_batch_size (*int С omitempty). kind/batch_mode — inline oapi-enum.
// Имя структуры = контрактное имя схемы.
type VoyagePreviewReply struct {
	BatchMode          VoyagePreviewReplyBatchMode `json:"batch_mode"`
	EffectiveBatchSize *int                        `json:"effective_batch_size,omitempty"`
	Kind               VoyagePreviewReplyKind      `json:"kind"`
	ScopeSize          int                         `json:"scope_size"`
	TotalBatches       int                         `json:"total_batches"`
}

// VoyageCancelReply — native 202-тело DELETE /v1/voyages/{id} (форма 1:1 с
// VoyageCancelReply, types.gen.go :3830): voyage_id + status:cancelled (required).
// status — inline oapi-enum. Имя структуры = контрактное имя схемы.
type VoyageCancelReply struct {
	Status   VoyageCancelReplyStatus `json:"status"`
	VoyageID string                  `json:"voyage_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// === проекторы handlers.X → api-native (граница api↔handlers, byte-exact form passthrough) ===
//
// Handler-native (T5d): извлечённые *Typed-функции отдают плоские handlers-DTO (plain-string
// enum-поля, target-pointer-slice — handlers/voyage.go). Эти проекторы переводят их В api-native
// wire-DTO (named-enum-поля → huma-schema, value-slice target). Конвертеров legacy-генерата больше нет —
// граница строит wire-DTO из доменных handler-полей напрямую (паттерн huma_operator_reply.go).

// ptrVoyageBatchMode / ptrVoyageOnFailure — каст указателя plain-string → native-enum БЕЗ потери
// nil-ности (nil → nil, для byte-exact omitempty). Underlying string идентичен.
func ptrVoyageBatchMode(s *string) *VoyageBatchMode {
	if s == nil {
		return nil
	}
	v := VoyageBatchMode(*s)
	return &v
}

func ptrVoyageOnFailure(s *string) *VoyageOnFailure {
	if s == nil {
		return nil
	}
	v := VoyageOnFailure(*s)
	return &v
}

func toVoyageSummary(o *handlers.VoyageSummaryDTO) *VoyageSummary {
	if o == nil {
		return nil
	}
	return &VoyageSummary{
		Cancelled: o.Cancelled,
		Failed:    o.Failed,
		NoMatch:   o.NoMatch,
		Succeeded: o.Succeeded,
		Total:     o.Total,
	}
}

// toVoyageTarget — проекция handler-DTO target → native api.VoyageTarget (КЛАСС A reuse).
// handlers.VoyageTargetDTO несёт pointer-slice/pointer-string (*[]string/*string); native —
// value-slice/value-string с omitempty. Разыменование сохраняет byte-exact: nil-указатель →
// nil-slice/"" → (omitempty) ключ опущен. Voyage.target опущен целиком (graceful), когда у
// домена нет target_origin.
func toVoyageTarget(o *handlers.VoyageTargetDTO) *VoyageTarget {
	if o == nil {
		return nil
	}
	out := &VoyageTarget{}
	if o.Incarnations != nil {
		out.Incarnations = *o.Incarnations
	}
	if o.Service != nil {
		out.Service = *o.Service
	}
	if o.Sids != nil {
		out.SIDs = *o.Sids
	}
	if o.Where != nil {
		out.Where = *o.Where
	}
	if o.Coven != nil {
		out.Coven = *o.Coven
	}
	return out
}

func toVoyage(o handlers.VoyageDTO) Voyage {
	return Voyage{
		Attempt:           o.Attempt,
		BatchMode:         ptrVoyageBatchMode(o.BatchMode),
		BatchPercent:      o.BatchPercent,
		BatchSize:         o.BatchSize,
		Concurrency:       o.Concurrency,
		CreatedAt:         o.CreatedAt,
		CurrentBatchIndex: o.CurrentBatchIndex,
		DryRun:            o.DryRun,
		FailThreshold:     o.FailThreshold,
		FinishedAt:        o.FinishedAt,
		Kind:              VoyageKind(o.Kind),
		Module:            o.Module,
		OnFailure:         ptrVoyageOnFailure(o.OnFailure),
		RequireAlive:      o.RequireAlive,
		ScenarioName:      o.ScenarioName,
		ScheduleAt:        o.ScheduleAt,
		ScopeSize:         o.ScopeSize,
		StartedAt:         o.StartedAt,
		StartedByAID:      o.StartedByAID,
		Status:            VoyageStatus(o.Status),
		Summary:           toVoyageSummary(o.Summary),
		Target:            toVoyageTarget(o.Target),
		TotalBatches:      o.TotalBatches,
		VoyageID:          o.VoyageID,
	}
}

func toVoyageTargetEntry(o handlers.VoyageTargetEntryDTO) VoyageTargetEntry {
	return VoyageTargetEntry{
		ApplyID:    o.ApplyID,
		BatchIndex: o.BatchIndex,
		ErrandID:   o.ErrandID,
		FinishedAt: o.FinishedAt,
		Status:     VoyageTargetEntryStatus(o.Status),
		TargetID:   o.TargetID,
		TargetKind: VoyageTargetEntryTargetKind(o.TargetKind),
	}
}

func toVoyageListReply(o handlers.VoyageListReply) VoyageListReply {
	// Сохраняем nil-ность среза (nil → wire `null`, [] → `[]`) для byte-exact.
	var items []Voyage
	if o.Items != nil {
		items = make([]Voyage, len(o.Items))
		for i := range o.Items {
			items[i] = toVoyage(o.Items[i])
		}
	}
	return VoyageListReply{Items: items, Limit: o.Limit, Offset: o.Offset, Total: o.Total}
}

func toVoyageTargetsReply(o handlers.VoyageTargetsReply) VoyageTargetsReply {
	var targets []VoyageTargetEntry
	if o.Targets != nil {
		targets = make([]VoyageTargetEntry, len(o.Targets))
		for i := range o.Targets {
			targets[i] = toVoyageTargetEntry(o.Targets[i])
		}
	}
	return VoyageTargetsReply{Targets: targets, VoyageID: o.VoyageID}
}

func toVoyageCreateReply(o handlers.VoyageCreateReply) VoyageCreateReply {
	return VoyageCreateReply{
		Kind:      VoyageCreateReplyKind(o.Kind),
		Location:  o.Location,
		ScopeSize: o.ScopeSize,
		Status:    VoyageCreateReplyStatus(o.Status),
		VoyageID:  o.VoyageID,
	}
}

func toVoyagePreviewReply(o handlers.VoyagePreviewReply) VoyagePreviewReply {
	return VoyagePreviewReply{
		BatchMode:          VoyagePreviewReplyBatchMode(o.BatchMode),
		EffectiveBatchSize: o.EffectiveBatchSize,
		Kind:               VoyagePreviewReplyKind(o.Kind),
		ScopeSize:          o.ScopeSize,
		TotalBatches:       o.TotalBatches,
	}
}

func toVoyageCancelReply(o handlers.VoyageCancelReply) VoyageCancelReply {
	return VoyageCancelReply{Status: VoyageCancelReplyStatus(o.Status), VoyageID: o.VoyageID}
}
