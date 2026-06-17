// Module-catalog-handler Operator API (`GET /v1/modules`) — публикует доступные
// для прогона модули + input-метаданные. Назначение — module-search в UI
// Run→Command (замена free-text «custom module»): оператор выбирает модуль из
// каталога вместо ручного ввода имени.
//
// Два источника:
//   - core — статическая doc-таблица [coreModuleDocs] (keeper не видит
//     soul/internal/coremod по ADR-011; реализации не несут декларативной
//     input-схемы — params core пусты, см. modulecatalog_coredata.go);
//   - plugin — активные (не отозванные) записи plugin_sigils, params читаются
//     из manifest `spec.states[*].input` (shared/plugin-парсер).
//
// RBAC — service.list (read-only-каталог; read без audit, паттерн service.list /
// role.list / plugin.list). Permission переиспользована, не заводится новая.
package handlers

import (
	"context"
	"io"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// PluginCatalogEntry — активный plugin-допуск для каталога: координаты +
// byte-exact manifest (для разбора params). Возвращается [ModuleCatalogPlugins].
type PluginCatalogEntry struct {
	Namespace   string
	Name        string
	Ref         string
	ManifestRaw []byte
}

// ModuleCatalogPlugins — поверхность чтения активных plugin-допусков для
// каталога. Реализуется адаптером поверх sigil-store (production-wire-up). При
// nil в [ModuleCatalogHandler] каталог отдаёт только core (plugin-секция
// пуста) — keeper без Sigil остаётся рабочим (паттерн опциональных Deps).
type ModuleCatalogPlugins interface {
	// ActivePlugins возвращает активные (не отозванные) plugin-допуски. Порядок
	// не важен — handler сортирует выдачу сам.
	ActivePlugins(ctx context.Context) ([]PluginCatalogEntry, error)
}

// ModuleCatalogHandler — `GET /v1/modules` + `GET /v1/modules/{name}`.
//
// Зависимости immutable; safe for concurrent use — состояние между запросами не
// держит (core-таблица read-only, plugin-lister thread-safe по контракту).
type ModuleCatalogHandler struct {
	plugins ModuleCatalogPlugins
	logger  *slog.Logger
}

// NewModuleCatalogHandler создаёт handler. plugins опционален (nil → только
// core). logger nil → io.Discard.
func NewModuleCatalogHandler(plugins ModuleCatalogPlugins, logger *slog.Logger) *ModuleCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ModuleCatalogHandler{plugins: plugins, logger: logger}
}

// moduleParam — один параметр модуля в выдаче. Заполняется из manifest-схемы:
// для plugin — из manifest.yaml, для core — из coremanifest-реестра. Поля
// Enum/Pattern/Format/Source отражают [plugin.InputParamDef] (ADR-045) — backend
// строит из них UI-форму модуля.
type moduleParam struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret,omitempty"`
	Description string `json:"description,omitempty"`

	// Поля UI-формы (ADR-045 S2). omitempty — параметры без расширенной схемы
	// остаются компактными.
	Enum    []any              `json:"enum,omitempty"`
	Pattern string             `json:"pattern,omitempty"`
	Format  string             `json:"format,omitempty"`
	Source  *moduleInputSource `json:"source,omitempty"`

	// Multiline/Example — декларативные UI-подсказки (ADR-045 B3): большое
	// textarea + placeholder. omitempty — поля без подсказок остаются компактными.
	Multiline bool   `json:"multiline,omitempty"`
	Example   string `json:"example,omitempty"`

	// Items — тип элементов списка (ADR-045 S7). Для type=list/array сообщает
	// UI, что строить типизированный список (напр. list[int]), а не свободный
	// список строк.
	Items *moduleParam `json:"items,omitempty"`
}

// moduleCatalogItem — одна запись каталога. Имя типа = контрактное имя схемы рукописи
// (docs/keeper/openapi.yaml :5392 → ModuleCatalogItem): huma DefaultSchemaNamer
// капитализирует первую букву → "ModuleCatalogItem".
type moduleCatalogItem struct {
	Name        string        `json:"name"`
	Kind        string        `json:"kind"` // "core" | "plugin"
	Namespace   string        `json:"namespace,omitempty"`
	Description string        `json:"description,omitempty"`
	States      []string      `json:"states"`
	ErrandSafe  bool          `json:"errand_safe"`
	Params      []moduleParam `json:"params"`
}

// moduleCatalogReply — тело `GET /v1/modules`. Имя типа = контрактное имя схемы рукописи
// (docs/keeper/openapi.yaml :5424 → ModuleCatalogReply): huma DefaultSchemaNamer
// капитализирует первую букву → "ModuleCatalogReply".
type moduleCatalogReply struct {
	Items []moduleCatalogItem `json:"items"`
}

// ModuleCatalogItem / ModuleCatalogReply — экспортированные алиасы на внутренние wire-
// типы каталога, через которые huma-роуты (пакет api) типизируют output без
// форка wire-формы (поля несут те же json-теги; huma строит из них схему 200-
// тела). Категория C-эквивалент module-домена: локальные типы остаются
// unexported для теста handler-а, алиасы дают доступ извне.
type (
	ModuleCatalogItem  = moduleCatalogItem
	ModuleCatalogReply = moduleCatalogReply
)

// ModuleCatalogSpecStub — непустой *ModuleCatalogHandler-заглушка для генерации
// huma-OpenAPI-фрагмента (HumaModuleSpecYAML): при dump доменный handler не
// вызывается, но huma.Register требует non-nil для nil-проверки (parity
// [RoleSpecStub]). plugins nil — handler в spec-режиме не исполняется.
func ModuleCatalogSpecStub() *ModuleCatalogHandler {
	return &ModuleCatalogHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ListTyped — извлечённая доменная функция `GET /v1/modules` (FULL-TYPED разворот
// ADR-054 §Pattern): каталог без http.ResponseWriter/*http.Request. onlyErrandSafe
// — фильтр `?errand_safe=true`. Ошибка чтения plugin-реестра → *problemError (500).
func (h *ModuleCatalogHandler) ListTyped(ctx context.Context, onlyErrandSafe bool) (ModuleCatalogReply, error) {
	items, err := h.buildCatalog(ctx)
	if err != nil {
		h.logger.Error("module.catalog: list plugins failed", slog.Any("error", err))
		return ModuleCatalogReply{}, &problemError{problem.New(problem.TypeInternalError, "", "list modules failed")}
	}
	out := make([]moduleCatalogItem, 0, len(items))
	for _, it := range items {
		if onlyErrandSafe && !it.ErrandSafe {
			continue
		}
		out = append(out, it)
	}
	return ModuleCatalogReply{Items: out}, nil
}

// GetTyped — извлечённая доменная функция `GET /v1/modules/{name}`. Ошибки —
// *problemError (404 нет модуля / 500 сбой реестра); успех — [ModuleCatalogItem].
func (h *ModuleCatalogHandler) GetTyped(ctx context.Context, name string) (ModuleCatalogItem, error) {
	items, err := h.buildCatalog(ctx)
	if err != nil {
		h.logger.Error("module.catalog: get plugins failed", slog.Any("error", err))
		return ModuleCatalogItem{}, &problemError{problem.New(problem.TypeInternalError, "", "get module failed")}
	}
	for _, it := range items {
		if it.Name == name {
			return it, nil
		}
	}
	return ModuleCatalogItem{}, &problemError{problem.New(problem.TypeNotFound, "", "no such module: "+name)}
}

// buildCatalog собирает полный каталог (core + plugin), отсортированный по name.
// Возвращает ошибку только при сбое чтения plugin-реестра (core — статика).
func (h *ModuleCatalogHandler) buildCatalog(ctx context.Context) ([]moduleCatalogItem, error) {
	items := make([]moduleCatalogItem, 0, len(coreModuleDocs))
	for _, c := range coreModuleDocs {
		var params []moduleParam
		if m, ok := coremanifest.Default().Lookup(c.Name); ok {
			params = manifestToParams(m.Spec)
		} else {
			params = []moduleParam{}
		}
		items = append(items, moduleCatalogItem{
			Name:        c.Name,
			Kind:        "core",
			Description: c.Description,
			States:      c.States,
			ErrandSafe:  len(c.ErrandSafeStates) > 0,
			Params:      params,
		})
	}

	if h.plugins != nil {
		entries, err := h.plugins.ActivePlugins(ctx)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			items = append(items, pluginCatalogItem(e))
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// pluginCatalogItem строит запись каталога из активного plugin-допуска. Имя —
// `<namespace>.<name>` (адресная форма Soul Stack). States и params читаются из
// manifest; невалидный/нечитаемый manifest даёт запись с пустыми states/params
// (плагин допущен → виден в каталоге, но без метаданных, чем молча скрывать).
func pluginCatalogItem(e PluginCatalogEntry) moduleCatalogItem {
	it := moduleCatalogItem{
		Name:      e.Namespace + "." + e.Name,
		Kind:      "plugin",
		Namespace: e.Namespace,
		States:    []string{},
		Params:    []moduleParam{},
	}

	m, _ := plugin.LoadFromBytes(plugin.FileName, e.ManifestRaw)
	if m == nil || m.Kind != plugin.KindSoulModule {
		// soul_module-каталог: cloud_driver/ssh_provider/soul_beacon не
		// применяются как Destiny-шаг в Run→Command. Manifest нечитаем →
		// запись без states/params (координаты допуска остаются).
		return it
	}

	states := make([]string, 0, len(m.Spec.States))
	for state := range m.Spec.States {
		states = append(states, state)
	}
	sort.Strings(states)
	it.States = states
	it.Params = manifestToParams(m.Spec)
	return it
}

// manifestToParams сводит input-схему всех state-ов manifest-а в плоский
// дедуплицированный список параметров каталога. Общая для core (coremanifest) и
// plugin: один param может встречаться в нескольких state-ах (vault-secret в
// installed и promoted) — в каталог выносим его один раз. required/secret =
// true, если он такой хотя бы в одном state; Type/Description/Pattern/Format/
// Enum/Source берутся из первого state-а, где они заданы (детерминизм за счёт
// сортировки порядка). Возвращает non-nil срез (пустой при отсутствии input).
func manifestToParams(spec plugin.ManifestSpec) []moduleParam {
	type pdef struct {
		typ, desc, pattern, format, example string
		required, secret, multiline         bool
		enum                                []any
		source                              *plugin.InputSource
		items                               *plugin.InputParamDef
	}
	seen := make(map[string]*pdef)
	order := make([]string, 0)
	for _, def := range spec.States {
		for pname, p := range def.Input {
			cur, ok := seen[pname]
			if !ok {
				cur = &pdef{}
				seen[pname] = cur
				order = append(order, pname)
			}
			if p.Type != "" {
				cur.typ = p.Type
			}
			if p.Description != "" {
				cur.desc = p.Description
			}
			if p.Pattern != "" {
				cur.pattern = p.Pattern
			}
			if p.Format != "" {
				cur.format = p.Format
			}
			if cur.enum == nil && p.Enum != nil {
				cur.enum = p.Enum
			}
			if cur.source == nil && p.Source != nil {
				cur.source = p.Source
			}
			if cur.items == nil && p.Items != nil {
				cur.items = p.Items
			}
			if p.Example != "" {
				cur.example = p.Example
			}
			cur.required = cur.required || p.Required
			cur.secret = cur.secret || p.Secret
			cur.multiline = cur.multiline || p.Multiline
		}
	}
	sort.Strings(order)

	params := make([]moduleParam, 0, len(order))
	for _, pname := range order {
		d := seen[pname]
		params = append(params, moduleParam{
			Name:        pname,
			Type:        d.typ,
			Required:    d.required,
			Secret:      d.secret,
			Description: d.desc,
			Enum:        d.enum,
			Pattern:     d.pattern,
			Format:      d.format,
			Source:      toModuleInputSource(d.source),
			Multiline:   d.multiline,
			Example:     d.example,
			Items:       toModuleParamItems(d.items),
		})
	}
	return params
}

// toModuleParamItems рекурсивно прокидывает тип элементов списка (ADR-045 S7)
// в DTO. Имя элемента в форме смысла не несёт — оставляем пустым.
func toModuleParamItems(it *plugin.InputParamDef) *moduleParam {
	if it == nil {
		return nil
	}
	return &moduleParam{
		Type:        it.Type,
		Required:    it.Required,
		Secret:      it.Secret,
		Description: it.Description,
		Enum:        it.Enum,
		Pattern:     it.Pattern,
		Format:      it.Format,
		Source:      toModuleInputSource(it.Source),
		Multiline:   it.Multiline,
		Example:     it.Example,
		Items:       toModuleParamItems(it.Items),
	}
}

// moduleInputSource — NATIVE wire-форма source-дискриминатора параметра (handler-native
// T5d-2c-full, заменяет ModuleInputSource). Форма 1:1 с прежней: choir (*string
// omitempty) — SID-ы конкретной Choir-партии; incarnation_hosts (*bool omitempty) — все SID
// текущей инкарнации. Имя типа = контрактное имя схемы рукописи (huma DefaultSchemaNamer
// капитализирует → "ModuleInputSource").
type moduleInputSource struct {
	Choir            *string `json:"choir,omitempty"`
	IncarnationHosts *bool   `json:"incarnation_hosts,omitempty"`
}

// ModuleInputSource — экспортированный алиас на wire-тип source-дискриминатора, через
// который huma-роут (пакет api) ссылается на схему без форка wire-формы.
type ModuleInputSource = moduleInputSource

// toModuleInputSource проецирует доменный [plugin.InputSource] (value-поля) в
// wire-тип [moduleInputSource] (pointer-optional, ADR-051(c) категория C).
// nil source → nil (omitempty). Пустые под-ключи опускаются: false/"" не
// выносятся в wire, симметрично json-omitempty доменной формы — оператор видит
// ровно тот под-ключ, что задан в манифесте.
func toModuleInputSource(s *plugin.InputSource) *moduleInputSource {
	if s == nil {
		return nil
	}
	out := &moduleInputSource{}
	if s.IncarnationHosts {
		v := true
		out.IncarnationHosts = &v
	}
	if s.Choir != "" {
		out.Choir = &s.Choir
	}
	return out
}
