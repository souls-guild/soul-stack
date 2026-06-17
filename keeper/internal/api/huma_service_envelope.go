package api

// Выравнивание имени scenarios-list-envelope SERVICE-домена под committed-рукопись
// (ENVELOPE-механизм, тираж-батч N1 по эталону huma_incarnation_envelope.go).
//
// ПРОБЛЕМА. GET /v1/services/{name}/scenarios несёт в Body тип handlers.ServiceScenariosReply
// (НЕ alias на ServiceScenariosListReply: его элемент — domain artifact.Scenario с
// plain-string Kind, а не типизированный enum, см. handlers/service.go). huma
// DefaultSchemaNamer берёт reflect.Type.Name() → эмитит схему "ServiceScenariosReply".
// Рукопись (docs/keeper/openapi.yaml) объявляет envelope как "ServiceScenariosListReply"
// — UI ждёт именно его. Прочие service-list-envelope (ServiceListReply / ServiceRefsList-
// Reply) уже несут oapi-типы с контрактными именами — им выравнивание не нужно.
//
// МЕХАНИЗМ (структурный аналог incarnation-envelope): named-struct serviceScenariosListReply
// с контрактной формой (service/ref/scenarios[], сверено с рукописью ServiceScenarios-
// ListReply) + element artifact.Scenario (тот же тип, что в handler-типе → items.$ref на
// контрактный "Scenario") + RegisterTypeAlias(handlers.ServiceScenariosReply → named) в
// registerServiceEnvelopes (зовётся из newHumaCadenceAPI). Wire-тип (тело
// handlers.ServiceScenariosReply) НЕ меняется — те же json-поля; меняется лишь имя
// OpenAPI-схемы Body.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// serviceScenariosListReply — alias-цель схемы GET /v1/services/{name}/scenarios envelope.
// Форма сверена с committed-рукописью (docs/keeper/openapi.yaml → ServiceScenariosListReply):
// service/ref (string) + scenarios[] (required все три). items-element — artifact.Scenario
// (тот же domain-тип, что несёт handlers.ServiceScenariosReply) → items.$ref на контрактный
// "Scenario". Имя типа = контрактное имя схемы (huma DefaultSchemaNamer капитализирует →
// "ServiceScenariosListReply").
type serviceScenariosListReply struct {
	Service   string              `json:"service" doc:"имя Service-а (дубль path-параметра)"`
	Ref       string              `json:"ref" doc:"git-ref, на котором составлен listing"`
	Scenarios []artifact.Scenario `json:"scenarios" doc:"scenario из снапшота git-репо Service-а"`
}

// registerServiceEnvelopes вешает на registry huma-alias handlers.ServiceScenariosReply →
// named-struct envelope, чтобы huma строил схему scenarios-Body под контрактным именем
// (ServiceScenariosListReply вместо handler-Go-имени ServiceScenariosReply). Вызывается в
// newHumaCadenceAPI для каждой собранной huma.API. Wire-тип НЕ меняется.
func registerServiceEnvelopes(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.ServiceScenariosReply](),
		reflect.TypeFor[serviceScenariosListReply](),
	)
}
