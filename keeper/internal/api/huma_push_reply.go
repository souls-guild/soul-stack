package api

// HUMA-NATIVE reply-DTO PUSH-домена (handler-native T5d-2c-full). Reply/output Body
// huma-операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Register-func (huma_push.go)
// проецирует плоские доменные view-ы handler-а (PushApplyResultView / PushRunListEntryView)
// напрямую В ЭТИ типы — конвертеров legacy-генерата→native больше нет. Ключевое для push:
//
//   - ФОРМА байт-в-байт = прежняя legacy-генерата (json-теги/omitempty/date-time/nullable категории A-D).
//   - ИМЯ СХЕМЫ = контрактное (PushApplyReply / PushApplyView / PushRunListReply /
//     PushRunListEntry / PushSummaryCounts): huma DefaultSchemaNamer берёт
//     reflect.Type.Name() → схема под тем же именем, что давал прежний legacy-генерата.
//   - ENUM-поля Status (PushApplyView.Status / PushRunListEntry.Status) — native
//     PushApplyViewStatus / PushRunListEntryStatus (huma_enums.go, INLINE-enum): рукопись
//     инлайнит статус как `type: string` + enum (standalone-схемы нет), huma инлайнит
//     string-named-тип одинаково → schema byte-identical (parity ServiceView.GitRefType).
//   - PushRunListReply — НЕ generic-envelope (не sharedapi.PagedResponse), а обычный
//     reply с полем items[]PushRunListEntry + offset/limit/total (int) → `type: integer`
//     без format (assertOffsetEnvelopeNoFormat). Top-level reply-DTO, не через alias.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). apply_id — машинно ULID (audit.NewULID,
// pushorch/run.go:182); started_by_aid ← operator.AIDPattern. Формат для клиент-
// кодогена; pattern не влияет на json.Marshal. inventory_sids НЕ тегируется:
// per-element pattern на массиве-output-поле не покрыт этим батчем.

import (
	"time"
)

// === top-level reply-DTO (форма 1:1 с прежней legacy-генерата-формой) ===

// PushApplyReply — native 202-тело POST /v1/push/apply (apply_id async). Форма 1:1 с
// PushApplyReply.
type PushApplyReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// PushApplyView — native 200-тело GET /v1/push/{apply_id}. Форма 1:1 с PushApplyView:
// finished_at/input/ssh_provider/started_by_aid/summary — `*`-поля С omitempty (nil → ключ
// опущен); inventory_sids — массив; started_at — наносекундный time-wire; status — enum-тип
// PushApplyViewStatus (wire-строка).
type PushApplyView struct {
	ApplyID       string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	CleanupStale  bool                    `json:"cleanup_stale"`
	DestinyRef    string                  `json:"destiny_ref"`
	FinishedAt    *time.Time              `json:"finished_at,omitempty"`
	Input         *map[string]interface{} `json:"input,omitempty"`
	InventorySids []string                `json:"inventory_sids"`
	SSHProvider   *string                 `json:"ssh_provider,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	StartedByAID  *string                 `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status        PushApplyViewStatus     `json:"status"`
	Summary       *map[string]interface{} `json:"summary,omitempty"`
}

// PushRunListReply — native 200-тело GET /v1/push-runs (offset-envelope: items/offset/
// limit/total). items — native PushRunListEntry; offset/limit/total — int (parity legacy-генерата →
// `type: integer` без format). Форма 1:1 с PushRunListReply.
type PushRunListReply struct {
	Items  []PushRunListEntry `json:"items"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
	Total  int                `json:"total"`
}

// === nested reply-DTO ===

// PushRunListEntry — native compact-строка push_runs (element PushRunListReply.items).
// Форма 1:1 с PushRunListEntry: finished_at/ssh_provider/started_by_aid/summary_counts —
// `*`-поля С omitempty; inventory_sids — массив; started_at — наносекундный time-wire;
// status — enum-тип PushRunListEntryStatus.
type PushRunListEntry struct {
	ApplyID       string                 `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	CleanupStale  bool                   `json:"cleanup_stale"`
	DestinyRef    string                 `json:"destiny_ref"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty"`
	InventorySids []string               `json:"inventory_sids"`
	SSHProvider   *string                `json:"ssh_provider,omitempty"`
	StartedAt     time.Time              `json:"started_at"`
	StartedByAID  *string                `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status        PushRunListEntryStatus `json:"status"`
	SummaryCounts *PushSummaryCounts     `json:"summary_counts,omitempty"`
}

// PushSummaryCounts — native агрегат counts (PushRunListEntry.summary_counts). Все поля —
// `*int` С omitempty (nil → ключ опущен). Форма 1:1 с прежней PushSummaryCounts.
type PushSummaryCounts struct {
	FailCount    *int `json:"fail_count,omitempty"`
	SuccessCount *int `json:"success_count,omitempty"`
	Total        *int `json:"total,omitempty"`
}
