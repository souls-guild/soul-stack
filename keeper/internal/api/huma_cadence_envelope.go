package api

// Выравнивание имени runs-envelope CADENCE-домена под committed-рукопись (ENVELOPE-
// механизм, тираж-батч N4 по эталону huma_incarnation_envelope.go / huma_operator_envelope.go).
//
// ПРОБЛЕМА. GET /v1/cadences/{id}/runs несёт в Body тип handlers.CadenceRunsReply —
// Go type-ALIAS на sharedapi.PagedResponse[voyageDTO] (handlers/cadence.go). Go-alias
// прозрачен для reflect → huma DefaultSchemaNamer видит инстанцированный generic
// sharedapi.PagedResponse[Voyage] и эмитит схему "PagedResponseVoyage". Рукопись
// (docs/keeper/openapi.yaml :2378) объявляет runs-response как $ref на VoyageListReply
// (тот же envelope, что list voyage — дочерние Voyage переиспользуют Voyage-DTO).
//
// МЕХАНИЗМ. RegisterTypeAlias(PagedResponse[Voyage] → api.VoyageListReply): при
// встрече инстанцированного generic huma строит схему через NATIVE api.VoyageListReply
// (huma_voyage_reply.go), который DefaultSchemaNamer именует "VoyageListReply" с контрактной
// 4-поля-формой (items.$ref на native Voyage; БЕЗ cursor-полей — keyset у домена soul, не
// cadence). Это ТА ЖЕ схема, что несёт voyage list (voyageListOutput.Body = api.VoyageListReply,
// финал T5b группа 4) → runs и voyage-list сходятся на ОДНУ named-схему VoyageListReply.
//
// ★ ДЕДУП voyage+cadence СИНХРОННО (architect major): TestFullSpec_NoSchemaCollision
// дедуплицирует одноимённые Voyage/VoyageListReply/VoyageSummary/VoyageTarget ТОЛЬКО при
// byte-identical теле. После перевода voyage-домена на native (api.Voyage) cadence-runs ОБЯЗАН
// ссылаться на ТОТ ЖЕ native-набор — иначе два разных тела под именем Voyage → коллизия. Один
// api.VoyageListReply (→ api.Voyage) для обоих → тело идентично by construction. Wire-тело runs
// (handler marshalит PagedResponse[Voyage]) НЕ меняется — alias подменяет лишь OpenAPI-схему.
//
// ФОРМА-СОВМЕСТИМОСТЬ (alias generic→oapi-named допустим): wire-тело runs —
// PagedResponse[voyageDTO], сериализует ровно 4 поля (next_cursor/total_approximate —
// omitempty, в offset-режиме zero → опущены); VoyageListReply — ровно 4 поля. Schema
// меняется (контрактное имя + форма без cursor-полей), wire-тело НЕ меняется → golden
// runs byte-exact.

// === CADENCE-LIST + Cadence-DTO выравнивание (батч N6) ===
//
// GET /v1/cadences несёт Body handlers.CadenceListReply = sharedapi.PagedResponse[cadenceDTO];
// element cadenceDTO эмитил схему "CadenceDTO", а инстанцированный generic — "PagedResponseCadenceDTO".
// Рукопись: list-envelope = "CadenceListReply" (:8147, items.$ref на element "Cadence", форма 4-поля
// offset), element = "Cadence" (:8078, target=$ref VoyageTarget).
//
// ОБА имени выравниваются ИМЕНОВАНИЕМ (не трогая wire):
//   - element: named-struct cadence (api-слой; huma DefaultSchemaNamer капитализирует → "Cadence")
//     с формой рукописи Cadence + alias handlers.CadenceDTO → cadence. ★ ИМЯ "cadence" безопасно:
//     пакет internal/cadence в api-слой НЕ импортируется (проверено) — коллизии идентификатора нет.
//   - envelope: named-struct cadenceListReply (api-слой; → "CadenceListReply") 4-поля offset, items.$ref
//     на element + alias generic PagedResponse[cadenceDTO] → cadenceListReply.
//
// ★ TARGET (WIRE-БЕЗОПАСНОСТЬ): wire-тело cadenceDTO.target = json.RawMessage сериализует сырой
// JSON-объект as-is. Alias подменяет ТОЛЬКО OpenAPI-схему (huma строит её из полей named-struct
// cadence), сериализация остаётся на handler-типе cadenceDTO → wire byte-exact НЕ меняется. Поэтому
// named-struct cadence.target = *VoyageTarget (схема $ref VoyageTarget, рукопись :8106), а сами байты
// ответа идут прежним RawMessage-путём. golden cadence get/list/patch остаётся byte-exact.

import (
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// cadence — alias-цель схемы element Cadence (GET /v1/cadences[/{id}]). Форма сверена с
// committed-рукописью (docs/keeper/openapi.yaml :8078 → Cadence): required:[cadence_id,name,enabled,
// schedule_kind,overlap_policy,kind,created_by_aid,created_at,updated_at]; target — $ref VoyageTarget
// (НЕ free-form). Имя типа = контрактное имя схемы (huma DefaultSchemaNamer капитализирует первую
// букву → "Cadence"). Имя "cadence" не коллизирует — пакет internal/cadence в api-слой не импортируется.
// json-теги повторяют handler-тип cadenceDTO (wire сериализует он, не этот тип) → wire byte-exact.
type cadence struct {
	CadenceID            string        `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID расписания"` // ULID (audit.NewULID)
	Name                 string        `json:"name"`
	Enabled              bool          `json:"enabled"`
	ScheduleKind         string        `json:"schedule_kind" enum:"interval,cron"`
	IntervalSeconds      *int          `json:"interval_seconds,omitempty"`
	CronExpr             string        `json:"cron_expr,omitempty"`
	OverlapPolicy        string        `json:"overlap_policy" enum:"skip,queue,parallel"`
	Kind                 string        `json:"kind" enum:"scenario,command"`
	ScenarioName         string        `json:"scenario_name,omitempty"`
	Module               string        `json:"module,omitempty"`
	Target               *VoyageTarget `json:"target,omitempty" doc:"декларативный таргет прогона (declarative, отдаётся as-is)"`
	BatchSize            *int          `json:"batch_size,omitempty"`
	BatchPercent         *int          `json:"batch_percent,omitempty"`
	Concurrency          *int          `json:"concurrency,omitempty"`
	BatchMode            string        `json:"batch_mode,omitempty" enum:"barrier,window"`
	FailThreshold        *int          `json:"fail_threshold,omitempty"`
	FailThresholdPercent *int          `json:"fail_threshold_percent,omitempty"`
	RequireAlive         *bool         `json:"require_alive,omitempty"`
	OnFailure            string        `json:"on_failure,omitempty" enum:"abort,continue"`
	NextRunAt            *time.Time    `json:"next_run_at,omitempty"`
	LastRunAt            *time.Time    `json:"last_run_at,omitempty"`
	CreatedByAID         string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

// cadenceListReply — alias-цель схемы GET /v1/cadences envelope. Форма сверена с committed-
// рукописью (:8147 → CadenceListReply): РОВНО 4 поля (items/offset/limit/total), required все, items.$ref
// на element Cadence. Имя типа → "CadenceListReply". Wire-тело (PagedResponse[cadenceDTO]) НЕ меняется.
type cadenceListReply struct {
	Items  []cadence `json:"items" doc:"страница расписаний"`
	Offset int32     `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32     `json:"limit" doc:"размер страницы"`
	Total  int32     `json:"total" doc:"общее число записей в наборе"`
}

// registerCadenceEnvelopes вешает на registry huma-alias-ы cadence-домена. Вызывается в
// newHumaCadenceAPI для каждой собранной huma.API. Wire-тела (PagedResponse / cadenceDTO) НЕ меняются —
// меняются лишь OpenAPI-имена/формы схем:
//   - runs-envelope: generic PagedResponse[Voyage] → NATIVE api.VoyageListReply (рукопись :2378,
//     та же native-схема, что voyage list — дедуп byte-identical Voyage/VoyageListReply);
//   - element: handlers.CadenceDTO → named-struct cadence ("Cadence", target=$ref VoyageTarget);
//   - list-envelope: generic PagedResponse[cadenceDTO] → named-struct cadenceListReply ("CadenceListReply").
func registerCadenceEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// HANDLER-NATIVE T5d: cadence-runs wire-тело — PagedResponse[handlers.VoyageDTO]
	// (native handler-DTO, плоский voyageDTO; handlers/voyage.go). Alias сводит его
	// OpenAPI-схему на ТУ ЖЕ native api.VoyageListReply (→ api.Voyage), что несёт voyage
	// list → дедуп byte-identical Voyage/VoyageListReply (★ инвариант architect).
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.VoyageDTO]](),
		reflect.TypeFor[VoyageListReply](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.CadenceDTO](),
		reflect.TypeFor[cadence](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.CadenceDTO]](),
		reflect.TypeFor[cadenceListReply](),
	)
}
