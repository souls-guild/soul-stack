package api

// Named-схема IncarnationStatus ($ref) для components/schemas — общая истина enum-набора.
//
// huma DefaultSchemaNamer выносит в components/schemas (getsRef=true) ТОЛЬКО struct-типы;
// string-based named-тип huma всегда ИНЛАЙНИТ как `type: string`. Рукопись (docs/keeper/
// openapi.yaml) объявляет IncarnationStatus отдельной схемой с enum-значениями и ссылается на
// неё через $ref в каждом status-поле — UI ждёт именно named-схему.
//
// handler-native T5d: native enum-тип IncarnationStatus (huma_enums.go) сам реализует
// huma.SchemaProvider — его метод Schema() читает константы этого файла (schemaName/Ref/Enum/
// Description), регистрирует named-схему "IncarnationStatus" и возвращает $ref. Reply/get/list/
// unlock-Body несут native IncarnationStatus НАПРЯМУЮ (поля проецируются из доменных
// handlers.*View плоских string-ов), поэтому отдельный RegisterTypeAlias IncarnationStatus
// → native более НЕ нужен (нет ни одного IncarnationStatus-поля в reflected-Body).

// incarnationStatusSchemaName — имя named-схемы в components/schemas (контрактное имя
// из рукописи; UI ссылается на него по $ref).
const incarnationStatusSchemaName = "IncarnationStatus"

// incarnationStatusSchemaRef — стандартный huma-prefix компонент-схем (huma.DefaultConfig
// конфигурирует registry с этим prefix) + имя. Возвращается из SchemaProvider как $ref.
const incarnationStatusSchemaRef = "#/components/schemas/" + incarnationStatusSchemaName

// incarnationStatusEnum — допустимые значения статуса runtime-инстанса (ADR-009/031/
// S-D). Порядок и состав — по committed-рукописи docs/keeper/openapi.yaml
// (IncarnationStatus.enum), которая авторитетна для OpenAPI-контракта. `provisioning` —
// пост-MVP значение каталога (см. internal/incarnation.Status: там его ещё нет, но
// контракт его уже резервирует).
var incarnationStatusEnum = []any{
	"provisioning",
	"ready",
	"applying",
	"error_locked",
	"migration_failed",
	"drift",
	"destroying",
	"destroy_failed",
}

// incarnationStatusDescription — описание схемы (parity рукописи).
const incarnationStatusDescription = "Статус runtime-инстанса. В proto константы имеют " +
	"family-prefix (INCARNATION_STATUS_READY), в JSON API — короткие формы. `drift` — " +
	"информационный статус Scry (ADR-031), НЕ блокирующий: remediation = обычный apply, " +
	"который при успехе вернёт incarnation в `ready`."
