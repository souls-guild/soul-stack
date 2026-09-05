// The wire shape of the label mutation, stated once for all ten registries
// (ADR-0085, NIM-728). Code is the OpenAPI source (ADR-054), so this one Go
// type is one schema in the spec, referenced by all ten
// `PUT /v1/<collection>/{id}/label` operations - a caller, and the generated
// types.gen.ts in the companion UI, learns the body once.
//
// Every one of the ten operations is otherwise declared in its own domain file
// in keeper, next to the routes it belongs with, because the path parameter's
// pattern is that registry's own name grammar and the 200 body is that
// registry's own row.

package wire

// LabelSetRequest — the body of PUT /v1/<collection>/{name}/label.
//
// Exactly one field, and it is both nullable and OPTIONAL on purpose:
// `{"label": null}` and `{}` both CLEAR the caption, after which a consumer
// shows the identifier again. There is no third state to express — nothing else
// on this endpoint could be left unchanged.
//
// `required:"false"` is explicit rather than incidental. huma marks a field
// required unless told otherwise, and the default would have made `{}` a 422 on
// REST while the MCP twin (`schema<X>LabelSetInput`, `required: ["name"]`) went
// on accepting an omitted `label` — the two primary operator surfaces
// (ADR-004) disagreeing about what clearing a caption looks like.
//
// And it is `required:"false"` rather than `json:",omitempty"`, which would ALSO
// have made the field optional but at a cost: omitempty drops the `"null"` arm
// of the generated type, so `{"label": null}` — the explicit spelling of
// "clear it", and the one the MCP schema accepts — would have become a schema
// violation. Optional and nullable are two properties here and both are wanted.
//
// No `pattern` and no `maxLength`: the field is free text with capitals, spaces
// and punctuation, which is the entire reason it exists beside the identifier
// (ADR-0085). The narrow grammar lives on the identifier in the path, which this
// request cannot touch.
type LabelSetRequest struct {
	Label *string `json:"label" required:"false" doc:"Display caption: free text, may carry capitals and spaces. null - or an omitted field, or an empty body - clears it, after which consumers show the identifier instead. Never used to derive a Vault path, an RBAC scope, a snapshot directory or a CEL root: changing it moves nothing"`
}
