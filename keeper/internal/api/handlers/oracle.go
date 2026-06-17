// Operator API handler-ы реестров Oracle (Vigil — Soul-side проверка, Decree —
// правило reactor; ADR-030, beacons S3). Тот же [oracle.Service] вызывает
// MCP-tool-handler (keeper.oracle.vigil.* / keeper.oracle.decree.*), один источник
// правды.
//
// T5d-2c (handler-native): домен oracle отвязан от legacy-генерата. *Typed-функции
// принимают NATIVE request-типы (handlers.VigilCreateInput / DecreeCreateInput;
// huma-input в пакете api биндит и валидирует тело по этим полям) и возвращают
// доменные result-ы с ПЛОСКИМИ wire-полями (handlers.VigilView / DecreeView) — НЕ
// legacy-генерата-Body. Native wire-DTO (схему OpenAPI) строит пакет api из этих полей
// (register-func huma_oracle.go), oapi-генерёные типы в oracle-домене не участвуют.
// (w,r)-оболочки сняты: HTTP обслуживает huma full-typed, MCP зовёт oracle.Service
// напрямую (мимо handler — Service-direct, не httptest).
//
// Бизнес-логика (валидация name/interval/check/субъект для Vigil; name/on_beacon/
// incarnation_name/scenario/субъект/where-CEL для Decree) — в [oracle.Service];
// handler делает path/query-валидацию и маппит sentinel-ы в RFC 7807. RBAC — в
// middleware (router.go).
//
// БЕЗОПАСНОСТЬ: params Vigil-а / action_input Decree-а — конфигурация проверки/
// сценария, не секрет; vault-ref в action_input едет КАК ЕСТЬ (инвариант A
// ADR-027), значения секретов через этот path не проходят.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// reOracleName — формат path-сегмента {name} Vigil / Decree (kebab 1..63,
// oracle.NamePattern). Path-сегмент без слешей/`..` — безопасен от traversal.
var reOracleName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// OracleHandler — REST-эндпоинты реестров Oracle (vigils + decrees). Делегирует
// бизнес-логику в [oracle.Service]. Все зависимости immutable; safe for
// concurrent use.
type OracleHandler struct {
	svc    *oracle.Service
	logger *slog.Logger
}

// NewOracleHandler создаёт handler. svc обязателен (паника при nil —
// единственная точка misconfiguration; caller обязан передать non-nil).
func NewOracleHandler(svc *oracle.Service, logger *slog.Logger) *OracleHandler {
	if svc == nil {
		panic("handlers.NewOracleHandler: oracle.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &OracleHandler{svc: svc, logger: logger}
}

// OracleSpecStub — непустой *OracleHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaOracleSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [AugurSpecStub]).
func OracleSpecStub() *OracleHandler {
	return &OracleHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Vigil ------------------------------------------------------------

// VigilView — ПЛОСКАЯ wire-форма Vigil-а (create-201 / list-item / get-200),
// handler-native. Coven — `*[]string` (nil при пустом, паритет omitempty); SID/
// CreatedByAID — *string nullable (nil → ключ опущен). params — byte-passthrough
// JSONB ([json.RawMessage], ADR-051 категория D): сырые байты отдаются as-is, БЕЗ
// unmarshal→map→marshal (re-marshal переупорядочил бы ключи). created_at/updated_at —
// UTC + Truncate(Second) (фиксируется здесь, как в эталоне oracle (w,r)).
type VigilView struct {
	Name         string
	Coven        *[]string
	SID          *string
	Interval     string
	Check        string
	Params       json.RawMessage
	Enabled      bool
	CreatedByAID *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func toVigilView(v *oracle.Vigil) VigilView {
	params := v.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return VigilView{
		Name:         v.Name,
		Coven:        slicePtrIfNotEmpty(v.Coven),
		SID:          v.SID,
		Interval:     v.IntervalSpec,
		Check:        v.CheckAddr,
		Params:       params,
		Enabled:      v.Enabled,
		CreatedByAID: v.CreatedByAID,
		CreatedAt:    v.CreatedAt.UTC().Truncate(time.Second),
		UpdatedAt:    v.UpdatedAt.UTC().Truncate(time.Second),
	}
}

// VigilCreateInput — NATIVE request-форма POST /v1/vigils (handler-native).
// Заменяет VigilCreateRequest: subject — XOR coven/sid; params —
// `json.RawMessage` (byte-passthrough JSONB, ADR-051 категория D); enabled —
// pointer-optional (опущено → true). XOR-субъект / форма interval/check/params
// валидирует service.
type VigilCreateInput struct {
	Name     string
	Coven    *[]string
	SID      *string
	Interval string
	Check    string
	Params   *json.RawMessage
	Enabled  *bool
}

// VigilCreateReply — извлечённый результат [OracleHandler.CreateVigilTyped]
// (handler-native). Несёт плоский 201-вид (View) + check/interval/subject + caller
// AID (для audit-payload; params в audit НЕ кладётся).
type VigilCreateReply struct {
	View      VigilView
	Check     string
	Interval  string
	Subject   string
	CallerAID string
}

// AuditPayload собирает audit-payload vigil.create-роута (parity легаси:
// name/check/interval/subject/created_by_aid; params НЕ кладётся).
func (r VigilCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.View.Name,
		"check":          r.Check,
		"interval":       r.Interval,
		"subject":        r.Subject,
		"created_by_aid": r.CallerAID,
	}
}

// CreateVigilTyped — доменная функция POST /v1/vigils (handler-native):
// svc.CreateVigil + sentinel→problem. params — byte-passthrough JSONB (ADR-051
// категория D). Ошибки — *problemError; успех — [VigilCreateReply] (плоский 201-вид
// + audit-поля).
func (h *OracleHandler) CreateVigilTyped(ctx context.Context, claims *keeperjwt.Claims, req VigilCreateInput) (VigilCreateReply, error) {
	var zero VigilCreateReply
	callerAID := claims.Subject
	v, err := h.svc.CreateVigil(ctx, oracle.CreateVigilInput{
		Name:      req.Name,
		Coven:     derefStrings(req.Coven),
		SID:       req.SID,
		Interval:  req.Interval,
		Check:     req.Check,
		Params:    derefRawMessage(req.Params),
		Enabled:   enabledOrDefault(req.Enabled),
		CallerAID: &callerAID,
	})
	if err != nil {
		return zero, h.vigilError("oracle.vigil.create", req.Name, callerAID, err)
	}
	return VigilCreateReply{
		View:      toVigilView(v),
		Check:     v.CheckAddr,
		Interval:  v.IntervalSpec,
		Subject:   vigilSubject(v),
		CallerAID: callerAID,
	}, nil
}

// VigilListPage — доменный paged-результат GET /v1/vigils (handler-native). Плоские
// offset/limit/total + срез VigilView; пакет api проецирует в native envelope
// VigilListReply.
type VigilListPage struct {
	Items  []VigilView
	Offset int
	Limit  int
	Total  int
}

// ListVigilsTyped — доменная функция GET /v1/vigils (handler-native, read-with-
// typed-query, БЕЗ audit). offset/limit приходят уже провалидированными (huma-bind
// int32); диапазон enforce-ит CheckPageBounds → 400. Ошибка чтения → *problemError
// (500).
func (h *OracleHandler) ListVigilsTyped(ctx context.Context, offset, limit int) (VigilListPage, error) {
	var zero VigilListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	vigils, total, err := h.svc.ListVigils(ctx, offset, limit)
	if err != nil {
		h.logger.Error("oracle.vigil.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list vigils failed")}
	}

	items := make([]VigilView, 0, len(vigils))
	for _, v := range vigils {
		items = append(items, toVigilView(v))
	}
	return VigilListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetVigilTyped — доменная функция GET /v1/vigils/{name} (handler-native, read-with-
// path, БЕЗ audit): валидация path-name + svc.GetVigil + sentinel→problem
// (404/422/500). Ошибки — *problemError; успех — [VigilView].
func (h *OracleHandler) GetVigilTyped(ctx context.Context, name string) (VigilView, error) {
	var zero VigilView
	if !reOracleName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOracleName.String())}
	}
	v, err := h.svc.GetVigil(ctx, name)
	switch {
	case err == nil:
		return toVigilView(v), nil
	case errors.Is(err, oracle.ErrVigilNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "vigil "+name+" not found")}
	default:
		h.logger.Error("oracle.vigil.get: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get vigil failed")}
	}
}

// VigilDeleteReply — извлечённый результат [OracleHandler.DeleteVigilTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type VigilDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload vigil.delete-роута (parity легаси: name).
func (r VigilDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteVigilTyped — доменная функция DELETE /v1/vigils/{name} (handler-native):
// валидация path-name + svc.DeleteVigil + sentinel→problem. Ошибки — *problemError;
// успех — [VigilDeleteReply].
func (h *OracleHandler) DeleteVigilTyped(ctx context.Context, name string) (VigilDeleteReply, error) {
	var zero VigilDeleteReply
	if !reOracleName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOracleName.String())}
	}
	err := h.svc.DeleteVigil(ctx, name)
	switch {
	case err == nil:
		return VigilDeleteReply{Name: name}, nil
	case errors.Is(err, oracle.ErrVigilNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "vigil "+name+" not found")}
	default:
		h.logger.Error("oracle.vigil.delete: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete vigil failed")}
	}
}

// vigilError маппит sentinel-ы [oracle.Service] (Vigil create) в *problemError:
//   - ErrValidation         → validation-failed (422).
//   - ErrVigilAlreadyExists  → vigil-already-exists (409).
func (h *OracleHandler) vigilError(op, name, callerAID string, err error) error {
	switch {
	case errors.Is(err, oracle.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, oracle.ErrVigilAlreadyExists):
		return &problemError{problem.New(problem.TypeVigilExists, "", "vigil "+name+" already exists")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("name", name), slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// --- Decree -----------------------------------------------------------

// DecreeView — ПЛОСКАЯ wire-форма Decree-а (create-201 / list-item / get-200),
// handler-native. Coven — `*[]string` (nil при пустом); Where/SID/CreatedByAID —
// *string nullable (nil → ключ опущен). action_input — byte-passthrough JSONB
// ([json.RawMessage], ADR-051 категория D): сырые байты отдаются as-is. created_at/
// updated_at — UTC + Truncate(Second).
type DecreeView struct {
	Name            string
	OnBeacon        string
	Where           *string
	Coven           *[]string
	SID             *string
	IncarnationName string
	ActionScenario  string
	ActionInput     json.RawMessage
	Cooldown        string
	Enabled         bool
	CreatedByAID    *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func toDecreeView(d *oracle.Decree) DecreeView {
	input := d.ActionInput
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return DecreeView{
		Name:            d.Name,
		OnBeacon:        d.OnBeacon,
		Where:           d.WhereCEL,
		Coven:           slicePtrIfNotEmpty(d.SubjectCoven),
		SID:             d.SubjectSID,
		IncarnationName: d.IncarnationName,
		ActionScenario:  d.ActionScenario,
		ActionInput:     input,
		Cooldown:        d.Cooldown,
		Enabled:         d.Enabled,
		CreatedByAID:    d.CreatedByAID,
		CreatedAt:       d.CreatedAt.UTC().Truncate(time.Second),
		UpdatedAt:       d.UpdatedAt.UTC().Truncate(time.Second),
	}
}

// DecreeCreateInput — NATIVE request-форма POST /v1/decrees (handler-native).
// Заменяет DecreeCreateRequest: subject — XOR coven/sid; action_input —
// `json.RawMessage` (byte-passthrough JSONB, ADR-051 категория D); cooldown/enabled —
// pointer-optional (enabled опущено → true). XOR-субъект / where-CEL / cooldown
// валидирует service.
type DecreeCreateInput struct {
	Name            string
	OnBeacon        string
	Coven           *[]string
	SID             *string
	IncarnationName string
	ActionScenario  string
	ActionInput     *json.RawMessage
	Where           *string
	Cooldown        *string
	Enabled         *bool
}

// DecreeCreateReply — извлечённый результат [OracleHandler.CreateDecreeTyped]
// (handler-native). Несёт плоский 201-вид (View) + субъект и caller AID (для
// audit-payload; where-CEL и action_input в audit НЕ кладутся — action_input может
// транзитом нести vault-ref).
type DecreeCreateReply struct {
	View      DecreeView
	Subject   string
	CallerAID string
}

// AuditPayload собирает audit-payload decree.create-роута (parity легаси:
// name/on_beacon/incarnation/action_scenario/subject/created_by_aid; where-CEL и
// action_input НЕ кладутся).
func (r DecreeCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":            r.View.Name,
		"on_beacon":       r.View.OnBeacon,
		"incarnation":     r.View.IncarnationName,
		"action_scenario": r.View.ActionScenario,
		"subject":         r.Subject,
		"created_by_aid":  r.CallerAID,
	}
}

// CreateDecreeTyped — доменная функция POST /v1/decrees (handler-native):
// svc.CreateDecree + sentinel→problem. action_input — byte-passthrough JSONB
// (ADR-051 категория D), едет в service напрямую. Ошибки — *problemError; успех —
// [DecreeCreateReply] (плоский 201-вид + audit-поля).
func (h *OracleHandler) CreateDecreeTyped(ctx context.Context, claims *keeperjwt.Claims, req DecreeCreateInput) (DecreeCreateReply, error) {
	var zero DecreeCreateReply
	callerAID := claims.Subject
	d, err := h.svc.CreateDecree(ctx, oracle.CreateDecreeInput{
		Name:            req.Name,
		OnBeacon:        req.OnBeacon,
		WhereCEL:        req.Where,
		Coven:           derefStrings(req.Coven),
		SID:             req.SID,
		IncarnationName: req.IncarnationName,
		ActionScenario:  req.ActionScenario,
		ActionInput:     derefRawMessage(req.ActionInput),
		Cooldown:        derefString(req.Cooldown),
		Enabled:         enabledOrDefault(req.Enabled),
		CallerAID:       &callerAID,
	})
	if err != nil {
		return zero, h.decreeError("oracle.decree.create", req.Name, callerAID, err)
	}
	return DecreeCreateReply{View: toDecreeView(d), Subject: decreeSubject(d), CallerAID: callerAID}, nil
}

// DecreeListPage — доменный paged-результат GET /v1/decrees (handler-native). Пакет
// api проецирует в native envelope DecreeListReply.
type DecreeListPage struct {
	Items  []DecreeView
	Offset int
	Limit  int
	Total  int
}

// ListDecreesTyped — доменная функция GET /v1/decrees (handler-native, read-with-
// typed-query, БЕЗ audit). offset/limit приходят уже провалидированными (huma-bind
// int32); диапазон enforce-ит CheckPageBounds → 400. Ошибка чтения → *problemError
// (500).
func (h *OracleHandler) ListDecreesTyped(ctx context.Context, offset, limit int) (DecreeListPage, error) {
	var zero DecreeListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	decrees, total, err := h.svc.ListDecrees(ctx, offset, limit)
	if err != nil {
		h.logger.Error("oracle.decree.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list decrees failed")}
	}

	items := make([]DecreeView, 0, len(decrees))
	for _, d := range decrees {
		items = append(items, toDecreeView(d))
	}
	return DecreeListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetDecreeTyped — доменная функция GET /v1/decrees/{name} (handler-native, read-
// with-path, БЕЗ audit): валидация path-name + svc.GetDecree + sentinel→problem
// (404/422/500). Ошибки — *problemError; успех — [DecreeView].
func (h *OracleHandler) GetDecreeTyped(ctx context.Context, name string) (DecreeView, error) {
	var zero DecreeView
	if !reOracleName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOracleName.String())}
	}
	d, err := h.svc.GetDecree(ctx, name)
	switch {
	case err == nil:
		return toDecreeView(d), nil
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "decree "+name+" not found")}
	default:
		h.logger.Error("oracle.decree.get: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get decree failed")}
	}
}

// DecreeDeleteReply — извлечённый результат [OracleHandler.DeleteDecreeTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type DecreeDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload decree.delete-роута (parity легаси: name).
func (r DecreeDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteDecreeTyped — доменная функция DELETE /v1/decrees/{name} (handler-native):
// валидация path-name + svc.DeleteDecree + sentinel→problem (каскад чистит
// cooldown-state). Ошибки — *problemError; успех — [DecreeDeleteReply].
func (h *OracleHandler) DeleteDecreeTyped(ctx context.Context, name string) (DecreeDeleteReply, error) {
	var zero DecreeDeleteReply
	if !reOracleName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOracleName.String())}
	}
	err := h.svc.DeleteDecree(ctx, name)
	switch {
	case err == nil:
		return DecreeDeleteReply{Name: name}, nil
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "decree "+name+" not found")}
	default:
		h.logger.Error("oracle.decree.delete: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete decree failed")}
	}
}

// decreeError маппит sentinel-ы [oracle.Service] (Decree create) в *problemError:
//   - ErrValidation          → validation-failed (422).
//   - ErrDecreeAlreadyExists   → decree-already-exists (409).
func (h *OracleHandler) decreeError(op, name, callerAID string, err error) error {
	switch {
	case errors.Is(err, oracle.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, oracle.ErrDecreeAlreadyExists):
		return &problemError{problem.New(problem.TypeDecreeExists, "", "decree "+name+" already exists")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("name", name), slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// enabledOrDefault: опущенный `enabled` → true (активная проверка/правило,
// симметрично DEFAULT true в миграции 041).
func enabledOrDefault(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// derefRawMessage разыменовывает optional JSONB-поле из request-тела (huma даёт
// `*json.RawMessage` для omitempty params/action_input); nil → nil ([json.RawMessage]).
// Сырые байты НЕ копируются и НЕ переупорядочиваются (ADR-051 категория D,
// byte-passthrough); пустой JSONB нормализует в `{}` уже reply-граница (toVigilView/
// toDecreeView).
func derefRawMessage(p *json.RawMessage) json.RawMessage {
	if p == nil {
		return nil
	}
	return *p
}

// vigilSubject / decreeSubject — человекочитаемая форма субъекта для
// audit-payload (`coven=<v1,v2>` / `sid=<v>`). XOR гарантирован валидацией.
func vigilSubject(v *oracle.Vigil) string { return subjectLabel(v.Coven, v.SID) }

func decreeSubject(d *oracle.Decree) string { return subjectLabel(d.SubjectCoven, d.SubjectSID) }

func subjectLabel(coven []string, sid *string) string {
	if len(coven) > 0 {
		s := "coven="
		for i, c := range coven {
			if i > 0 {
				s += ","
			}
			s += c
		}
		return s
	}
	if sid != nil && *sid != "" {
		return "sid=" + *sid
	}
	return ""
}
