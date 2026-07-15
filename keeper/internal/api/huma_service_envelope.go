package api

// Aligns the name of the scenarios-list envelope of the SERVICE domain to the committed
// hand-written spec (ENVELOPE mechanism, rollout batch N1 following huma_incarnation_envelope.go).
//
// PROBLEM. GET /v1/services/{name}/scenarios carries the type handlers.ServiceScenariosReply
// in Body (NOT an alias for ServiceScenariosListReply: its element is the domain
// artifact.Scenario with a plain-string Kind, not a typed enum, see handlers/service.go). huma
// DefaultSchemaNamer takes reflect.Type.Name() → emits the schema "ServiceScenariosReply".
// The hand-written spec (docs/keeper/openapi.yaml) declares the envelope as "ServiceScenariosListReply"
// — the UI expects exactly that. The other service-list envelopes (ServiceListReply / ServiceRefsList-
// Reply) already carry oapi types with contract names — they need no alignment.
//
// MECHANISM (structural analog of the incarnation-envelope): a named struct serviceScenariosListReply
// with the contract shape (service/ref/scenarios[], checked against hand-written ServiceScenarios-
// ListReply) + element artifact.Scenario (the same type as in the handler type → items.$ref to the
// contract "Scenario") + RegisterTypeAlias(handlers.ServiceScenariosReply → named) in
// registerServiceEnvelopes (called from newHumaCadenceAPI). The wire type (body of
// handlers.ServiceScenariosReply) does NOT change — the same json fields; only the name of the
// Body OpenAPI schema changes.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// serviceScenariosListReply — alias target for the GET /v1/services/{name}/scenarios envelope schema.
// Shape checked against the committed hand-written spec (docs/keeper/openapi.yaml → ServiceScenariosListReply):
// service/ref (string) + scenarios[] (all three required). items-element — artifact.Scenario
// (the same domain type handlers.ServiceScenariosReply carries) → items.$ref to the contract
// "Scenario". The type name = the contract schema name (huma DefaultSchemaNamer capitalizes →
// "ServiceScenariosListReply").
type serviceScenariosListReply struct {
	Service   string              `json:"service" doc:"имя Service-а (дубль path-параметра)"`
	Ref       string              `json:"ref" doc:"git-ref, на котором составлен listing"`
	Scenarios []artifact.Scenario `json:"scenarios" doc:"scenario из снапшота git-репо Service-а"`
}

// registerServiceEnvelopes registers a huma alias handlers.ServiceScenariosReply →
// the named-struct envelope on the registry, so huma builds the scenarios-Body schema under the
// contract name (ServiceScenariosListReply instead of the handler Go name ServiceScenariosReply).
// Called in newHumaCadenceAPI for each assembled huma.API. The wire type does NOT change.
func registerServiceEnvelopes(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.ServiceScenariosReply](),
		reflect.TypeFor[serviceScenariosListReply](),
	)
}
