package api

// Выравнивание имён list/history-envelope incarnation-домена под committed-рукопись
// (T4b pilot, ENVELOPE-механизм — третий механизм паттерна вслед за request-rename и
// enum-alias).
//
// ПРОБЛЕМА. list/history-роуты несут в Body тип handlers.IncarnationListReply /
// handlers.IncarnationHistoryReply — это Go type-ALIAS на sharedapi.PagedResponse[T]
// (handlers/incarnation_typed.go). Go-alias прозрачен для reflect → huma DefaultSchemaNamer
// видит инстанцированный generic sharedapi.PagedResponse[IncarnationGetReply] и эмитит
// схему "PagedResponseIncarnationGetReply" / "PagedResponseStateHistoryEntry" (скобки
// generic схлопываются в конкатенацию имён). Рукопись (docs/keeper/openapi.yaml) объявляет
// envelope как "IncarnationListReply" / "IncarnationHistoryReply" — UI ждёт именно их.
//
// МЕХАНИЗМ (структурный аналог enum-alias huma_incarnation_status.go):
//
//   - NAMED-STRUCT (НЕ alias, НЕ generic) в huma-слое: incarnationListReply /
//     incarnationHistoryReply. Имя Go-типа = контрактное имя схемы: huma DefaultSchemaNamer
//     берёт reflect.Type.Name() и капитализирует первую букву → unexported
//     incarnationListReply даёт ровно "IncarnationListReply" (как enum incarnationStatus →
//     "IncarnationStatus"). Форма — РОВНО контрактные поля рукописи: 4 поля int32
//     (items/offset/limit/total) БЕЗ cursor-полей. Это сознательно УЖЕ generic
//     PagedResponse[T] (тот несёт ещё next_cursor/total_approximate omitempty — нужны
//     keyset-домену soul, НЕ incarnation). items.$ref на контрактный native element
//     (IncarnationGetReply / StateHistoryEntry — T5a huma-native reply-DTO).
//   - RegisterTypeAlias (registerIncarnationEnvelopes): при встрече инстанцированного
//     generic sharedapi.PagedResponse[<element>] huma подставляет схему named-struct.
//     huma matched alias по reflect.Type ключу инстанциации (registry.go:100) — Go-alias
//     IncarnationXReply прозрачен, reflect видит ровно ту же инстанциацию → схема строится
//     из envelope. Вызывается в newHumaCadenceAPI (общая фабрика всех huma.API) рядом с
//     aliasIncarnationStatus — wire-тип (тело PagedResponse) НЕ меняется, меняется лишь
//     OpenAPI-схема Body: контрактное имя + контрактная форма вместо generic.
//
// WIRE НЕ ТРОГАЕТ: handlers.IncarnationListReply/IncarnationHistoryReply (alias на
// PagedResponse) и доменные *Typed остаются на generic; envelope несёт ТЕ ЖЕ json-поля
// (items/offset/limit/total) → golden byte-exact list/history не меняется.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// incarnationListReply — alias-цель схемы GET /v1/incarnations envelope. Форма сверена с
// committed-рукописью (docs/keeper/openapi.yaml → IncarnationListReply): РОВНО 4 поля
// int32 (items/offset/limit/total), required все, БЕЗ cursor-полей (cursor — у keyset-
// домена soul, не incarnation). items.$ref на контрактный native element IncarnationGetReply
// (T5a). Имя типа = контрактное имя схемы (huma DefaultSchemaNamer капитализирует → "IncarnationListReply").
type incarnationListReply struct {
	Items  []IncarnationGetReply `json:"items" doc:"страница инкарнаций"`
	Offset int32                 `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32                 `json:"limit" doc:"размер страницы"`
	Total  int32                 `json:"total" doc:"общее число записей в наборе"`
}

// incarnationHistoryReply — alias-цель схемы GET /v1/incarnations/{name}/history envelope.
// Форма сверена с committed-рукописью (docs/keeper/openapi.yaml → IncarnationHistoryReply):
// РОВНО 4 поля int32 (items/offset/limit/total), required все, БЕЗ cursor-полей. items.$ref
// на контрактный native element StateHistoryEntry (T5a). Имя типа = контрактное имя схемы.
type incarnationHistoryReply struct {
	Items  []StateHistoryEntry `json:"items" doc:"страница записей state_history"`
	Offset int32               `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32               `json:"limit" doc:"размер страницы"`
	Total  int32               `json:"total" doc:"общее число записей в наборе"`
}

// incarnationRunsReply — alias-цель схемы GET /v1/incarnations/{name}/runs envelope.
// Та же контрактная форма (4 поля int32 items/offset/limit/total, все required, БЕЗ
// cursor-полей), items.$ref на native element RunSummaryEntry. Имя типа = контрактное
// имя схемы (huma DefaultSchemaNamer капитализирует → "IncarnationRunsReply").
type incarnationRunsReply struct {
	Items  []RunSummaryEntry `json:"items" doc:"страница прогонов инкарнации (свёртка apply_runs)"`
	Offset int32             `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32             `json:"limit" doc:"размер страницы"`
	Total  int32             `json:"total" doc:"общее число прогонов инкарнации"`
}

// registerIncarnationEnvelopes вешает на registry huma-alias инстанцированных generic
// sharedapi.PagedResponse[<element>] → named-struct envelope, чтобы huma строил схему
// list/history/runs-Body под контрактным именем и контрактной формой (4 поля int32,
// $ref на контрактный element). Вызывается в newHumaCadenceAPI для каждой собранной
// huma.API. Wire-тип (тело PagedResponse) НЕ меняется — меняется лишь OpenAPI-схема.
func registerIncarnationEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// ★ handler-native T5d: wire-тип list/history-Body — PagedResponse[handlers.*View].
	// element-схема View сводится через тот же alias на native-envelope, чьё items.$ref
	// указывает на КОНТРАКТНУЮ схему IncarnationGetReply/StateHistoryEntry (та же, что эмитит
	// native get-Body) → дедуп безопасен, имя/4-полей-форма стабильны.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.IncarnationGetView]](),
		reflect.TypeFor[incarnationListReply](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.StateHistoryView]](),
		reflect.TypeFor[incarnationHistoryReply](),
	)
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.RunSummaryView]](),
		reflect.TypeFor[incarnationRunsReply](),
	)
}
