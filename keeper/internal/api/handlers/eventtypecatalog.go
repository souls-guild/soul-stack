// EventType-catalog-handler Operator API (`GET /v1/event-types`) — публикует
// машиночитаемый каталог event-types, допустимых для подписки Tiding (ADR-052(b)).
// Назначение — UI Tiding-формы (`POST /v1/tidings`): UI фетчит допустимые типы из
// каталога вместо хардкода (фикс ADR-042, паттерн permission/module-каталога).
//
// Источник правды — keeper/internal/herald/eventtypes.go (тот же scope, что
// валидирует CRUD Tiding через herald.ValidateEventTypes). Handler НЕ дублирует
// список: читает его через геттеры [herald.RunScopeAreas] / [herald.RunScopePointEvents].
// Расширение scope амендом ADR-052 (правка runScope* в herald) автоматически
// отражается в выдаче — рассинхрон каталога и валидатора невозможен.
//
// RBAC — только аутентификация (валидный JWT), БЕЗ отдельной permission: каталог
// самоописывающий (паттерн permission-каталога — требование права на чтение
// списка значений даёт «курицу-яйцо»). Read-only, без audit (паттерн health/meta).
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

// EventTypeCatalogHandler — `GET /v1/event-types`. Состояние не держит (каталог
// read-only-статика из пакета herald); safe for concurrent use.
type EventTypeCatalogHandler struct {
	logger *slog.Logger
}

// NewEventTypeCatalogHandler создаёт handler. logger nil → io.Discard.
func NewEventTypeCatalogHandler(logger *slog.Logger) *EventTypeCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &EventTypeCatalogHandler{logger: logger}
}

// Тела ответа — ПЛОСКИЕ доменные типы (handler-native T5d). Пакет api проецирует
// их в native-схему EventTypeCatalogReply (register-func).
type (
	// eventTypeArea — одна area-glob-область выдачи.
	eventTypeArea = EventTypeAreaView
	// eventTypePoint — один точечный event-type выдачи.
	eventTypePoint = EventTypePointView
)

// EventTypeAreaView — ПЛОСКАЯ доменная area-glob-запись (name).
type EventTypeAreaView struct {
	Name string
}

// EventTypePointView — ПЛОСКИЙ доменный точечный event-type (name).
type EventTypePointView struct {
	Name string
}

// EventTypeCatalog — ПЛОСКОЕ доменное тело `GET /v1/event-types` (handler-native T5d).
type EventTypeCatalog struct {
	Areas       []EventTypeAreaView
	PointEvents []EventTypePointView
}

// ListTyped — доменная функция `GET /v1/event-types` (handler-native T5d, READ без
// audit): собирает каталог без http.ResponseWriter/*http.Request. Каталог — read-only-
// статика из пакета herald, ошибки невозможны → возвращает только значение (native-
// проекция в api строит wire). Wire-форма (areas/point_events non-nil) сохранена —
// golden фиксирует её байт-в-байт.
func (h *EventTypeCatalogHandler) ListTyped() EventTypeCatalog {
	return buildEventTypeCatalog()
}

// buildEventTypeCatalog собирает каталог из единого источника правды (herald).
// Срезы non-nil (native-проекция отдаёт `[]`, не `null`) даже при пустом scope.
func buildEventTypeCatalog() EventTypeCatalog {
	areaNames := herald.RunScopeAreas()
	areas := make([]eventTypeArea, 0, len(areaNames))
	for _, name := range areaNames {
		// area-glob в готовой форме `<area>.*` — подписываемой as-is. Голое имя
		// области (`scenario_run`) НЕ валидно для herald.ValidateEventTypes
		// (требует `<area>.*` или `<area>.<action>`), поэтому каталог отдаёт глоб.
		areas = append(areas, eventTypeArea{Name: name + ".*"})
	}

	pointNames := herald.RunScopePointEvents()
	points := make([]eventTypePoint, 0, len(pointNames))
	for _, name := range pointNames {
		points = append(points, eventTypePoint{Name: name})
	}

	return EventTypeCatalog{Areas: areas, PointEvents: points}
}
