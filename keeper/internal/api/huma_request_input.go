package api

// The wire→native projection of every remaining request body and query input, in
// one place ([NIM-831]) — the sibling of huma_create_input.go, which holds the
// eight captioned creates [NIM-817] extracted first.
//
// WHY THEY MOVED OUT OF THE CLOSURES. A route binds one struct for the schema
// (huma) and calls the handler with another, and between the two sits a
// hand-written field list. Written inline in the `huma.Register` closure, that
// list is reachable only over HTTP: there is no function to reflect over, so
// nothing can fail for a field nobody thought to send. That is the exact shape
// `label` went missing in on eight registries at once. [NIM-824] closed the SET of
// such literals — a new one is red the day it is written — but closing the set is
// not the same as checking the fields, and thirty of them were still inline.
//
// As named functions they are driven by huma_wire_projection_guard_test.go, which
// fills the request three ways — every field at once with a value unique to it,
// one field at a time, and an empty body — and fails on any native field that does
// not arrive holding what it should. The property that buys: ADD A FIELD TO A WIRE
// STRUCT AND LEAVE THE PROJECTION ALONE, AND SOMETHING IS RED.
//
// A field a projection deliberately does not carry is a decision to state, not an
// omission to make quietly: name it in the guard's `unmapped` set with the reason.
//
// TWO SHAPES OF WIRE ROOT, and the reason for each:
//
//   - the BODY alone, where every field the projection reads comes from it. A path
//     parameter that also belongs in the mapping is then a second argument —
//     `toUpdatePermissionsInput` is the one case, and it is rooted on the body
//     DESPITE that, because its three [Optional] fields each contribute a `Set…`
//     boolean and the guard can only tell four booleans apart by isolating them
//     one at a time, which it does per field of the ROOT.
//   - the whole huma INPUT struct, for the list/query projections below, whose
//     fields are query parameters with no body at all. Rooting there puts each
//     parameter inside what the guard walks.
//
// Three projections take an argument beyond the wire root, for a value the ROUTE
// computes: a page parsed and VALIDATED before the projection runs, the dynamic
// `state.<field>` filters that arrive from the request context because huma cannot
// bind them, and a {name} path parameter. The guard's reflection passes supply
// those themselves and so cannot judge them — they are asserted one by one in
// TestWireProjections_CarryTheArgumentsTheyAreHanded, which a fourth such
// projection is required to join.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// --- write bodies: the [NIM-817] shape itself ---

// toRiteCreateInput projects the POST /v1/augur/rites body onto the handler input.
func toRiteCreateInput(b RiteCreateRequest) handlers.RiteCreateInput {
	return handlers.RiteCreateInput{
		Omen:         b.Omen,
		Subject:      b.Subject.selector(),
		Allow:        b.Allow,
		Delegate:     b.Delegate,
		TokenTTL:     b.TokenTTL,
		TokenNumUses: b.TokenNumUses,
	}
}

// toChoirCreateInput projects the POST /v1/incarnations/{id}/choirs body onto the
// handler input. The incarnation comes from the path and is passed separately.
func toChoirCreateInput(b ChoirCreateRequest) handlers.ChoirCreateInput {
	return handlers.ChoirCreateInput{
		ChoirName:   b.ChoirName,
		Description: b.Description,
		MinSize:     b.MinSize,
		MaxSize:     b.MaxSize,
	}
}

// toVoiceAddInput projects the POST .../choirs/{choir}/voices body onto the handler
// input.
func toVoiceAddInput(b VoiceAddRequest) handlers.VoiceAddInput {
	return handlers.VoiceAddInput{
		SID:      b.SID,
		Role:     b.Role,
		Position: b.Position,
	}
}

// toHeraldUpdateInput projects the PUT /v1/heralds/{id} body onto the handler input.
func toHeraldUpdateInput(b HeraldUpdateRequest) handlers.HeraldUpdateInput {
	return handlers.HeraldUpdateInput{
		Type:      b.Type,
		Config:    b.Config,
		SecretRef: b.SecretRef,
		Secret:    b.Secret,
		Enabled:   b.Enabled,
	}
}

// toTidingUpdateInput projects the PUT /v1/tidings/{id} body onto the handler input.
func toTidingUpdateInput(b TidingUpdateRequest) handlers.TidingUpdateInput {
	return handlers.TidingUpdateInput{
		Herald:       b.Herald,
		EventTypes:   b.EventTypes,
		OnlyFailures: b.OnlyFailures,
		OnlyChanges:  b.OnlyChanges,
		Incarnation:  b.Incarnation,
		Cadence:      b.Cadence,
		Task:         b.Task,
		Annotations:  b.Annotations,
		Projection:   b.Projection,
		Enabled:      b.Enabled,
	}
}

// toResolveIDRequest projects the POST /v1/incarnations/resolve-id body onto the
// handler input. The route writes nothing, so a dropped field here costs a wrong
// lookup rather than a lost write — but a wrong lookup is what an operator then
// names their incarnation after.
func toResolveIDRequest(b IncarnationResolveIDRequest) handlers.ResolveIDRequest {
	return handlers.ResolveIDRequest{
		Service:        b.Service,
		CreateScenario: b.CreateScenario,
		Input:          b.Input,
		Covens:         b.Covens,
	}
}

// toOperatorCreateInput projects the POST /v1/operators body onto the handler input.
func toOperatorCreateInput(b OperatorCreateRequest) handlers.OperatorCreateInput {
	return handlers.OperatorCreateInput{
		AID:         b.AID,
		DisplayName: b.DisplayName,
		Roles:       b.Roles,
	}
}

// toProvisioningPolicyUpdateInput projects the PUT /v1/provisioning-policy body
// onto the handler input.
func toProvisioningPolicyUpdateInput(b ProvisioningPolicyUpdateRequest) handlers.ProvisioningPolicyUpdateInput {
	return handlers.ProvisioningPolicyUpdateInput{
		AllowedMethods: b.AllowedMethods,
	}
}

// toPushProviderUpdateInput projects the PUT /v1/push-providers/{id} body onto the
// handler input.
func toPushProviderUpdateInput(b PushProviderUpdateRequest) handlers.PushProviderUpdateInput {
	return handlers.PushProviderUpdateInput{
		Params: b.Params,
	}
}

// toRoleCreateInput projects the POST /v1/roles body onto the handler input.
func toRoleCreateInput(b RoleCreateRequest) handlers.RoleCreateInput {
	return handlers.RoleCreateInput{
		Name:         b.Name,
		Description:  b.Description,
		Permissions:  b.Permissions,
		DefaultScope: b.DefaultScope,
		ParentRole:   b.ParentRole,
		ScopeMode:    b.ScopeMode,
	}
}

// toUpdatePermissionsInput projects the PATCH /v1/roles/{name}/permissions body onto
// the handler input; the role's name is a path parameter and is passed separately.
//
// Three of the body's fields are [Optional], whose presence bit becomes a `Set…`
// field of its own on the native side — four booleans that no all-at-once fixture
// can tell apart, since every one of them is simply true. The wire root is the BODY
// rather than the huma input so that the guard's one-field-at-a-time pass isolates
// them: with only `confirm_cascade` sent, a `ConfirmCascade` reading
// `DefaultScope.Set` arrives false, and swapping any two of the `Set` bits is the
// same shape. Rooting on the input struct would put all five under one `Body` field
// and check them only together.
func toUpdatePermissionsInput(b RolePermissionsUpdateRequest, name string) handlers.UpdatePermissionsInput {
	return handlers.UpdatePermissionsInput{
		Name:            name,
		Permissions:     b.Permissions,
		SetDefaultScope: b.DefaultScope.Set,
		DefaultScope:    optionalToPtr(b.DefaultScope),
		SetParentRole:   b.ParentRole.Set,
		ParentRole:      optionalToPtr(b.ParentRole),
		SetScopeMode:    b.ScopeMode.Set,
		ScopeMode:       optionalString(b.ScopeMode),
		ConfirmCascade:  b.ConfirmCascade,
	}
}

// toServiceUpdateInput projects the PATCH /v1/services/{id} body onto the handler
// input.
func toServiceUpdateInput(b ServiceUpdateRequest) handlers.ServiceUpdateInput {
	return handlers.ServiceUpdateInput{
		Git:     b.Git,
		Ref:     b.Ref,
		Refresh: b.Refresh,
	}
}

// toSigilAllowInput projects the POST /v1/plugins/sigils body onto the handler input.
func toSigilAllowInput(b PluginSigilAllowRequest) handlers.SigilAllowInput {
	return handlers.SigilAllowInput{
		Alias:  b.Alias,
		Source: b.Source,
		Ref:    b.Ref,
	}
}

// toSynodCreateInput projects the POST /v1/synods body onto the handler input.
func toSynodCreateInput(b SynodCreateRequest) handlers.SynodCreateInput {
	return handlers.SynodCreateInput{
		Name:        b.Name,
		Description: b.Description,
	}
}

// toSynodUpdateInput projects the PATCH /v1/synods/{name} body onto the handler
// input. The group name comes from the path and is passed separately.
func toSynodUpdateInput(b SynodUpdateRequest) handlers.SynodUpdateInput {
	return handlers.SynodUpdateInput{
		Description: b.Description,
	}
}

// --- the caption ---

// toLabelSetInput projects a `PUT …/label` body onto the handler input.
//
// ONE function for all eight registries, because the body is one type. Eight
// identical one-field literals were eight places for the ninth registry's caption
// to be written differently; there is now one, and the guard walks it.
func toLabelSetInput(b LabelSetRequest) handlers.LabelSetInput {
	return handlers.LabelSetInput{Label: b.Label}
}

// --- list / query inputs ---
//
// The same hazard with a different symptom: a dropped FILTER widens a read rather
// than losing a write, so it surfaces as an operator being shown rows they asked
// to exclude instead of vanishing without trace. On a list narrowed by an RBAC
// purview that is still a disclosure.

// toConsoleRecordingListInput projects the GET /v1/console/recordings query onto the
// handler input.
func toConsoleRecordingListInput(in *consoleRecordingListInput) handlers.ConsoleRecordingListInput {
	return handlers.ConsoleRecordingListInput{
		SID:           in.SID,
		ArchonAID:     in.ArchonAID,
		Kind:          in.Kind,
		StartedAfter:  in.StartedAfter,
		StartedBefore: in.StartedBefore,
		Offset:        int(in.Offset),
		Limit:         int(in.Limit),
	}
}

// toErrandListInput projects the GET /v1/errands query onto the handler input.
func toErrandListInput(in *errandListInput) handlers.ErrandListInput {
	return handlers.ErrandListInput{
		SID:          in.SID,
		Status:       in.Status,
		StartedAfter: in.StartedAfter,
		Modules:      in.Modules,
		Offset:       int(in.Offset),
		Limit:        int(in.Limit),
	}
}

// toIncarnationListQuery projects the GET /v1/incarnations query onto the handler
// input.
//
// stateParams is a second argument rather than a field of the input because the
// `state.<field>` filters have DYNAMIC keys, which huma cannot bind — they are
// read from the raw query on the context (stateParamsFromContext).
func toIncarnationListQuery(in *incListInput, stateParams map[string][]string) handlers.IncarnationListQuery {
	return handlers.IncarnationListQuery{
		Offset:      int(in.Offset),
		Limit:       int(in.Limit),
		Service:     in.Service,
		Status:      in.Status,
		Coven:       in.Coven,
		SortBy:      in.SortBy,
		SortDir:     in.SortDir,
		StateParams: stateParams,
	}
}

// toAllRunsInput projects the GET /v1/runs query onto the handler input.
func toAllRunsInput(in *runsListInput) handlers.AllRunsInput {
	return handlers.AllRunsInput{
		Status:        in.Status,
		Incarnation:   in.Incarnation,
		Service:       in.Service,
		Q:             in.Q,
		StartedAfter:  in.StartedAfter,
		StartedBefore: in.StartedBefore,
		Sort:          in.Sort,
		SortDir:       in.SortDir,
		Offset:        int(in.Offset),
		Limit:         int(in.Limit),
	}
}

// toSoulHistoryInput projects the GET /v1/souls/{sid}/history path+query onto the
// handler input.
func toSoulHistoryInput(in *soulHistoryInput) handlers.SoulHistoryInput {
	return handlers.SoulHistoryInput{
		SID:    in.SID,
		Types:  in.Types,
		Since:  in.Since,
		Offset: int(in.Offset),
		Limit:  int(in.Limit),
	}
}

// toSoulListInput projects the GET /v1/souls query onto the handler input.
//
// page and cursor are arguments rather than fields read here because soulParsePage
// VALIDATES them — out-of-range bounds and a malformed cursor are a 400 the route
// must answer before it projects anything, and offset together with a cursor is a
// 422. Carrying the pair through this function would mean either answering that
// from inside a projection or losing the refusal.
func toSoulListInput(in *soulListInput, page sharedapi.Page, cursor *sharedapi.KeysetCursor) handlers.SoulListInput {
	return handlers.SoulListInput{
		Covens:     in.Coven,
		Status:     in.Status,
		Transport:  in.Transport,
		Unassigned: in.Unassigned,
		SIDPrefix:  in.SIDPrefix,
		Page:       page,
		Cursor:     cursor,
	}
}

// toVoyageListInput projects the GET /v1/voyages query onto the handler input. The
// bounds are checked by the route before this runs (CheckPageBounds → 400).
func toVoyageListInput(in *voyageListInput) handlers.VoyageListInput {
	return handlers.VoyageListInput{
		Kind:     in.Kind,
		Statuses: in.Statuses,
		Page:     sharedapi.Page{Offset: int(in.Offset), Limit: int(in.Limit)},
	}
}
