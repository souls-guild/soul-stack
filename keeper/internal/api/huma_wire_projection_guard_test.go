package api

// Structural guard against a request field being ACCEPTED AND DROPPED ([NIM-817],
// generalised to every wire→native projection in the package by [NIM-824]).
//
// PROBLEM. A route binds one struct for the schema (huma) and calls the handler
// with another (handler-native), and the projection between them is a hand-written
// field list. Omit a line and there is no symptom anywhere: the body validates
// against a schema that declares the field, the route answers 2xx, and the value is
// gone. No existing test fails, because every per-domain test sends only the fields
// it already knows about. `label` was lost that way on all eight registries at once
// for the whole of NIM-728/729 — the schema declared it, the handler input carried
// it, the domain stored it, and eight literals in huma_<domain>.go did not mention
// it.
//
// That silence is the defect, not the field. A client that gets a 2xx cannot tell
// "stored" from "discarded" without reading the row back, so the wire contract is
// not "the schema says the field exists" — it is "a field the schema accepts
// reaches the write".
//
// WHY THIS FILE IS NOT NINE MORE TESTS. NIM-817 closed the eight captioned creates
// and left the other hand-written projections in this package uncovered, because
// they TRANSFORM as they project and a value-equality check reported a false drop on
// every transformed field. The ticket called those nine; the source sweep below
// found TWENTY projection functions — the eight captioned creates plus twelve — and
// thirty more mappings still written inline, which [NIM-831] has since extracted
// into huma_request_input.go. There are now FORTY-THREE, and no inline mapping left.
// Writing nine more per-route tests would have been green today and hollow tomorrow:
// a per-route test guards the fields somebody remembered to enumerate, and the fields
// that go missing are exactly the ones nobody enumerated. So the subject here is the
// MECHANISM, and the property it has to have is this one:
//
//	add a field to a wire struct without touching the projection → something is red.
//
// DETECTION MECHANISM — a completeness half and a per-field half, then the spec:
//
//  1. COMPLETENESS, derived from the package's own source rather than from a list.
//     TestWireProjections_CoverEveryProjectionInThePackage parses every non-test file
//     here and looks for BOTH forms a mapping is written in:
//     — a projection FUNCTION (one wire-side parameter, one `handlers.` result),
//     which must be driven below or exempted BY NAME with a reason;
//     — a `handlers.X{…}` LITERAL outside any such function, which must be named in
//     inlineProjections with a reason.
//     Both are red the day a new one appears unlisted. The second form is the one
//     NIM-817's defect had; there were thirty of them, inventoried but NOT driven,
//     and [NIM-831] extracted all thirty, so inlineProjections is now empty and
//     every mapping in this package is driven. The matcher errs wide on purpose; it
//     was narrow once, missed toAuditListFilter for having a pointer parameter, and
//     reported full coverage while doing so.
//
//  2. PER-FIELD, by reflection over the real projection function, in three passes,
//     and none of the three subsumes another:
//     — ALL FIELDS AT ONCE, each holding a value unique to it: the native field the
//     wire field maps onto must hold that same value. Uniqueness catches
//     mis-SOURCING (`Label: b.Task`), not only omission.
//     — ONE FIELD AT A TIME, which identifies each field's source outright and so
//     covers the types with too few values to be unique (a bool holds one bit).
//     Slice-rooted projections are isolated one ELEMENT field at a time, which is
//     what reaches the two `*bool` on wire.VoyageNotify.
//     — AN EMPTY BODY, where every native field must be zero. The other two only
//     ever see non-zero input, so a projection that ignores the request and writes
//     a CONSTANT satisfies both; this one has no true to explain. It is also the
//     only pass that exercises the drop arm of `if b.X != "" { … }`.
//
//     The walk descends the WIRE value and resolves each leaf against the native
//     side BY PATH, so a nested struct is not a black box: `Subject.Trait.Key` is
//     checked as a field in its own right. Where the path does not land on a field
//     of the same name, the mapping is DECLARED (`renames`) — which is what makes a
//     new nested field red rather than silently unchecked.
//
//  3. COMPLETENESS OF THE CAPTIONED CREATES, derived from the spec:
//     TestCreateProjections_CoverEveryCaptionedCreateRoute requires every POST
//     operation in buildFullOpenAPISpec whose request body declares `label` to have
//     a projection here. A ninth registry that grows a caption and forgets the
//     projection goes red there; it is invisible to half 2, which only checks
//     projections it already knows about, and to half 1, which sees a projection
//     that exists rather than one that is missing.
//
// A DEPARTURE FROM "SAME NAME, SAME VALUE" IS A DECLARATION, NOT A GAP. This is the
// whole of [NIM-824]: "we decided not to carry this" and "we forgot to carry this"
// looked identical before, and each of the three escape hatches now requires a
// written reason, checked:
//
//   - `renames`  — the native field is there under a different path. Subject's four
//     wire members landing on six flat fields of subject.Selector is
//     the large case; `Selector.Sids` → `SIDs` is the small one.
//   - `shaped`   — the native form is not comparable to the wire form by value, so
//     the expected value is COMPUTED from the wire value, or (with no
//     `want`) the field is checked for presence alone.
//   - `unmapped` — deliberately not carried at all.
//
// An undeclared shape change is an ERROR rather than a silent downgrade to a
// presence check, which is the one design decision here worth arguing about: it
// means a legitimate change to a projection can turn this file red. That is the
// point. The alternative — degrade quietly — is how `Subject` sat unchecked below
// its top level for as long as it did.
//
// WHAT IT DOES NOT COVER, deliberately and by name:
//
//   - AN INLINE MAPPING, if one is written again. inlineProjections names every
//     `handlers.X{…}` literal outside a projection function, and naming is ALL it
//     does: there is no function to call, so no pass below would prove a field of
//     its arrives. It is EMPTY today — [NIM-831] extracted the thirty that were
//     there — so the weak half currently covers nothing, and the guarantee is the
//     strong one. A new literal is in neither registry and goes red.
//   - A route that stops CALLING its projection. This drives the functions, not the
//     routes. TestLabelOnCreate_ReachesTheWriteAndTheReply (huma_label_test.go)
//     closes it FOR THE CAPTION by driving all eight create routes over HTTP. Only
//     for the caption: a closure that went back to an inline literal carrying
//     `Label` but dropping some other field leaves both guards green — though the
//     literal itself would now have to be declared.
//   - A VALUE THE PROJECTION IS HANDED rather than reads — by THE REFLECTION
//     PASSES, which is why it is not left there. Three extracted mappings take an
//     argument beyond the wire root (a page the route parsed and could REFUSE, the
//     dynamic `state.<field>` filters huma cannot bind, a {name} path parameter),
//     and the walk SUPPLIES it, so deleting the line that carries one leaves all
//     three passes green. They are driven instead by
//     TestWireProjections_CarryTheArgumentsTheyAreHanded, and the completeness half
//     refuses any projection with extra parameters that is not named in
//     extraArgumentsDriven — so the assertion cannot be forgotten for a fourth.
//   - A mapping whose shape neither half recognises: a second result that is not
//     `error`, a METHOD with a receiver, or a result outside package handlers.
//     `Subject.selector()` (huma_subject.go) is all three at once and is invisible
//     to both halves — its fields are covered here only because subjectFlattening
//     spells them out on the two projections that call it. The pointer, slice and
//     variadic forms ARE matched, after one of them was missed, and since
//     [NIM-831] so are extra parameters in any position.
//   - A mapping written by assignment rather than as a literal
//     (`in := handlers.XInput{}; in.A = b.A`). The inline sweep looks for a non-empty
//     composite literal, so this form is in neither inventory. None exists today.
//   - Whether a declared transform is the RIGHT one. `[].Annotations` is compared
//     against an independent json.Marshal rather than against marshalAnnotations,
//     so that one is not a tautology; a transform written as a call to the
//     production helper would be, and should not be added.
//   - A body schema reached through a second level of `$ref`, through `allOf`, or a
//     create that is not a POST — the blind spots of half 3's spec sweep.
//   - Anything outside this package. The sweep in half 1 reads this directory only.
//
// HOW TO BREAK IT ON PURPOSE — every line below was RUN as a real edit and produces
// a red on the projection it is applied to:
//   - delete any `Label:` line in huma_create_input.go → that route's subtest names
//     the field;
//   - change `Label: b.Label` to `Label: b.Task` in toTidingCreateInput → same,
//     reported as reading the wrong source rather than as a drop;
//   - change `Enabled: b.Enabled` to `Enabled: b.OnlyFailures` there → caught by the
//     one-field-at-a-time pass, and by that pass only;
//   - swap `OnlyFailures`/`OnlyChanges` inside toNotifyRequests → caught by that
//     pass reaching into the SLICE ELEMENT, and by nothing else;
//   - replace `Enabled: b.Enabled` with a hardcoded `&true` → caught by the
//     empty-body pass, and by nothing else;
//   - drop `Coven:` from the target literal in toCadenceCreateRequest, or `Offset:`
//     from toAuditListFilter → named as a dropped field;
//   - add a field to any wire struct — `wire.VoyageTarget`, `Subject`,
//     `wire.VoyageNotify`, `auditListInput` — and leave the projection alone →
//     named as having no field on the handler input;
//   - add a new to<X>Input function and forget to register it, or write a new
//     `handlers.X{…}` literal in a closure → half 1 names it;
//   - add a field to ServiceUpdateRequest and leave toServiceUpdateInput alone →
//     all three passes name it. That same edit was GREEN before [NIM-831]: the
//     mapping was a literal inside the register closure, inlineProjections is keyed
//     by file/func/type and a new wire field changes no key, so the only thing that
//     knew about the field was the schema;
//   - drop `Modules:` from toErrandListInput → named as a dropped filter, which on
//     a list narrowed by an RBAC purview WIDENS the read;
//   - change `SortDir: in.SortDir` to `in.SortBy` in toIncarnationListQuery →
//     reported as reading the wrong source, not as a drop.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// fieldTransform — a wire field whose native form cannot be compared to it by
// value, and what to do about it.
//
// want computes the value the native side must hold, from the wire value of this
// field. A nil want means the field is checked for PRESENCE only — a weaker claim,
// which is why the reason is mandatory either way.
type fieldTransform struct {
	why  string
	want func(wire any) any
}

// wireProjection — one hand-written wire→native projection, in a form a test can
// drive: `wire` is a zero value of the request type (reflection fills it), `project`
// runs the production function on a filled copy.
//
// The three declaration maps are keyed by the wire field's path RELATIVE TO THE
// PROJECTION ROOT, with slice indices normalised to `[]`: "Label",
// "Subject.Trait.Key", "[].Annotations". Empty on a projection that maps
// field-for-field by name, which most of these still do.
type wireProjection struct {
	// fn is the production function's name — the key half 1 compares the package's
	// source against, so a typo here is a red test and not a silent hole.
	fn      string
	wire    any
	project func(any) any

	// renames maps a wire path onto the native path that carries it, when the two
	// differ. Both are paths from the root, because a native form may FLATTEN a
	// nested wire one (Subject.Incarnation.Name → Subject.Incarnation) and a
	// per-segment rename could not express that. A key may not contain `[]`.
	renames map[string]string

	// shaped names the wire paths whose native form is not comparable by value.
	shaped map[string]fieldTransform

	// unmapped names the wire paths the projection deliberately does not carry at
	// all, with the reason. Two projections have one ([NIM-831]):
	// optionalNullHasNoFieldOfItsOwn and pageParsedBeforeProjection.
	unmapped map[string]string
}

// subjectFlattening — Subject's four wire members land on six flat fields of
// subject.Selector, so no amount of name matching finds them: `incarnation` is an
// object on the wire carrying `service` and `name`, and a flat pair of strings in
// the domain, where `name` is spelled `Incarnation`.
//
// Declaring the six leaves rather than treating Subject as opaque is what makes a
// FIFTH member of Subject red here: it would resolve to no native field and be
// reported, where a presence check on the whole object would not notice.
// selector() itself is pinned by huma_subject.go's own tests.
var subjectFlattening = map[string]string{
	"Subject.Coven":               "Subject.Covens",
	"Subject.SID":                 "Subject.SIDs",
	"Subject.Incarnation.Name":    "Subject.Incarnation",
	"Subject.Incarnation.Service": "Subject.Service",
	"Subject.Trait.Key":           "Subject.TraitKey",
	"Subject.Trait.Value":         "Subject.TraitValue",
}

// notifyAnnotations — the shared declaration for the notify element's annotations,
// which reach the write as raw JSON rather than as a map.
//
// The expected value is an independent json.Marshal, NOT a call to
// marshalAnnotations: comparing a projection against the helper it calls would pass
// whatever the helper did. Map key order is not a hazard — encoding/json sorts them.
var notifyAnnotations = fieldTransform{
	why: "the wire map is stored as the raw JSON of the tiding's annotations column",
	want: func(v any) any {
		b, err := json.Marshal(v.(map[string]any))
		if err != nil {
			panic(fmt.Sprintf("annotations fixture does not marshal: %v", err))
		}
		return json.RawMessage(b)
	},
}

// wireProjections — every hand-written wire→native projection in this package.
// Half 1 requires this list to be the list the package's source declares.
var wireProjections = []wireProjection{
	// The eight captioned create bodies ([ADR-0085]), in huma_create_input.go.
	{
		fn:      "toServiceRegisterInput",
		wire:    ServiceRegisterRequest{},
		project: func(b any) any { return toServiceRegisterInput(b.(ServiceRegisterRequest)) },
	},
	{
		fn:      "toIncarnationCreateInput",
		wire:    IncarnationCreateRequest{},
		project: func(b any) any { return toIncarnationCreateInput(b.(IncarnationCreateRequest)) },
	},
	{
		fn:      "toHeraldCreateInput",
		wire:    HeraldCreateRequest{},
		project: func(b any) any { return toHeraldCreateInput(b.(HeraldCreateRequest)) },
	},
	{
		fn:      "toTidingCreateInput",
		wire:    TidingCreateRequest{},
		project: func(b any) any { return toTidingCreateInput(b.(TidingCreateRequest)) },
	},
	{
		fn:      "toVigilCreateInput",
		wire:    VigilCreateRequest{},
		project: func(b any) any { return toVigilCreateInput(b.(VigilCreateRequest)) },
		renames: subjectFlattening,
	},
	{
		fn:      "toDecreeCreateInput",
		wire:    DecreeCreateRequest{},
		project: func(b any) any { return toDecreeCreateInput(b.(DecreeCreateRequest)) },
		renames: subjectFlattening,
	},
	{
		fn:      "toOmenCreateInput",
		wire:    OmenCreateRequest{},
		project: func(b any) any { return toOmenCreateInput(b.(OmenCreateRequest)) },
	},
	{
		fn:      "toPushProviderCreateInput",
		wire:    PushProviderCreateRequest{},
		project: func(b any) any { return toPushProviderCreateInput(b.(PushProviderCreateRequest)) },
	},

	// The twelve that transform as they project ([NIM-824]) — the ticket's nine, the
	// two notify element converters they delegate to, and toAuditListFilter.
	{
		fn:      "toCadenceCreateRequest",
		wire:    CadenceCreateRequest{},
		project: func(b any) any { return toCadenceCreateRequest(b.(CadenceCreateRequest)) },
		shaped:  map[string]fieldTransform{"Notify[].Annotations": notifyAnnotations},
	},
	{
		fn:      "toCadencePatchRequest",
		wire:    CadencePatchRequest{},
		project: func(b any) any { return toCadencePatchRequest(b.(CadencePatchRequest)) },
	},
	{
		fn:      "toVoyageCreateRequest",
		wire:    VoyageCreateRequest{},
		project: func(b any) any { return toVoyageCreateRequest(b.(VoyageCreateRequest)) },
		shaped:  map[string]fieldTransform{"Notify[].Annotations": notifyAnnotations},
	},
	{
		fn:      "toNotifyRequests",
		wire:    []VoyageNotify{},
		project: func(b any) any { return toNotifyRequests(b.([]VoyageNotify)) },
		shaped:  map[string]fieldTransform{"[].Annotations": notifyAnnotations},
	},
	{
		fn:      "toVoyageNotifyRequests",
		wire:    []VoyageNotify{},
		project: func(b any) any { return toVoyageNotifyRequests(b.([]VoyageNotify)) },
		shaped:  map[string]fieldTransform{"[].Annotations": notifyAnnotations},
	},
	{
		fn:      "toPushApplyInput",
		wire:    PushApplyRequest{},
		project: func(b any) any { return toPushApplyInput(b.(PushApplyRequest)) },
	},
	{
		fn:      "toModuleFormPrepInput",
		wire:    ModuleFormPrepRequest{},
		project: func(b any) any { return toModuleFormPrepInput(b.(ModuleFormPrepRequest)) },
	},
	{
		fn:      "toSoulCovenAssignInput",
		wire:    SoulCovenAssignRequest{},
		project: func(b any) any { return toSoulCovenAssignInput(b.(SoulCovenAssignRequest)) },
		renames: map[string]string{"Selector.Sids": "Selector.SIDs"},
	},
	{
		fn:      "toSoulTraitsAssignInput",
		wire:    SoulTraitsAssignRequest{},
		project: func(b any) any { return toSoulTraitsAssignInput(b.(SoulTraitsAssignRequest)) },
		renames: map[string]string{"Selector.Sids": "Selector.SIDs"},
	},
	{
		fn:      "toSoulSshTargetInput",
		wire:    SoulSshTarget{},
		project: func(b any) any { return toSoulSshTargetInput(b.(SoulSshTarget)) },
	},
	{
		fn:      "toErrandExecRequest",
		wire:    ErrandRunRequest{},
		project: func(b any) any { return toErrandExecRequest(b.(ErrandRunRequest)) },
	},
	{
		// A QUERY-parameter projection rather than a body one, and the reason the
		// matcher above had to be widened: its parameter is a POINTER, which the
		// first revision of the sweep did not unwrap — so it was uncovered while the
		// sweep reported full coverage.
		fn:      "toAuditListFilter",
		wire:    auditListInput{},
		project: func(b any) any { v := b.(auditListInput); return toAuditListFilter(&v) },
		shaped: map[string]fieldTransform{
			"Offset": widenInt32,
			"Limit":  widenInt32,
		},
	},

	// The thirty that were written as a literal inside a huma.Register closure
	// until [NIM-831] extracted them, in huma_request_input.go: fourteen write
	// bodies plus one read-resolve, the eight captions now sharing one projection,
	// and seven list/query inputs.
	{
		fn:      "toRiteCreateInput",
		wire:    RiteCreateRequest{},
		project: func(b any) any { return toRiteCreateInput(b.(RiteCreateRequest)) },
		renames: subjectFlattening,
	},
	{
		fn:      "toChoirCreateInput",
		wire:    ChoirCreateRequest{},
		project: func(b any) any { return toChoirCreateInput(b.(ChoirCreateRequest)) },
	},
	{
		fn:      "toVoiceAddInput",
		wire:    VoiceAddRequest{},
		project: func(b any) any { return toVoiceAddInput(b.(VoiceAddRequest)) },
	},
	{
		fn:      "toHeraldUpdateInput",
		wire:    HeraldUpdateRequest{},
		project: func(b any) any { return toHeraldUpdateInput(b.(HeraldUpdateRequest)) },
	},
	{
		fn:      "toTidingUpdateInput",
		wire:    TidingUpdateRequest{},
		project: func(b any) any { return toTidingUpdateInput(b.(TidingUpdateRequest)) },
	},
	{
		fn:      "toResolveIDRequest",
		wire:    IncarnationResolveIDRequest{},
		project: func(b any) any { return toResolveIDRequest(b.(IncarnationResolveIDRequest)) },
	},
	{
		fn:      "toOperatorCreateInput",
		wire:    OperatorCreateRequest{},
		project: func(b any) any { return toOperatorCreateInput(b.(OperatorCreateRequest)) },
	},
	{
		fn:      "toProvisioningPolicyUpdateInput",
		wire:    ProvisioningPolicyUpdateRequest{},
		project: func(b any) any { return toProvisioningPolicyUpdateInput(b.(ProvisioningPolicyUpdateRequest)) },
	},
	{
		fn:      "toPushProviderUpdateInput",
		wire:    PushProviderUpdateRequest{},
		project: func(b any) any { return toPushProviderUpdateInput(b.(PushProviderUpdateRequest)) },
	},
	{
		fn:      "toRoleCreateInput",
		wire:    RoleCreateRequest{},
		project: func(b any) any { return toRoleCreateInput(b.(RoleCreateRequest)) },
	},
	{
		// Rooted on the BODY even though the role's name is a path parameter, so
		// that the one-field-at-a-time pass can isolate the four booleans each
		// [Optional] contributes; `name` is asserted separately. See the
		// projection's own doc for why the input struct would be worse.
		fn:   "toUpdatePermissionsInput",
		wire: RolePermissionsUpdateRequest{},
		project: func(b any) any {
			return toUpdatePermissionsInput(b.(RolePermissionsUpdateRequest), "")
		},
		renames: map[string]string{
			"DefaultScope.Set":   "SetDefaultScope",
			"DefaultScope.Value": "DefaultScope",
			"ParentRole.Set":     "SetParentRole",
			"ParentRole.Value":   "ParentRole",
			"ScopeMode.Set":      "SetScopeMode",
			"ScopeMode.Value":    "ScopeMode",
		},
		unmapped: optionalNullHasNoFieldOfItsOwn,
	},
	{
		fn:      "toServiceUpdateInput",
		wire:    ServiceUpdateRequest{},
		project: func(b any) any { return toServiceUpdateInput(b.(ServiceUpdateRequest)) },
	},
	{
		fn:      "toSigilAllowInput",
		wire:    PluginSigilAllowRequest{},
		project: func(b any) any { return toSigilAllowInput(b.(PluginSigilAllowRequest)) },
	},
	{
		fn:      "toSynodCreateInput",
		wire:    SynodCreateRequest{},
		project: func(b any) any { return toSynodCreateInput(b.(SynodCreateRequest)) },
	},
	{
		fn:      "toSynodUpdateInput",
		wire:    SynodUpdateRequest{},
		project: func(b any) any { return toSynodUpdateInput(b.(SynodUpdateRequest)) },
	},
	{
		// One projection for all eight captioned registries — the eight identical
		// literals it replaced were eight places for a ninth to be written
		// differently.
		fn:      "toLabelSetInput",
		wire:    LabelSetRequest{},
		project: func(b any) any { return toLabelSetInput(b.(LabelSetRequest)) },
	},
	{
		fn:      "toConsoleRecordingListInput",
		wire:    consoleRecordingListInput{},
		project: func(b any) any { v := b.(consoleRecordingListInput); return toConsoleRecordingListInput(&v) },
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
	{
		fn:      "toErrandListInput",
		wire:    errandListInput{},
		project: func(b any) any { v := b.(errandListInput); return toErrandListInput(&v) },
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
	{
		fn:      "toIncarnationListQuery",
		wire:    incListInput{},
		project: func(b any) any { v := b.(incListInput); return toIncarnationListQuery(&v, nil) },
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
	{
		fn:      "toAllRunsInput",
		wire:    runsListInput{},
		project: func(b any) any { v := b.(runsListInput); return toAllRunsInput(&v) },
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
	{
		fn:      "toSoulHistoryInput",
		wire:    soulHistoryInput{},
		project: func(b any) any { v := b.(soulHistoryInput); return toSoulHistoryInput(&v) },
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
	{
		fn:   "toSoulListInput",
		wire: soulListInput{},
		project: func(b any) any {
			v := b.(soulListInput)
			return toSoulListInput(&v, sharedapi.Page{}, nil)
		},
		renames:  map[string]string{"Coven": "Covens"},
		unmapped: pageParsedBeforeProjection,
	},
	{
		fn:      "toVoyageListInput",
		wire:    voyageListInput{},
		project: func(b any) any { v := b.(voyageListInput); return toVoyageListInput(&v) },
		renames: map[string]string{"Offset": "Page.Offset", "Limit": "Page.Limit"},
		shaped:  map[string]fieldTransform{"Offset": widenInt32, "Limit": widenInt32},
	},
}

// optionalNullHasNoFieldOfItsOwn — an [Optional]'s `Null` bit, on each of the three
// PATCH-presence fields of the role-permissions body.
//
// It is not dropped: it CHOOSES between the two values the native pointer can hold
// — present-and-null becomes nil, present-with-a-value becomes &Value — so there is
// no field for the walk to land on, and declaring a rename onto DefaultScope would
// claim the bit arrives there when what arrives is its consequence.
//
// What is lost by not walking it is the `null` branch of optionalToPtr, and that is
// covered where it belongs: TestOptional_UnmarshalJSON_ThreeBranches and
// TestOptional_optionalToPtr (huma_optional_test.go) drive all three readings of a
// PATCH key — omitted, null, valued — against the helper every one of these
// projections calls.
var optionalNullHasNoFieldOfItsOwn = map[string]string{
	"DefaultScope.Null": "present-and-null is carried as a NIL DefaultScope rather than by a field of " +
		"its own; the branch itself is driven by TestOptional_optionalToPtr",
	"ParentRole.Null": "as DefaultScope.Null — an explicit null becomes a nil ParentRole",
	"ScopeMode.Null": "as DefaultScope.Null, and flatter still: optionalString reads null and empty " +
		"as the same thing, which is what the domain already treats as unset",
}

// pageParsedBeforeProjection — the three soul-list query fields that do NOT reach
// the handler input through the projection, because soulParsePage turns them into
// the (Page, Cursor) pair FIRST and can refuse the request while doing it.
//
// Declared rather than carried: moving the parse inside the projection would mean
// answering a 400 from inside one, and the walk supplies the pair itself, so
// checking it here would assert the fixture rather than the code.
var pageParsedBeforeProjection = map[string]string{
	"Offset": "soulParsePage validates it (out of range -> 400, offset together with a cursor -> 422) " +
		"and returns it inside sharedapi.Page before this projection runs; the route passes that pair in",
	"Limit": "as Offset — validated and folded into sharedapi.Page by soulParsePage before the projection",
	"Cursor": "soulParsePage DECODES it (malformed -> 400) into a *sharedapi.KeysetCursor; the string " +
		"never reaches the handler input, the decoded value does",
}

// widenInt32 — pagination crosses the boundary as a widening cast. Declared rather
// than waved through: `int(x)` and "some other int" are the same type on the native
// side, so only computing the expected value tells a carried field from a hardcoded
// one.
var widenInt32 = fieldTransform{
	why:  "int32 on the query, int in the domain — a widening cast, lossless in this range",
	want: func(v any) any { return int(v.(int32)) },
}

// projectionsNotDriven — projection FUNCTIONS the sweep finds and this file
// deliberately does not drive, with the reason.
//
// Empty. An entry here is a decision on record; silence would be the defect
// [NIM-824] is about.
var projectionsNotDriven = map[string]string{}

// inlineProjections — wire→native mappings still written as a `handlers.X{…}`
// literal inside a huma.Register closure instead of as a named function, keyed
// "<file>:<enclosing func>:<type>".
//
// EMPTY, and that is the state [NIM-831] left the package in: all thirty were
// extracted into huma_request_input.go and are now DRIVEN by the three reflection
// passes rather than merely counted. An empty map is not a dead one — the sweep
// still runs, and a literal written inline tomorrow is in neither registry and goes
// red the same day.
//
// An entry here is a deliberate exception, not a backlog item. It says "this
// mapping stays inline, and here is why" — and the reason had better be stronger
// than "it is small", because the literal that lost `label` on eight registries
// was small.
var inlineProjections = map[string]string{}

// captionedCreateProjections — the create route each captioned body reaches, by
// projection name. Half 3 compares its key set against the spec's own topology, and
// its values against wireProjections, so it cannot name a projection that is not
// driven.
var captionedCreateProjections = map[route]string{
	{http.MethodPost, "/v1/services"}:       "toServiceRegisterInput",
	{http.MethodPost, "/v1/incarnations"}:   "toIncarnationCreateInput",
	{http.MethodPost, "/v1/heralds"}:        "toHeraldCreateInput",
	{http.MethodPost, "/v1/tidings"}:        "toTidingCreateInput",
	{http.MethodPost, "/v1/vigils"}:         "toVigilCreateInput",
	{http.MethodPost, "/v1/decrees"}:        "toDecreeCreateInput",
	{http.MethodPost, "/v1/augur/omens"}:    "toOmenCreateInput",
	{http.MethodPost, "/v1/push-providers"}: "toPushProviderCreateInput",
}

// labelIsNotACaption — POST routes whose body carries a `label` that is NOT a
// display caption, with the reason. The spec sweep below cannot tell the two apart
// by shape: both are a JSON string called `label`, and only the meaning differs.
//
// Listing them is the deliberate half of the same decision the projections are: an
// entry here says "checked, different field", where silence would say nothing.
var labelIsNotACaption = map[route]string{
	{http.MethodPost, "/v1/souls/coven"}: "ADR-008: `label` here is a COVEN TAG being appended or removed in bulk, " +
		"not a display caption — the route creates nothing and the value is already carried by " +
		"toSoulCovenAssignInput (huma_soul.go)",
}

// TestWireProjections_CarryEveryRequestField drives each projection with a body in
// which every field holds a value unique to it, and pins that every field arrives
// unchanged and from the right source.
func TestWireProjections_CarryEveryRequestField(t *testing.T) {
	for _, p := range wireProjections {
		t.Run(p.fn, func(t *testing.T) {
			filled := reflect.New(reflect.TypeOf(p.wire)).Elem()
			(&uniqueFiller{}).fill(t, filled, p.fn)

			w := &projectionWalk{t: t, p: p, native: reflect.ValueOf(p.project(filled.Interface()))}
			w.walk(filled, "")
		})
	}
}

// TestWireProjections_ReadEachFieldFromItsOwnSource fills ONE request field at a
// time and requires the native field it maps onto to be the one that arrives.
//
// It exists because value-uniqueness cannot settle every type. A `bool` holds one
// bit, so with three `*bool` on TidingCreateRequest two of them necessarily carry
// the same value however the filler chooses them, and `Enabled: b.OnlyFailures` was
// green under the all-fields pass. Here it is not: with only `Enabled` set, a
// projection reading `OnlyFailures` reads nil and the field arrives empty.
//
// One field at a time is what makes the source unambiguous, and it is a sound
// fixture ONLY because these projections are straight copies — no field's treatment
// depends on another's. A projection that branched on a second field would need its
// own case rather than this loop.
//
// The two passes are complementary and both are needed: this one identifies the
// SOURCE of each field but says nothing about a projection that misbehaves on a
// fully-populated body, which is the shape a real request has.
func TestWireProjections_ReadEachFieldFromItsOwnSource(t *testing.T) {
	for _, p := range wireProjections {
		wireT := reflect.TypeOf(p.wire)
		// A slice-rooted projection is isolated one ELEMENT field at a time, in a
		// one-element slice. Skipping it instead — which an earlier revision did —
		// leaves the two `*bool` on wire.VoyageNotify with no pass that ever tells
		// them apart, and `OnlyFailures: derefBool(n.OnlyChanges)` swapped with its
		// neighbour stays green through both other passes.
		elemT, prefix := wireT, ""
		if wireT.Kind() == reflect.Slice {
			elemT, prefix = wireT.Elem(), "[0]"
		}
		if elemT.Kind() != reflect.Struct {
			continue
		}
		t.Run(p.fn, func(t *testing.T) {
			for i := 0; i < elemT.NumField(); i++ {
				f := elemT.Field(i)
				if !f.IsExported() {
					continue
				}
				at := prefix + "." + f.Name
				if _, skip := p.unmapped[ruleKey(at)]; skip {
					continue
				}
				t.Run(f.Name, func(t *testing.T) {
					elem := reflect.New(elemT).Elem()
					(&uniqueFiller{}).fill(t, elem.Field(i), p.fn+"."+f.Name)

					body := elem
					if prefix != "" {
						body = reflect.Append(reflect.MakeSlice(wireT, 0, 1), elem)
					}
					w := &projectionWalk{
						t:      t,
						p:      p,
						native: reflect.ValueOf(p.project(body.Interface())),
						oneHot: true,
					}
					w.walk(elem.Field(i), at)
				})
			}
		})
	}
}

// TestWireProjections_InventNothingFromAnEmptyBody projects a body in which nothing
// is set and requires the handler input to hold nothing either.
//
// It closes the one hole the other two passes share: they only ever see NON-ZERO
// wire values, so a projection that ignores its input and writes a CONSTANT
// satisfies both. `Enabled: ptrTrue()` in place of `Enabled: b.Enabled` arrives
// holding true in the all-fields pass (which expected true) and non-zero in the
// one-field pass (which asked only for non-zero) — and the client's `false` is
// silently overwritten on every request. Here the empty body has no true to explain
// it.
//
// It is also the only pass that exercises the DROP arm of a conditional projection
// (`if b.SSHProvider != ""`), so the two directions of "omitted stays omitted" are
// both covered rather than only the carrying one.
//
// A declared transform is NOT applied here: the claim is "nothing arrives", not
// "this particular nothing arrives", and marshalAnnotations(nil) and json.Marshal(nil)
// legitimately differ on the empty case.
func TestWireProjections_InventNothingFromAnEmptyBody(t *testing.T) {
	for _, p := range wireProjections {
		t.Run(p.fn, func(t *testing.T) {
			zero := reflect.New(reflect.TypeOf(p.wire)).Elem()
			w := &projectionWalk{
				t:        t,
				p:        p,
				native:   reflect.ValueOf(p.project(zero.Interface())),
				zeroBody: true,
			}
			w.walk(zero, "")
		})
	}
}

// TestWireProjections_CoverEveryProjectionInThePackage is the completeness half
// that generalises past `label`: the set of projections driven above must be the set
// the package's OWN SOURCE declares.
//
// The shape it matches — a package-local parameter somewhere, one `handlers.`
// result — is what every wire→native projection here looks like and what nothing
// else here looks like. The reply projections go the other way (`handlers.X` in, a
// local type out) and are not this ticket's hazard: a field missing from a reply is
// a field missing from the JSON, which a client sees.
//
// Over-matching is the safe direction. A future function of this shape that is not a
// projection is red until someone names it in projectionsNotDriven with a reason,
// which costs one line and records the decision; under-matching would be a hole of
// exactly the kind this test exists to close.
func TestWireProjections_CoverEveryProjectionInThePackage(t *testing.T) {
	byName := map[string]wireProjection{}
	for _, p := range wireProjections {
		if _, dup := byName[p.fn]; dup {
			t.Errorf("wireProjections lists %s twice", p.fn)
		}
		byName[p.fn] = p
		for wirePath, nativePath := range p.renames {
			if strings.Contains(wirePath, "[]") {
				t.Errorf("%s: rename key %q crosses a slice; the resolver maps whole paths and "+
					"cannot rebuild indices — declare the element's field under `shaped` instead",
					p.fn, wirePath)
			}
			if nativePath == "" {
				t.Errorf("%s: rename of %q names no native path", p.fn, wirePath)
			}
			if strings.ContainsAny(nativePath, "[]") {
				t.Errorf("%s: rename value %q for %q contains an index; the resolver maps "+
					"whole paths and does not rebuild them", p.fn, nativePath, wirePath)
			}
		}
		for path, tr := range p.shaped {
			if tr.why == "" {
				t.Errorf("%s: %q is declared as shape-changed with no reason; an unexplained "+
					"departure is what this guard exists to catch", p.fn, path)
			}
		}
		for path, why := range p.unmapped {
			if why == "" {
				t.Errorf("%s: %q is listed as unmapped with no reason; an unexplained gap is "+
					"what this guard exists to catch", p.fn, path)
			}
		}
	}

	declared, err := projectionsDeclaredInPackage(".")
	if err != nil {
		t.Fatalf("sweeping the package source: %v", err)
	}
	if len(declared.funcs) == 0 {
		t.Fatal("the source sweep matched no projection at all — either the package was " +
			"restructured or the matcher stopped recognising the shape, and this test would " +
			"pass vacuously from here on")
	}

	var missing, stale, undrivenArgs []string
	for fn, d := range declared.funcs {
		_, driven := byName[fn]
		why, exempt := projectionsNotDriven[fn]
		switch {
		case driven && exempt:
			t.Errorf("%s is both driven and exempt — the two registries must be disjoint", fn)
		case exempt && why == "":
			t.Errorf("%s is exempt with no reason recorded", fn)
		case !driven && !exempt:
			missing = append(missing, fmt.Sprintf("%s (%s)", fn, d.at))
		}
		// A parameter beyond the wire root is invisible to all three reflection
		// passes: the walk descends the ROOT, and the extra value is supplied by
		// the fixture, so deleting the line that carries it leaves every pass
		// green. Each one therefore owes an explicit assertion — but only if it is
		// DRIVEN at all; telling the reader to assert a projection that is on
		// record as not driven would be unactionable.
		if _, checked := extraArgumentsDriven[fn]; d.params > 1 && driven && !checked {
			undrivenArgs = append(undrivenArgs, fmt.Sprintf("%s (%s), %d parameters", fn, d.at, d.params))
		}
	}
	for fn, what := range extraArgumentsDriven {
		if what == "" {
			t.Errorf("%s is listed in extraArgumentsDriven with no argument named — the entry has to "+
				"say WHAT is asserted, or it records only that somebody looked", fn)
		}
		if d, ok := declared.funcs[fn]; !ok || d.params <= 1 {
			stale = append(stale, "extraArgumentsDriven: "+fn)
		}
	}
	for fn := range byName {
		if _, ok := declared.funcs[fn]; !ok {
			stale = append(stale, "wireProjections: "+fn)
		}
	}
	for fn := range projectionsNotDriven {
		if _, ok := declared.funcs[fn]; !ok {
			stale = append(stale, "projectionsNotDriven: "+fn)
		}
	}

	if len(declared.collisions) > 0 {
		sort.Strings(declared.collisions)
		t.Errorf("TWO INLINE MAPPINGS SHARE ONE INVENTORY KEY — %d:\n  %s\n"+
			"-> the key is \"<file>:<func>:<type>\", so a second literal of the same type in "+
			"the same function is accounted for by the first one's entry and nothing lists it. "+
			"Extract at least one of them into a named function ([NIM-831]).",
			len(declared.collisions), strings.Join(declared.collisions, "\n  "))
	}

	// The inline half. These cannot be driven by reflection — there is no function to
	// call — so the claim here is weaker and deliberately so: each one is COUNTED and
	// NAMED, which is what separates "known, not extracted yet" from silence.
	var undeclaredInline []string
	for key, where := range declared.inline {
		why, known := inlineProjections[key]
		switch {
		case !known:
			undeclaredInline = append(undeclaredInline, fmt.Sprintf("%s (%s)", key, where))
		case why == "":
			t.Errorf("%s is listed in inlineProjections with no reason", key)
		}
	}
	for key := range inlineProjections {
		if _, ok := declared.inline[key]; !ok {
			stale = append(stale, "inlineProjections: "+key)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	sort.Strings(undeclaredInline)

	sort.Strings(undrivenArgs)
	if len(undrivenArgs) > 0 {
		t.Errorf("A PROJECTION TAKES A VALUE NOTHING CHECKS — %d:\n  %s\n"+
			"-> the three reflection passes descend the WIRE ROOT and the test supplies every other "+
			"argument, so deleting the line that carries one leaves all three green and the call site "+
			"compiling (an unused parameter is legal Go). Assert it in "+
			"TestWireProjections_CarryTheArgumentsTheyAreHanded and name it in extraArgumentsDriven.",
			len(undrivenArgs), strings.Join(undrivenArgs, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("HAND-WRITTEN PROJECTION WITH NO GUARD — %d:\n  %s\n"+
			"-> add each to wireProjections. Until then nothing proves the fields it enumerates "+
			"reach the handler, which is the whole of [NIM-817]/[NIM-824]. If it is not a "+
			"wire->native projection, say so in projectionsNotDriven.",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(undeclaredInline) > 0 {
		t.Errorf("WIRE->NATIVE MAPPING WRITTEN INLINE, AND NOT DECLARED — %d:\n  %s\n"+
			"-> a `handlers.X{…}` literal outside a projection function is the exact form "+
			"[NIM-817] was: a hand-written field list with no function to point a guard at. "+
			"Extract it into a named function and add it to wireProjections (which is what "+
			"NIM-817 did for the eight create bodies), or record it in inlineProjections with "+
			"the reason it stays inline. Do not leave it unlisted — an unlisted one is "+
			"indistinguishable from one nobody noticed ([NIM-824]).",
			len(undeclaredInline), strings.Join(undeclaredInline, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("STALE DECLARATION — %d entries naming something this package no longer "+
			"declares:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
}

// packageProjections — what the source sweep found, in the TWO forms this package
// writes a wire→native mapping. Both are hand-written field lists and both carry the
// [NIM-817] hazard; only the first can be driven by reflection.
type packageProjections struct {
	// funcs — named projection functions, by name → what the sweep learned.
	funcs map[string]declaredProjection
	// inline — a `handlers.X{…}` composite literal written OUTSIDE any projection
	// function, which is where the mapping sits when nobody has extracted it. This
	// is the form the eight create bodies had when `label` went missing. Keyed by
	// "<file>:<enclosing func>:<type>", which survives an edit above it where a line
	// number would not.
	inline map[string]string
	// collisions — two inline literals that share one key, which the inventory
	// cannot tell apart. Reported rather than deduplicated: the second one would
	// otherwise be accounted for by the first one's entry and go unlisted.
	collisions []string
}

// projectionsDeclaredInPackage parses every non-test file in dir and returns both
// forms of hand-written mapping it declares.
// declaredProjection — one projection function as the source declares it.
type declaredProjection struct {
	at string
	// params is the full parameter count. Anything above one is a value the
	// projection is HANDED rather than reads off the wire root, which no
	// reflection pass can drive — see extraArgumentsDriven.
	params int
}

func projectionsDeclaredInPackage(dir string) (packageProjections, error) {
	out := packageProjections{funcs: map[string]declaredProjection{}, inline: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+name, nil, 0)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv == nil && isWireToNativeShape(fd.Type) {
				out.funcs[fd.Name.Name] = declaredProjection{
					at:     fmt.Sprintf("%s:%d", name, fset.Position(fd.Pos()).Line),
					params: paramCount(fd.Type),
				}
				// Its own literals ARE the projection this file drives by reflection.
				continue
			}
			where := "<package-level>"
			if ok {
				where = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				// An EMPTY literal maps no field and so cannot drop one; it is a type
				// reference, not a projection.
				if !ok || len(lit.Elts) == 0 {
					return true
				}
				t, isHandlers := handlersTypeName(lit.Type)
				if !isHandlers {
					return true
				}
				key := fmt.Sprintf("%s:%s:%s", name, where, t)
				at := fmt.Sprintf("%s:%d", name, fset.Position(lit.Pos()).Line)
				if prior, dup := out.inline[key]; dup {
					// Two literals of one type in one function collapse onto the same
					// key, and the inventory would then account for only one of them.
					// Recorded as its own defect rather than silently overwritten.
					out.collisions = append(out.collisions,
						fmt.Sprintf("%s (%s and %s)", key, prior, at))
					return true
				}
				out.inline[key] = at
				return true
			})
		}
	}
	return out, nil
}

// isWireToNativeShape reports whether a signature is "a wire-side value in, one
// handlers value out" — the shape of every projection function in this package.
//
// Slices, pointers and variadics are unwrapped on both sides, and a trailing `error`
// result is allowed, because none of those changes what the function IS. The matcher
// was narrower once and missed toAuditListFilter, whose parameter is a pointer, while
// reporting full coverage — so it errs wide: a false match costs one line in
// projectionsNotDriven, a false miss is an unguarded projection that looks guarded.
//
// [NIM-831] widened it twice more, to "ANY parameter is wire-side" and to accepting a
// grouped parameter list. Three of the thirty literals it extracted read a value that
// is neither the body nor a bound parameter — a page that had to be parsed and
// REFUSED before the projection ran, the dynamic `state.<field>` filters huma cannot
// bind, a path parameter — and the one-parameter rule left the choice between
// changing what the route does and leaving the mapping inline. Which parameter is the
// WIRE ROOT is then a convention the registry entry states rather than something the
// matcher can know, so the matcher only has to FIND the function; the arguments the
// walk cannot drive are covered by extraArgumentsDriven.
func isWireToNativeShape(ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) == 0 {
		return false
	}
	if ft.Results == nil || len(ft.Results.List) == 0 || len(ft.Results.List) > 2 {
		return false
	}
	if len(ft.Results.List) == 2 && !isErrorType(ft.Results.List[1].Type) {
		return false
	}
	if !isHandlersType(ft.Results.List[0].Type) {
		return false
	}
	// ANY parameter, not the first: which one is the wire root is a convention
	// the registry entry states, and a projection written `to<X>(h handlers.Foo,
	// in *fooInput)` would otherwise be invisible to BOTH halves — matched by
	// neither the function sweep nor the inline one. Wider again, same direction.
	for _, p := range ft.Params.List {
		if isWireSideType(p.Type) {
			return true
		}
	}
	return false
}

// paramCount counts a signature's parameters, expanding a grouped declaration
// (`a, b string` is two). A projection with more than one is taking a value it
// does not READ from the wire root, which the walk cannot drive — so the count is
// what makes such a projection owe an entry in extraArgumentsDriven.
func paramCount(ft *ast.FuncType) int {
	n := 0
	for _, p := range ft.Params.List {
		if len(p.Names) == 0 {
			n++
			continue
		}
		n += len(p.Names)
	}
	return n
}

// unwrapToNamed strips the wrappers that do not change which type is being named:
// slices, pointers and a variadic ellipsis.
func unwrapToNamed(e ast.Expr) ast.Expr {
	for {
		switch t := e.(type) {
		case *ast.ArrayType:
			if t.Len != nil {
				return e
			}
			e = t.Elt
		case *ast.StarExpr:
			e = t.X
		case *ast.Ellipsis:
			e = t.Elt
		default:
			return e
		}
	}
}

// isWireSideType reports whether a parameter names a type from the wire side — one
// declared in this package, or qualified by any package other than handlers. The
// aliases in huma_wire_alias.go are a convention, not a rule, so a projection may
// legitimately spell its parameter `wire.PushApplyRequest`.
func isWireSideType(e ast.Expr) bool {
	switch t := unwrapToNamed(e).(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && x.Name != "handlers"
	}
	return false
}

func isHandlersType(e ast.Expr) bool {
	_, ok := handlersTypeName(e)
	return ok
}

// handlersTypeName returns the bare type name when e names a type in the handlers
// package.
func handlersTypeName(e ast.Expr) (string, bool) {
	s, ok := unwrapToNamed(e).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	x, ok := s.X.(*ast.Ident)
	if !ok || x.Name != "handlers" {
		return "", false
	}
	return s.Sel.Name, true
}

func isErrorType(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "error"
}

// projectionWalk descends a filled WIRE value and checks every leaf against the
// projection's output, resolved BY PATH rather than by mirroring the descent — which
// is what lets a native form flatten a nested wire one and still be checked field by
// field.
type projectionWalk struct {
	t      *testing.T
	p      wireProjection
	native reflect.Value

	// oneHot is set by the one-field-at-a-time pass, where only one wire field is
	// filled: every leaf under it must be NON-ZERO on the native side, and nothing
	// else is asserted.
	oneHot bool

	// zeroBody is set by the empty-body pass, where NOTHING is set: every leaf must
	// be zero on the native side. Transforms are not applied — see that test.
	zeroBody bool
}

func (w *projectionWalk) walk(v reflect.Value, path string) {
	w.t.Helper()
	key := ruleKey(path)
	if why, ok := w.p.unmapped[key]; ok {
		if why == "" {
			w.t.Errorf("%s is listed as unmapped with no reason; an unexplained gap is what "+
				"this guard exists to catch", w.at(path))
		}
		return
	}
	if tr, ok := w.p.shaped[key]; ok {
		w.compare(v, path, tr)
		return
	}

	switch {
	case v.Kind() == reflect.Pointer && isWalkableStruct(v.Type().Elem()):
		if v.IsNil() {
			if w.zeroBody {
				// An OMITTED object still has to arrive as nothing, so descend a zero
				// value of what it points at rather than returning. Returning here
				// left every leaf below an omitted pointer unchecked — five under
				// CadencePatchRequest.Target alone — which is the hole this pass
				// exists to close, one pointer deeper.
				w.walk(reflect.New(v.Type().Elem()).Elem(), path)
				return
			}
			w.t.Fatalf("%s: the filler left this nil, so the walk cannot descend", w.at(path))
		}
		w.walk(v.Elem(), path)
	case isWalkableStruct(v.Type()):
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			w.walk(v.Field(i), path+"."+f.Name)
		}
	case v.Kind() == reflect.Slice && isWalkableStruct(v.Type().Elem()):
		got, absent, problem := w.resolve(path)
		if problem != "" {
			w.reportUnreachable(path, problem)
			return
		}
		if absent {
			// A nil pointer on the way: nothing arrived. Correct for an empty body,
			// a drop for a filled one — and reported as a drop rather than as a shape
			// mismatch, so the reader is not sent to declare a mapping that exists.
			if v.Len() > 0 {
				w.t.Errorf("%s was set with %d element(s) on the body and arrives nil on the "+
					"handler input — accepted and silently dropped ([NIM-817]).", w.at(path), v.Len())
			}
			return
		}
		for got.Kind() == reflect.Pointer && !got.IsNil() {
			got = got.Elem()
		}
		if got.Kind() != reflect.Slice {
			w.t.Errorf("%s is a slice on the body and a %s on the handler input; declare the "+
				"mapping in `shaped` or `renames`", w.at(path), got.Kind())
			return
		}
		if got.Len() != v.Len() {
			// Two different defects share this comparison, and saying "dropped" for
			// an element that was INVENTED would send the reader looking for a
			// missing line that is not missing.
			verb := "elements are being dropped"
			if got.Len() > v.Len() {
				verb = "elements are being invented, so a caller's omission is overruled"
			}
			w.t.Errorf("%s was set with %d element(s) on the body and arrives with %d on the "+
				"handler input — %s ([NIM-817]).", w.at(path), v.Len(), got.Len(), verb)
			return
		}
		for i := 0; i < v.Len(); i++ {
			w.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	default:
		w.compare(v, path, fieldTransform{})
	}
}

// compare resolves one wire leaf on the native side and checks it.
func (w *projectionWalk) compare(wire reflect.Value, path string, tr fieldTransform) {
	w.t.Helper()
	got, absent, problem := w.resolve(path)
	if problem != "" {
		w.reportUnreachable(path, problem)
		return
	}
	if absent {
		// Nothing arrived here, because a pointer on the way is nil. That is the
		// CORRECT outcome for an empty body and a drop for a filled one.
		if !w.zeroBody {
			w.t.Errorf("%s was SET on the body and arrives nil on the handler input — "+
				"accepted and silently dropped ([NIM-817]).", w.at(path))
		}
		return
	}
	if w.zeroBody {
		if !got.IsZero() {
			w.t.Errorf("nothing was set on the body, yet %s arrives on the handler input "+
				"holding %s — the projection is INVENTING this value rather than carrying it, "+
				"so a caller's own value is overwritten on every request ([NIM-824]).\n"+
				"Check the right-hand side in %s: it does not read the request.",
				w.at(path), render(got), w.p.fn)
		}
		return
	}
	if w.oneHot {
		if got.IsZero() {
			w.t.Errorf("with ONLY this field set on the body, %s arrives empty on the handler "+
				"input — the projection is not reading it ([NIM-817]).\nEither the line is "+
				"missing from %s, or its right-hand side names a different field of the request.",
				w.at(path), w.p.fn)
		}
		return
	}
	if tr.want == nil {
		if tr.why != "" {
			// A declared presence-only field: the weakest check this file makes, and
			// the reason it is weak is on the record beside it.
			if got.IsZero() {
				w.t.Errorf("%s was SET on the body and is zero on the handler input — accepted "+
					"and silently dropped ([NIM-817]).", w.at(path))
				return
			}
			w.t.Logf("%s: checked for PRESENCE only — %s", w.at(path), tr.why)
			return
		}
		w.equal(wire, got, path)
		return
	}
	w.equal(reflect.ValueOf(tr.want(wire.Interface())), got, path)
}

// equal is one leaf's comparison, in three arms plus a refusal:
//
//   - same type → the values must be EQUAL. This is the check that catches
//     cross-wiring, and it is the one that applies to almost every field.
//   - the native side is a pointer to the wire's type (a map that becomes a *map
//     because "omitted" and "empty" differ to the write) → dereference and compare.
//     Without this, a projection that pointed at a fresh empty map would read as
//     carrying the caller's.
//   - the wire side is a pointer to the native's type (*bool → bool through
//     derefBool, where the schema distinguishes "not set" and the domain does not)
//     → dereference and compare.
//   - anything else → an ERROR naming the two types, because an undeclared shape
//     change is indistinguishable from a field going missing, and quietly weakening
//     to a presence check is how one hides inside the other.
func (w *projectionWalk) equal(want, got reflect.Value, path string) {
	w.t.Helper()
	at := w.at(path)
	switch {
	case want.Type() == got.Type():
		if reflect.DeepEqual(want.Interface(), got.Interface()) {
			return
		}
		if got.IsZero() {
			w.t.Errorf("%s was SET on the body and arrives empty on the handler input — "+
				"accepted and silently dropped ([NIM-817]).\nCarry it in %s, or declare it in "+
				"`unmapped` with the reason it is dropped.", at, w.p.fn)
			return
		}
		w.t.Errorf("%s arrives on the handler input holding %s, want %s — the projection "+
			"carries a value for this field, but not THIS field's value: it is reading the "+
			"wrong source ([NIM-817]).\nCheck the right-hand side in %s.",
			at, render(got), render(want), w.p.fn)
	case got.Kind() == reflect.Pointer && got.Type().Elem() == want.Type():
		if got.IsNil() {
			w.t.Errorf("%s was SET on the body and arrives nil on the handler input — "+
				"accepted and silently dropped ([NIM-817]).", at)
			return
		}
		w.equal(want, got.Elem(), path)
	case want.Kind() == reflect.Pointer && want.Type().Elem() == got.Type():
		if want.IsNil() {
			w.t.Fatalf("%s: the filler left the body field nil, so nothing is being asserted", at)
			return
		}
		w.equal(want.Elem(), got, path)
	default:
		w.t.Errorf("%s changes shape across the boundary (%s -> %s) and nothing says how.\n"+
			"Declare it in `shaped` with a `want` that computes the native value from the wire "+
			"one — or with no `want` for a presence check — and say why. An undeclared shape "+
			"change reads exactly like a dropped field ([NIM-824]).",
			at, want.Type(), got.Type())
	}
}

// resolve follows a wire path on the NATIVE side, applying the projection's renames
// and dereferencing pointers.
//
// The two failure results are different facts and the callers treat them so:
// `absent` means the path is structurally right but a pointer on the way is nil —
// nothing arrived, which is a DROP under the two filled passes and the CORRECT
// outcome under the empty-body one. `problem` means the path does not exist at all,
// which is wrong under every pass.
func (w *projectionWalk) resolve(path string) (v reflect.Value, absent bool, problem string) {
	if native, renamed := w.p.renames[ruleKey(path)]; renamed {
		path = "." + native
	}
	v = w.native
	walked := ""
	for _, seg := range pathSegments(path) {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, true, ""
			}
			v = v.Elem()
		}
		if i, isIndex := sliceIndex(seg); isIndex {
			if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
				return reflect.Value{}, false, fmt.Sprintf("%q is a %s on the handler input, not a slice", walked, v.Kind())
			}
			if i >= v.Len() {
				return reflect.Value{}, false, fmt.Sprintf("%q carries only %d element(s) on the handler input", walked, v.Len())
			}
			v, walked = v.Index(i), walked+seg
			continue
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false, fmt.Sprintf("%q is a %s on the handler input, not a struct", walked, v.Kind())
		}
		f := v.FieldByName(seg)
		if !f.IsValid() {
			return reflect.Value{}, false, fmt.Sprintf("%s has NO field %q", v.Type(), seg)
		}
		v = f
		if walked == "" {
			walked = seg
		} else {
			walked += "." + seg
		}
	}
	return v, false, ""
}

func (w *projectionWalk) reportUnreachable(path, problem string) {
	w.t.Helper()
	w.t.Errorf("%s does not reach the handler input: %s.\n"+
		"The schema accepts this field, so a caller will send it and get a 2xx — and it reaches "+
		"nothing. Carry it in %s, declare where it lands in `renames`, or list it in `unmapped` "+
		"with the reason it is dropped ([NIM-817]).", w.at(path), problem, w.p.fn)
}

// at renders a wire path for a failure message, rooted at the projection.
func (w *projectionWalk) at(path string) string {
	return fmt.Sprintf("%s: %s%s", w.p.fn, reflect.TypeOf(w.p.wire), path)
}

// ruleKey normalises a wire path into the form the declaration maps are keyed by:
// no leading dot, and every slice index collapsed to `[]`, so one declaration covers
// every element.
func ruleKey(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '[' {
			b.WriteByte(path[i])
			continue
		}
		b.WriteString("[]")
		for i < len(path) && path[i] != ']' {
			i++
		}
	}
	return strings.TrimPrefix(b.String(), ".")
}

// pathSegments splits a wire path into field names and `[N]` index steps.
func pathSegments(path string) []string {
	var out []string
	for _, part := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		if part == "" {
			continue
		}
		for {
			open := strings.IndexByte(part, '[')
			if open < 0 {
				break
			}
			shut := strings.IndexByte(part[open:], ']')
			if shut < 0 {
				// An unterminated `[` — only reachable from a typo in a declaration.
				// Emit the rest verbatim so resolve reports "no such field" rather
				// than looping forever on a segment that never shrinks.
				break
			}
			if open > 0 {
				out = append(out, part[:open])
			}
			out = append(out, part[open:open+shut+1])
			part = part[open+shut+1:]
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sliceIndex(seg string) (int, bool) {
	if len(seg) < 3 || seg[0] != '[' || seg[len(seg)-1] != ']' {
		return 0, false
	}
	i, err := strconv.Atoi(seg[1 : len(seg)-1])
	// A negative index would panic in reflect.Value.Index rather than fail the test.
	return i, err == nil && i >= 0
}

// isWalkableStruct reports whether a type is a struct the walk should descend into.
//
// An opaque struct — time.Time, or anything else with no exported fields — is a LEAF
// here: there is nothing inside it to compare field by field, and descending would
// assert nothing while reporting success.
func isWalkableStruct(t reflect.Type) bool {
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(time.Time{}) {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

// render prints a value with pointers followed, so a mismatch report names the two
// captions rather than two addresses.
func render(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		return fmt.Sprintf("&%v", v.Elem().Interface())
	}
	if v.Type() == reflect.TypeOf(json.RawMessage(nil)) {
		return string(v.Interface().(json.RawMessage))
	}
	return fmt.Sprintf("%v", v.Interface())
}

// TestCreateProjections_CoverEveryCaptionedCreateRoute is the spec-derived half: the
// set of captioned create routes above must be the set the SPEC says exists.
//
// A registry that grows a caption on create and never wires the projection is the
// exact recurrence of [NIM-817], and it is invisible to the per-field halves — those
// check projections that already exist. The spec is the source of the set for the
// same reason the source sweep is: an inventory maintained by hand forgets things in
// precisely the situation it is meant to catch.
//
// Its own blind spots, since they bound what a green result means: a body schema
// reached through a second level of `$ref` or through `allOf`, and any create that
// is not a POST.
func TestCreateProjections_CoverEveryCaptionedCreateRoute(t *testing.T) {
	driven := map[string]struct{}{}
	for _, p := range wireProjections {
		driven[p.fn] = struct{}{}
	}
	for r, fn := range captionedCreateProjections {
		if _, ok := driven[fn]; !ok {
			t.Errorf("%s names the projection %s, which wireProjections does not drive", r, fn)
		}
	}

	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	captioned := map[route]struct{}{}
	for path, item := range spec.Paths {
		op, ok := pathItemOps(item)[http.MethodPost]
		if !ok {
			continue
		}
		if bodyDeclaresLabel(spec, op) {
			captioned[route{method: http.MethodPost, path: normalizePath(path)}] = struct{}{}
		}
	}
	if len(captioned) == 0 {
		t.Fatal("no POST route in the spec declares `label` in its body — either the schemas regressed " +
			"or the resolver below stopped following $ref, and the guard would pass vacuously")
	}

	var missing, stale []string
	for r := range captioned {
		_, projected := captionedCreateProjections[r]
		why, exempt := labelIsNotACaption[r]
		switch {
		case projected && exempt:
			t.Errorf("%s is in both captionedCreateProjections and labelIsNotACaption — "+
				"the two registries must be disjoint", r)
		case exempt && why == "":
			t.Errorf("%s is exempt with no reason recorded", r)
		case !projected && !exempt:
			missing = append(missing, r.String())
		}
	}
	for r := range captionedCreateProjections {
		if _, ok := captioned[r]; !ok {
			stale = append(stale, "captionedCreateProjections: "+r.String())
		}
	}
	for r := range labelIsNotACaption {
		if _, ok := captioned[r]; !ok {
			stale = append(stale, "labelIsNotACaption: "+r.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("CREATE ROUTE ACCEPTS `label` WITH NO GUARDED PROJECTION — %d:\n  %s\n"+
			"-> add each to captionedCreateProjections. Until then nothing proves the caption reaches the write, "+
			"which is the whole of [NIM-817]. If the field is not a display caption, say so in "+
			"labelIsNotACaption.", len(missing), strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("STALE DECLARATION — %d entries with no matching POST route declaring `label` "+
			"in the spec:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
}

// bodyDeclaresLabel reports whether the operation's JSON request body has a `label`
// property, following one level of $ref into components (huma emits the body schema
// as a reference).
func bodyDeclaresLabel(spec *huma.OpenAPI, op *huma.Operation) bool {
	if op == nil || op.RequestBody == nil {
		return false
	}
	mt, ok := op.RequestBody.Content["application/json"]
	if !ok || mt.Schema == nil {
		return false
	}
	sch := mt.Schema
	if sch.Ref != "" {
		name := sch.Ref[strings.LastIndex(sch.Ref, "/")+1:]
		resolved, ok := spec.Components.Schemas.Map()[name]
		if !ok {
			return false
		}
		sch = resolved
	}
	_, has := sch.Properties["label"]
	return has
}

// uniqueFiller writes a value into every exported field reachable from a request
// struct, and — for every type that can hold one — a value no other field holds.
//
// Uniqueness is the point. A filler that wrote the same token everywhere would prove
// only that a field is non-empty afterwards, which a projection reading the WRONG
// source field satisfies just as well as a correct one.
//
// Every value is NON-ZERO, which is what lets "the native field is zero" mean
// "dropped" with no further qualification. The cost is that a projection which drops
// an EMPTY value (`if b.SSHProvider != ""`, `if b.CleanupStaleVersions`) is only
// driven down its carrying arm here, and that a hardcoded non-zero constant looks
// like a carried value; TestWireProjections_InventNothingFromAnEmptyBody is the
// other side of both and runs the zero body no filler can produce.
//
// Values are shaped to be plausible rather than minimal — a one-element slice, a
// one-entry map, an allocated pointer — because the difference between "empty" and
// "absent" is load-bearing on this boundary and a fixture that could not tell them
// apart would let the real omission through.
type uniqueFiller struct {
	n int
}

func (f *uniqueFiller) next() int { f.n++; return f.n }

func (f *uniqueFiller) fill(t *testing.T, v reflect.Value, where string) {
	t.Helper()
	// time.Time has no exported field to fill, so the struct arm below would reject
	// it; a fixed instant per call keeps it distinguishable and deterministic.
	if v.Type() == reflect.TypeOf(time.Time{}) {
		v.Set(reflect.ValueOf(time.Unix(int64(f.next()), 0).UTC()))
		return
	}
	// [Optional] is a THREE-field presence carrier and only two of its eight
	// combinations are representable: `Null` is documented as "Set must be true",
	// and `Value` as "the decoded value when Set && !Null". Filling all three
	// field-by-field produces present-AND-null-AND-valued, a body no decoder can
	// produce — and optionalToPtr then correctly answers nil, which the walk reads
	// as a dropped field. So the fixture builds the one shape a real PATCH has:
	// present, not null, carrying a value.
	if isOptionalCarrier(v.Type()) {
		v.FieldByName("Set").SetBool(true)
		f.next()
		f.fill(t, v.FieldByName("Value"), where)
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("guard-%d", f.next()))
	case reflect.Bool:
		// A bool holds one bit and cannot be made unique, so the all-fields pass
		// cannot tell two of them apart whatever it writes here; the
		// one-field-at-a-time pass closes that, INCLUDING inside a slice element,
		// which is the only thing that reaches the two *bool on wire.VoyageNotify.
		//
		// `true` rather than the alternation an earlier revision used, so that every
		// filled field is non-zero and a nil on the native side always means the
		// value was dropped. What the alternation bought — catching a hardcoded
		// constant, about half the time — the empty-body pass now buys outright.
		v.SetBool(true)
		f.next()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.next()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(f.next()))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(f.next()))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(t, v.Elem(), where)
	case reflect.Interface:
		if v.NumMethod() != 0 {
			t.Fatalf("%s: the filler cannot produce a value for the interface %s — "+
				"teach it one rather than letting the field look dropped", where, v.Type())
		}
		v.Set(reflect.ValueOf(fmt.Sprintf("guard-%d", f.next())))
	case reflect.Slice:
		// json.RawMessage is a []byte that must PARSE, not an arbitrary byte: a
		// projection is free to unmarshal it, and garbage there would fail for a
		// reason that has nothing to do with what is guarded here.
		if v.Type() == reflect.TypeOf(json.RawMessage(nil)) {
			v.Set(reflect.ValueOf(json.RawMessage(fmt.Sprintf(`{"guard":%d}`, f.next()))))
			return
		}
		elem := reflect.New(v.Type().Elem()).Elem()
		f.fill(t, elem, where)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		f.fill(t, key, where)
		val := reflect.New(v.Type().Elem()).Elem()
		f.fill(t, val, where)
		v.SetMapIndex(key, val)
	case reflect.Struct:
		exported := 0
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			exported++
			f.fill(t, v.Field(i), where)
		}
		// An opaque struct (netip.Addr) would be left at its zero value, and the
		// comparison would then pass whatever the projection did with it.
		if exported == 0 {
			t.Fatalf("%s: the filler cannot produce a distinguishable %s — it has no "+
				"exported fields, so the field would look carried whatever happens to it",
				where, v.Type())
		}
	default:
		t.Fatalf("%s: the filler cannot produce a distinguishable %s — "+
			"a field of an unhandled kind would look dropped whatever the projection does", where, v.Kind())
	}
}

// isOptionalCarrier reports whether t is an [Optional] instantiation, by SHAPE
// rather than by name: a struct of exactly `Set bool`, `Null bool`, `Value T`.
//
// By shape because a generic instantiation has no stable name to compare against
// across type arguments, and because a second carrier written to the same shape
// would have the same unrepresentable-combination problem and should get the same
// treatment.
func isOptionalCarrier(t reflect.Type) bool {
	if t.Kind() != reflect.Struct || t.NumField() != 3 {
		return false
	}
	set, setOK := t.FieldByName("Set")
	null, nullOK := t.FieldByName("Null")
	_, valueOK := t.FieldByName("Value")
	return setOK && nullOK && valueOK &&
		set.Type.Kind() == reflect.Bool && null.Type.Kind() == reflect.Bool
}

// extraArgumentsDriven — the projections that take a value they are HANDED
// rather than read off the wire root, and the fact that
// TestWireProjections_CarryTheArgumentsTheyAreHanded asserts about each.
//
// The three reflection passes cannot reach these. They fill the wire root and
// compare the native side against it; an argument the ROUTE computes is supplied
// by the fixture itself, so `StateParams: stateParams` can be deleted outright and
// all three stay green while every `state.<field>` filter silently stops narrowing
// the list. That is the [NIM-817] defect with a different entry point, and the
// completeness half above refuses a projection with extra parameters that is not
// named here.
var extraArgumentsDriven = map[string]string{
	"toIncarnationListQuery":   "stateParams — the dynamic `state.<field>` filters, which huma cannot bind",
	"toSoulListInput":          "page and cursor — the pair soulParsePage validated before the projection ran",
	"toUpdatePermissionsInput": "name — the {name} path parameter naming the role being edited",
}

// TestWireProjections_CarryTheArgumentsTheyAreHanded drives the values the walk
// cannot: one assertion per extra argument, each with the value arriving distinct
// from anything else in the struct so "carried" is not confused with "defaulted".
func TestWireProjections_CarryTheArgumentsTheyAreHanded(t *testing.T) {
	// Every projection the registry claims is covered has to be covered HERE.
	// Without this, membership in extraArgumentsDriven is the only thing the
	// completeness half checks, and a fourth extra-argument projection is green on
	// one map line and no assertion — which is the gap this test exists to close,
	// reopened one level up.
	asserted := map[string]bool{}
	t.Cleanup(func() {
		for fn := range extraArgumentsDriven {
			if !asserted[fn] {
				t.Errorf("%s is named in extraArgumentsDriven and has no subtest here — the registry "+
					"says its handed-in argument is checked and nothing checks it", fn)
			}
		}
	})

	asserted["toIncarnationListQuery"] = true
	t.Run("toIncarnationListQuery/stateParams", func(t *testing.T) {
		want := map[string][]string{"state.version": {"7.2"}}
		got := toIncarnationListQuery(&incListInput{}, want)
		if !reflect.DeepEqual(got.StateParams, want) {
			t.Errorf("StateParams = %v, want %v — the `state.<field>` filters do not reach the query, "+
				"so a list narrowed by them returns rows the caller asked to exclude", got.StateParams, want)
		}
	})

	asserted["toSoulListInput"] = true
	t.Run("toSoulListInput/page+cursor", func(t *testing.T) {
		page := sharedapi.Page{Offset: 11, Limit: 22}
		cursor := &sharedapi.KeysetCursor{}
		got := toSoulListInput(&soulListInput{}, page, cursor)
		if got.Page != page {
			t.Errorf("Page = %+v, want %+v — the page soulParsePage validated is not the page the "+
				"handler reads, so the bounds it enforced apply to nothing", got.Page, page)
		}
		if got.Cursor != cursor {
			t.Errorf("Cursor = %p, want %p — the decoded keyset cursor is dropped and the request "+
				"silently falls back to offset pagination", got.Cursor, cursor)
		}
	})

	asserted["toUpdatePermissionsInput"] = true
	t.Run("toUpdatePermissionsInput/name", func(t *testing.T) {
		got := toUpdatePermissionsInput(RolePermissionsUpdateRequest{}, "role-under-edit")
		if got.Name != "role-under-edit" {
			t.Errorf("Name = %q, want role-under-edit — the path parameter naming the role does not "+
				"reach the update, so the write would land on no role or the wrong one", got.Name)
		}
	})
}

// TestRoleUpdatePermissionsInput_HasOnlyTheFieldsTheProjectionAccountsFor — the
// tripwire that pays for rooting toUpdatePermissionsInput on the BODY.
//
// The wire root is `RolePermissionsUpdateRequest`, so the huma input struct around
// it is walked by nothing: a THIRD field added to it — a new query parameter on
// PATCH /v1/roles/{name}/permissions, say — would reach no guard, which is exactly
// the silence this file exists to end. Its two fields are accounted for by name
// (`Name` is asserted in TestWireProjections_CarryTheArgumentsTheyAreHanded,
// `Body` is the wire root), so a third is a decision somebody has to make.
func TestRoleUpdatePermissionsInput_HasOnlyTheFieldsTheProjectionAccountsFor(t *testing.T) {
	accounted := map[string]string{
		"Name": "the {name} path parameter, passed to toUpdatePermissionsInput as its second argument",
		"Body": "the wire root the three reflection passes walk",
	}
	rt := reflect.TypeOf(roleUpdatePermissionsInput{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, ok := accounted[f.Name]; !ok {
			t.Errorf("roleUpdatePermissionsInput grew the field %q, which nothing checks: the "+
				"projection is rooted on the BODY (see toUpdatePermissionsInput), so this struct is "+
				"walked by no pass. Carry it in the projection and assert it, or account for it here "+
				"with the reason it is not carried ([NIM-831]).", f.Name)
		}
		delete(accounted, f.Name)
	}
	for name := range accounted {
		t.Errorf("roleUpdatePermissionsInput no longer has %q — this list is stale", name)
	}
}
