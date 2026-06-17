package api

// FULL-TYPED форма SOUL-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// ТИРАЖ-БАТЧ-2e (soul read+write на huma по эталонам role/operator + audit-endpoint):
// create — WRITE+AUDIT (soul.created, 201+body); coven-assign — WRITE+AUDIT (soul.coven-changed,
// 200+body custom XOR); issue-token — WRITE+AUDIT (soul.token-issued, 200+body как operator
// issue-token); ssh-target — WRITE+AUDIT (soul.ssh-target.updated, 200+body); list — read-with-
// typed-query (coven/status/transport + offset/limit/cursor); get/soulprint — read-with-path;
// history — read-with-typed-query (type[]/since/offset/limit, paginated → CheckPageBounds).
//
// POST /v1/souls/{sid}/exec (ErrandExec) — WRITE+AUDIT (errand.invoked) с ДВУМЯ
// success-кодами: 200 sync ErrandResult (терминал до server-cap) / 202 async
// ErrandAccepted + Location-header (escalation). Body пред-маршалится в
// json.RawMessage (форма errand GET), Status/Location — field-конвенция huma.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// huma-output Body-алиасы на доменные wire-типы (handlers). Через них huma строит схему
// 200-тел и сериализует уже собранные handler-ом значения (custom MarshalJSON / paged-
// envelope / byte-passthrough typed_facts сохраняются).
type (
	soulCovenAssignReplyBody = handlers.SoulCovenAssignResponse
	soulListReplyBody        = handlers.SoulListReply
	soulSoulprintReplyBody   = handlers.SoulprintReadReply
)

// === POST /v1/souls (create) — WRITE+AUDIT soul.created (201+body) ===

// soulCreateInput — huma-input POST /v1/souls (FULL-TYPED). Body — типизированное тело.
type soulCreateInput struct {
	Body SoulCreateRequest
}

// SoulCreateRequest — Go-форма тела POST /v1/souls (code-first источник схемы И валидации).
// Имя структуры = контрактное имя схемы рукописи (docs/keeper/openapi.yaml → SoulCreateRequest):
// huma DefaultSchemaNamer берёт reflect.Type.Name() напрямую. sid + transport + опц. covens +
// server-only note (запись в souls.note; рукописный SoulCreateRequest поля note НЕ объявляет —
// здесь оно присутствует как code-first расширение тела, не wire-затрагивающее golden). Формат
// sid/transport/coven — доменная валидация (422 в CreateTyped). additionalProperties:false
// (huma-дефолт) → unknown поле тела → 400.
type SoulCreateRequest struct {
	SID       string   `json:"sid" required:"true" doc:"SID нового хоста = FQDN"`
	Transport string   `json:"transport" required:"true" enum:"agent,ssh" doc:"способ доставки: agent (mTLS gRPC stream) / ssh (push без агента)"`
	Covens    []string `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"стабильные Coven-метки хоста (kebab-case, ADR-008)"`
	Note      string   `json:"note,omitempty" doc:"server-only заметка (souls.note)"`
}

// soulCreateOutput — huma-output POST /v1/souls (FULL-TYPED). Status=201; Body — huma-native
// 201-тело (SoulCreateReply, форма 1:1 с SoulCreateReply; bootstrap_token только для
// transport=agent). Wire-форма зафиксирована golden-JSON byte-exact-тестом
// (huma_soul_reply_test.go).
type soulCreateOutput struct {
	Status int `json:"-"`
	Body   SoulCreateReply
}

// soulCreateOperation — метаданные POST /v1/souls. Path = "/" относительно chi-группы
// /v1/souls. DefaultStatus=201. Permission soul.create + audit soul.created. Errors: 400
// unknown/malformed, 403 RBAC, 409 soul-exists, 422 валидация sid/transport/coven, 500.
func soulCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createSoul",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Зарегистрировать Soul",
		Description:   "Онбординг хоста в реестр souls (status: pending). Для transport=agent выпускается bootstrap-токен. Permission soul.create. 409 — SID занят.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/coven (coven-assign) — WRITE+AUDIT soul.coven-changed (200+body) ===

// soulCovenAssignInput — huma-input POST /v1/souls/coven (FULL-TYPED). Body — typed тело
// (mode + label/labels XOR + selector). DryRun также из query (?dry_run=true, OR с body).
type soulCovenAssignInput struct {
	Body   SoulCovenAssignRequest
	DryRun bool `query:"dry_run" doc:"посчитать matched без UPDATE (OR с body.dry_run)"`
}

// SoulCovenAssignRequest — Go-форма тела POST /v1/souls/coven. Имя структуры = контрактное имя
// схемы рукописи (docs/keeper/openapi.yaml → SoulCovenAssignRequest). mode (append/remove/
// replace) + XOR label↔labels (домен валидирует XOR → 422) + selector (хотя бы один критерий) +
// опц. dry_run. additionalProperties:false → unknown поле → 400.
type SoulCovenAssignRequest struct {
	Mode     string                  `json:"mode" required:"true" enum:"append,remove,replace" doc:"append — добавить метку; remove — снять; replace — заменить набор"`
	Label    string                  `json:"label,omitempty" maxLength:"63" doc:"метка для append/remove (запрещена для replace)"`
	Labels   []string                `json:"labels,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"набор для replace (может быть пустым = снять все; запрещён для append/remove)"`
	DryRun   bool                    `json:"dry_run,omitempty" doc:"посчитать matched без UPDATE"`
	Selector SoulCovenAssignSelector `json:"selector" required:"true" doc:"таргетинг (хотя бы один критерий; комбинации AND)"`
}

// SoulCovenAssignSelector — Go-форма селектора (all/sids/coven/incarnation/status). Имя
// структуры = контрактное имя схемы рукописи (SoulCovenAssignSelector; input-only — КЛАСС C).
type SoulCovenAssignSelector struct {
	All         bool     `json:"all,omitempty" doc:"без host-фильтра (весь реестр ∩ scope)"`
	Sids        []string `json:"sids,omitempty" doc:"точечный список хостов (SID = FQDN)"`
	Coven       string   `json:"coven,omitempty" maxLength:"63" doc:"хосты с этой Coven-меткой"`
	Incarnation string   `json:"incarnation,omitempty" maxLength:"63" doc:"хосты этой incarnation (корневая Coven-метка)"`
	Status      string   `json:"status,omitempty" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"статус Soul в реестре"`
}

// soulCovenAssignOutput — huma-output POST /v1/souls/coven (FULL-TYPED). Status=200; Body —
// typed 200-тело (handlers.SoulCovenAssignBody; custom MarshalJSON XOR label↔labels).
type soulCovenAssignOutput struct {
	Status int `json:"-"`
	Body   soulCovenAssignReplyBody
}

// soulCovenAssignOperation — метаданные POST /v1/souls/coven. DefaultStatus=200. Permission
// soul.coven-assign + audit soul.coven-changed. Errors: 400 unknown/malformed, 403 RBAC,
// 422 валидация mode/label(s)/selector/scope, 500.
func soulCovenAssignOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "assignSoulCoven",
		Method:        http.MethodPost,
		Path:          "/coven",
		Summary:       "Массовое назначение Coven-меток",
		Description:   "Bulk append/remove одной метки либо replace набора на хостах под selector ∩ scope (ADR-008). Permission soul.coven-assign. partial → 200 status:partial.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/{sid}/issue-token (issue-token) — WRITE+AUDIT soul.token-issued (200+body) ===

// soulIssueTokenInput — huma-input POST /v1/souls/{sid}/issue-token. SID — path; Force —
// query (?force=true). Body нет.
type soulIssueTokenInput struct {
	SID   string `path:"sid" doc:"SID (FQDN) Soul-а"`
	Force bool   `query:"force" doc:"истечь активный токен и выписать новый"`
}

// soulIssueTokenOutput — huma-output POST /v1/souls/{sid}/issue-token (FULL-TYPED). Status=200;
// Body — huma-native 200-тело (SoulIssueTokenReply: sid/bootstrap_token/expires_at). Отличие от
// 204-write-роутов — issue-token возвращает выпущенный токен (parity operator issue-token).
type soulIssueTokenOutput struct {
	Status int `json:"-"`
	Body   SoulIssueTokenReply
}

// soulIssueTokenOperation — метаданные POST /v1/souls/{sid}/issue-token. DefaultStatus=200.
// Permission soul.issue-token + audit soul.token-issued. Errors: 403 RBAC, 404 нет soul,
// 409 активный токен без force, 422 невалидный sid / transport=ssh, 500.
func soulIssueTokenOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueSoulToken",
		Method:        http.MethodPost,
		Path:          "/{sid}/issue-token",
		Summary:       "Перевыпустить bootstrap-токен",
		Description:   "Повторная выписка bootstrap-токена для transport=agent (?force=true истекает активный). Permission soul.issue-token. 409 — активный токен без force.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === PUT /v1/souls/{sid}/ssh-target (ssh-target) — WRITE+AUDIT soul.ssh-target.updated (200+body) ===

// soulSshTargetInput — huma-input PUT /v1/souls/{sid}/ssh-target. SID — path; Body — typed тело.
type soulSshTargetInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) Soul-а"`
	Body SoulSshTarget
}

// SoulSshTarget — Go-форма тела PUT /v1/souls/{sid}/ssh-target (КЛАСС A, shared input↔output).
// Имя структуры = контрактное имя схемы рукописи (docs/keeper/openapi.yaml → SoulSshTarget;
// рукописный SoulSshTargetRequest — $ref на SoulSshTarget). Все поля required, кроме
// ssh_provider (optional 3-tier routing) — required-набор [ssh_port,ssh_user,soul_path] сверен
// с рукописью :6394. OUTPUT (SoulSshTargetReply.ssh_target) сведён на ту же схему через
// aliasSoulSshTarget (SoulSSHTarget → SoulSshTarget). Диапазон ssh_port / абсолютность
// soul_path / формат ssh_provider — доменная валидация (422). additionalProperties:false →
// unknown → 400.
// ★ Порядок полей повторяет SoulSSHTarget (soul_path, ssh_port, ssh_provider, ssh_user):
// encoding/json маршалит в порядке объявления → выровненный порядок даёт byte-exact wire
// nested ssh_target в SoulSshTargetReply (output) vs legacy legacy-генерата. Для input-парсинга порядок
// JSON-ключей нерелевантен.
type SoulSshTarget struct {
	SoulPath    string `json:"soul_path" required:"true" pattern:"^/" doc:"абсолютный путь установки soul-бинаря (начинается с /)"`
	SSHPort     int    `json:"ssh_port" required:"true" minimum:"1" maximum:"65535" doc:"SSH-порт [1..65535]"`
	SSHProvider string `json:"ssh_provider,omitempty" doc:"опц. имя SshProvider (3-tier routing); пусто → coven/cluster default"`
	SSHUser     string `json:"ssh_user" required:"true" minLength:"1" doc:"SSH-пользователь"`
}

// soulSshTargetOutput — huma-output PUT /v1/souls/{sid}/ssh-target (FULL-TYPED). Status=200;
// Body — huma-native 200-тело (SoulSshTargetReply: snapshot сохранённого target-а; nested
// ssh_target — class-A reuse native SoulSshTarget). Wire-форма зафиксирована golden-JSON
// byte-exact-тестом (huma_soul_reply_test.go).
type soulSshTargetOutput struct {
	Status int `json:"-"`
	Body   SoulSshTargetReply
}

// soulSshTargetOperation — метаданные PUT /v1/souls/{sid}/ssh-target. DefaultStatus=200.
// Permission soul.ssh-target-update + audit soul.ssh-target.updated. Errors: 400 unknown/
// malformed, 403 RBAC, 404 нет soul, 422 валидация sid/port/user/path/provider, 500.
func soulSshTargetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateSoulSSHTarget",
		Method:        http.MethodPut,
		Path:          "/{sid}/ssh-target",
		Summary:       "Обновить SSH-реквизиты Soul-а",
		Description:   "Per-host SSH-реквизиты push-flow (ADR-032 S7-1). Replace-семантика (полный набор). Permission soul.ssh-target-update. 404 — нет soul.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls (list) — READ-with-typed-query (БЕЗ audit) ===

// soulListInput — huma-input GET /v1/souls (FULL-TYPED typed-query). coven/transport — string-
// фильтры; status — enum закрытого набора (вне набора → 422). cursor — keyset-курсор (string).
// offset/limit — int32 с default; bad-int → 400; диапазон / offset+cursor-конфликт / битый
// cursor разрешаются в (w,r)-обёртке через ParsePageWithCursor (huma-роут зовёт ListTyped с
// уже-распарсенными page/cursor). Здесь offset/limit/cursor биндятся для схемы;
// бизнес-разбор пагинации делает register-handler через ParsePageWithCursor над теми же
// query-значениями.
type soulListInput struct {
	Coven     string `query:"coven" doc:"фильтр по Coven-метке (AND внутри scope)"`
	Status    string `query:"status" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"фильтр по статусу; вне enum → 422"`
	Transport string `query:"transport" enum:"agent,ssh" doc:"фильтр по transport; вне enum → 422"`
	Cursor    string `query:"cursor" doc:"keyset-курсор продолжения (regex-режим scope)"`
	Offset    int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400; offset+cursor → 422)"`
	Limit     int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// soulListOutput — huma-output GET /v1/souls (FULL-TYPED). Body — typed 200-envelope
// (handlers.SoulListReply: items/offset/limit/total[/total_approximate/next_cursor]).
type soulListOutput struct {
	Body soulListReplyBody
}

// soulListOperation — метаданные GET /v1/souls. Path = "/" относительно chi-группы /v1/souls.
// DefaultStatus=200. READ-роут: audit НЕ навешан. Permission soul.list. Errors: 400 (bad
// pagination / битый cursor), 403 RBAC, 422 (bad status/transport enum / offset+cursor), 500.
func soulListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listSouls",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Soul-ов (paged, scoped)",
		Description:   "Реестр souls со scoped-видимостью (ADR-047) и фильтрами coven/status/transport. offset-fast-path либо keyset (режим выбирает сервер из Purview). Permission soul.list. Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid} (get) — READ-with-path (БЕЗ audit) ===

// soulGetInput — huma-input GET /v1/souls/{sid}. SID — path.
type soulGetInput struct {
	SID string `path:"sid" doc:"SID (FQDN) Soul-а"`
}

// soulGetOutput — huma-output GET /v1/souls/{sid} (FULL-TYPED). Body — huma-native 200-тело
// (SoulListEntry — та же проекция, что element list-envelope; shared get-Body + envelope-element).
type soulGetOutput struct {
	Body SoulListEntry
}

// soulGetOperation — метаданные GET /v1/souls/{sid}. DefaultStatus=200. READ-роут: audit НЕ
// навешан. Permission soul.list. Errors: 403, 404 (нет soul / вне scope), 422 bad sid, 500.
func soulGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoul",
		Method:        http.MethodGet,
		Path:          "/{sid}",
		Summary:       "Карточка Soul-а",
		Description:   "Одна строка реестра souls для detail-page (ADR-047 scoped). Permission soul.list. Вне scope → 404. Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid}/soulprint (soulprint) — READ-with-path (БЕЗ audit) ===

// soulSoulprintInput — huma-input GET /v1/souls/{sid}/soulprint. SID — path.
type soulSoulprintInput struct {
	SID string `path:"sid" doc:"SID (FQDN) Soul-а"`
}

// soulSoulprintOutput — huma-output GET /v1/souls/{sid}/soulprint (FULL-TYPED). Body — typed
// 200-тело (handlers.SoulprintReadReply: sid/typed_facts/collected_at/received_at).
type soulSoulprintOutput struct {
	Body soulSoulprintReplyBody
}

// soulSoulprintOperation — метаданные GET /v1/souls/{sid}/soulprint. DefaultStatus=200. READ-
// роут: audit НЕ навешан. Permission soul.list. Errors: 403, 404 (нет soul / вне scope),
// 410 (soulprint не получен), 422 bad sid, 500.
func soulSoulprintOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulprint",
		Method:        http.MethodGet,
		Path:          "/{sid}/soulprint",
		Summary:       "Soulprint Soul-а",
		Description:   "Последний typed-SoulprintReport (ADR-018) со scope-гейтом. Permission soul.list. 410 — soulprint ни разу не приходил. Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid}/history (history) — READ-with-typed-query (БЕЗ audit) ===

// soulHistoryInput — huma-input GET /v1/souls/{sid}/history (FULL-TYPED typed-query). SID —
// path. Types — multi-value (?type=X&type=Y) OR (explode:true ОБЯЗАТЕЛЕН). Since — date-time
// (bad value → 400). offset/limit — int32 с default; диапазон → CheckPageBounds 400.
type soulHistoryInput struct {
	SID    string    `path:"sid" doc:"SID (FQDN) Soul-а"`
	Types  []string  `query:"type,explode" enum:"scenario,errand" doc:"multi-value ?type=X&type=Y — OR по источнику; вне enum → 422"`
	Since  time.Time `query:"since" doc:"started_at > since (RFC3339); bad value → 400"`
	Offset int32     `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32     `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// soulHistoryOutput — huma-output GET /v1/souls/{sid}/history (FULL-TYPED). Body — huma-native
// 200-envelope (SoulHistoryReply: sid/items/offset/limit/total + nested SoulHistoryItem;
// самостоятельный envelope, НЕ generic PagedResponse).
type soulHistoryOutput struct {
	Body SoulHistoryReply
}

// soulHistoryOperation — метаданные GET /v1/souls/{sid}/history. DefaultStatus=200. READ-роут:
// audit НЕ навешан. Permission soul.list. Errors: 400 (out-of-range pagination / bad since),
// 403, 404 (нет soul / вне scope), 422 (bad sid / type enum), 500.
func soulHistoryOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulHistory",
		Method:        http.MethodGet,
		Path:          "/{sid}/history",
		Summary:       "История прогонов Soul-а (paged)",
		Description:   "Per-host timeline (scenario apply_runs + ad-hoc errands) со scope-гейтом. Permission soul.list. Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/{sid}/exec (exec) — WRITE+AUDIT errand.invoked (200 sync / 202 async) ===

// errandExecInput — huma-input POST /v1/souls/{sid}/exec. SID — path; Body — typed
// тело (module required + опц. input/timeout_seconds/dry_run). additionalProperties:false
// (huma-дефолт) → unknown поле тела → 400.
type errandExecInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) целевого Soul-а"`
	Body ErrandRunRequest
}

// ErrandRunRequest — Go-форма тела POST /v1/souls/{sid}/exec (code-first источник схемы И
// валидации). Имя структуры = контрактное имя request-схемы рукописи (docs/keeper/openapi.yaml
// → ErrandRunRequest, $ref у requestBody exec). module — required (пустой → 422 в ExecTyped
// через dispatcher); input/timeout_seconds/dry_run — optional-pointer (handler разыменовывает).
// Диапазон timeout / dry_run-для-verb / формат module — доменная валидация (422/400 в ExecTyped).
type ErrandRunRequest struct {
	Module         string          `json:"module" required:"true" doc:"fully-qualified <ns>.<name>.<state> (core.cmd.shell / core.exec.run / ErrandReadSafe-модуль)"`
	Input          *map[string]any `json:"input,omitempty" doc:"input для модуля (валидируется против input_schema)"`
	TimeoutSeconds *int            `json:"timeout_seconds,omitempty" maximum:"300" doc:"полный timeout Errand-а [1..300]; 0/опущено → дефолт 30s; > server-cap (30s) → 202 + Location"`
	DryRun         *bool           `json:"dry_run,omitempty" doc:"только для PlanReadSafe-модулей; verb-модуль (shell/exec) → 400"`
}

// errandExecOutput — huma-output POST /v1/souls/{sid}/exec с ДВУМЯ success-кодами под
// одним OperationID (200 sync ErrandResult / 202 async ErrandAccepted — разные тела +
// Location только на 202). Status — field-конвенция huma (override response-кода).
// Location — header-поле: пустая строка НЕ пишется (нативный omitempty huma), ставится
// ТОЛЬКО на 202. Body — json.RawMessage: handler пред-маршалит выбранное тело (форма
// errand GET; схема во фрагменте = `{}`, committed openapi.yaml несёт типизированные
// 200/ErrandResult + 202/ErrandAccepted — авторитет). Wire-байты тела идентичны легаси.
type errandExecOutput struct {
	Status   int             `json:"-"`
	Location string          `header:"Location" json:"-"`
	Body     json.RawMessage `json:"body"`
}

// errandExecOperation — метаданные POST /v1/souls/{sid}/exec. DefaultStatus=200 (sync
// терминал). 202 (async escalation) — дополнительный success-код (handler сам ставит
// Status=202 + Location). Permission errand.run + audit errand.invoked. Errors: 202
// async, 400 unknown/malformed/dry_run-verb, 403 RBAC, 404 soul-not-connected, 422
// невалидный sid/module/timeout, 500.
func errandExecOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "ErrandExec",
		Method:        http.MethodPost,
		Path:          "/{sid}/exec",
		Summary:       "Запустить Errand на Soul-е",
		Description:   "Pull-ad-hoc exec модуля на одном хосте (ADR-033). 200 sync (терминал до server-cap 30s) либо 202 + Location async-escalation. Permission errand.run. 404 — Soul не подключён.",
		Tags:          []string{"Errand"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusAccepted, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
