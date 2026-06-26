package api

// FULL-TYPED форма INCARNATION-домена (code-first источник OpenAPI, ADR-054 §Pattern,
// батч-2g). Go-типы — единственный источник правды: huma строит из них JSON Schema
// OpenAPI-фрагмента, валидацию входа (required/enum/additionalProperties:false ЧЕСТНЫЙ)
// и typed-output. MIXED домен — ДВА класса audit:
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): huma-audit-middleware пишет
//     event СНАРУЖИ (вариант B). registerHuma*-func кладёт payload через
//     SetHumaAuditPayload из *Typed-reply.AuditPayload.
//   - SELF-AUDIT (rerun-create / check-drift / destroy / update-hosts): audit пишет
//     САМ handler ВНУТРИ *Typed; audit-middleware НЕ навешан (newHumaCadenceAPI).
//
// Все incarnation-huma-op несут ПОЛНЫЙ путь /{name}[/...] относительно группы
// /v1/incarnations (chi.Route("/{name}") СНЯТ — иначе sibling-затенение → 405, как
// блокер батча-2f cadence). Сосуществует с choir-mount (батч-2f) на той же группе.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
)

// === POST /v1/incarnations (create) — MIDDLEWARE-AUDIT incarnation.created (202+body) ===

// incCreateInput — huma-input POST /v1/incarnations (FULL-TYPED). Body — typed тело.
type incCreateInput struct {
	Body IncarnationCreateRequest
}

// IncarnationCreateRequest — Go-форма тела POST /v1/incarnations. name/service
// required; covens/input опциональны. Формат name/service/coven — доменная валидация
// (422 в CreateTyped). additionalProperties:false (huma-дефолт) → unknown поле → 400.
// Имя структуры = контрактное имя схемы в OpenAPI (huma DefaultSchemaNamer берёт
// reflect.Type.Name() напрямую) — выровнено под committed-рукопись (T4b pilot).
type IncarnationCreateRequest struct {
	Name    string         `json:"name" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"имя нового instance (kebab-case), корневая Coven-метка"`
	Service string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"имя сервиса из реестра (ADR-029)"`
	Covens  []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment-теги (ADR-008 amendment a)"`
	Input   map[string]any `json:"input,omitempty" doc:"input для scenario create"`
	// Traits — operator-set trait-метки инкарнации (ADR-060 amend R1): map ключ →
	// scalar | list of scalars. Кладутся в incarnation.traits (источник истины) и
	// материализованно проецируются в souls.traits хостов-членов. Формат/значение
	// валидирует домен (вложенный объект/массив → 422). Day-2 замена — PUT
	// .../traits.
	Traits map[string]any `json:"traits,omitempty" doc:"operator-set trait-метки (ключ → scalar|list of scalars), ADR-060"`
}

// incCreateOutput — huma-output POST /v1/incarnations (FULL-TYPED). Status=202;
// Body — native 202-тело (IncarnationCreateReply: incarnation + опц. apply_id).
type incCreateOutput struct {
	Status int `json:"-"`
	Body   IncarnationCreateReply
}

func incCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createIncarnation",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать инкарнацию",
		Description:   "Runtime-инстанс сервиса (ADR-029). Запускает scenario create (async, если lifecycle.auto_create). Permission incarnation.create.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations (list) — READ-with-typed-query (БЕЗ audit) ===

// incListInput — huma-input GET /v1/incarnations (FULL-TYPED typed-query). offset/limit
// — int32 с default; диапазон enforce-ит CheckPageBounds в ListTyped → 400 (parity
// ParsePage). Остальные фильтры string/enum (422-валидацию ведёт ListTyped). state-
// фильтры `state.<field>` huma как typed-параметры НЕ биндит (динамические ключи) —
// caller передаёт их из исходного query (см. registerHumaIncarnationList).
type incListInput struct {
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
	Service string `query:"service" doc:"фильтр по имени сервиса"`
	Status  string `query:"status" doc:"фильтр по статусу (ready/applying/error_locked/migration_failed); невалидный → 422"`
	Coven   string `query:"coven" doc:"exact-match по covens[] (ADR-008); невалидная метка → 422"`
	SortBy  string `query:"sort" doc:"поле сортировки (created_at/name/status/service или state.<field>)"`
	SortDir string `query:"sort_dir" doc:"направление сортировки (asc/desc)"`
}

// incListOutput — huma-output GET /v1/incarnations (FULL-TYPED). Body — TAGGED native
// envelope incarnationListReply (items.$ref на native IncarnationGetReply с json-тегами:
// snake_case-wire). Прежде Body был handlers.IncarnationListReply (= PagedResponse[
// IncarnationGetView]) — untagged View → PascalCase-wire (контракт-баг #7). Register-func
// проецирует reply.Items через newIncarnationGetReply. Схема OpenAPI не меняется (та же
// alias-цель incarnationListReply).
type incListOutput struct {
	Body incarnationListReply
}

func incListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listIncarnations",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список инкарнаций (paged)",
		Description:   "Фильтры service/status/coven/state.<field> + сортировка. Видимость scoped по RBAC (ADR-047). Permission incarnation.list. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name} (get) — READ-with-path (БЕЗ audit) ===

// incGetInput — huma-input GET /v1/incarnations/{name}. Name — path.
type incGetInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
}

// incGetOutput — huma-output GET /v1/incarnations/{name} (FULL-TYPED). Body — полный
// native IncarnationGetReply (byte-exact с legacy GET {name}).
type incGetOutput struct {
	Body IncarnationGetReply
}

func incGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnation",
		Method:        http.MethodGet,
		Path:          "/{name}",
		Summary:       "Получить инкарнацию",
		Description:   "Деталь runtime-инстанса. Вне RBAC-scope → 404 (не палим существование). Permission incarnation.get. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/incarnations/{name}/history (history) — READ-with-typed-query (БЕЗ audit) ===

// incHistoryInput — huma-input GET /v1/incarnations/{name}/history. Name — path;
// apply_id — опц. ULID-фильтр (bad → 400 в HistoryTyped); offset/limit — int32 с
// default (out-of-range → 400).
type incHistoryInput struct {
	Name    string `path:"name" doc:"имя инкарнации"`
	ApplyID string `query:"apply_id" doc:"опц. ULID-фильтр по state_history.apply_id; не-ULID → 400"`
	Offset  int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit   int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// incHistoryOutput — huma-output GET /v1/incarnations/{name}/history (FULL-TYPED). Body
// — TAGGED native envelope incarnationHistoryReply (items.$ref на native StateHistoryEntry
// с json-тегами: snake_case-wire). Прежде Body был handlers.IncarnationHistoryReply (=
// PagedResponse[StateHistoryView]) — untagged View → PascalCase-wire (контракт-баг #7).
// Register-func проецирует reply.Items через newStateHistoryEntry. Схема OpenAPI не
// меняется (та же alias-цель incarnationHistoryReply).
type incHistoryOutput struct {
	Body incarnationHistoryReply
}

func incHistoryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getIncarnationHistory",
		Method:        http.MethodGet,
		Path:          "/{name}/history",
		Summary:       "История state-переходов инкарнации (paged)",
		Description:   "state_history с фильтром apply_id и пагинацией. Вне RBAC-scope → 404. Permission incarnation.history. Read-only.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/scenarios/{scenario} (run) — MIDDLEWARE-AUDIT incarnation.scenario_started (202+body) ===

// incRunInput — huma-input POST .../scenarios/{scenario}. Name/Scenario — path; Body —
// ПОИНТЕР (опц. тело: huma помечает RequestBody.Required=false для *T, на пустом body
// Body=nil — parity legacy io.EOF→zero-value). input опционален.
type incRunInput struct {
	Name     string                 `path:"name" doc:"имя инкарнации"`
	Scenario string                 `path:"scenario" doc:"имя сценария"`
	Body     *IncarnationRunRequest `doc:"опц. тело: input scenario"`
}

// IncarnationRunRequest — Go-форма тела POST .../scenarios/{scenario}. name/scenario
// echo из path игнорируются (авторитет — path). input опционален.
// additionalProperties:false → unknown поле → 400. Имя = контрактное имя схемы (T4b).
type IncarnationRunRequest struct {
	Name     *string        `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	Scenario *string        `json:"scenario,omitempty" doc:"echo path-scenario (игнорируется)"`
	Input    map[string]any `json:"input,omitempty" doc:"input scenario"`
}

// incRunOutput — huma-output POST .../scenarios/{scenario} (FULL-TYPED). Status=202;
// Body — native IncarnationRunReply (apply_id + echo incarnation/scenario).
type incRunOutput struct {
	Status int `json:"-"`
	Body   IncarnationRunReply
}

func incRunOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "runIncarnationScenario",
		Method:        http.MethodPost,
		Path:          "/{name}/scenarios/{scenario}",
		Summary:       "Запустить сценарий инкарнации",
		Description:   "Async-прогон именованного scenario (ADR-009). Блокируется при cluster:degraded (503). Permission incarnation.run.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/unlock (unlock) — MIDDLEWARE-AUDIT incarnation.unlocked (200+body) ===

// incUnlockInput — huma-input POST .../unlock. Name — path; Body — typed тело.
type incUnlockInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUnlockRequest
}

// IncarnationUnlockRequest — Go-форма тела POST .../unlock. reason required; name echo
// игнорируется. additionalProperties:false → unknown поле → 400. Имя = контрактное
// имя схемы (T4b).
type IncarnationUnlockRequest struct {
	Name   *string `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	Reason string  `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"свободный текст подтверждения"`
}

// incUnlockOutput — huma-output POST .../unlock (FULL-TYPED). Status=200; Body —
// native IncarnationUnlockReply.
type incUnlockOutput struct {
	Status int `json:"-"`
	Body   IncarnationUnlockReply
}

func incUnlockOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "unlockIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/unlock",
		Summary:       "Снять блокирующий статус инкарнации",
		Description:   "error_locked / migration_failed → ready под FOR UPDATE; state не меняется (ADR-009/019). Permission incarnation.unlock.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/upgrade (upgrade) — MIDDLEWARE-AUDIT incarnation.upgrade_started (202+body) ===

// incUpgradeInput — huma-input POST .../upgrade. Name — path; Body — typed тело.
type incUpgradeInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUpgradeRequest
}

// IncarnationUpgradeRequest — Go-форма тела POST .../upgrade. to_version required; name
// echo игнорируется. additionalProperties:false → unknown поле → 400. Имя = контрактное
// имя схемы (T4b).
type IncarnationUpgradeRequest struct {
	Name      *string `json:"name,omitempty" doc:"echo path-name (игнорируется)"`
	ToVersion string  `json:"to_version" required:"true" doc:"целевая версия сервиса (git-ref)"`
}

// incUpgradeOutput — huma-output POST .../upgrade (FULL-TYPED). Status=202; Body —
// native IncarnationUpgradeReply (apply_id).
type incUpgradeOutput struct {
	Status int `json:"-"`
	Body   IncarnationUpgradeReply
}

func incUpgradeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "upgradeIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/upgrade",
		Summary:       "Перевести инкарнацию на новую версию",
		Description:   "Sync-под-202 миграция state_schema (ADR-019) + смена service_version одной tx. Permission incarnation.upgrade.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/rerun-create (rerun-create) — SELF-AUDIT incarnation.create_rerun (202+body) ===

// incRerunInput — huma-input POST .../rerun-create. Name — path; Body — typed тело.
type incRerunInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationRerunCreateRequest
}

// IncarnationRerunCreateRequest — Go-форма тела POST .../rerun-create. reason required.
// additionalProperties:false → unknown поле → 400. Имя = контрактное имя схемы (T4b).
type IncarnationRerunCreateRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"свободный текст подтверждения"`
}

// incRerunOutput — huma-output POST .../rerun-create (FULL-TYPED). Status=202; Body —
// native IncarnationRerunCreateReply (apply_id + echo incarnation).
type incRerunOutput struct {
	Status int `json:"-"`
	Body   IncarnationRerunCreateReply
}

func incRerunOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "rerunCreateIncarnation",
		Method:        http.MethodPost,
		Path:          "/{name}/rerun-create",
		Summary:       "Перезапустить create из error_locked",
		Description:   "Снимает error_locked и тем же действием перезапускает scenario create (одна tx FOR UPDATE). Permission incarnation.create-rerun.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/incarnations/{name}/check-drift (check-drift) — SELF-AUDIT incarnation.drift_checked (200+body) ===

// incCheckDriftInput — huma-input POST .../check-drift. Name — path; Body — ПОИНТЕР
// (опц. тело: huma RequestBody.Required=false для *T, на пустом body Body=nil — parity
// legacy io.EOF→zero-value).
type incCheckDriftInput struct {
	Name string                        `path:"name" doc:"имя инкарнации"`
	Body *IncarnationCheckDriftRequest `doc:"опц. тело: override converge-параметров"`
}

// IncarnationCheckDriftRequest — Go-форма тела POST .../check-drift. input — override
// converge-параметров (опц.). additionalProperties:false → unknown поле → 400. Имя =
// контрактное имя схемы (T4b).
type IncarnationCheckDriftRequest struct {
	Input map[string]any `json:"input,omitempty" doc:"override converge-параметров (ADR-031 Slice B)"`
}

// incCheckDriftOutput — huma-output POST .../check-drift (FULL-TYPED). Status=200; Body
// — *scenario.DriftReport (тот же тип, что писал legacy writeJSON). CheckDriftTyped на
// успехе возвращает non-nil.
type incCheckDriftOutput struct {
	Body *scenario.DriftReport
}

func incCheckDriftOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "checkIncarnationDrift",
		Method:        http.MethodPost,
		Path:          "/{name}/check-drift",
		Summary:       "Проверить drift инкарнации (Scry)",
		Description:   "Sync dry_run converge → DriftReport (ADR-031 Slice B). Информационная маркировка status=drift. Permission incarnation.check-drift.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === DELETE /v1/incarnations/{name} (destroy) — SELF-AUDIT incarnation.destroy_started (202+body) ===

// incDestroyInput — huma-input DELETE /v1/incarnations/{name}. Name — path; AllowDestroy
// — required boolean query (confirmation-flag). huma биндит bool типизированно: missing/
// non-boolean → 400 (parity strict required-param + legacy ParseBool).
type incDestroyInput struct {
	Name         string `path:"name" doc:"имя инкарнации"`
	AllowDestroy bool   `query:"allow_destroy" required:"true" doc:"confirmation-flag: true → destroy без teardown"`
}

// incDestroyOutput — huma-output DELETE /v1/incarnations/{name} (FULL-TYPED). Status=202;
// Body — native IncarnationDestroyReply (apply_id).
type incDestroyOutput struct {
	Status int `json:"-"`
	Body   IncarnationDestroyReply
}

func incDestroyOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "destroyIncarnation",
		Method:        http.MethodDelete,
		Path:          "/{name}",
		Summary:       "Снести инкарнацию",
		Description:   "allow_destroy=true → DELETE без teardown; false → scenario destroy (S-D4). Permission incarnation.destroy.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PATCH /v1/incarnations/{name}/hosts (update-hosts) — SELF-AUDIT incarnation.hosts_updated (200+body) ===
//
// PATCH-presence: mode-семантика (replace/append/remove) НЕ требует различения
// omitted/null/value (mode/hosts — required-семантика операции, не sparse-update поля).
// Поэтому форма — `*string omitempty` для role (parity legacy IncarnationSpecHost), НЕ
// Optional[T] presence-tier huma_optional.go (см. huma_optional.go §«Прочие PATCH ...
// presence НЕ детектят»).

// incUpdateHostsInput — huma-input PATCH .../hosts. Name — path; Body — typed тело.
type incUpdateHostsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationUpdateHostsRequest
}

// IncarnationSpecHost — одна запись hosts[]. sid required; role опц. (kebab-case 1..63
// или пусто — доменная валидация 422). additionalProperties:false → unknown поле → 400.
// Имя = контрактное имя схемы (T4b); huma-форма с валидационными тегами, отличается от
// IncarnationSpecHost (доменная модель без huma-тегов).
type IncarnationSpecHost struct {
	SID  string  `json:"sid" required:"true" doc:"SID (FQDN) хоста — обязан существовать в souls"`
	Role *string `json:"role,omitempty" maxLength:"63" doc:"declared-роль (kebab-case 1..63) или null"`
}

// IncarnationUpdateHostsRequest — Go-форма тела PATCH .../hosts. mode required (enum);
// hosts — массив (пустой legitimate для replace). additionalProperties:false → unknown
// поле → 400. Имя = контрактное имя схемы (T4b).
type IncarnationUpdateHostsRequest struct {
	Mode  string                `json:"mode" required:"true" enum:"replace,append,remove" doc:"тип операции над spec.hosts[]"`
	Hosts []IncarnationSpecHost `json:"hosts" required:"true" doc:"список hosts для mode-операции (пустой legitimate для replace)"`
}

// incUpdateHostsOutput — huma-output PATCH .../hosts (FULL-TYPED). Status=200; Body —
// полный native IncarnationGetReply после правки (byte-exact с legacy).
type incUpdateHostsOutput struct {
	Body IncarnationGetReply
}

func incUpdateHostsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateIncarnationHosts",
		Method:        http.MethodPatch,
		Path:          "/{name}/hosts",
		Summary:       "Править declared spec.hosts[] инкарнации",
		Description:   "Три mode (replace/append/remove) над declared hosts (ADR-008). Permission incarnation.update-hosts.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/incarnations/{name}/traits (set-traits) — SELF-AUDIT incarnation.traits_changed (200+body) ===

// incSetTraitsInput — huma-input PUT .../traits. Name — path; Body — typed тело.
type incSetTraitsInput struct {
	Name string `path:"name" doc:"имя инкарнации"`
	Body IncarnationSetTraitsRequest
}

// IncarnationSetTraitsRequest — Go-форма тела PUT .../traits. traits — целостная
// замена operator-set trait-меток (key → scalar|list of scalars); пустой/отсутствует
// = очистить. Формат значения (запрет nested) валидирует домен → 422.
// additionalProperties:false → unknown поле → 400. Имя = контрактное имя схемы.
type IncarnationSetTraitsRequest struct {
	Traits map[string]any `json:"traits,omitempty" doc:"полный набор trait-меток (ключ → scalar|list of scalars); пустой/опущен = очистить (ADR-060)"`
}

// incSetTraitsOutput — huma-output PUT .../traits (FULL-TYPED). Status=200; Body —
// полный native IncarnationGetReply после замены (byte-exact с GET / update-hosts).
type incSetTraitsOutput struct {
	Body IncarnationGetReply
}

func incSetTraitsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "setIncarnationTraits",
		Method:        http.MethodPut,
		Path:          "/{name}/traits",
		Summary:       "Заменить operator-set trait-метки инкарнации",
		Description:   "Целостная замена incarnation.traits (ADR-060) — источника истины, проецируемого в souls.traits хостов-членов. Permission incarnation.traits-set.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
