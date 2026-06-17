package api

// Регистрация и spec-dump MODULE-домена на huma full-typed (ТИРАЖ-БАТЧ-2e по эталону
// catalog read-bare + form-prep read-with-body, ADR-054 §Pattern). list/get — read-каталог
// (RBAC service.list), form-prep — read-резолв SID под форму (RBAC incarnation.run). Все три
// — READ-only, audit НЕ навешивается ни на один роут. Доменные *Typed-функции
// (handlers/modulecatalog.go + moduleformprep.go) извлечены из (w,r); старый (w,r) —
// тонкая strict-оболочка. MCP module-домена НЕТ (каталог без MCP-tool-ов — извлечение
// ничего в mcp не затрагивает).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaModuleList монтирует GET /v1/modules через huma (READ-with-typed-query,
// БЕЗ audit). moduleCatalogH nil → no-op. Handler: typed-query (errand_safe) → ListTyped →
// typed envelope-output. RBAC service.list — на группе (huma наследует).
func registerHumaModuleList(humaAPI huma.API, moduleCatalogH *handlers.ModuleCatalogHandler) {
	if moduleCatalogH == nil {
		return
	}
	huma.Register(humaAPI, moduleListOperation(), func(ctx context.Context, in *moduleListInput) (*moduleListOutput, error) {
		reply, err := moduleCatalogH.ListTyped(ctx, in.ErrandSafe)
		if err != nil {
			return nil, moduleProblem(err)
		}
		return &moduleListOutput{Body: reply}, nil
	})
}

// registerHumaModuleGet монтирует GET /v1/modules/{name} через huma (READ-with-path,
// БЕЗ audit). moduleCatalogH nil → no-op. Handler: GetTyped(name) → typed output
// (404 через problem). RBAC service.list — на группе.
func registerHumaModuleGet(humaAPI huma.API, moduleCatalogH *handlers.ModuleCatalogHandler) {
	if moduleCatalogH == nil {
		return
	}
	huma.Register(humaAPI, moduleGetOperation(), func(ctx context.Context, in *moduleGetInput) (*moduleGetOutput, error) {
		reply, err := moduleCatalogH.GetTyped(ctx, in.Name)
		if err != nil {
			return nil, moduleProblem(err)
		}
		return &moduleGetOutput{Body: reply}, nil
	})
}

// registerHumaModuleFormPrep монтирует POST /v1/modules/{name}/form-prep через huma
// (READ-with-body, БЕЗ audit — read-only-резолв). moduleFormPrepH nil → no-op. Handler:
// конверт typed-body → FormPrepTyped → typed output. RBAC incarnation.run — на группе.
func registerHumaModuleFormPrep(humaAPI huma.API, moduleFormPrepH *handlers.ModuleFormPrepHandler) {
	if moduleFormPrepH == nil {
		return
	}
	huma.Register(humaAPI, moduleFormPrepOperation(), func(ctx context.Context, in *moduleFormPrepInput) (*moduleFormPrepOutput, error) {
		reply, err := moduleFormPrepH.FormPrepTyped(ctx, toModuleFormPrepInput(in.Body))
		if err != nil {
			return nil, moduleProblem(err)
		}
		return &moduleFormPrepOutput{Body: newModuleFormPrepReply(reply)}, nil
	})
}

// moduleProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity roleProblem).
func moduleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaModuleSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma module-роутов
// (list/get/form-prep) как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaModuleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaModuleList(api, handlers.ModuleCatalogSpecStub())
		registerHumaModuleGet(api, handlers.ModuleCatalogSpecStub())
		registerHumaModuleFormPrep(api, handlers.ModuleFormPrepSpecStub())
		return nil
	})
}
