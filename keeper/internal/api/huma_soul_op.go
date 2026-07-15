package api

// FULL-TYPED form of the SOUL domain (code-first OpenAPI source, ADR-054 §Pattern).
// ROLLOUT-BATCH-2e (soul read+write on huma per the role/operator references + audit endpoint):
// create — WRITE+AUDIT (soul.created, 201+body); coven-assign — WRITE+AUDIT (soul.coven-changed,
// 200+body custom XOR); issue-token — WRITE+AUDIT (soul.token-issued, 200+body like operator
// issue-token); ssh-target — WRITE+AUDIT (soul.ssh-target.updated, 200+body); list — read-with-
// typed-query (coven/status/transport + offset/limit/cursor); get/soulprint — read-with-path;
// history — read-with-typed-query (type[]/since/offset/limit, paginated → CheckPageBounds).
//
// POST /v1/souls/{sid}/exec (ErrandExec) — WRITE+AUDIT (errand.invoked) with TWO
// success codes: 200 sync ErrandResult (terminal up to server-cap) / 202 async
// ErrandAccepted + Location header (escalation). Body is pre-marshaled into
// json.RawMessage (errand GET shape), Status/Location — huma field convention.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// huma-output Body aliases for the domain wire types (handlers). Through them huma builds the
// schema of the 200 bodies and serializes the values already assembled by the handler (custom
// MarshalJSON / paged envelope / byte-passthrough typed_facts are preserved).
type (
	soulCovenAssignReplyBody = handlers.SoulCovenAssignResponse
	soulSoulprintReplyBody   = handlers.SoulprintReadReply
)

// === POST /v1/souls (create) — WRITE+AUDIT soul.created (201+body) ===

// soulCreateInput — huma input POST /v1/souls (FULL-TYPED). Body — a typed body.
type soulCreateInput struct {
	Body SoulCreateRequest
}

// SoulCreateRequest — Go form of the POST /v1/souls body (code-first source of schema AND validation).
// Struct name = contract schema name of the reference (docs/keeper/openapi.yaml → SoulCreateRequest):
// huma DefaultSchemaNamer takes reflect.Type.Name() directly. sid + transport + opt. covens +
// server-only note (written to souls.note; the reference SoulCreateRequest does NOT declare a note field —
// here it's present as a code-first body extension, not wire-affecting for golden). The format of
// sid/transport/coven — domain validation (422 in CreateTyped). additionalProperties:false
// (huma default) → unknown body field → 400.
type SoulCreateRequest struct {
	SID       string   `json:"sid" required:"true" doc:"SID нового хоста = FQDN"`
	Transport string   `json:"transport" required:"true" enum:"agent,ssh" doc:"способ доставки: agent (mTLS gRPC stream) / ssh (push без агента)"`
	Covens    []string `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"стабильные Coven-метки хоста (kebab-case, ADR-008)"`
	Note      string   `json:"note,omitempty" doc:"server-only заметка (souls.note)"`
}

// soulCreateOutput — huma output POST /v1/souls (FULL-TYPED). Status=201; Body — huma-native
// 201 body (SoulCreateReply, shape 1:1 with SoulCreateReply; bootstrap_token only for
// transport=agent). Wire shape is pinned by a golden-JSON byte-exact test
// (huma_soul_reply_test.go).
type soulCreateOutput struct {
	Status int `json:"-"`
	Body   SoulCreateReply
}

// soulCreateOperation — metadata of POST /v1/souls. Path = "/" relative to the chi group
// /v1/souls. DefaultStatus=201. Permission soul.create + audit soul.created. Errors: 400
// unknown/malformed, 403 RBAC, 409 soul-exists, 422 sid/transport/coven validation, 500.
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

// soulCovenAssignInput — huma input POST /v1/souls/coven (FULL-TYPED). Body — typed body
// (mode + label/labels XOR + selector). DryRun also from query (?dry_run=true, OR with body).
type soulCovenAssignInput struct {
	Body   SoulCovenAssignRequest
	DryRun bool `query:"dry_run" doc:"посчитать matched без UPDATE (OR с body.dry_run)"`
}

// SoulCovenAssignRequest — Go form of the POST /v1/souls/coven body. Struct name = contract name of
// the reference schema (docs/keeper/openapi.yaml → SoulCovenAssignRequest). mode (append/remove/
// replace) + XOR label↔labels (the domain validates the XOR → 422) + selector (at least one criterion) +
// opt. dry_run. additionalProperties:false → unknown field → 400.
type SoulCovenAssignRequest struct {
	Mode     string                  `json:"mode" required:"true" enum:"append,remove,replace" doc:"append — добавить метку; remove — снять; replace — заменить набор"`
	Label    string                  `json:"label,omitempty" maxLength:"63" doc:"метка для append/remove (запрещена для replace)"`
	Labels   []string                `json:"labels,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"набор для replace (может быть пустым = снять все; запрещён для append/remove)"`
	DryRun   bool                    `json:"dry_run,omitempty" doc:"посчитать matched без UPDATE"`
	Selector SoulCovenAssignSelector `json:"selector" required:"true" doc:"таргетинг (хотя бы один критерий; комбинации AND)"`
}

// SoulCovenAssignSelector — Go form of the selector (all/sids/coven/incarnation/status). Struct
// name = contract name of the reference schema (SoulCovenAssignSelector; input-only — CLASS C).
type SoulCovenAssignSelector struct {
	All         bool     `json:"all,omitempty" doc:"без host-фильтра (весь реестр ∩ scope)"`
	Sids        []string `json:"sids,omitempty" doc:"точечный список хостов (SID = FQDN)"`
	Coven       string   `json:"coven,omitempty" maxLength:"63" doc:"хосты с этой Coven-меткой"`
	Incarnation string   `json:"incarnation,omitempty" maxLength:"63" doc:"хосты этой incarnation (корневая Coven-метка)"`
	Status      string   `json:"status,omitempty" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"статус Soul в реестре"`
}

// soulCovenAssignOutput — huma output POST /v1/souls/coven (FULL-TYPED). Status=200; Body —
// typed 200 body (handlers.SoulCovenAssignBody; custom MarshalJSON XOR label↔labels).
type soulCovenAssignOutput struct {
	Status int `json:"-"`
	Body   soulCovenAssignReplyBody
}

// soulCovenAssignOperation — metadata of POST /v1/souls/coven. DefaultStatus=200. Permission
// soul.coven-assign + audit soul.coven-changed. Errors: 400 unknown/malformed, 403 RBAC,
// 422 mode/label(s)/selector/scope validation, 500.
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

// === POST /v1/souls/traits (traits-assign) — WRITE+AUDIT soul.traits-changed (200+body) ===

// soulTraitsAssignInput — huma input POST /v1/souls/traits (FULL-TYPED). Body — typed body
// (mode + traits/keys XOR + selector). DryRun also from query (?dry_run=true, OR with body).
type soulTraitsAssignInput struct {
	Body   SoulTraitsAssignRequest
	DryRun bool `query:"dry_run" doc:"посчитать matched без UPDATE (OR с body.dry_run)"`
}

// SoulTraitsAssignRequest — Go form of the POST /v1/souls/traits body (code-first source of schema AND
// validation; ADR-060). Struct name = contract schema name (huma DefaultSchemaNamer). mode
// (merge/replace/remove, default merge) + XOR traits↔keys (the domain validates → 422) + selector
// (at least one criterion) + opt. dry_run. traits — map key→(scalar|list of scalars); nested
// objects/arrays are rejected by the domain. additionalProperties:false → unknown field → 400.
type SoulTraitsAssignRequest struct {
	Mode     string                  `json:"mode,omitempty" enum:"merge,replace,remove" doc:"merge (дефолт) — set/overwrite ключи; replace — заменить весь map; remove — удалить ключи из keys"`
	Traits   map[string]any          `json:"traits,omitempty" doc:"набор ключ→значение для merge/replace (значение — scalar или list of scalars); запрещён для remove"`
	Keys     []string                `json:"keys,omitempty" doc:"список имён ключей для remove (kebab-case); запрещён для merge/replace"`
	DryRun   bool                    `json:"dry_run,omitempty" doc:"посчитать matched без UPDATE"`
	Selector SoulCovenAssignSelector `json:"selector" required:"true" doc:"таргетинг (хотя бы один критерий; комбинации AND)"`
}

// soulTraitsAssignOutput — huma output POST /v1/souls/traits (FULL-TYPED). Status=200; Body —
// typed 200 body (handlers.SoulTraitsAssignResponse).
type soulTraitsAssignOutput struct {
	Status int `json:"-"`
	Body   handlers.SoulTraitsAssignResponse
}

// soulTraitsAssignOperation — metadata of POST /v1/souls/traits. DefaultStatus=200. Permission
// soul.traits-assign + audit soul.traits-changed. Errors: 400 unknown/malformed, 403 RBAC,
// 422 mode/traits/keys/selector validation, 500.
func soulTraitsAssignOperation() huma.Operation {
	return huma.Operation{
		OperationID: "assignSoulTraits",
		Method:      http.MethodPost,
		Path:        "/traits",
		Summary:     "Массовое назначение trait-меток (deprecated)",
		// DEPRECATED (ADR-060 amend R1): operator-set trait management moved
		// per-soul → per-incarnation. The source of truth is incarnation.traits
		// (PUT /v1/incarnations/{name}/traits), projected into souls.traits
		// by a sync hook. A per-soul write here is overwritten by the next projection.
		// The endpoint is kept forward-compat (NOT removed); a call writes a warn log.
		Deprecated:    true,
		Description:   "DEPRECATED (ADR-060): используйте PUT /v1/incarnations/{name}/traits (incarnation.traits — источник истины, проецируется в souls.traits). Bulk merge/replace/remove operator-set trait-меток (souls.traits jsonb) на хостах под selector ∩ coven-scope. Per-soul write перетирается проекцией incarnation.traits. Permission soul.traits-assign. partial → 200 status:partial.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/souls/{sid}/issue-token (issue-token) — WRITE+AUDIT soul.token-issued (200+body) ===

// soulIssueTokenInput — huma input POST /v1/souls/{sid}/issue-token. SID — path; Force —
// query (?force=true). No Body.
type soulIssueTokenInput struct {
	SID   string `path:"sid" doc:"SID (FQDN) Soul-а"`
	Force bool   `query:"force" doc:"истечь активный токен и выписать новый"`
}

// soulIssueTokenOutput — huma output POST /v1/souls/{sid}/issue-token (FULL-TYPED). Status=200;
// Body — huma-native 200 body (SoulIssueTokenReply: sid/bootstrap_token/expires_at). Unlike the
// 204 write routes, issue-token returns the issued token (parity operator issue-token).
type soulIssueTokenOutput struct {
	Status int `json:"-"`
	Body   SoulIssueTokenReply
}

// soulIssueTokenOperation — metadata of POST /v1/souls/{sid}/issue-token. DefaultStatus=200.
// Permission soul.issue-token + audit soul.token-issued. Errors: 403 RBAC, 404 no soul,
// 409 active token without force, 422 invalid sid / transport=ssh, 500.
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

// soulSshTargetInput — huma input PUT /v1/souls/{sid}/ssh-target. SID — path; Body — typed body.
type soulSshTargetInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) Soul-а"`
	Body SoulSshTarget
}

// SoulSshTarget — Go form of the PUT /v1/souls/{sid}/ssh-target body (CLASS A, shared input↔output).
// Struct name = contract schema name of the reference (docs/keeper/openapi.yaml → SoulSshTarget;
// the reference SoulSshTargetRequest is a $ref to SoulSshTarget). All fields required except
// ssh_provider (optional 3-tier routing) — the required set [ssh_port,ssh_user,soul_path] is verified
// against the reference :6394. OUTPUT (SoulSshTargetReply.ssh_target) is collapsed onto the same schema via
// aliasSoulSshTarget (SoulSSHTarget → SoulSshTarget). ssh_port range / soul_path absoluteness /
// ssh_provider format — domain validation (422). additionalProperties:false →
// unknown → 400.
// ★ Field order mirrors SoulSSHTarget (soul_path, ssh_port, ssh_provider, ssh_user):
// encoding/json marshals in declaration order → the aligned order gives a byte-exact wire
// nested ssh_target in SoulSshTargetReply (output) vs the former legacy generator. For input parsing the order
// of JSON keys is irrelevant.
type SoulSshTarget struct {
	SoulPath    string `json:"soul_path" required:"true" pattern:"^/" doc:"абсолютный путь установки soul-бинаря (начинается с /)"`
	SSHPort     int    `json:"ssh_port" required:"true" minimum:"1" maximum:"65535" doc:"SSH-порт [1..65535]"`
	SSHProvider string `json:"ssh_provider,omitempty" doc:"опц. имя SshProvider (3-tier routing); пусто → coven/cluster default"`
	SSHUser     string `json:"ssh_user" required:"true" minLength:"1" doc:"SSH-пользователь"`
}

// soulSshTargetOutput — huma output PUT /v1/souls/{sid}/ssh-target (FULL-TYPED). Status=200;
// Body — huma-native 200 body (SoulSshTargetReply: snapshot of the saved target; nested
// ssh_target — class-A reuse of native SoulSshTarget). Wire shape is pinned by a golden-JSON
// byte-exact test (huma_soul_reply_test.go).
type soulSshTargetOutput struct {
	Status int `json:"-"`
	Body   SoulSshTargetReply
}

// soulSshTargetOperation — metadata of PUT /v1/souls/{sid}/ssh-target. DefaultStatus=200.
// Permission soul.ssh-target-update + audit soul.ssh-target.updated. Errors: 400 unknown/
// malformed, 403 RBAC, 404 no soul, 422 sid/port/user/path/provider validation, 500.
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

// === GET /v1/souls (list) — READ with typed query (no audit) ===

// soulListInput — huma input GET /v1/souls (FULL-TYPED typed query). coven/transport — string
// filters; status — a closed-set enum (out of set → 422). cursor — a keyset cursor (string).
// offset/limit — int32 with default; bad-int → 400; range / offset+cursor conflict / broken
// cursor are resolved in the (w,r) wrapper via ParsePageWithCursor (the huma route calls ListTyped with
// already-parsed page/cursor). Here offset/limit/cursor are bound for the schema;
// the business pagination parse is done by the register handler via ParsePageWithCursor over the same
// query values.
type soulListInput struct {
	Coven     string `query:"coven" doc:"фильтр по Coven-метке (AND внутри scope)"`
	Status    string `query:"status" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"фильтр по статусу; вне enum → 422"`
	Transport string `query:"transport" enum:"agent,ssh" doc:"фильтр по transport; вне enum → 422"`
	Cursor    string `query:"cursor" doc:"keyset-курсор продолжения (regex-режим scope)"`
	Offset    int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400; offset+cursor → 422)"`
	Limit     int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// soulListOutput — huma output GET /v1/souls (FULL-TYPED). Body — a TAGGED native envelope
// soulListReply (CURSOR, 6 fields: items.$ref to native SoulListEntry with json tags +
// next_cursor/total_approximate omitempty). Body used to be handlers.SoulListReply (=
// PagedResponse[SoulListView]) — untagged View → PascalCase wire (contract bug #7).
// The register func projects reply.Items through newSoulListEntry and CARRIES the cursor fields
// (next_cursor/total_approximate) byte-exact. The OpenAPI schema doesn't change (same alias target
// soulListReply).
type soulListOutput struct {
	Body soulListReply
}

// soulListOperation — metadata of GET /v1/souls. Path = "/" relative to the chi group /v1/souls.
// DefaultStatus=200. READ route: audit not wired. Permission soul.list. Errors: 400 (bad
// pagination / broken cursor), 403 RBAC, 422 (bad status/transport enum / offset+cursor), 500.
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

// === GET /v1/souls/stats (stats) — READ aggregate (no audit) ===

// soulStatsInput — huma input GET /v1/souls/stats. No parameters: the aggregate is
// computed over the operator's entire visible scope (the boundary comes from Purview, NOT the query).
type soulStatsInput struct{}

// soulStatsOutput — huma output GET /v1/souls/stats (FULL-TYPED). Body — a native
// aggregate DTO (soulStatsReply: by_status/by_transport/by_coven/total/stale_count).
type soulStatsOutput struct {
	Body soulStatsReply
}

// soulStatsOperation — metadata of GET /v1/souls/stats. Path = "/stats" relative to
// the chi group /v1/souls. DefaultStatus=200. READ route: audit not wired. Permission
// soul.list (the same registry-read right as list/get). Errors: 403 RBAC, 500.
func soulStatsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getSoulsStats",
		Method:        http.MethodGet,
		Path:          "/stats",
		Summary:       "Агрегат реестра Souls (Overview)",
		Description:   "Сводка по status/transport/coven + total + stale_count для Souls Overview со scoped-видимостью (ADR-047). transport — agent/ssh (UI маппит на pull/push). stale_count — по mark_disconnected.stale_after. Permission soul.list. Read-only, без audit.",
		Tags:          []string{"soul"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === GET /v1/souls/{sid} (get) — READ with path (no audit) ===

// soulGetInput — huma input GET /v1/souls/{sid}. SID — path.
type soulGetInput struct {
	SID string `path:"sid" doc:"SID (FQDN) Soul-а"`
}

// soulGetOutput — huma output GET /v1/souls/{sid} (FULL-TYPED). Body — huma-native 200 body
// (SoulListEntry — the same projection as the list-envelope element; shared get Body + envelope element).
type soulGetOutput struct {
	Body SoulListEntry
}

// soulGetOperation — metadata of GET /v1/souls/{sid}. DefaultStatus=200. READ route: audit not
// wired. Permission soul.list. Errors: 403, 404 (no soul / out of scope), 422 bad sid, 500.
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

// === GET /v1/souls/{sid}/soulprint (soulprint) — READ with path (no audit) ===

// soulSoulprintInput — huma input GET /v1/souls/{sid}/soulprint. SID — path.
type soulSoulprintInput struct {
	SID string `path:"sid" doc:"SID (FQDN) Soul-а"`
}

// soulSoulprintOutput — huma output GET /v1/souls/{sid}/soulprint (FULL-TYPED). Body — typed
// 200 body (handlers.SoulprintReadReply: sid/typed_facts/collected_at/received_at).
type soulSoulprintOutput struct {
	Body soulSoulprintReplyBody
}

// soulSoulprintOperation — metadata of GET /v1/souls/{sid}/soulprint. DefaultStatus=200. READ
// route: audit not wired. Permission soul.list. Errors: 403, 404 (no soul / out of scope),
// 410 (soulprint not received), 422 bad sid, 500.
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

// === GET /v1/souls/{sid}/history (history) — READ with typed query (no audit) ===

// soulHistoryInput — huma input GET /v1/souls/{sid}/history (FULL-TYPED typed query). SID —
// path. Types — multi-value (?type=X&type=Y) OR (explode:true is MANDATORY). Since — date-time
// (bad value → 400). offset/limit — int32 with default; range → CheckPageBounds 400.
type soulHistoryInput struct {
	SID    string    `path:"sid" doc:"SID (FQDN) Soul-а"`
	Types  []string  `query:"type,explode" enum:"scenario,errand" doc:"multi-value ?type=X&type=Y — OR по источнику; вне enum → 422"`
	Since  time.Time `query:"since" doc:"started_at > since (RFC3339); bad value → 400"`
	Offset int32     `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit  int32     `query:"limit" default:"50" doc:"размер страницы 1..1000 (out-of-range → 400)"`
}

// soulHistoryOutput — huma output GET /v1/souls/{sid}/history (FULL-TYPED). Body — a huma-native
// 200 envelope (SoulHistoryReply: sid/items/offset/limit/total + nested SoulHistoryItem;
// a standalone envelope, NOT generic PagedResponse).
type soulHistoryOutput struct {
	Body SoulHistoryReply
}

// soulHistoryOperation — metadata of GET /v1/souls/{sid}/history. DefaultStatus=200. READ route:
// audit not wired. Permission soul.list. Errors: 400 (out-of-range pagination / bad since),
// 403, 404 (no soul / out of scope), 422 (bad sid / type enum), 500.
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

// errandExecInput — huma input POST /v1/souls/{sid}/exec. SID — path; Body — a typed
// body (module required + opt. input/timeout_seconds/dry_run). additionalProperties:false
// (huma default) → unknown body field → 400.
type errandExecInput struct {
	SID  string `path:"sid" doc:"SID (FQDN) целевого Soul-а"`
	Body ErrandRunRequest
}

// ErrandRunRequest — Go form of the POST /v1/souls/{sid}/exec body (code-first source of schema AND
// validation). Struct name = contract request-schema name of the reference (docs/keeper/openapi.yaml
// → ErrandRunRequest, $ref at requestBody exec). module — required (empty → 422 in ExecTyped
// via dispatcher); input/timeout_seconds/dry_run — optional-pointer (the handler dereferences).
// timeout range / dry_run-for-verb / module format — domain validation (422/400 in ExecTyped).
type ErrandRunRequest struct {
	Module         string          `json:"module" required:"true" doc:"fully-qualified <ns>.<name>.<state> (core.cmd.shell / core.exec.run / ErrandReadSafe-модуль)"`
	Input          *map[string]any `json:"input,omitempty" doc:"input для модуля (валидируется против input_schema)"`
	TimeoutSeconds *int            `json:"timeout_seconds,omitempty" maximum:"300" doc:"полный timeout Errand-а [1..300]; 0/опущено → дефолт 30s; > server-cap (30s) → 202 + Location"`
	DryRun         *bool           `json:"dry_run,omitempty" doc:"только для PlanReadSafe-модулей; verb-модуль (shell/exec) → 400"`
}

// errandExecOutput — huma output POST /v1/souls/{sid}/exec with TWO success codes under
// one OperationID (200 sync ErrandResult / 202 async ErrandAccepted — different bodies +
// Location only on 202). Status — huma field convention (response-code override).
// Location — a header field: an empty string is NOT written (huma native omitempty), set
// ONLY on 202. Body — json.RawMessage: the handler pre-marshals the chosen body (errand GET
// shape; the schema in the fragment = `{}`, committed openapi.yaml carries the typed
// 200/ErrandResult + 202/ErrandAccepted — authoritative). The wire body bytes are identical to legacy.
type errandExecOutput struct {
	Status   int             `json:"-"`
	Location string          `header:"Location" json:"-"`
	Body     json.RawMessage `json:"body"`
}

// errandExecOperation — metadata of POST /v1/souls/{sid}/exec. DefaultStatus=200 (sync
// terminal). 202 (async escalation) — an additional success code (the handler sets
// Status=202 + Location itself). Permission errand.run + audit errand.invoked. Errors: 202
// async, 400 unknown/malformed/dry_run-verb, 403 RBAC, 404 soul-not-connected, 422
// invalid sid/module/timeout, 500.
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
