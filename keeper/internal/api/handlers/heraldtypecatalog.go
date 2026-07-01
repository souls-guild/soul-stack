// Herald-type-catalog-handler Operator API (`GET /v1/herald-types`, ADR-052
// amendment) — публикует машиночитаемый каталог типов Herald-канала и их config-
// полей. Назначение — UI Herald-формы (`POST /v1/heralds`): UI строит форму
// per-type (какие поля, что обязательно, что секрет) из каталога вместо хардкода
// (паттерн permission/event-type/module-каталога).
//
// Источник правды — herald.TypeCatalog (те же [herald.HeraldFieldSpec], что
// валидируют CRUD Herald через herald.ValidateConfig). Handler НЕ дублирует
// набор: расширение типа (правка channelDrivers/emailFields) автоматически
// отражается в выдаче — рассинхрон каталога и валидатора невозможен.
//
// RBAC — только аутентификация (валидный JWT), БЕЗ отдельной permission: каталог
// само-описывающий (паттерн event-type-каталога). Read-only, без audit.
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

// HeraldTypeCatalogHandler — `GET /v1/herald-types`. Состояние не держит (каталог
// read-only-статика из пакета herald); safe for concurrent use.
type HeraldTypeCatalogHandler struct {
	logger *slog.Logger
}

// NewHeraldTypeCatalogHandler создаёт handler. logger nil → io.Discard.
func NewHeraldTypeCatalogHandler(logger *slog.Logger) *HeraldTypeCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &HeraldTypeCatalogHandler{logger: logger}
}

// HeraldFieldView — ПЛОСКАЯ доменная запись одного config-поля типа (name/label/
// required/secret/kind/enum_values). kind — строка ([herald.FieldKind]) для form-
// рендера UI. EnumValues непуст только у kind=enum (набор допустимых строк, вкл.
// "" = «поле опущено/plain») — UI рендерит такое поле как select, не text-input.
type HeraldFieldView struct {
	Name       string
	Label      string
	Required   bool
	Secret     bool
	Kind       string
	EnumValues []string
}

// HeraldTypeView — ПЛОСКИЙ доменный дескриптор одного типа канала (type + fields +
// secret_required). SecretRequired=true ⟹ у типа есть top-level secret_ref
// (webhook) — UI показывает поле по этому признаку, не по хардкоду типа.
type HeraldTypeView struct {
	Type           string
	Fields         []HeraldFieldView
	SecretRequired bool
}

// HeraldTypeCatalog — ПЛОСКОЕ доменное тело `GET /v1/herald-types` (handler-native).
type HeraldTypeCatalog struct {
	Types []HeraldTypeView
}

// ListTyped — доменная функция `GET /v1/herald-types` (READ без audit): собирает
// каталог из единого источника правды (herald.TypeCatalog). Ошибки невозможны →
// возвращает только значение (native-проекция в api строит wire).
func (h *HeraldTypeCatalogHandler) ListTyped() HeraldTypeCatalog {
	return buildHeraldTypeCatalog()
}

// buildHeraldTypeCatalog проецирует herald.TypeCatalog в плоские view. Срезы
// non-nil (native-проекция отдаёт `[]`, не `null`).
func buildHeraldTypeCatalog() HeraldTypeCatalog {
	descriptors := herald.TypeCatalog()
	types := make([]HeraldTypeView, 0, len(descriptors))
	for _, d := range descriptors {
		fields := make([]HeraldFieldView, 0, len(d.Fields))
		for _, f := range d.Fields {
			fields = append(fields, HeraldFieldView{
				Name:       f.Name,
				Label:      f.Label,
				Required:   f.Required,
				Secret:     f.Secret,
				Kind:       string(f.Kind),
				EnumValues: f.EnumValues,
			})
		}
		types = append(types, HeraldTypeView{Type: string(d.Type), Fields: fields, SecretRequired: d.SecretRequired})
	}
	return HeraldTypeCatalog{Types: types}
}
