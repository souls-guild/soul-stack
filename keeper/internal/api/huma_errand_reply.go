package api

// HUMA-NATIVE wire-DTO ERRAND-домена (handler-native T5d-2c-full). Reply/output Body
// huma read-роутов (list + get) — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler
// (handlers/errand.go) возвращает доменные result-ы с ПЛОСКИМИ полями (ErrandResultView /
// ErrandListPage); register-func (huma_errand.go) проецирует их В ЭТИ типы напрямую —
// конвертеров legacy-генерата → native больше нет (граница api↔handlers строит wire-DTO из
// доменных полей). Ключевое:
//
//   - ИМЯ СХЕМЫ = контрактное (ErrandResult / ErrandListReply): huma DefaultSchemaNamer
//     берёт reflect.Type.Name() → схема под тем же именем (errand-schema-test пинит
//     items.$ref → ErrandResult, envelope → ErrandListReply).
//   - ENUM-поле Status — native ErrandResultStatus (huma_enums.go, INLINE-enum): huma
//     инлайнит string-named-тип как `type: string` без $ref; на wire — строка.
//   - ENVELOPE: element list-схемы — этот native ErrandResult; ErrandListReply несёт
//     items/limit/offset/total (Go-int, parity прежней oapi-формы).
//   - ФОРМА wire (json-теги/omitempty/date-time/nullable/ПОРЯДОК полей) 1:1 с прежним
//     legacy-генерата; golden byte-exact фиксирует huma_errand_reply_test.go.
//   - ErrandAccepted (202-тело errand-get running) типизируется отдельно как
//     schema-builder pre-seed (errandAccepted, huma_errand_accepted.go); на wire его
//     сериализует register-func get-роута из плоской handlers.ErrandAcceptedView.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). errand_id — машинно ULID (audit.NewULID,
// dispatcher.go:262); sid ← soul.SIDPattern; started_by_aid ← operator.AIDPattern.
// Формат для клиент-кодогена; pattern не влияет на json.Marshal (golden цел).

import (
	"time"
)

// ErrandResult — native элемент errand-list / 200-тело errand-get-терминал. Форма 1:1
// с прежним ErrandResult (ПОРЯДОК полей под oapi byte-order): duration_ms/
// error_message/exit_code/finished_at/output/stderr/stderr_truncated/stdout/
// stdout_truncated — опц. указатели С omitempty (nil → ключ опущен); status —
// native enum ErrandResultStatus (inline-схема, wire — строка); started_at —
// наносекундный time-wire; finished_at — `*time.Time` omitempty (running → опущен).
type ErrandResult struct {
	DurationMs      *int64                  `json:"duration_ms,omitempty"`
	ErrandID        string                  `json:"errand_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	ErrorMessage    *string                 `json:"error_message,omitempty"`
	ExitCode        *int32                  `json:"exit_code,omitempty"`
	FinishedAt      *time.Time              `json:"finished_at,omitempty"`
	Module          string                  `json:"module"`
	Output          *map[string]interface{} `json:"output,omitempty"`
	SID             string                  `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	StartedAt       time.Time               `json:"started_at"`
	StartedByAID    string                  `json:"started_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Status          ErrandResultStatus      `json:"status"`
	Stderr          *string                 `json:"stderr,omitempty"`
	StderrTruncated *bool                   `json:"stderr_truncated,omitempty"`
	Stdout          *string                 `json:"stdout,omitempty"`
	StdoutTruncated *bool                   `json:"stdout_truncated,omitempty"`
}

// ErrandListReply — native 200-envelope GET /v1/errands. Форма 1:1 с прежней oapi-
// формой (items/limit/offset/total; offset/limit/total — Go-int parity legacy-генерата). Items —
// []ErrandResult (native element). nil-ность Items проецируется register-func-ом
// (nil→nil, []→[]) byte-exact с прежним.
type ErrandListReply struct {
	Items  []ErrandResult `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
	Total  int            `json:"total"`
}
