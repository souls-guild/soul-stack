package api

// Registration and spec-dump of the MODULE domain on huma full-typed (rollout batch
// 2e following the catalog read-bare + form-prep read-with-body reference pattern,
// ADR-054 §Pattern). list/get — a read catalog (RBAC service.list), form-prep — a
// read resolve of SIDs for the form (RBAC incarnation.run). All three are READ-only,
// audit is not wired on any route. The domain *Typed functions
// (handlers/modulecatalog.go + moduleformprep.go) are extracted from (w,r); the old
// (w,r) is a thin strict wrapper. There is NO MCP for the module domain (a catalog
// without MCP tools — the extraction touches nothing in mcp).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// registerHumaModuleList mounts GET /v1/modules via huma (READ with typed query, no
// audit). moduleCatalogH nil → no-op. Handler: typed-query (errand_safe) → ListTyped
// → typed envelope output. RBAC service.list — on the group (huma inherits it).
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

// registerHumaModuleGet mounts GET /v1/modules/{name} via huma (READ with path, no
// audit). moduleCatalogH nil → no-op. Handler: GetTyped(name) → typed output (404
// via problem). RBAC service.list — on the group.
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

// registerHumaModuleFormPrep mounts POST /v1/modules/{name}/form-prep via huma (READ
// with body, no audit — a read-only resolve). moduleFormPrepH nil → no-op. Handler:
// typed-body envelope → FormPrepTyped → typed output. RBAC incarnation.run — on the
// group.
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

// moduleProblem delivers a *Typed function's error through huma as problem+json. A
// domain *handlers.problemError → humaProblemError; a non-problem → 500 (parity with
// roleProblem).
func moduleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaModuleSpecYAML assembles the OpenAPI fragment of ALL module routes migrated to
// huma (list/get/form-prep) as a YAML string, WITHOUT mounting on a real router. A
// hook for the rollout's spec-merge target and a guard test. Delegates to the
// generic [humaDumpSpec] through the same register functions (a single register
// path). Returns a 3.1.0 spec (the huma default).
func HumaModuleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaModuleList(api, handlers.ModuleCatalogSpecStub())
		registerHumaModuleGet(api, handlers.ModuleCatalogSpecStub())
		registerHumaModuleFormPrep(api, handlers.ModuleFormPrepSpecStub())
		return nil
	})
}
