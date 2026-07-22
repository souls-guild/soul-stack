package api

// Aligns the CADENCE-domain runs-envelope name with the committed reference (ENVELOPE
// mechanism, rollout batch N4 per huma_incarnation_envelope.go / huma_operator_envelope.go).
//
// PROBLEM. GET /v1/cadences/{id}/runs carries in Body the type handlers.CadenceRunsReply —
// a Go type ALIAS for sharedapi.PagedResponse[voyageDTO] (handlers/cadence.go). A Go alias is
// transparent to reflect → huma DefaultSchemaNamer sees the instantiated generic
// sharedapi.PagedResponse[Voyage] and emits schema "PagedResponseVoyage". The reference
// (docs/keeper/openapi.yaml :2378) declares the runs-response as $ref to VoyageListReply
// (the same envelope as voyage list — child Voyages reuse the Voyage DTO).
//
// MECHANISM. RegisterTypeAlias(PagedResponse[Voyage] → api.VoyageListReply): on
// encountering the instantiated generic, huma builds the schema via NATIVE api.VoyageListReply
// (huma_voyage_reply.go), which DefaultSchemaNamer names "VoyageListReply" with the contract
// 4-field shape (items.$ref to native Voyage; NO cursor fields — keyset belongs to the soul
// domain, not cadence). This is the SAME schema voyage list carries (voyageListOutput.Body =
// api.VoyageListReply, T5b final group 4) → runs and voyage-list converge on ONE named schema VoyageListReply.
//
// ★ DEDUP voyage+cadence IN SYNC (architect major): TestFullSpec_NoSchemaCollision
// deduplicates same-named Voyage/VoyageListReply/VoyageSummary/VoyageTarget ONLY when the
// body is byte-identical. After moving the voyage domain to native (api.Voyage) cadence-runs MUST
// reference the SAME native set — otherwise two different bodies under the name Voyage → collision. One
// api.VoyageListReply (→ api.Voyage) for both → the body is identical by construction. The runs wire body
// (handler marshals PagedResponse[Voyage]) does NOT change — the alias swaps only the OpenAPI schema.
//
// SHAPE COMPATIBILITY (a generic→oapi-named alias is allowed): the runs wire body is
// PagedResponse[voyageDTO], serializing exactly 4 fields (next_cursor/total_approximate —
// omitempty, zero in offset mode → omitted); VoyageListReply — exactly 4 fields. The schema
// changes (contract name + shape without cursor fields), the wire body does NOT change → golden
// runs byte-exact.

// === CADENCE-LIST + Cadence-DTO alignment (batch N6) ===
//
// GET /v1/cadences carries Body handlers.CadenceListReply = sharedapi.PagedResponse[cadenceDTO];
// element cadenceDTO emitted schema "CadenceDTO", and the instantiated generic — "PagedResponseCadenceDTO".
// Reference: list-envelope = "CadenceListReply" (:8147, items.$ref to element "Cadence", 4-field
// offset shape), element = "Cadence" (:8078, target=$ref VoyageTarget).
//
// BOTH names are aligned by NAMING (without touching the wire):
//   - element: named struct cadence (api layer; huma DefaultSchemaNamer capitalizes → "Cadence")
//     with the reference Cadence shape + alias handlers.CadenceDTO → cadence. ★ The name "cadence" is safe:
//     package internal/cadence is NOT imported into the api layer (verified) — no identifier collision.
//   - envelope: named struct cadenceListReply (api layer; → "CadenceListReply") 4-field offset, items.$ref
//     to element + alias generic PagedResponse[cadenceDTO] → cadenceListReply.
//
// ★ TARGET (WIRE SAFETY): the cadenceDTO.target wire body = json.RawMessage serializes the raw
// JSON object as-is. The alias swaps ONLY the OpenAPI schema (huma builds it from the fields of named
// struct cadence), serialization stays on the handler type cadenceDTO → wire byte-exact does NOT change. So
// named struct cadence.target = *VoyageTarget (schema $ref VoyageTarget, reference :8106), while the response
// bytes themselves go via the former RawMessage path. golden cadence get/list/patch stays byte-exact.

import (
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// cadence — alias target of the element Cadence schema (GET /v1/cadences[/{id}]). Shape verified against
// the committed reference (docs/keeper/openapi.yaml :8078 → Cadence): required:[cadence_id,name,enabled,
// schedule_kind,overlap_policy,kind,created_by_aid,created_at,updated_at]; target — $ref VoyageTarget
// (NOT free-form). Type name = contract schema name (huma DefaultSchemaNamer capitalizes the first
// letter → "Cadence"). The name "cadence" doesn't collide — package internal/cadence isn't imported into the api layer.
// json tags mirror handler type cadenceDTO (it serializes the wire, not this type) → wire byte-exact.
type cadence struct {
	CadenceID            string        `json:"cadence_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID of schedule"` // ULID (audit.NewULID)
	Name                 string        `json:"name"`
	Enabled              bool          `json:"enabled"`
	ScheduleKind         string        `json:"schedule_kind" enum:"interval,cron"`
	IntervalSeconds      *int          `json:"interval_seconds,omitempty"`
	CronExpr             string        `json:"cron_expr,omitempty"`
	OverlapPolicy        string        `json:"overlap_policy" enum:"skip,queue,parallel"`
	Kind                 string        `json:"kind" enum:"scenario,command"`
	ScenarioName         string        `json:"scenario_name,omitempty"`
	Module               string        `json:"module,omitempty"`
	Target               *VoyageTarget `json:"target,omitempty" doc:"declarative run target (declarative, passed through as-is)"`
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

// cadenceListReply — alias target of the GET /v1/cadences envelope schema. Shape verified against the
// committed reference (:8147 → CadenceListReply): EXACTLY 4 fields (items/offset/limit/total), all required, items.$ref
// to element Cadence. Type name → "CadenceListReply". The wire body (PagedResponse[cadenceDTO]) does NOT change.
type cadenceListReply struct {
	Items  []cadence `json:"items" doc:"page of schedules"`
	Offset int32     `json:"offset" doc:"offset from start of set"`
	Limit  int32     `json:"limit" doc:"page size"`
	Total  int32     `json:"total" doc:"total number of entries in set"`
}

// registerCadenceEnvelopes registers the cadence-domain huma aliases on the registry. Called in
// newHumaCadenceAPI for each assembled huma.API. Wire bodies (PagedResponse / cadenceDTO) do NOT change —
// only the OpenAPI schema names/shapes change:
//   - runs-envelope: generic PagedResponse[Voyage] → NATIVE api.VoyageListReply (reference :2378,
//     the same native schema as voyage list — dedup byte-identical Voyage/VoyageListReply);
//   - element: handlers.CadenceDTO → named struct cadence ("Cadence", target=$ref VoyageTarget);
//   - list-envelope: generic PagedResponse[cadenceDTO] → named struct cadenceListReply ("CadenceListReply").
func registerCadenceEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// HANDLER-NATIVE T5d: cadence-runs wire body — PagedResponse[handlers.VoyageDTO]
	// (native handler DTO, flat voyageDTO; handlers/voyage.go). The alias collapses its
	// OpenAPI schema to the SAME native api.VoyageListReply (→ api.Voyage) that voyage
	// list carries → dedup byte-identical Voyage/VoyageListReply (★ architect invariant).
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
