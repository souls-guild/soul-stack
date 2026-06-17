package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// prepareNotifyErr — валидация/авторизация блока notify ДО открытия tx (ADR-052(g)
// amendment N2; FULL-TYPED ADR-054 §Pattern, батч-2f self-audit) без http.ResponseWriter/
// *http.Request. Собирает шаблоны ephemeral-Tiding; voyage_id/name стемпятся позже в
// stampEphemeralTidings (после генерации voyage_id). nil notify → (nil, nil). store=nil
// → fail-closed 500. Делегирует общему ядру [prepareNotifyTidingsErr] (единый источник
// с Cadence-permanent путём); *problemError при отказе. ctx — request-context.
func (h *VoyageHandler) prepareNotifyErr(
	ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest, kind voyage.Kind,
) ([]herald.Tiding, error) {
	if len(req.Notify) == 0 {
		return nil, nil
	}
	if h.store == nil {
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"voyage orchestrator is not configured")}
	}
	tidings, perr := prepareNotifyTidingsErr(prepareNotifyDeps{
		store:    h.store,
		enforcer: h.enforcer,
		logName:  "voyage.notify",
		logger:   h.logger,
	}, ctx, claims, req.Notify, kind, notifyTidingShape{ephemeral: true})
	if perr != nil {
		return nil, &problemError{*perr}
	}
	return tidings, nil
}

// prepareNotifyDeps — зависимости общего [prepareNotifyTidingsErr], извлечённые из
// конкретного handler-а (Voyage/Cadence). store — herald-CRUD-пул (existence-
// чек канала), enforcer — RBAC (herald.read-guard), logName — префикс лога
// источника ("voyage.notify"/"cadence.notify").
type prepareNotifyDeps struct {
	store    herald.ExecQueryRower
	enforcer middleware.PermissionChecker
	logName  string
	logger   *slog.Logger
}

// notifyTidingShape — форма результирующего Tiding-а, различающая ephemeral
// (Voyage: разовое правило, привязка voyage_id ставится позже) от permanent
// (Cadence: постоянное правило с origin-маркером created_from_cadence_id и
// cadence-селектором, привязанными СРАЗУ по ULID расписания).
type notifyTidingShape struct {
	// ephemeral=true → Voyage-путь (Ephemeral=true, voyage_id/name стемпятся в
	// stampEphemeralTidings). false → Cadence-путь (постоянное правило).
	ephemeral bool
	// cadenceID — ULID расписания (cadences.id), привязка постоянного правила: в
	// Ephemeral=false-режиме проставляется И в Cadence-селектор (фильтр подписки
	// «слать только про прогоны этого расписания»), И в CreatedFromCadenceID
	// (origin-маркер для каскад-удаления). Пусто в ephemeral-режиме.
	cadenceID string
	// namePrefix — детерминированный префикс имени постоянного правила
	// (<cadence-name>-notify, уникальный суффикс добавляет caller). Пусто в
	// ephemeral-режиме (имя — eph-<ULID> в stampEphemeralTidings).
	namePrefix string
}

// prepareNotifyTidingsErr — общий валидатор/авторизатор/строитель notify-блока
// (ADR-052(g)/(m); FULL-TYPED ADR-054 §Pattern), единый для Voyage-ephemeral и
// Cadence-permanent путей. Чтобы форма/валидация/RBAC двух точек не разъехались, вся
// логика блока notify живёт здесь; различие ephemeral⟺permanent — в [notifyTidingShape]
// (caller выбирает). Без http.ResponseWriter/*http.Request — возвращает
// (*problem.Details) вместо problem.Write (вызывающий слой решает, как доставить:
// huma-конверт / (w,r)-оболочка); instance в Details пуст (caller проставит путь).
//
// Порядок проверок (fail-closed, security-критичный, идентичен для обоих путей):
//   - синтаксис (herald-name / on-enum / annotations-object / projection-paths)
//     → 422 ДО любого похода в БД;
//   - existence канала (несуществующий herald) → 422 (а не FK-500 при insert в tx);
//   - RBAC herald.read на КАЖДЫЙ канал (нельзя подписать уведомление на канал
//     без доступа, ADR-052(g)) → 403.
//
// kind определяет маппинг On→event_types (scenario_run.* / command_run.*).
func prepareNotifyTidingsErr(
	deps prepareNotifyDeps, ctx context.Context, claims *jwt.Claims,
	notify []voyageNotifyRequest, kind voyage.Kind, shape notifyTidingShape,
) ([]herald.Tiding, *problem.Details) {
	templates := make([]herald.Tiding, 0, len(notify))
	for i := range notify {
		n := &notify[i]
		idx := "notify[" + strconv.Itoa(i) + "]"

		if !herald.ValidName(n.Herald) {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".herald: имя "+n.Herald+" must match "+herald.NamePattern)
		}
		eventTypes, etErr := notifyEventTypes(kind, n.On)
		if etErr != "" {
			return nil, problemDetailsPtr(problem.TypeValidationFailed, idx+".on: "+etErr)
		}
		if err := herald.ValidateAnnotationsJSON(n.Annotations); err != nil {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".annotations: "+publicErr(err))
		}
		annotations := decodeAnnotations(n.Annotations)
		if err := herald.ValidateProjection(n.Projection); err != nil {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".projection: "+publicErr(err))
		}

		// Existence канала: несуществующий herald → 422 (а не FK-500 при insert в
		// tx). Тот же store-pool, что родительский CRUD (herald.ExecQueryRower ⊂
		// voyage/cadence.ExecQueryRower).
		if _, err := herald.SelectHeraldByName(ctx, deps.store, n.Herald); err != nil {
			if errors.Is(err, herald.ErrHeraldNotFound) {
				return nil, problemDetailsPtr(problem.TypeValidationFailed,
					idx+": herald "+n.Herald+" does not exist")
			}
			deps.logger.Error(deps.logName+": herald existence check failed", slog.Any("error", err))
			return nil, problemDetailsPtr(problem.TypeInternalError, "notify herald check failed")
		}

		// RBAC herald.read на канал (ADR-052(g)): нельзя подписать уведомление на
		// канал без доступа. bare-check (herald-каналы не scoped по контексту в MVP).
		if perr := checkHeraldReadPermissionErr(deps.enforcer, claims.Subject); perr != nil {
			return nil, perr
		}

		t := herald.Tiding{
			Herald:       n.Herald,
			EventTypes:   eventTypes,
			OnlyFailures: n.OnlyFailures,
			OnlyChanges:  n.OnlyChanges,
			Annotations:  annotations,
			Projection:   n.Projection,
			Enabled:      true,
			CreatedByAID: aidPtr(claims.Subject),
		}
		if shape.ephemeral {
			// Voyage-путь: разовое правило. voyage_id/name стемпятся позже
			// (stampEphemeralTidings), здесь только Ephemeral-флаг.
			t.Ephemeral = true
		} else {
			// Cadence-путь: постоянное правило, привязанное СРАЗУ по ULID расписания
			// (rename-safe). Cadence-селектор фильтрует подписку на прогоны ЭТОГО
			// расписания; CreatedFromCadenceID — origin-маркер каскад-удаления (ADR-052
			// §m / ADR-046 §9). Имя детерминированно-уникально (caller — addNotifyName).
			cadenceID := shape.cadenceID
			t.Cadence = &cadenceID
			t.CreatedFromCadenceID = &cadenceID
			t.Name = permanentNotifyName(shape.namePrefix, i)
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// problemDetailsPtr — хелпер: [problem.Details] на куче для возврата из *Err-
// функций (instance пуст — caller проставит). Сокращает шум &-литералов.
func problemDetailsPtr(typ, detail string) *problem.Details {
	d := problem.New(typ, "", detail)
	return &d
}

// checkHeraldReadPermissionErr — bare-check RBAC herald.read (ADR-052(g)), общий
// для Voyage/Cadence notify-путей (FULL-TYPED ADR-054 §Pattern). nil → разрешено;
// при deny — *problem.Details (revoked → TypeOperatorRevokedToken, no-perm → 403).
func checkHeraldReadPermissionErr(enforcer middleware.PermissionChecker, aid string) *problem.Details {
	if err := enforcer.Check(aid, "herald", "read", nil); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return problemDetailsPtr(problem.TypeOperatorRevokedToken, "archon "+aid+" has been revoked")
		}
		return problemDetailsPtr(problem.TypeForbidden, "operator lacks required permission herald.read")
	}
	return nil
}

// permanentNotifyName строит детерминированно-уникальное имя постоянного
// notify-правила Cadence: первый элемент — `<prefix>-notify`, последующие —
// `<prefix>-notify-<i+1>` (i — индекс в массиве notify). Tiding.Name — PK,
// коллизия недопустима; индекс в стабильном массиве уникален by construction.
// prefix уже усечён caller-ом (cappedNotifyPrefix) до допустимой длины
// (NamePattern ^[a-z0-9-]{1,63}$).
func permanentNotifyName(prefix string, i int) string {
	name := prefix + "-notify"
	if i > 0 {
		name += "-" + strconv.Itoa(i+1)
	}
	return name
}

// stampEphemeralTidings проставляет в шаблоны (из prepareNotify) сгенерированный
// voyage_id и детерминированно-уникальное имя — после того, как voyage_id
// известен (buildVoyageRow). Имя — `eph-<lowercase-ULID>` (свежий ULID на
// правило): уникально, матчит NamePattern (^[a-z0-9-]{1,63}$, ULID Crockford
// lowercased ⊂ [a-z0-9]). Свежий ULID на каждое правило исключает коллизию имён
// при нескольких notify-элементах на один herald.
func stampEphemeralTidings(templates []herald.Tiding, voyageID string) {
	for i := range templates {
		vid := voyageID
		templates[i].VoyageID = &vid
		templates[i].Name = "eph-" + strings.ToLower(audit.NewULID())
	}
}

// notifyEventTypes маппит notify.on (терминалы прогона) в audit-event-types по
// kind (ADR-052(g)). Пустой on ⇒ все три терминала. Неизвестное значение → err.
//
//	completed → scenario_run.completed       / command_run.completed
//	failed    → scenario_run.failed          / command_run.failed
//	partial   → scenario_run.partial_failed  / command_run.partial_failed
func notifyEventTypes(kind voyage.Kind, on []string) (eventTypes []string, errMsg string) {
	terminals := on
	if len(terminals) == 0 {
		terminals = []string{notifyOnCompleted, notifyOnFailed, notifyOnPartial}
	}
	prefix := "scenario_run."
	if kind == voyage.KindCommand {
		prefix = "command_run."
	}
	seen := make(map[string]struct{}, len(terminals))
	out := make([]string, 0, len(terminals))
	for _, t := range terminals {
		var action string
		switch t {
		case notifyOnCompleted:
			action = "completed"
		case notifyOnFailed:
			action = "failed"
		case notifyOnPartial:
			action = "partial_failed"
		default:
			return nil, "значение " + t + " must be one of {completed, failed, partial}"
		}
		et := prefix + action
		if _, dup := seen[et]; dup {
			continue
		}
		seen[et] = struct{}{}
		out = append(out, et)
	}
	return out, ""
}

// decodeAnnotations распаковывает сырой JSON annotations в map (object-форму уже
// гарантировал ValidateAnnotationsJSON). Пустой/null → nil (= нет статических
// полей).
func decodeAnnotations(raw json.RawMessage) map[string]any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var m map[string]any
	// Безопасно: ValidateAnnotationsJSON уже подтвердил валидный JSON-object.
	_ = json.Unmarshal(raw, &m)
	return m
}

// publicErr — public-safe текст ошибки herald-валидатора (он уже формирует
// сообщение без internal SQL/stack; срезаем только pkg-префикс `herald: `).
func publicErr(err error) string {
	return strings.TrimPrefix(err.Error(), "herald: ")
}
