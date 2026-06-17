// Module-form-prep-handler Operator API (`POST /v1/modules/{name}/form-prep`) —
// общий резолвер source-каталогов для UI-формы модуля (ADR-045 S3). UI строит
// форму Run→Command по schema из GET /v1/modules/{name} (S2); поля с
// `input.source` (incarnation_hosts / choir) нуждаются в живом списке SID-ов для
// автокомплита. Этот эндпоинт — единственный резолвер таких source-каталогов.
//
// Cluster-aware: SID-ы берутся из `souls` (registry всего кластера), а не из
// scope одного запроса. Prefix-фильтр + жёсткий cap (флот до 100k — отдавать
// весь список нельзя): сначала сужаем по prefix, потом обрезаем по cap и
// сигналим truncated.
//
// RBAC — incarnation.run (эндпоинт обслуживает подготовку прогона Run→Command:
// кто запускает прогон, тот и резолвит SID-ы под его поля). Новая permission не
// заводится (reuse под-прогонной permission, паттерн module-catalog →
// service.list).
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// formPrepSIDCap — верхний предел числа SID-ов в одном ответе (DoS-guard, флот
// 100k). Резолвер тянет на один больше, чтобы отличить «ровно cap» от «есть ещё»
// (truncated). UI-автокомплит сужает выдачу prefix-ом; cap страхует пустой
// prefix на большой инкарнации.
const formPrepSIDCap = 50

// FormPrepInput — NATIVE request-форма POST /v1/modules/{name}/form-prep (handler-native
// T5d-2c-full). Заменяет ModuleFormPrepRequest: huma-input (пакет api) биндит/валидирует
// тело и проецирует его в эти поля. Source — дискриминатор (ровно один непустой вариант,
// XOR проверяет handler → 422); Prefix — опц. LIKE-prefix.
type FormPrepInput struct {
	Source FormPrepSourceInput
	Prefix string
}

// FormPrepSourceInput — NATIVE source-дискриминатор: incarnation_hosts (имя incarnation)
// XOR choir (координаты Choir-source). Пустые поля = «не задан» (паритет omitempty).
type FormPrepSourceInput struct {
	IncarnationHosts string
	Choir            *FormPrepChoirSource
}

// FormPrepChoirSource — координаты Choir-source: incarnation + имя Choir-а. Используется
// и во внутреннем [FormPrepFilter] (передача в SQL-резолвер), и как под-объект source.
type FormPrepChoirSource struct {
	Incarnation string
	Name        string
}

// FormPrepResult — NATIVE результат POST /v1/modules/{name}/form-prep (handler-native).
// Отсортированный slice SID-ов (non-nil) + флаг обрезки по cap. Пакет api проецирует его
// в native-схему ModuleFormPrepReply (register-func huma_module.go).
type FormPrepResult struct {
	Sids      []string
	Truncated bool
}

// FormPrepFilter — резолвленный source для [FormPrepSIDResolver]. Ровно одно из
// IncarnationHosts / Choir непусто (handler гарантирует). Prefix опционален.
// Внутренний доменный тип (не wire): handler сводит к нему дискриминатор source в плоскую
// форму для резолвера.
type FormPrepFilter struct {
	IncarnationHosts string
	Choir            *FormPrepChoirSource
	Prefix           string
}

// FormPrepSIDResolver — резолв source-каталога формы в живые SID-ы. Возвращает
// отсортированный (по SID) slice ≤ cap и truncated=true, если упёрся в cap.
type FormPrepSIDResolver interface {
	ResolveSIDs(ctx context.Context, filter FormPrepFilter) (sids []string, truncated bool, err error)
}

// ModuleFormPrepHandler — `POST /v1/modules/{name}/form-prep`.
//
// {name} в пути сейчас не используется при резолве (source-каталог не зависит от
// модуля), но остаётся в контракте: форма строится per-module, и эндпоинт
// логически принадлежит модулю. Зависимости immutable; safe for concurrent use.
type ModuleFormPrepHandler struct {
	resolver FormPrepSIDResolver
	logger   *slog.Logger
}

// NewModuleFormPrepHandler создаёт handler. resolver обязателен для
// production-маршрута (router монтирует роут только при non-nil handler).
// logger nil → io.Discard.
func NewModuleFormPrepHandler(resolver FormPrepSIDResolver, logger *slog.Logger) *ModuleFormPrepHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ModuleFormPrepHandler{resolver: resolver, logger: logger}
}

// ModuleFormPrepSpecStub — непустой *ModuleFormPrepHandler-заглушка для генерации
// huma-OpenAPI-фрагмента (parity [RoleSpecStub]). resolver nil — handler в
// spec-режиме не исполняется.
func ModuleFormPrepSpecStub() *ModuleFormPrepHandler {
	return &ModuleFormPrepHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// FormPrepTyped — доменная функция `POST /v1/modules/{name}/form-prep` (handler-native):
// резолв source-каталога без http-границы. req — native request-форма (huma пакет api
// биндит/валидирует тело и проецирует в неё; huma отбивает unknown → 400 до вызова).
// Ошибки — *problemError (422 невалидный source / 500 сбой резолва); успех —
// [FormPrepResult] (sids non-nil, отсортирован).
func (h *ModuleFormPrepHandler) FormPrepTyped(ctx context.Context, req FormPrepInput) (FormPrepResult, error) {
	filter, perr := toFilter(req)
	if perr != "" {
		return FormPrepResult{}, &problemError{problem.New(problem.TypeValidationFailed, "", perr)}
	}
	sids, truncated, err := h.resolver.ResolveSIDs(ctx, filter)
	if err != nil {
		h.logger.Error("module.form-prep: resolve sids failed", slog.Any("error", err))
		return FormPrepResult{}, &problemError{problem.New(problem.TypeInternalError, "", "resolve form source failed")}
	}
	if sids == nil {
		sids = []string{}
	}
	return FormPrepResult{Sids: sids, Truncated: truncated}, nil
}

// toFilter валидирует source (ровно один непустой вариант) и собирает [FormPrepFilter].
// Возвращает текст ошибки валидации (пустой → ок). source-поля — value (huma пакет api
// уже спроецировал pointer-optional в плоскую native-форму).
func toFilter(req FormPrepInput) (FormPrepFilter, string) {
	inc := req.Source.IncarnationHosts
	prefix := req.Prefix
	hasInc := inc != ""
	hasChoir := req.Source.Choir != nil

	switch {
	case hasInc && hasChoir:
		return FormPrepFilter{}, "source must specify exactly one of incarnation_hosts/choir"
	case hasInc:
		if !incarnation.ValidName(inc) {
			return FormPrepFilter{}, "invalid incarnation name"
		}
		return FormPrepFilter{IncarnationHosts: inc, Prefix: prefix}, ""
	case hasChoir:
		c := req.Source.Choir
		if c.Incarnation == "" || c.Name == "" {
			return FormPrepFilter{}, "choir source requires incarnation and name"
		}
		if !incarnation.ValidName(c.Incarnation) {
			return FormPrepFilter{}, "invalid incarnation name"
		}
		return FormPrepFilter{Choir: c, Prefix: prefix}, ""
	default:
		return FormPrepFilter{}, "source must specify one of incarnation_hosts/choir"
	}
}

// --- production-реализация поверх pgxpool.Pool ---

// FormPrepPGResolver — production-реализация [FormPrepSIDResolver] поверх
// `souls` / `incarnation_choir_voices`. Cluster-wide резолв source → SID[].
// Presence-фильтр — `souls.status IN ('connected','dormant')` (paritet
// [VoyageCommandPGResolver]: лёгкий SQL-снимок без Redis-lease — автокомплит
// формы не несёт security-инварианта прогона, точность presence здесь не
// критична).
type FormPrepPGResolver struct {
	db voyageResolverDB
}

// NewFormPrepPGResolver конструирует resolver. db обязателен.
func NewFormPrepPGResolver(db voyageResolverDB) *FormPrepPGResolver {
	return &FormPrepPGResolver{db: db}
}

// incarnationHostsSQL — живые SID-ы хостов incarnation: souls с Coven-меткой
// `$1 = ANY(coven)` (ADR-008: incarnation.name — корневая Coven-метка),
// online-снимок, опц. prefix-фильтр ($2 = ” → без фильтра), cap+1 ($3) для
// детекта truncated. ORDER BY sid — детерминизм + стабильный автокомплит.
const formPrepIncarnationHostsSQL = `
SELECT sid FROM souls
WHERE $1 = ANY(coven)
  AND status IN ('connected', 'dormant')
  AND ($2 = '' OR sid LIKE $2 || '%')
ORDER BY sid ASC
LIMIT $3
`

// choirVoicesSQL — живые SID-ы Voice-ов Choir-а: join souls с
// incarnation_choir_voices по (incarnation_name, choir_name) (ADR-044).
// Cross-incarnation isolation — фильтр по incarnation_name. Presence/prefix/cap —
// как в incarnationHostsSQL.
const formPrepChoirVoicesSQL = `
SELECT s.sid FROM souls s
JOIN incarnation_choir_voices v ON v.sid = s.sid
WHERE v.incarnation_name = $1
  AND v.choir_name = $2
  AND s.status IN ('connected', 'dormant')
  AND ($3 = '' OR s.sid LIKE $3 || '%')
ORDER BY s.sid ASC
LIMIT $4
`

// ResolveSIDs резолвит source → ≤ cap отсортированных SID-ов + truncated.
// Тянет cap+1 строк: если пришло > cap — обрезаем до cap и truncated=true.
func (r *FormPrepPGResolver) ResolveSIDs(ctx context.Context, filter FormPrepFilter) ([]string, bool, error) {
	const limit = formPrepSIDCap + 1

	var (
		rows pgx.Rows
		err  error
	)
	if filter.Choir != nil {
		rows, err = r.db.Query(ctx, formPrepChoirVoicesSQL,
			filter.Choir.Incarnation, filter.Choir.Name, filter.Prefix, limit)
	} else {
		rows, err = r.db.Query(ctx, formPrepIncarnationHostsSQL,
			filter.IncarnationHosts, filter.Prefix, limit)
	}
	if err != nil {
		return nil, false, errors.Join(errors.New("form-prep resolver: query souls"), err)
	}
	defer rows.Close()

	out := make([]string, 0, formPrepSIDCap)
	truncated := false
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, false, errors.Join(errors.New("form-prep resolver: scan"), err)
		}
		if len(out) == formPrepSIDCap {
			truncated = true // пришла (cap+1)-я строка → есть ещё.
			break
		}
		out = append(out, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, false, errors.Join(errors.New("form-prep resolver: iter"), err)
	}
	return out, truncated, nil
}
