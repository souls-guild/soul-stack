package api

// GrantOperatorRequest — общая huma-Go-форма тела «привязать архонта по AID» для двух
// под-ресурсов: POST /v1/roles/{name}/operators (role.grant-operator) и
// POST /v1/synods/{name}/operators (synod.add-operator). Committed-рукопись
// (docs/keeper/openapi.yaml) объявляет ОБА эндпоинта через ОДНУ схему GrantOperatorRequest
// (GrantOperatorRequest — общий тип генерёного пакета, см. GrantRoleOperatorJSONRequestBody
// / AddSynodOperatorJSONRequestBody). Поэтому huma-форма тоже одна — иначе агрегатор-merge
// получил бы две схемы с одним именем (либо два разных имени против контракта).
//
// Имя структуры = контрактное имя схемы в OpenAPI (huma DefaultSchemaNamer берёт
// reflect.Type.Name()). AID required:"true" в схеме; пустой/битый формат —
// доменная валидация operator.ValidAID (422) в Grant/AddOperatorTyped.
type GrantOperatorRequest struct {
	AID string `json:"aid" required:"true" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID архонта, назначаемого в роль/группу (naming-rules.md)"`
}
