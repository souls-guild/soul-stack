package api

// Выравнивание имени list-envelope OPERATOR-домена под committed-рукопись (ENVELOPE-
// механизм, handler-native PILOT T5d по эталону huma_incarnation_envelope.go).
//
// ПРОБЛЕМА. GET /v1/operators несёт в Body тип sharedapi.PagedResponse[Operator]
// напрямую — huma DefaultSchemaNamer видит инстанцированный generic и эмитит схему
// "PagedResponseOperator" (скобки generic схлопываются в конкатенацию). Рукопись
// (docs/keeper/openapi.yaml) объявляет envelope как "OperatorListReply" — UI ждёт его.
//
// МЕХАНИЗМ (структурный аналог incarnation-envelope): named-struct operatorListReply с
// контрактной offset-формой (4 поля int32 items/offset/limit/total БЕЗ cursor-полей,
// сверено с рукописью OperatorListReply) + items.$ref на native element Operator +
// RegisterTypeAlias(PagedResponse[Operator] → named) в registerOperatorEnvelopes
// (зовётся из newHumaCadenceAPI). Wire-тип (тело PagedResponse) НЕ меняется —
// меняется лишь OpenAPI-схема Body.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// operatorListReply — alias-цель схемы GET /v1/operators envelope. Форма сверена с
// committed-рукописью (docs/keeper/openapi.yaml → OperatorListReply): РОВНО 4 поля
// int32 (items/offset/limit/total), required все, БЕЗ cursor-полей. items.$ref на
// native element Operator (handler-native PILOT: wire-DTO полностью native). Имя типа =
// контрактное имя схемы (huma DefaultSchemaNamer капитализирует → "OperatorListReply").
type operatorListReply struct {
	Items  []Operator `json:"items" doc:"страница операторов"`
	Offset int32      `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32      `json:"limit" doc:"размер страницы"`
	Total  int32      `json:"total" doc:"общее число записей в наборе"`
}

// registerOperatorEnvelopes вешает на registry huma-alias инстанцированного generic
// sharedapi.PagedResponse[Operator] → named-struct envelope, чтобы huma строил
// схему list-Body под контрактным именем/формой. Вызывается в newHumaCadenceAPI для
// каждой собранной huma.API. Wire-тип (тело PagedResponse) НЕ меняется.
func registerOperatorEnvelopes(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[Operator]](),
		reflect.TypeFor[operatorListReply](),
	)
}
