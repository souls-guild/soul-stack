package api

// БАТЧ-1 read-tier тиража OpenAPI spec-first → code-first на huma v2, FULL-TYPED
// форма (ADR-054 §Pattern, READ-вариант pilot-1 БЕЗ audit). Переводит три
// READ-каталога со strict (bridge+strictWrapper.X) на huma full-typed: typed input
// (пустой) → существующая read-логика handler-а (*Typed-функция) → typed output.
//
// Все три каталога — auth-only (RequireJWT на /v1/* выше), БЕЗ RequirePermission:
// само-описывающие (требование права на чтение списка прав/типов = «курица-яйцо»,
// architect-вердикт). Audit НЕ навешивается (read не пишет audit). Старый (w,r) —
// тонкая strict-оболочка над *Typed (до финального сноса strict-методов).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaPermissionsList монтирует GET /v1/permissions через huma на
// переданный chi.Router (та группа, что уже несёт RequireJWT/maxBody/metrics).
// permH — доменный handler; nil → no-op (паттерн opt-in-домена router.go).
// READ-вариант pilot-1: huma вызывает ListTyped → typed output, БЕЗ audit-
// middleware.
func registerHumaPermissionsList(humaAPI huma.API, permH *handlers.PermissionCatalogHandler) {
	if permH == nil {
		return
	}
	huma.Register(humaAPI, permissionsListOperation(), func(_ context.Context, _ *permissionsListInput) (*permissionsListOutput, error) {
		return &permissionsListOutput{Body: newPermissionCatalogReply(permH.ListTyped())}, nil
	})
}

// registerHumaEventTypesList монтирует GET /v1/event-types через huma. eventTypeH
// nil → no-op. READ-вариант pilot-1: huma вызывает ListTyped → typed output, без audit.
func registerHumaEventTypesList(humaAPI huma.API, eventTypeH *handlers.EventTypeCatalogHandler) {
	if eventTypeH == nil {
		return
	}
	huma.Register(humaAPI, eventTypesListOperation(), func(_ context.Context, _ *eventTypesListInput) (*eventTypesListOutput, error) {
		return &eventTypesListOutput{Body: newEventTypeCatalogReply(eventTypeH.ListTyped())}, nil
	})
}

// registerHumaHeraldTypesList монтирует GET /v1/herald-types через huma.
// heraldTypeH nil → no-op. READ-вариант: huma вызывает ListTyped → typed output,
// без audit.
func registerHumaHeraldTypesList(humaAPI huma.API, heraldTypeH *handlers.HeraldTypeCatalogHandler) {
	if heraldTypeH == nil {
		return
	}
	huma.Register(humaAPI, heraldTypesListOperation(), func(_ context.Context, _ *heraldTypesListInput) (*heraldTypesListOutput, error) {
		return &heraldTypesListOutput{Body: newHeraldTypeCatalogReply(heraldTypeH.ListTyped())}, nil
	})
}

// registerHumaMyPermissionsList монтирует GET /v1/me/permissions через huma. meH
// nil → no-op. READ-вариант pilot-1: claims (AID) из ctx (RequireJWT положил до
// humachi) → GetTyped(aid) → typed output, без audit. Нет claims (auth-chain не
// собрана — серверная ошибка) → 500 problem+json (parity доменного Get).
func registerHumaMyPermissionsList(humaAPI huma.API, meH *handlers.MyPermissionsHandler) {
	if meH == nil {
		return
	}
	huma.Register(humaAPI, myPermissionsListOperation(), func(ctx context.Context, _ *myPermissionsListInput) (*myPermissionsListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing operator claims in request context")}
		}
		return &myPermissionsListOutput{Body: newMyPermissionsReply(meH.GetTyped(claims.Subject))}, nil
	})
}

// HumaCatalogSpecYAML собирает OpenAPI-фрагмент трёх мигрированных-на-huma READ-
// каталогов (permissions / event-types / me-permissions) как YAML-строку, БЕЗ
// монтирования на реальный router. Хук для спека-мерж-таргета тиража и guard-теста.
// Делегирует generic [humaDumpSpec], регистрируя операции через те же register-
// функции (единый register-путь — нет дубля dump-vs-mount): handler-методы при dump
// не вызываются (huma.Register их не исполняет), поэтому достаточно реальных
// конструкторов с nil-deps. Возвращает 3.1.0-спеку (huma-дефолт).
func HumaCatalogSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaPermissionsList(api, handlers.NewPermissionCatalogHandler(nil))
		registerHumaEventTypesList(api, handlers.NewEventTypeCatalogHandler(nil))
		registerHumaHeraldTypesList(api, handlers.NewHeraldTypeCatalogHandler(nil))
		registerHumaMyPermissionsList(api, handlers.NewMyPermissionsHandler(nil, nil))
		return nil
	})
}
