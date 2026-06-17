package api

// Доэмиссия схемы ErrandAccepted в агрегатор-спеку — Class C выравнивания (202-тело
// async-эскалации exec). По эталону schema-builder pre-seed (huma Registry.Schema
// регистрирует struct-тип даже без ссылающегося поля, registry.go :142).
//
// ПРОБЛЕМА. POST /v1/souls/{sid}/exec несёт dual-status 200/202: 200 — ErrandResult
// (sync-терминал), 202 — ErrandAccepted (async-escalation + Location). Оба тела
// handler пред-маршалит в один json.RawMessage-Body (errandExecOutput.Body), чтобы
// под одним OperationID отдать разные wire-тела (huma не выражает per-status-2xx
// типизированные тела одним output-типом). ErrandResult доезжает в components через
// ДРУГОЙ роут (errand-list: ErrandListReply.Items=[]ErrandResult типизирован),
// а ErrandAccepted НИГДЕ не типизирован ссылающимся huma-полем (errand-get тоже
// маршалит его через json.RawMessage) → схема не эмитилась, хотя рукопись
// (docs/keeper/openapi.yaml :7363) её объявляет и UI ждёт.
//
// МЕХАНИЗМ (schema-builder pre-seed — WIRE-БЕЗОПАСЕН). Регистрируем api-named-struct
// errandAccepted напрямую через Components.Schemas.Schema(..., allowRef=false): huma
// builds-и-кладёт схему в components/schemas под именем "ErrandAccepted" БЕЗ всякого
// ссылающегося output-поля. Никакая операция/response не меняется — dual-status-Body
// остаётся json.RawMessage, wire-байты 202-тела идентичны легаси (golden errand
// byte-exact цел). Меняется ТОЛЬКО присутствие схемы в components.
//
// ★ Форма сверена с рукописью :7363: errand_id (pattern ULID) + status (enum [running]);
// required:[errand_id, status]. Имя типа = контрактное (huma DefaultSchemaNamer
// капитализирует → "ErrandAccepted"). Это та же контрактная форма, что генерёный
// ErrandAccepted, но с enum/pattern в схеме (oapi-тип несёт string-enum-тип без
// Go-констант → huma бы дал голый string). Имя "errandAccepted" в api-слое не
// коллизирует (пакет errand в api-слой импортируется лишь под алиасом).

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// errandAccepted — source-of-form схемы ErrandAccepted (202-тело exec / errand-get
// while running). Форма по committed-рукописи (docs/keeper/openapi.yaml :7363):
// errand_id (ULID-pattern) + status (enum [running]); оба required. Это чисто
// schema-builder тип: на wire 202-тело сериализует handler через json.RawMessage,
// этот тип в сериализации НЕ участвует.
type errandAccepted struct {
	ErrandID string `json:"errand_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"ULID запущенного Errand-а"`
	Status   string `json:"status" enum:"running" doc:"строка ещё выполняется (async-escalation)"`
}

// registerErrandAccepted кладёт схему ErrandAccepted в components/schemas через
// Registry.Schema (pre-seed без ссылающегося поля). Вызывается в newHumaCadenceAPI для
// каждой собранной huma.API. Операции/тела НЕ затрагиваются — dual-status exec остаётся
// json.RawMessage, wire byte-exact цел.
func registerErrandAccepted(api huma.API) {
	api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[errandAccepted](), false, "ErrandAccepted")
}
