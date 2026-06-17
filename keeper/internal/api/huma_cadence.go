package api

// PILOT-1 разворота OpenAPI spec-first → code-first на huma v2, FULL-TYPED форма
// (ADR-054 Amendment 2026-06-12, supersede ADR-051). Доказывает ОДИН роут —
// POST /v1/cadences — поверх chi-mux через huma как ЭТАЛОН тиража ~20 доменов:
// typed input + извлечённая доменная функция + конверт + typed output, без
// делегационного RawBody-моста.
//
// === FULL-TYPED PATTERN (эталон-инструкция тиража) ===
//
//  1. TYPED INPUT. cadenceCreateInput{ Body cadenceCreateHumaBody } — huma декодит
//     и валидирует Body по схеме из huma-тегов (required/enum/additionalProperties:
//     false ЧЕСТНЫЙ). RawBody нет — строгость держит huma, не доменный декод.
//
//  2. ИЗВЛЕЧЁННАЯ ДОМЕННАЯ ФУНКЦИЯ. CadenceHandler.CreateTyped(ctx, claims, req)
//     (reply, error) — вся бизнес-логика (RBAC-guards / persist tx + notify /
//     invalidation / self-audit) без http.ResponseWriter/*http.Request. Ошибки —
//     *handlers.problemError (внутр.); legacy Create(w,r) остался тонкой оболочкой
//     над CreateTyped (strict-мост CreateCadence).
//
//  3. КОНВЕРТ (тонкий). huma-handler: typed input.Body → доменная модель
//     handlers.CadenceCreateRequest (поле-в-поле) → CreateTyped → доменная
//     CadenceCreateReply → конверт в typed cadenceCreateOutput. Оба конца
//     типизированы; конверт — единственный «клей».
//
//  4. TYPED OUTPUT. cadenceCreateOutput{ Status 201; Location header; Body }.
//     Заменяет ручную запись в (w). omitempty/nullable зафиксированы golden-JSON
//     snapshot-тестом (wire-регресс-guard тиража).
//
//  5. CLAIMS. RequireJWT (chi-middleware группы) положил claims в request-context
//     ДО humachi; huma отдаёт тот же ctx в handler → middleware.ClaimsFromContext(ctx)
//     достаёт их напрямую. Никакого context-bridge / Unwrap.
//
//  6. PROBLEM+JSON OVERRIDE (умный, FULL-TYPED). huma.NewError переопределён на наш
//     problem-формат (humaProblemError). На validation-fail huma зовёт его с
//     errs ...error (ErrorDetail-ы). Override детектит "unexpected property" →
//     400 TypeMalformedRequest (unknown→400, контракт кластера), прочее (missing-
//     required/enum) → 422 TypeValidationFailed. installHumaErrorOverride —
//     ОДИН явный вызов при buildRouter (единая точка тиража).
//
//  7. СПЕКА-МЕРЖ. huma эмитит OpenAPI 3.1; рукописный meta/openapi.yaml — 3.0.3.
//     Pilot-1 huma-фрагмент НЕ вмерживается (GET /openapi.yaml возвращает рукописный
//     3.0.3 — cadence-путь там описан, форма совпадает). huma-dump pilot-1 — только
//     в guard-тесте. Заголовок→3.1 — разово на первом мерж-батче тиража.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/validation"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// humaProblemError переопределяет huma.NewError на наш RFC 7807 problem+json.
// Назначается installHumaErrorOverride (глобально на пакет huma). Любую huma-
// сгенерированную ошибку (415, body-limit, validation тела) huma отдаёт через этот
// тип → единый error-контракт кластера сохраняется. Доменные ошибки идут через
// CreateTyped → конверт (см. registerHumaCadence) и сюда не попадают.
type humaProblemError struct {
	problem.Details
}

func (e humaProblemError) Error() string { return e.Details.Detail }

// GetStatus реализует huma.StatusError — huma выставит этот код ответа.
func (e humaProblemError) GetStatus() int { return e.Details.Status }

// MarshalJSON — тело ошибки = наш problem.Details (а не huma ErrorModel). Поля
// problem.Details несут json-теги RFC 7807 → stdlib-marshal даёт ровно наш формат.
func (e humaProblemError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Details)
}

// ContentType реализует huma.ContentTypeFilter — единый problem+json-контракт.
func (e humaProblemError) ContentType(string) string { return problem.ContentType }

// newHumaProblemError собирает problem-форму из huma-статуса/сообщения и списка
// validation-ошибок (FULL-TYPED классификация, ADR-054 §Инвариант-2):
//
//   - есть ErrorDetail с Message == "unexpected property" → 400 TypeMalformedRequest
//     (unknown-поле → 400, контракт кластера; иначе huma вернул бы 422 — расхождение);
//   - есть ErrorDetail c query.-Location И parse-Message (typed-query bind, ADR-054
//     §Pattern четвёртый tier) → 400 TypeMalformedRequest (bad date-time/int/bool/
//     float query → 400; enum-mismatch в этот набор НЕ попадает → 422, см. ниже);
//   - status 400/415 → TypeMalformedRequest (malformed body / unsupported media);
//   - прочие 4xx (missing-required/enum/type-mismatch, huma-дефолт 422) →
//     TypeValidationFailed;
//   - 5xx → TypeInternalError.
//
// instance пуст (huma не несёт URL пути в NewError-хуке).
func newHumaProblemError(status int, msg string, errs []error) humaProblemError {
	if hasUnexpectedProperty(errs) {
		d := problem.New(problem.TypeMalformedRequest, "", "unknown field in request body")
		return humaProblemError{Details: d}
	}
	if hasQueryParseError(errs) {
		d := problem.New(problem.TypeMalformedRequest, "", msg)
		d.Status = http.StatusBadRequest
		return humaProblemError{Details: d}
	}
	var typ string
	switch {
	case status == http.StatusBadRequest, status == http.StatusUnsupportedMediaType:
		typ = problem.TypeMalformedRequest
	case status >= 400 && status < 500:
		typ = problem.TypeValidationFailed
	default:
		typ = problem.TypeInternalError
	}
	d := problem.New(typ, "", msg)
	d.Status = status // сохраняем фактический huma-статус (не дефолт таблицы)
	return humaProblemError{Details: d}
}

// hasUnexpectedProperty ищет в validation-ошибках huma признак unknown-поля тела
// (validation.MsgUnexpectedProperty). huma кладёт его в *huma.ErrorDetail.Message
// при additionalProperties:false-нарушении. Наличие → классифицируем как 400.
func hasUnexpectedProperty(errs []error) bool {
	for _, e := range errs {
		if d, ok := e.(*huma.ErrorDetail); ok && d.Message == validation.MsgUnexpectedProperty {
			return true
		}
	}
	return false
}

// queryParseMessages — точные huma-сообщения parse-фазы typed-query (huma v2.38
// parseInto, huma.go:1685-1769). Bad-value bind типизированного query-параметра
// (time.Time/int/bool/float) добавляется как ErrorDetail c Location `query.<name>` и
// ОДНИМ из этих Message ДО schema-validation (huma.go:892-896 res.Add(pb, value,
// err.Error())). Enum-mismatch — НАОБОРОТ из schema-validation (validate.go:586
// `s.msgEnum` = "expected value to be one of …") и в этот набор НЕ попадает → падает
// в дефолтную 422-ветку. Так детект отличает parse-error (400) от enum-mismatch (422)
// на одинаковом query.-Location: дискриминатор — именно Message-литерал, не Location.
//
// COUPLING на huma-Message-литералы: при bump huma свериться с parseInto (точные
// строки errors.New(...)). `invalid date/time` — ПРЕФИКС (huma добавляет суффикс
// " for format <fmt>"), потому матч префиксный. Guard-набор (huma_audit_endpoint_test.go,
// особенно BadSource_422) ловит регресс детекта при рассинхроне со строками huma.
var queryParseMessages = []string{
	"invalid date/time",                   // huma.go:1747 "invalid date/time for format <fmt>" (префикс)
	"invalid integer",                     // huma.go:1694/1701
	"invalid boolean",                     // huma.go:1715
	"invalid float",                       // huma.go:1708
	"required query parameter is missing", // huma.go:876/887 missing required query-param → 400 (parity legacy strict RequiredParamError; incarnation destroy allow_destroy)
}

// hasQueryParseError ищет в validation-ошибках huma признак parse-фейла типизированного
// query-параметра: *huma.ErrorDetail c Location-префиксом `query.` И Message из
// queryParseMessages (префикс-матч). enum-mismatch query.source НЕ матчит (его Message
// другой) → остаётся 422. См. queryParseMessages про coupling и дискриминатор.
func hasQueryParseError(errs []error) bool {
	for _, e := range errs {
		d, ok := e.(*huma.ErrorDetail)
		if !ok || !strings.HasPrefix(d.Location, "query.") {
			continue
		}
		for _, m := range queryParseMessages {
			if strings.HasPrefix(d.Message, m) {
				return true
			}
		}
	}
	return false
}

// installHumaErrorOverride переопределяет глобальный huma.NewError на наш problem-
// формат. Идемпотентна. ЕДИНАЯ ТОЧКА: вызывается ОДИН раз явно при buildRouter
// (не в фабрике huma.API — для тиража единый install, не на каждый домен).
// huma.NewError — пакетная var, override глобальный на процесс (для всех huma.API);
// тираж это устраивает — контракт ошибок единый по всему API.
func installHumaErrorOverride() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return newHumaProblemError(status, msg, errs)
	}
}

// registerHumaCadence монтирует POST /v1/cadences через huma на переданный
// chi.Router (та группа, что уже несёт RequireJWT/RequirePermission/maxBody/metrics).
// cadenceH — доменный handler; nil → no-op (паттерн opt-in-домена router.go).
//
// FULL-TYPED handler: huma валидирует typed Body → конверт в доменную модель →
// CreateTyped → конверт reply в typed output. Доменные problem-ошибки доставляются
// через humaProblemError (тот же error-контракт, что huma-валидация).
//
// ВАЖНО (path): humaAPI создан newHumaCadenceAPI поверх chi-группы /v1/cadences с
// навеской RequirePermission(cadence.create). chi внутри группы матчит ОТНОСИТЕЛЬНО
// /v1/cadences → huma.Operation.Path = "/" (см. cadenceCreateOperation). chi
// смонтирует роут как /v1/cadences (chi.Walk видит его, drift-test зелёный).
func registerHumaCadence(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceCreateOperation(), func(ctx context.Context, in *cadenceCreateInput) (*cadenceCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, huma.NewError(http.StatusInternalServerError, "missing claims")
		}
		req := toCadenceCreateRequest(in.Body)
		reply, err := cadenceH.CreateTyped(ctx, claims, req)
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceCreateOutput{
			Status:   http.StatusCreated,
			Location: reply.Location,
			Body: CadenceCreateReply{
				CadenceID: reply.CadenceID,
				Name:      reply.Name,
				Enabled:   reply.Enabled,
				NextRunAt: reply.NextRunAt,
				Location:  reply.Location,
			},
		}, nil
	})
}

// registerHumaCadencePatch монтирует PATCH /v1/cadences/{id} через huma на chi-группе
// /v1/cadences (WRITE-SELF-AUDIT: cadence.updated пишет САМ handler внутри PatchTyped,
// audit-middleware НЕ навешан — отличие от middleware-audit-доменов role/operator).
// cadenceH nil → no-op. Handler: claims → конверт typed-body → PatchTyped (read-modify-
// write + self-audit) → 200 С ТЕЛОМ (cadenceDTO). {id} биндит huma, ULID-валидация —
// доменная (в PatchTyped).
func registerHumaCadencePatch(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadencePatchOperation(), func(ctx context.Context, in *cadencePatchInput) (*cadencePatchOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, cadenceMissingClaims()
		}
		dto, err := cadenceH.PatchTyped(ctx, claims, in.ID, toCadencePatchRequest(in.Body))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadencePatchOutput{Body: dto}, nil
	})
}

// registerHumaCadenceDelete монтирует DELETE /v1/cadences/{id} через huma (WRITE-SELF-
// AUDIT: cadence.deleted пишет САМ handler внутри DeleteTyped). cadenceH nil → no-op.
// Handler: claims → DeleteTyped (delete + invalidation + self-audit) → пустой 204-output.
func registerHumaCadenceDelete(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceDeleteOperation(), func(ctx context.Context, in *cadenceDeleteInput) (*cadenceDeleteOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, cadenceMissingClaims()
		}
		if err := cadenceH.DeleteTyped(ctx, claims, in.ID); err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceDeleteOutput{Status: http.StatusNoContent}, nil
	})
}

// registerHumaCadenceEnable монтирует POST /v1/cadences/{id}/enable через huma
// (WRITE-SELF-AUDIT: cadence.updated пишет САМ handler внутри SetEnabledTyped).
func registerHumaCadenceEnable(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceEnableOperation(), func(ctx context.Context, in *cadenceToggleInput) (*cadenceToggleOutput, error) {
		return cadenceToggle(ctx, cadenceH, in.ID, true)
	})
}

// registerHumaCadenceDisable монтирует POST /v1/cadences/{id}/disable через huma
// (WRITE-SELF-AUDIT: cadence.updated пишет САМ handler).
func registerHumaCadenceDisable(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceDisableOperation(), func(ctx context.Context, in *cadenceToggleInput) (*cadenceToggleOutput, error) {
		return cadenceToggle(ctx, cadenceH, in.ID, false)
	})
}

// registerHumaCadenceList монтирует GET /v1/cadences (list) через huma на chi-группе
// /v1/cadences (READ-with-typed-query, БЕЗ audit). cadenceH nil → no-op. Handler:
// typed-query (enabled/kind enum + offset/limit int32) → ListTyped → typed envelope-
// output. RBAC cadence.list — на группе. Out-of-range pagination enforce-ит ДОМЕН
// (CheckPageBounds → 400), не huma min/max. Teardown T1: снимает последний live
// strict-mount (strictWrapper.ListCadences) в /v1.
func registerHumaCadenceList(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceListOperation(), func(ctx context.Context, in *cadenceListInput) (*cadenceListOutput, error) {
		reply, err := cadenceH.ListTyped(ctx, in.Enabled, in.Kind, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceListOutput{Body: reply}, nil
	})
}

// registerHumaCadenceGet монтирует GET /v1/cadences/{id} через huma (READ-with-path,
// БЕЗ audit). cadenceH nil → no-op. Handler: GetTyped(id) → typed 200-output (404/422/500
// через problem). RBAC cadence.list — на группе. Перенос завершает cadence-домен на huma
// и снимает блокер sibling-саброутера (см. cadenceGetOperation).
func registerHumaCadenceGet(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceGetOperation(), func(ctx context.Context, in *cadenceGetInput) (*cadenceGetOutput, error) {
		dto, err := cadenceH.GetTyped(ctx, in.ID)
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceGetOutput{Body: dto}, nil
	})
}

// registerHumaCadenceRuns монтирует GET /v1/cadences/{id}/runs через huma (READ-with-
// typed-query, БЕЗ audit). cadenceH nil → no-op. Handler: typed-query → RunsTyped →
// typed envelope-output. RBAC incarnation.history — на группе. CheckPageBounds → 400
// (диапазон enforce-ит ДОМЕН, не huma min/max).
func registerHumaCadenceRuns(humaAPI huma.API, cadenceH *handlers.CadenceHandler) {
	if cadenceH == nil {
		return
	}
	huma.Register(humaAPI, cadenceRunsOperation(), func(ctx context.Context, in *cadenceRunsInput) (*cadenceRunsOutput, error) {
		reply, err := cadenceH.RunsTyped(ctx, in.ID, in.Statuses, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, cadenceProblem(err)
		}
		return &cadenceRunsOutput{Body: reply}, nil
	})
}

// cadenceToggle — общая ветка enable/disable (SetEnabledTyped + 200-body). claims из
// ctx → SetEnabledTyped (self-audit) → cadenceToggleOutput.
func cadenceToggle(ctx context.Context, cadenceH *handlers.CadenceHandler, id string, enabled bool) (*cadenceToggleOutput, error) {
	claims, ok := apimiddleware.ClaimsFromContext(ctx)
	if !ok {
		return nil, cadenceMissingClaims()
	}
	reply, err := cadenceH.SetEnabledTyped(ctx, claims, id, enabled)
	if err != nil {
		return nil, cadenceProblem(err)
	}
	return &cadenceToggleOutput{Body: CadenceEnabledReply{CadenceID: reply.CadenceID, Enabled: reply.Enabled}}, nil
}

// cadenceMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func cadenceMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// toCadencePatchRequest — конверт typed huma-body PATCH → доменная модель
// handlers.CadencePatchRequest (поле-в-поле, FULL-TYPED §Pattern шаг 3). Указатели
// пробрасываются как есть (nil = «не трогать»); target конвертируется в доменный
// VoyageTargetRequest (nil → nil = «не трогать»). Доменный applyCadencePatch
// трактует присутствие так же, что декодит legacy (w,r)-Patch.
func toCadencePatchRequest(b CadencePatchRequest) handlers.CadencePatchRequest {
	req := handlers.CadencePatchRequest{
		Name:            b.Name,
		Enabled:         b.Enabled,
		ScheduleKind:    b.ScheduleKind,
		IntervalSeconds: b.IntervalSeconds,
		CronExpr:        b.CronExpr,
		OverlapPolicy:   b.OverlapPolicy,
		ScenarioName:    b.ScenarioName,
		Module:          b.Module,
		Input:           b.Input,
		Batch:           b.Batch,
		BatchSize:       b.BatchSize,
		BatchPercent:    b.BatchPercent,
		Concurrency:     b.Concurrency,
		BatchMode:       b.BatchMode,
		MaxFailures:     b.MaxFailures,
		FailThreshold:   b.FailThreshold,
		RequireAlive:    b.RequireAlive,
		OnFailure:       b.OnFailure,
	}
	if b.Target != nil {
		req.Target = &handlers.VoyageTargetRequest{
			Incarnations: b.Target.Incarnations,
			Service:      b.Target.Service,
			SIDs:         b.Target.SIDs,
			Where:        b.Target.Where,
			Coven:        b.Target.Coven,
		}
	}
	return req
}

// toCadenceCreateRequest — конверт typed huma-body → доменная модель (поле-в-поле,
// FULL-TYPED §Pattern шаг 3). Тонкий клей; форма доменной модели — handlers.
// CadenceCreateRequest (алиас доменного типа, та же, что декодит legacy Create).
func toCadenceCreateRequest(b CadenceCreateRequest) handlers.CadenceCreateRequest {
	return handlers.CadenceCreateRequest{
		Name:            b.Name,
		Enabled:         b.Enabled,
		ScheduleKind:    b.ScheduleKind,
		IntervalSeconds: b.IntervalSeconds,
		CronExpr:        b.CronExpr,
		OverlapPolicy:   b.OverlapPolicy,
		Kind:            b.Kind,
		ScenarioName:    b.ScenarioName,
		Module:          b.Module,
		Input:           b.Input,
		Target: &handlers.VoyageTargetRequest{
			Incarnations: b.Target.Incarnations,
			Service:      b.Target.Service,
			SIDs:         b.Target.SIDs,
			Where:        b.Target.Where,
			Coven:        b.Target.Coven,
		},
		Batch:                b.Batch,
		BatchSize:            b.BatchSize,
		BatchPercent:         b.BatchPercent,
		Concurrency:          b.Concurrency,
		BatchMode:            b.BatchMode,
		MaxFailures:          b.MaxFailures,
		FailThreshold:        b.FailThreshold,
		InterBatchIntervalMS: b.InterBatchIntervalMS,
		InterUnitIntervalMS:  b.InterUnitIntervalMS,
		RequireAlive:         b.RequireAlive,
		OnFailure:            b.OnFailure,
		Notify:               toNotifyRequests(b.Notify),
	}
}

// toNotifyRequests конвертит huma notify[] в доменную форму (parity §Pattern).
// nil/пусто → nil (без уведомлений).
func toNotifyRequests(in []VoyageNotify) []handlers.VoyageNotifyRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]handlers.VoyageNotifyRequest, len(in))
	for i, n := range in {
		out[i] = handlers.VoyageNotifyRequest{
			Herald:       n.Herald,
			On:           n.On,
			OnlyFailures: derefBool(n.OnlyFailures),
			OnlyChanges:  derefBool(n.OnlyChanges),
			Annotations:  marshalAnnotations(n.Annotations),
			Projection:   n.Projection,
		}
	}
	return out
}

// derefBool — *bool → bool (nil → false). Доменный VoyageNotifyRequest несёт bool
// (omitempty), huma-форма — *bool (различить «не задано» в схеме); семантика та же
// (отсутствие = false).
func derefBool(p *bool) bool {
	return p != nil && *p
}

// cadenceProblem доставляет ошибку CreateTyped через huma как problem+json. Доменная
// *handlers.problemError → humaProblemError (его Details, статус из таблицы). Не-
// problem (нештатный путь) → 500 internal.
func cadenceProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaCadenceAPI собирает huma.API поверх chi-группы. OpenAPIPath/DocsPath/
// SchemasPath пусты — huma НЕ монтирует свои spec/docs/schemas-роуты (спеку отдаёт
// servedOpenAPIHandler). installHumaErrorOverride здесь НЕ вызывается — он выполняется
// ОДНИМ явным вызовом при buildRouter (единая точка тиража). Тиражируемая часть:
// одна huma.API на chi-группу с идентичной навеской.
func newHumaCadenceAPI(r chi.Router) huma.API {
	cfg := huma.DefaultConfig("Soul Stack Keeper Operator API", "v1")
	cfg.OpenAPIPath = "" // не монтировать huma-spec-роут (спеку отдаёт servedOpenAPIHandler)
	cfg.DocsPath = ""    // не монтировать huma-docs
	cfg.SchemasPath = "" // не монтировать huma-schemas
	// Снимаем SchemaLinkTransformer (huma-дефолт через CreateHooks): он добавляет в
	// тело ответа поле "$schema" + Link-заголовок (JSON-Schema-self-describe). Это
	// wire-CHANGE против legacy oapi-reply (его там нет) — golden-JSON-guard ловит.
	// Для тиража: единый «голый» конверт без huma-украшений тела. CreateHooks=nil
	// также убирает прочие дефолтные хуки (нам не нужны — спеку/доки huma не монтирует).
	cfg.CreateHooks = nil
	api := humachi.New(r, cfg)
	// INCARNATION (handler-native T5d): enum IncarnationStatus — native SchemaProvider-тип
	// (huma_enums.go/huma_incarnation_status.go), несётся reply-Body напрямую → отдельный
	// alias IncarnationStatus → native более НЕ нужен (нет oapi-status-поля в Body).
	// VOYAGE/CADENCE (handler-native T5d): OUTPUT-структуры (Voyage.target — native
	// api.VoyageTarget, CadenceDTO.target — json.RawMessage) более НЕ тянут генерёный
	// VoyageTarget → прежний alias aliasVoyageTarget удалён (см. huma_voyage_target.go).
	// Глобальный alias: generic sharedapi.PagedResponse[<incarnation-element>] → named-
	// struct envelope с контрактным именем/формой (IncarnationListReply/IncarnationHistory-
	// Reply). Меняет только OpenAPI-схему list/history-Body, не wire-тело (те же json-поля).
	// См. huma_incarnation_envelope.go.
	registerIncarnationEnvelopes(api)
	// Тираж-батч N1 (operator/service): generic PagedResponse[Operator] → OperatorList-
	// Reply (см. huma_operator_envelope.go); handlers.ServiceScenariosReply → ServiceScenarios-
	// ListReply (см. huma_service_envelope.go). Меняют только OpenAPI-имя/форму Body, не wire.
	registerOperatorEnvelopes(api)
	registerServiceEnvelopes(api)
	// Тираж-батч N4: generic PagedResponse[Voyage] (cadence runs-Body) →
	// VoyageListReply (см. huma_cadence_envelope.go). Сводит runs-response на ту же
	// named-схему VoyageListReply, что voyage list (рукопись :2378). Только OpenAPI-имя/
	// форма Body, не wire.
	registerCadenceEnvelopes(api)
	// SOUL (handler-native T5d): enum SoulStatus/SoulTransport — native SchemaProvider-типы
	// (huma_soul_status.go), несутся reply-Body напрямую → отдельный alias не нужен. generic
	// PagedResponse[handlers.SoulListView] → soulListReply (CURSOR, 6-полей форма SoulListReply,
	// см. huma_soul_envelope.go). nested SoulSshTarget — native input↔output (КЛАСС A). Только
	// OpenAPI-схемы, не wire.
	registerSoulEnvelopes(api)
	// Class C доэмиссия: typed-схема SoulprintFacts (+ 6 под-схем) для GET soulprint
	// (typed_facts=json.RawMessage не выводит вложенные типы reflect-обходом; alias на
	// typed *SoulprintFacts эмитит их) + ErrandAccepted (202-тело exec/errand-get
	// маршалится через json.RawMessage → схема не эмитилась). Только OpenAPI, не wire.
	// См. huma_soul_soulprint.go / huma_errand_accepted.go.
	registerSoulprintFacts(api)
	registerErrandAccepted(api)
	return api
}

// HumaCadenceSpecYAML собирает OpenAPI-фрагмент мигрированных-на-huma cadence-роутов
// (pilot-1 — только createCadence) как YAML-строку, БЕЗ монтирования на реальный
// router. Хук для спека-мерж-таргета тиража и guard/golden-тестов. Делегирует
// generic [humaDumpSpec], регистрируя операцию через тот же registerHumaCadence
// (единый register-путь — нет дубля dump-vs-mount): handler-заглушка при dump не
// вызывается. Возвращает 3.1.0-спеку (huma-дефолт).
func HumaCadenceSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.CadenceSpecStub()
		registerHumaCadence(api, stub)
		registerHumaCadenceList(api, stub)
		registerHumaCadenceGet(api, stub)
		registerHumaCadenceRuns(api, stub)
		registerHumaCadencePatch(api, stub)
		registerHumaCadenceDelete(api, stub)
		registerHumaCadenceEnable(api, stub)
		registerHumaCadenceDisable(api, stub)
		return nil
	})
}

// marshalAnnotations — JSON-сериализация annotations huma-формы в доменную
// RawMessage (доменный prepareNotifyErr валидирует object-форму). nil → nil.
func marshalAnnotations(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	// Безопасно: map[string]any всегда сериализуется; ошибка невозможна.
	b, _ := json.Marshal(m)
	return b
}
