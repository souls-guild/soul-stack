package api

// ВЫРАВНИВАНИЕ nested shared-схем voyage+cadence (target/notify) — единый Go-тип на
// каждую вложенную форму, как committed-рукопись (docs/keeper/openapi.yaml :7455/:7612).
//
// До этого каждый input-домен нёс СВОЙ Go-тип одинаковой формы
// (voyageTargetHumaBody/cadenceTargetHumaBody, voyageNotifyHumaBody/cadenceNotifyHumaBody)
// → huma DefaultSchemaNamer давал в спеке 4 технические схемы (VoyageTargetHumaBody/
// CadenceTargetHumaBody/VoyageNotifyHumaBody/CadenceNotifyHumaBody) вместо рукописных
// VoyageTarget/VoyageNotify, а OUTPUT (Voyage.target/CadenceDTO.target) тянул генерёный
// VoyageTarget — пятую схему той же формы. Здесь:
//   - VoyageTarget — ЕДИНЫЙ тип для ВСЕХ input-потребителей (VoyageCreateRequest.Target /
//     CadenceCreateRequest.Target / CadencePatchRequest.Target). Имя структуры =
//     контрактное имя схемы. КЛАСС A (shared input↔output): alias VoyageTarget →
//     VoyageTarget (aliasVoyageTarget) сводит и OUTPUT на ту же схему. Формы совместимы:
//     VoyageTarget — все поля pointer-optional без required; рукопись — все optional;
//     huma value-тип с omitempty без required:"true" — тоже optional. Одна валидная схема.
//   - VoyageNotify — ЕДИНЫЙ тип для input-тел (VoyageCreateRequest.Notify[] /
//     CadenceCreateRequest.Notify[]). КЛАСС B (shared между input-телами, НЕТ output-
//     потребителя) → БЕЗ alias.
//
// json-теги/форма — побайтово как у схлопнутых voyageTargetHumaBody/voyageNotifyHumaBody:
// wire не меняется (handlers/конверт читают те же поля), golden voyage+cadence цел.
//
// ★ HANDLER-NATIVE T5d: после перевода voyage+cadence на native прямых OUTPUT-потребителей
// генерёного VoyageTarget в huma-схемах НЕ осталось (Voyage.target — native api.VoyageTarget
// ниже; CadenceDTO.target — json.RawMessage). Прежний zero-net safety-alias aliasVoyageTarget
// (VoyageTarget → VoyageTarget) удалён вместе с последней oapi-зависимостью этого файла.
// Input-тела (VoyageCreateRequest.Target / CadenceCreateRequest.Target) ссылаются на api.VoyageTarget
// напрямую — alias им не нужен.

// VoyageTarget — декларативный таргет прогона (КЛАСС A, shared input↔output). scenario-
// режим: incarnations/service; command-режим: sids/where; общий coven. Все поля optional
// (рукопись :7455 — блока required НЕТ). Резолв в snapshot единиц — доменный (на спавне).
//
// ★ ПОРЯДОК ПОЛЕЙ = алфавитный (coven/incarnations/service/sids/where), как генерёный
// VoyageTarget. Когда VoyageTarget стал OUTPUT-схемой (Voyage.target native, финал T5b
// группа 4), json.Marshal эмитит ключи в порядке Go-полей — он ОБЯЗАН совпасть с прежним
// Voyage.target-wire (oapi-codegen сортирует поля по алфавиту), иначе golden voyage
// краснит на byte-order. Для INPUT порядок безразличен (unmarshal order-independent).
type VoyageTarget struct {
	Coven        []string `json:"coven,omitempty" doc:"coven-метки (env-тег scenario / метка хоста command)"`
	Incarnations []string `json:"incarnations,omitempty" doc:"имена инкарнаций (scenario-режим)"`
	Service      string   `json:"service,omitempty" doc:"имя сервиса (scenario-режим)"`
	SIDs         []string `json:"sids,omitempty" doc:"SID-ы хостов (command-режим)"`
	Where        string   `json:"where,omitempty" doc:"CEL-предикат как ДОПОЛНЕНИЕ к sids/coven (command-режим)"`
}

// VoyageNotify — разовая подписка на уведомления о прогоне (КЛАСС B, shared между input-
// телами). Форма; рантайм-валидацию (herald existence / RBAC herald.read / on-enum) делает
// доменный prepareNotifyErr. herald обязателен (рукопись :7612 — required:[herald]).
type VoyageNotify struct {
	Herald       string         `json:"herald" required:"true" pattern:"^[a-z0-9-]{1,63}$" doc:"имя канала-герольда"`
	On           []string       `json:"on,omitempty" doc:"терминалы/типы событий: completed|failed|partial"`
	OnlyFailures *bool          `json:"only_failures,omitempty"`
	OnlyChanges  *bool          `json:"only_changes,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Projection   []string       `json:"projection,omitempty"`
}
