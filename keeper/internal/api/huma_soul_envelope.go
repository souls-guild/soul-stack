package api

// Выравнивание имени/формы list-envelope SOUL-домена (CURSOR, 6-полей) под committed-рукопись +
// сведение nested SoulSshTarget на единую схему (КЛАСС A) — тираж-батч N5 (ENVELOPE+CLASS-A
// механизмы по эталонам huma_incarnation_envelope.go / huma_voyage_target.go).
//
// === ENVELOPE (CURSOR, 6 полей) ===
//
// ПРОБЛЕМА. GET /v1/souls несёт в Body тип handlers.SoulListReply — Go type-ALIAS на
// sharedapi.PagedResponse[SoulListEntry] (handlers/soul.go). Go-alias прозрачен для
// reflect → huma DefaultSchemaNamer видит инстанцированный generic и эмитит схему
// "PagedResponseSoulListEntry". Рукопись (docs/keeper/openapi.yaml :6766) объявляет envelope
// как "SoulListReply" — UI ждёт его.
//
// ★ ОТЛИЧИЕ ОТ incarnation/operator/voyage (4-поля offset): soul — ЕДИНСТВЕННЫЙ cursor-домен.
// Рукопись :6766 несёт ШЕСТЬ полей — items/offset/limit/total + next_cursor (string) +
// total_approximate (boolean) — гибрид offset/keyset (ADR-047 S3b-2a, режим выбирает сервер из
// Purview). named-struct soulListReply повторяет РОВНО эти 6 полей (НЕ 4-поля-форма incarnation),
// сверено с рукописью. items.$ref на контрактный element SoulListEntry; required:[items,offset,
// limit,total] (next_cursor/total_approximate — optional, omitempty).
//
// МЕХАНИЗМ. RegisterTypeAlias(PagedResponse[SoulListEntry] → soulListReply): huma строит
// схему list-Body под контрактным именем/формой. Wire-тело (PagedResponse) НЕ меняется — json-
// поля те же (next_cursor/total_approximate omitempty в offset-режиме опущены) → golden list
// byte-exact.
//
// === CLASS A (SoulSshTarget shared input↔output) ===
//
// SoulSshTarget — единый api-тип ВСЕХ потребителей: input (PUT ssh-target body, см.
// huma_soul_op.go) И output (SoulSshTargetReply.ssh_target несёт генерёный SoulSSHTarget).
// aliasSoulSshTarget сводит OUTPUT на ту же named-схему SoulSshTarget, что input. Формы
// совместимы: api.SoulSshTarget — required:[ssh_port,ssh_user,soul_path] (рукопись :6394),
// ssh_provider optional; SoulSSHTarget — те же поля (ssh_provider *string omitempty). Одна
// валидная схема SoulSshTarget; технический SoulSSHTarget (от генерёного output-типа) вытеснен.
//
// === REPLY-RENAME через ALIAS (SoulCovenAssignReply) — батч N6 ===
//
// Output-дрейф, имя которого нельзя выровнять простым rename Go-структуры:
//
//   - SoulCovenAssignReply: wire-body POST /v1/souls/coven несёт handler-тип
//     handlers.SoulCovenAssignResponse (custom MarshalJSON XOR label↔labels — переименовать
//     unexported-структуру можно, но имя handlers.SoulCovenAssignReply УЖЕ занято внутренним
//     контейнером {Body, AuditPayload}). DefaultSchemaNamer эмитил "SoulCovenAssignResponse".
//     Рукопись (:7140) объявляет wire-body как "SoulCovenAssignReply". МЕХАНИЗМ (как service-
//     envelope): api-named-struct soulCovenAssignReply (форма ровно по рукописи, matched/changed
//     int32, required:[mode,label,matched,changed,status,dry_run]) + alias
//     handlers.SoulCovenAssignResponse → soulCovenAssignReply. Wire-тело (custom MarshalJSON)
//     НЕ меняется — меняется лишь OpenAPI-схема/имя Body.
//
// SoulSshTargetReply (PUT ssh-target 200-тело) переведён на huma-native (huma_soul_reply.go,
// финал T5b): Body — native SoulSshTargetReply, native-Body даёт схему сам → rename-alias
// SoulSSHTargetReply → soulSshTargetReply СНЯТ. nested ssh_target — class-A reuse native
// SoulSshTarget (aliasSoulSshTarget на SoulSSHTarget ОСТАЁТСЯ — input PUT-тела ещё legacy-генерата).

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// soulCovenAssignReply — alias-цель схемы POST /v1/souls/coven 200-тела. Форма сверена с
// committed-рукописью (docs/keeper/openapi.yaml :7140 → SoulCovenAssignReply): mode/label/labels/
// matched/changed/status/dry_run; matched/changed — int32; required:[mode,label,matched,changed,
// status,dry_run] (labels — optional). Имя типа = контрактное имя схемы (huma DefaultSchemaNamer
// капитализирует первую букву → "SoulCovenAssignReply"). Wire-тело сериализует handler-тип
// (custom MarshalJSON XOR label↔labels) — здесь только форма для схемы.
//
// OUTPUT-PATTERN ИМЁН (батч 5, документационный): labels[] — output-эхо применённого replace-
// набора ← soul.CovenPattern (per-element); reply output-only (alias-цель, НЕ request-Body) →
// input-422-риска нет. label (singular append/remove-эхо) НЕ тегируется: для replace-режима
// он "" (XOR с labels), pattern бы ложно требовал coven у пустой строки.
type soulCovenAssignReply struct {
	Mode    string   `json:"mode" doc:"тип операции над covens[]"`
	Label   string   `json:"label" doc:"применённая метка для append/remove (зеркало input)"`
	Labels  []string `json:"labels,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" doc:"применённый набор меток для replace (зеркало input)"` // ← soul.CovenPattern (per-element, output-эхо)
	Matched int32    `json:"matched" doc:"сколько хостов попало под selector ∩ scope"`
	Changed int32    `json:"changed" doc:"сколько строк фактически изменено"`
	Status  string   `json:"status" enum:"completed,partial" doc:"completed — все чанки закоммичены; partial — фейл середины"`
	DryRun  bool     `json:"dry_run" doc:"dry-run-прогон без записи"`
}

// soulTraitsAssignReply — alias-цель схемы POST /v1/souls/traits 200-тела (ADR-060). Имя
// типа = контрактное имя схемы (huma DefaultSchemaNamer капитализирует → "SoulTraitsAssignReply").
// keys[] — output-эхо набора затронутых trait-ключей (per-element pattern = soul.TraitKeyPattern);
// trait-ЗНАЧЕНИЯ в выдачу НЕ кладутся (секрет-гигиена, симметрия с audit). matched/changed —
// int32 (соглашение envelope-домена soul). reply output-only → input-422-риска pattern нет.
type soulTraitsAssignReply struct {
	Mode    string   `json:"mode" doc:"режим операции (merge/replace/remove)"`
	Keys    []string `json:"keys" pattern:"^[a-z][a-z0-9]*([_-][a-z0-9]+)*$" doc:"затронутые trait-ключи (зеркало input)"` // ← soul.TraitKeyPattern (per-element, output-эхо)
	Matched int32    `json:"matched" doc:"сколько хостов попало под selector ∩ scope"`
	Changed int32    `json:"changed" doc:"сколько строк фактически изменено"`
	Status  string   `json:"status" enum:"completed,partial" doc:"completed — все чанки закоммичены; partial — фейл середины"`
	DryRun  bool     `json:"dry_run" doc:"dry-run-прогон без записи"`
}

// soulListReply — alias-цель схемы GET /v1/souls envelope (CURSOR, 6 полей). Форма сверена с
// committed-рукописью (docs/keeper/openapi.yaml :6766 → SoulListReply): items/offset/limit/total
// (required) + next_cursor (string, optional) + total_approximate (boolean, optional). offset/
// limit/total — int32 (рукопись format:int32). items.$ref на КОНТРАКТНЫЙ element native
// SoulListEntry (та же схема, что эмитит get-Body — финал T5b, иначе huma duplicate-name-паника
// между api.SoulListEntry и SoulListEntry). Имя типа = контрактное имя схемы (huma
// DefaultSchemaNamer капитализирует → "SoulListReply"). json-теги повторяют sharedapi.PagedResponse
// (next_cursor/total_approximate omitempty) → wire не меняется.
type soulListReply struct {
	Items            []SoulListEntry `json:"items" doc:"страница реестра souls"`
	Offset           int32           `json:"offset" doc:"сдвиг от начала набора (offset-режим)"`
	Limit            int32           `json:"limit" doc:"размер страницы"`
	Total            int32           `json:"total" doc:"общее число записей; значимо только в offset-режиме"`
	NextCursor       *string         `json:"next_cursor,omitempty" doc:"opaque keyset-курсор следующей страницы (keyset-режим); отсутствует в offset-режиме и когда набор исчерпан"`
	TotalApproximate *bool           `json:"total_approximate,omitempty" doc:"total НЕ точен (keyset-режим); в offset-режиме опущено"`
}

// registerSoulEnvelopes вешает на registry huma-alias инстанцированного generic
// sharedapi.PagedResponse[SoulListEntry] → named-struct soulListReply (контрактное имя/
// CURSOR-форма 6 полей) + reply-rename-алиас coven-assign (батч N6). Вызывается в
// newHumaCadenceAPI для каждой собранной huma.API. Wire-тип (тело) НЕ меняется.
//
// ★ Alias-ключ PagedResponse[SoulListEntry] НЕ тронут (финал T5b): handler-list marshalит
// именно этот wire-тип; element soulListReply.Items []SoulListEntry резолвится через ту же
// схему SoulListEntry, что эмитит native get-Body (формы идентичны → дедуп безопасен).
func registerSoulEnvelopes(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	// ★ handler-native T5d: wire-тип list-Body — sharedapi.PagedResponse[handlers.SoulListView]
	// (Go-alias handlers.SoulListReply). element-схема SoulListView сводится через тот же alias
	// PagedResponse → soulListReply, чьё items.$ref указывает на КОНТРАКТНУЮ схему SoulListEntry
	// (та же, что эмитит native get-Body) → дедуп безопасен, имя/CURSOR-форма стабильны.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[sharedapi.PagedResponse[handlers.SoulListView]](),
		reflect.TypeFor[soulListReply](),
	)
	// REPLY-RENAME (батч N6): handler-тип wire-body coven-assign → контрактное имя
	// SoulCovenAssignReply. SoulSshTargetReply — native (huma_soul_reply.go), alias снят.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulCovenAssignResponse](),
		reflect.TypeFor[soulCovenAssignReply](),
	)
	// traits-assign (ADR-060): handler-тип wire-body → контрактное имя SoulTraitsAssignReply.
	schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulTraitsAssignResponse](),
		reflect.TypeFor[soulTraitsAssignReply](),
	)
}
