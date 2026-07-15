package api

// ALIGNMENT of the nested shared voyage+cadence schemas (target/notify) — a single Go type per
// nested shape, as in the committed hand-written spec (docs/keeper/openapi.yaml :7455/:7612).
//
// Previously each input domain carried its OWN Go type of the same shape
// (voyageTargetHumaBody/cadenceTargetHumaBody, voyageNotifyHumaBody/cadenceNotifyHumaBody)
// → huma DefaultSchemaNamer emitted 4 technical schemas (VoyageTargetHumaBody/
// CadenceTargetHumaBody/VoyageNotifyHumaBody/CadenceNotifyHumaBody) instead of the spec's
// VoyageTarget/VoyageNotify, and OUTPUT (Voyage.target/CadenceDTO.target) pulled the generated
// VoyageTarget — a fifth schema of the same shape. Here:
//   - VoyageTarget — a SINGLE type for ALL input consumers (VoyageCreateRequest.Target /
//     CadenceCreateRequest.Target / CadencePatchRequest.Target). The struct name =
//     the contract schema name. CLASS A (shared input↔output): alias VoyageTarget →
//     VoyageTarget (aliasVoyageTarget) folds OUTPUT onto the same schema too. The shapes are
//     compatible: VoyageTarget — all fields pointer-optional without required; the spec — all
//     optional; a huma value type with omitempty and no required:"true" — also optional. One valid schema.
//   - VoyageNotify — a SINGLE type for input bodies (VoyageCreateRequest.Notify[] /
//     CadenceCreateRequest.Notify[]). CLASS B (shared between input bodies, NO output
//     consumer) → no alias.
//
// json tags/shape — byte-for-byte as the collapsed voyageTargetHumaBody/voyageNotifyHumaBody:
// the wire does not change (handlers/converters read the same fields), golden voyage+cadence holds.
//
// ★ HANDLER-NATIVE T5d: after moving voyage+cadence to native, no direct OUTPUT consumers of the
// generated VoyageTarget remain in huma schemas (Voyage.target — native api.VoyageTarget
// below; CadenceDTO.target — json.RawMessage). The former zero-net safety-alias aliasVoyageTarget
// (VoyageTarget → VoyageTarget) was removed along with this file's last oapi dependency.
// Input bodies (VoyageCreateRequest.Target / CadenceCreateRequest.Target) reference api.VoyageTarget
// directly — they need no alias.

// VoyageTarget — a declarative run target (CLASS A, shared input↔output). scenario
// mode: incarnations/service; command mode: sids/where; shared coven. All fields optional
// (spec :7455 — no required block). Resolution to a snapshot of units is domain-side (at spawn).
//
// ★ FIELD ORDER = alphabetical (coven/incarnations/service/sids/where), like the generated
// VoyageTarget. Once VoyageTarget became an OUTPUT schema (Voyage.target native, final of T5b
// group 4), json.Marshal emits keys in Go-field order — it MUST match the former
// Voyage.target wire (oapi-codegen sorts fields alphabetically), else golden voyage
// fails on byte-order. For INPUT the order is irrelevant (unmarshal is order-independent).
type VoyageTarget struct {
	Coven        []string `json:"coven,omitempty" doc:"coven-метки (env-тег scenario / метка хоста command)"`
	Incarnations []string `json:"incarnations,omitempty" doc:"имена инкарнаций (scenario-режим)"`
	Service      string   `json:"service,omitempty" doc:"имя сервиса (scenario-режим)"`
	SIDs         []string `json:"sids,omitempty" doc:"SID-ы хостов (command-режим)"`
	Where        string   `json:"where,omitempty" doc:"CEL-предикат как ДОПОЛНЕНИЕ к sids/coven (command-режим)"`
}

// VoyageNotify — a one-off subscription to run notifications (CLASS B, shared between input
// bodies). Shape only; runtime validation (herald existence / RBAC herald.read / on-enum) is done
// by the domain prepareNotifyErr. herald is required (spec :7612 — required:[herald]).
type VoyageNotify struct {
	Herald       string         `json:"herald" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя канала-герольда"`
	On           []string       `json:"on,omitempty" doc:"терминалы/типы событий: completed|failed|partial"`
	OnlyFailures *bool          `json:"only_failures,omitempty"`
	OnlyChanges  *bool          `json:"only_changes,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Projection   []string       `json:"projection,omitempty"`
}
