package api

// Aggregator of the unified huma-OpenAPI spec for all Operator API domains. Goal —
// "truth in code": one valid 3.1 spec assembled by a runtime dump of huma
// operations from the code (FastAPI-style), without a committed YAML as the
// source. Assembly entry point — HumaFullSpecYAML (below). The served mechanism is
// already switched to this aggregator (T4c): GET /openapi.yaml returns the
// runtime dump via servedOpenAPIHandler (router.go), the meta-embed package is
// removed; the committed docs/keeper/openapi.yaml is a derived artifact
// (make gen-openapi), checked against the dump by the check-openapi gate.
//
// MECHANISM (A2-bis, architect recommendation — do NOT change huma.Operation):
// a per-domain dump already exists (HumaXSpecYAML via humaDumpSpec on a
// temporary chi router). The problem — a dual path convention: for MOST domains
// huma-Operation.Path is RELATIVE to the chi group (`/`, `/{name}`), the full URL
// comes from the chi mount prefix; for oracle/augur/herald/errand/catalog/audit/
// push-runs the Path is already full under /v1. A naive merge into one huma.API →
// path+method collisions (multiple "POST /" from different groups). A2-bis: dump
// each REGISTRATION GROUP onto its OWN huma.API, SHIFT its paths keys by the
// per-group chi prefix (the same one used in buildRouter), then merge paths +
// components.schemas + tags into one 3.1 spec.
//
// The unit of assembly is the REGISTRATION GROUP (prefix + a set of register
// functions), not a "domain", because the prefix is a property of the group's
// chi mount, not of the domain: push mixes /v1/push (apply/get) and /v1
// (push-runs) in one HumaPushSpecYAML, hence it is split into two groups. The
// specGroups list mirrors the buildRouter topology — this is also the future
// drift-guard (the aggregator must not forget a domain).

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	yaml "gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// bearerSecuritySchemeName — the securityScheme name (http/bearer JWT) in the
// assembled spec. The /docs viewer reads it for "Try It"; the global security
// requirement references the same name. The standard name for bearer-JWT in the
// OpenAPI ecosystem.
const bearerSecuritySchemeName = "bearerAuth"

// yamlMarshalSchema serializes *huma.Schema into a canonical YAML string for a
// byte-exact comparison of same-named schema bodies during merge (gate-b
// collision detection).
func yamlMarshalSchema(s *huma.Schema) (string, error) {
	b, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// specGroup — a single registration group: the chi mount prefix (as in
// buildRouter) + a closure that registers this group's huma operations on the
// given API. prefix="/v1" — operations already carry a full under-/v1 path
// (oracle/augur/herald/…); prefix="/v1/<x>" — operations are relative to the chi
// group r.Route("/<x>").
type specGroup struct {
	prefix   string
	register func(huma.API) error
}

// fullSpecGroups — the full set of Operator API registration groups in the
// buildRouter topology. The source of truth for the aggregator; adding a domain
// to the router without a line here is caught by TestFullSpec_CoversAllRoutes
// (drift-guard).
//
// Prefixes are checked against router.go (chi mount of each group):
//   - r.Route("/<x>") + a relative Operation.Path  → prefix "/v1/<x>"
//   - a group on /v1 with a full under-/v1 Operation.Path → prefix "/v1"
//
// Special cases:
//   - choir is mounted on the /v1/incarnations group (Operation.Path =
//     /{name}/choirs/…) → prefix "/v1/incarnations" (NOT "/v1/choirs").
//   - push is split: apply/get are relative to /v1/push; push-runs is a full /v1.
func fullSpecGroups() []specGroup {
	return []specGroup{
		// Relative groups r.Route("/<x>").
		{"/v1/operators", func(api huma.API) error {
			stub := handlers.OperatorSpecStub()
			registerHumaOperatorCreate(api, stub)
			registerHumaOperatorList(api, stub)
			registerHumaOperatorGet(api, stub)
			registerHumaOperatorRevoke(api, stub)
			registerHumaOperatorIssueToken(api, stub)
			return nil
		}},
		{"/v1/roles", func(api huma.API) error {
			stub := handlers.RoleSpecStub()
			registerHumaRole(api, stub)
			registerHumaRoleList(api, stub)
			registerHumaRoleDelete(api, stub)
			registerHumaRoleUpdatePermissions(api, stub)
			registerHumaRoleGrantOperator(api, stub)
			registerHumaRoleRevokeOperator(api, stub)
			return nil
		}},
		{"/v1/synods", func(api huma.API) error {
			stub := handlers.SynodSpecStub()
			registerHumaSynodCreate(api, stub)
			registerHumaSynodList(api, stub)
			registerHumaSynodUpdate(api, stub)
			registerHumaSynodDelete(api, stub)
			registerHumaSynodAddOperator(api, stub)
			registerHumaSynodRemoveOperator(api, stub)
			registerHumaSynodGrantRole(api, stub)
			registerHumaSynodRevokeRole(api, stub)
			return nil
		}},
		{"/v1/incarnations", func(api huma.API) error {
			stub := handlers.IncarnationSpecStub()
			registerHumaIncarnationCreate(api, stub)
			registerHumaIncarnationList(api, stub)
			registerHumaIncarnationGet(api, stub)
			registerHumaIncarnationTelemetry(api, handlers.TelemetrySpecStub())
			registerHumaIncarnationUpgradePaths(api, stub)
			registerHumaIncarnationFormPrefill(api, stub)
			registerHumaIncarnationHistory(api, stub)
			registerHumaIncarnationRuns(api, stub)
			registerHumaIncarnationRunDetail(api, stub)
			registerHumaIncarnationRunTasks(api, stub)
			// SSE live run progress (ADR-068 §A3): non-nil zero-deps registers the
			// operation for the spec (the handler closure is not invoked during dump).
			registerHumaIncarnationRunEvents(api, &runEventsDeps{})
			registerHumaIncarnationRun(api, stub)
			registerHumaIncarnationUnlock(api, stub)
			registerHumaIncarnationUpgrade(api, stub)
			registerHumaIncarnationRerunLast(api, stub)
			registerHumaIncarnationCheckDrift(api, stub)
			registerHumaIncarnationDestroy(api, stub)
			registerHumaIncarnationUpdateHosts(api, stub)
			registerHumaIncarnationSetTraits(api, stub)
			registerHumaIncarnationRevealSecret(api, stub)
			registerHumaIncarnationRevealableSecrets(api, stub)
			return nil
		}},
		// choir is mounted on the /v1/incarnations group, Operation.Path carries
		// /{name}/choirs/… → the same prefix /v1/incarnations.
		{"/v1/incarnations", func(api huma.API) error {
			stub := handlers.ChoirSpecStub()
			registerHumaChoirCreate(api, stub)
			registerHumaChoirDelete(api, stub)
			registerHumaVoiceAdd(api, stub)
			registerHumaVoiceRemove(api, stub)
			registerHumaChoirList(api, stub)
			registerHumaVoiceList(api, stub)
			return nil
		}},
		{"/v1/runs", func(api huma.API) error {
			stub := handlers.IncarnationSpecStub()
			registerHumaRunsList(api, stub)
			registerHumaRunsStats(api, stub)
			return nil
		}},
		{"/v1/souls", func(api huma.API) error {
			stub := handlers.SoulSpecStub()
			registerHumaSoulCreate(api, stub)
			registerHumaSoulCovenAssign(api, stub)
			registerHumaSoulTraitsAssign(api, stub)
			registerHumaSoulList(api, stub)
			registerHumaSoulStats(api, stub, nil)
			registerHumaSoulGet(api, stub)
			registerHumaSoulSoulprint(api, stub)
			registerHumaSoulHistory(api, stub)
			registerHumaSoulTelemetry(api, handlers.TelemetrySpecStub())
			registerHumaSoulIssueToken(api, stub)
			registerHumaSoulSshTarget(api, stub)
			registerHumaSoulExec(api, handlers.ErrandSpecStub())
			return nil
		}},
		{"/v1/plugins/sigils", func(api huma.API) error {
			stub := handlers.SigilSpecStub()
			registerHumaSigilAllow(api, stub)
			registerHumaSigilList(api, stub)
			registerHumaSigilRevoke(api, stub)
			return nil
		}},
		{"/v1/sigil/keys", func(api huma.API) error {
			stub := handlers.SigilKeySpecStub()
			registerHumaSigilKeyIntroduce(api, stub)
			registerHumaSigilKeyList(api, stub)
			registerHumaSigilKeySetPrimary(api, stub)
			registerHumaSigilKeyRetire(api, stub)
			return nil
		}},
		{"/v1/services", func(api huma.API) error {
			stub := handlers.ServiceSpecStub()
			registerHumaServiceRegister(api, stub)
			registerHumaServiceList(api, stub)
			registerHumaServiceGet(api, stub)
			registerHumaServiceUpdate(api, stub)
			registerHumaServiceDeregister(api, stub)
			registerHumaServiceRefs(api, stub)
			registerHumaServiceScenarios(api, stub)
			registerHumaServiceStateSchema(api, stub)
			registerHumaServiceDependencies(api, stub)
			registerHumaServiceDirectives(api, stub)
			registerHumaServiceTelemetry(api, stub)
			return nil
		}},
		{"/v1/provisioning-policy", func(api huma.API) error {
			stub := handlers.ProvisioningPolicySpecStub()
			registerHumaProvisioningPolicyGet(api, stub)
			registerHumaProvisioningPolicyPut(api, stub)
			return nil
		}},
		{"/v1/modules", func(api huma.API) error {
			stub := handlers.ModuleCatalogSpecStub()
			registerHumaModuleList(api, stub)
			registerHumaModuleGet(api, stub)
			registerHumaModuleFormPrep(api, handlers.ModuleFormPrepSpecStub())
			return nil
		}},
		{"/v1/push", func(api huma.API) error {
			stub := handlers.PushSpecStub()
			registerHumaPushApply(api, stub)
			registerHumaPushGet(api, stub)
			return nil
		}},
		{"/v1/push-providers", func(api huma.API) error {
			stub := handlers.PushProviderSpecStub()
			registerHumaPushProviderCreate(api, stub)
			registerHumaPushProviderList(api, stub)
			registerHumaPushProviderGet(api, stub)
			registerHumaPushProviderUpdate(api, stub)
			registerHumaPushProviderDelete(api, stub)
			return nil
		}},
		{"/v1/providers", func(api huma.API) error {
			stub := handlers.ProviderSpecStub()
			registerHumaProviderCreate(api, stub)
			registerHumaProviderList(api, stub)
			registerHumaProviderGet(api, stub)
			registerHumaProviderDelete(api, stub)
			return nil
		}},
		{"/v1/profiles", func(api huma.API) error {
			stub := handlers.ProfileSpecStub()
			registerHumaProfileCreate(api, stub)
			registerHumaProfileList(api, stub)
			registerHumaProfileGet(api, stub)
			registerHumaProfileDelete(api, stub)
			return nil
		}},
		{"/v1/voyages", func(api huma.API) error {
			stub := handlers.VoyageSpecStub()
			registerHumaVoyageCreate(api, stub)
			registerHumaVoyagePreview(api, stub)
			registerHumaVoyageList(api, stub)
			registerHumaVoyageGet(api, stub)
			registerHumaVoyageTargets(api, stub)
			registerHumaVoyageCancel(api, stub)
			return nil
		}},
		{"/v1/cadences", func(api huma.API) error {
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
		}},

		// Groups on /v1 — Operation.Path is already a full under-/v1 path.
		{"/v1", func(api huma.API) error {
			registerHumaAuditList(api, handlers.AuditSpecStub())
			return nil
		}},
		{"/v1", func(api huma.API) error {
			registerHumaPermissionsList(api, handlers.NewPermissionCatalogHandler(nil))
			registerHumaEventTypesList(api, handlers.NewEventTypeCatalogHandler(nil))
			registerHumaHeraldTypesList(api, handlers.NewHeraldTypeCatalogHandler(nil))
			registerHumaMyPermissionsList(api, handlers.NewMyPermissionsHandler(nil, nil))
			return nil
		}},
		// GET /v1/cluster — Operation.Path is full under-/v1 (/cluster).
		{"/v1", func(api huma.API) error {
			registerHumaClusterGet(api, handlers.ClusterSpecStub())
			return nil
		}},
		// augur is mounted via r.Route("/augur") — Operation.Path is relative
		// (/omens, /rites), the full URL = /v1/augur/… (NOT full under-/v1 like
		// oracle/herald, which are mounted directly on /v1).
		{"/v1/augur", func(api huma.API) error {
			stub := handlers.AugurSpecStub()
			registerHumaOmenCreate(api, stub)
			registerHumaOmenList(api, stub)
			registerHumaOmenGet(api, stub)
			registerHumaOmenDelete(api, stub)
			registerHumaRiteCreate(api, stub)
			registerHumaRiteList(api, stub)
			registerHumaRiteDelete(api, stub)
			return nil
		}},
		{"/v1", func(api huma.API) error {
			stub := handlers.OracleSpecStub()
			registerHumaVigilCreate(api, stub)
			registerHumaVigilList(api, stub)
			registerHumaVigilGet(api, stub)
			registerHumaVigilDelete(api, stub)
			registerHumaDecreeCreate(api, stub)
			registerHumaDecreeList(api, stub)
			registerHumaDecreeGet(api, stub)
			registerHumaDecreeDelete(api, stub)
			return nil
		}},
		{"/v1", func(api huma.API) error {
			stub := handlers.HeraldSpecStub()
			registerHumaHeraldCreate(api, stub)
			registerHumaHeraldList(api, stub)
			registerHumaHeraldGet(api, stub)
			registerHumaHeraldUpdate(api, stub)
			registerHumaHeraldDelete(api, stub)
			registerHumaTidingCreate(api, stub)
			registerHumaTidingList(api, stub)
			registerHumaTidingGet(api, stub)
			registerHumaTidingUpdate(api, stub)
			registerHumaTidingDelete(api, stub)
			return nil
		}},
		{"/v1", func(api huma.API) error {
			stub := handlers.ErrandSpecStub()
			registerHumaErrandList(api, stub)
			registerHumaErrandGet(api, stub)
			registerHumaErrandCancel(api, stub)
			return nil
		}},
		// push-runs is mounted directly on /v1 (outside r.Route("/push")) — the full path
		// /push-runs is in the Operation, a separate group from /v1/push apply/get.
		{"/v1", func(api huma.API) error {
			registerHumaPushRunsList(api, handlers.PushSpecStub())
			return nil
		}},

		// auth.* — federated authentication OUTSIDE /v1 (ADR-058): a group on
		// r.Route("/auth"), Operation.Path is relative (/ldap/login) → the full URL is
		// /auth/ldap/login. prefix "/auth" so it lands in the committed openapi.yaml.
		{"/auth", func(api huma.API) error {
			registerHumaLDAPLogin(api, ldapAuthSpecStub())
			return nil
		}},
		// OIDC endpoints (ADR-058 stage 2): /auth/oidc/{login,callback}. A separate
		// group from LDAP, so the huma-API does not share operations — each domain dumps
		// its own paths (LDAP-POST and OIDC-GET do not overlap by path).
		{"/auth", func(api huma.API) error {
			registerHumaOIDCLogin(api, oidcAuthSpecStub())
			return nil
		}},
	}
}

// schemaCollisionError — two domains produced a schema with the SAME name but a DIFFERENT body.
// A naive merge would silently overwrite one with the other (a broken spec); pilot-gate (b)
// detects this and stops the build (needs_architect: how to namespace it).
type schemaCollisionError struct {
	name  string
	bodyA string
	bodyB string
}

func (e *schemaCollisionError) Error() string {
	return fmt.Sprintf("schema %q: name collision with DIFFERENT bodies between domains (cannot silently dedup)\n--- variant A ---\n%s\n--- variant B ---\n%s",
		e.name, e.bodyA, e.bodyB)
}

// pathMethodCollisionError — two domains produced an operation on the SAME full path+method
// after prefixing (pilot-gate (a)).
type pathMethodCollisionError struct {
	method string
	path   string
}

func (e *pathMethodCollisionError) Error() string {
	return fmt.Sprintf("operation %s %s declared twice after prefixing (path+method collision between domains)", e.method, e.path)
}

// buildFullOpenAPISpec assembles a single huma.OpenAPI object from all registration
// groups via A2-bis. Returns an error on a path+method collision (gate a) or on a
// schema-name collision with a different body (gate b — needs_architect signal).
//
// The spec does NOT need middleware (audit/RBAC/Toll) — only operations/schemas: each
// group is dumped onto a "bare" newHumaCadenceAPI (without audit wiring). installHuma-
// ErrorOverride is called so the error-response schemas (HumaProblemError) match
// the served form.
func buildFullOpenAPISpec() (*huma.OpenAPI, error) {
	installHumaErrorOverride()

	full := newHumaCadenceAPI(chi.NewRouter()).OpenAPI()
	full.Paths = map[string]*huma.PathItem{}
	if full.Components == nil {
		full.Components = &huma.Components{}
	}

	// The bearerAuth securityScheme + the global security requirement. This is SCHEMA-ONLY:
	// wire-auth is already in RequireJWT (router.go), this is just a declaration for the viewer —
	// RapiDoc "Try It" reads components.securitySchemes + security and sends
	// Authorization: Bearer (the /docs page prefills the JWT via setApiKey('bearerAuth')).
	// The global (top-level) requirement covers ALL operations in the spec; all of them are
	// /v1 (the meta routes /healthz/openapi.yaml/docs are not part of the spec), and /v1 as a whole
	// is behind JWT — so per-operation security is not needed.
	if full.Components.SecuritySchemes == nil {
		full.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	full.Components.SecuritySchemes[bearerSecuritySchemeName] = &huma.SecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  "Archon JWT (Authorization: Bearer <jwt>). All /v1 operations require a valid token.",
	}
	full.Security = []map[string][]string{{bearerSecuritySchemeName: {}}}
	// The base Registry schema of the full spec is filled in below from the per-group dumps;
	// we keep its map for merge collision detection.
	fullSchemas := full.Components.Schemas.Map()

	// Tag dedup by name (Operation.Tags reference them by string).
	tagSeen := map[string]struct{}{}
	for _, t := range full.Tags {
		tagSeen[t.Name] = struct{}{}
	}

	for _, g := range fullSpecGroups() {
		// Each group is dumped onto its OWN temporary huma.API (isolation: operations
		// of different groups with the same relative path "/" do not collide on
		// one API). Prefixing during merge splits them apart by full URL.
		subAPI := newHumaCadenceAPI(chi.NewRouter())
		if err := g.register(subAPI); err != nil {
			return nil, err
		}
		if err := mergeGroup(full, fullSchemas, tagSeen, g.prefix, subAPI.OpenAPI()); err != nil {
			return nil, err
		}
	}

	return full, nil
}

// mergeGroup merges the paths/schemas/tags of one group (sub) into full, shifting
// the paths keys by prefix. Detects both pilot-gate collisions.
func mergeGroup(full *huma.OpenAPI, fullSchemas map[string]*huma.Schema, tagSeen map[string]struct{}, prefix string, sub *huma.OpenAPI) error {
	// paths: shift the key by prefix; "/" → prefix itself.
	for rel, item := range sub.Paths {
		abs := joinPrefix(prefix, rel)
		dst, exists := full.Paths[abs]
		if !exists {
			full.Paths[abs] = item
			continue
		}
		// The same full path already exists from another group (e.g. /v1/incarnations/{name}
		// from the incarnation- and choir-groups does not collide on different sub-paths, but
		// a shared abs is possible) — merge operations by method, WITHOUT overwriting item
		// wholesale; a collision on one method → gate (a) error.
		if err := mergeOps(abs, dst, item); err != nil {
			return err
		}
	}

	// components.schemas: the same name + identical body → dedup; otherwise a collision.
	for name, sch := range sub.Components.Schemas.Map() {
		prev, exists := fullSchemas[name]
		if !exists {
			fullSchemas[name] = sch
			continue
		}
		ab, err := yamlMarshalSchema(prev)
		if err != nil {
			return err
		}
		bb, err := yamlMarshalSchema(sch)
		if err != nil {
			return err
		}
		if ab != bb {
			return &schemaCollisionError{name: name, bodyA: ab, bodyB: bb}
		}
		// identical — dedup (do nothing).
	}

	// tags: dedup by name.
	for _, t := range sub.Tags {
		if _, seen := tagSeen[t.Name]; seen {
			continue
		}
		tagSeen[t.Name] = struct{}{}
		full.Tags = append(full.Tags, t)
	}

	return nil
}

// mergeOps merges the operations of src-PathItem into dst by HTTP method. A method already
// occupied in dst is a path+method collision (gate a). Covers the rare case of one full
// path coming from two different registration groups.
func mergeOps(path string, dst, src *huma.PathItem) error {
	type slot struct {
		get func() *huma.Operation
		set func(*huma.Operation)
		m   string
	}
	slots := []slot{
		{func() *huma.Operation { return src.Get }, func(o *huma.Operation) { dst.Get = o }, "GET"},
		{func() *huma.Operation { return src.Put }, func(o *huma.Operation) { dst.Put = o }, "PUT"},
		{func() *huma.Operation { return src.Post }, func(o *huma.Operation) { dst.Post = o }, "POST"},
		{func() *huma.Operation { return src.Delete }, func(o *huma.Operation) { dst.Delete = o }, "DELETE"},
		{func() *huma.Operation { return src.Options }, func(o *huma.Operation) { dst.Options = o }, "OPTIONS"},
		{func() *huma.Operation { return src.Head }, func(o *huma.Operation) { dst.Head = o }, "HEAD"},
		{func() *huma.Operation { return src.Patch }, func(o *huma.Operation) { dst.Patch = o }, "PATCH"},
		{func() *huma.Operation { return src.Trace }, func(o *huma.Operation) { dst.Trace = o }, "TRACE"},
	}
	dstOps := pathItemOps(dst)
	for _, s := range slots {
		op := s.get()
		if op == nil {
			continue
		}
		if dstOps[s.m] != nil {
			return &pathMethodCollisionError{method: s.m, path: path}
		}
		s.set(op)
	}
	return nil
}

// pathItemOps unpacks a PathItem into map[METHOD]*Operation for iteration/detection.
// nil-item → an empty map (safe for the merge check).
func pathItemOps(item *huma.PathItem) map[string]*huma.Operation {
	if item == nil {
		return map[string]*huma.Operation{}
	}
	ops := map[string]*huma.Operation{}
	if item.Get != nil {
		ops["GET"] = item.Get
	}
	if item.Put != nil {
		ops["PUT"] = item.Put
	}
	if item.Post != nil {
		ops["POST"] = item.Post
	}
	if item.Delete != nil {
		ops["DELETE"] = item.Delete
	}
	if item.Options != nil {
		ops["OPTIONS"] = item.Options
	}
	if item.Head != nil {
		ops["HEAD"] = item.Head
	}
	if item.Patch != nil {
		ops["PATCH"] = item.Patch
	}
	if item.Trace != nil {
		ops["TRACE"] = item.Trace
	}
	return ops
}

// joinPrefix glues a huma operation's relative path to the group's chi prefix.
// rel=="/" (the group root) → prefix itself (POST /v1/roles, not /v1/roles/).
func joinPrefix(prefix, rel string) string {
	if rel == "/" {
		return prefix
	}
	return prefix + rel
}

// HumaFullSpecYAML returns the single aggregated 3.1 spec of all domains as a YAML
// string. The entry point for the future served mechanism (T4c) and the pilot proof-test.
func HumaFullSpecYAML() (string, error) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		return "", err
	}
	// The determinism of huma's YAML output depends on map traversal (paths/schemas) — for
	// guard comparisons we serialize as-is; huma performs stable sorting of YAML keys
	// itself during marshaling (map keys are sorted).
	y, err := spec.YAML()
	if err != nil {
		return "", err
	}
	return string(y), nil
}
