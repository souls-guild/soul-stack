package api

// GrantOperatorRequest — the shared huma Go form of the "attach an archon by AID" body for two
// sub-resources: POST /v1/roles/{name}/operators (role.grant-operator) and
// POST /v1/synods/{name}/operators (synod.add-operator). The committed reference
// (docs/keeper/openapi.yaml) declares BOTH endpoints via ONE schema GrantOperatorRequest
// (GrantOperatorRequest is a shared type of the generated package, see GrantRoleOperatorJSONRequestBody
// / AddSynodOperatorJSONRequestBody). So the huma form is also one — otherwise the aggregator merge
// would get two schemas with one name (or two different names, against the contract).
//
// Struct name = contract schema name in OpenAPI (huma DefaultSchemaNamer takes
// reflect.Type.Name()). AID required:"true" in the schema; empty/broken format —
// domain validation operator.ValidAID (422) in Grant/AddOperatorTyped.
type GrantOperatorRequest struct {
	AID string `json:"aid" required:"true" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID архонта, onзonчаемого в роль/группу (naming-rules.md)"`
}
