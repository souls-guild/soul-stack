package api

// Агрегатор единой huma-OpenAPI-спеки всех доменов Operator API. Цель — «правда в
// коде»: одна валидная 3.1-спека, собранная runtime-дампом huma-операций из кода
// (FastAPI-стиль), без committed-YAML как источника. Точка входа сборки —
// HumaFullSpecYAML (ниже). Served-механизм уже переключён на этот агрегатор (T4c):
// GET /openapi.yaml отдаёт runtime-дамп через servedOpenAPIHandler (router.go),
// meta-embed-пакет удалён; committed docs/keeper/openapi.yaml — производный генерат
// (make gen-openapi), сверяется с дампом гейтом check-openapi.
//
// МЕХАНИЗМ (A2-bis, architect-рекомендация — НЕ менять huma.Operation):
// per-домен dump уже существует (HumaXSpecYAML через humaDumpSpec на временном
// chi-роутере). Проблема — двойная path-конвенция: у БОЛЬШИНСТВА доменов
// huma-Operation.Path ОТНОСИТЕЛЕН chi-группе (`/`, `/{name}`), полный URL даёт
// chi-mount-префикс; у oracle/augur/herald/errand/catalog/audit/push-runs Path уже
// полный под /v1. Наивный merge на одну huma.API → коллизии path+method (множество
// «POST /» от разных групп). A2-bis: каждую РЕГИСТРАЦИОННУЮ ГРУППУ дампим на СВОЮ
// huma.API, СДВИГАЕМ её paths-ключи на per-группу chi-префикс (тот же, что в
// buildRouter), затем мержим paths + components.schemas + tags в одну 3.1-спеку.
//
// Единица сборки — РЕГИСТРАЦИОННАЯ ГРУППА (prefix + набор register-функций), а НЕ
// «домен», потому что префикс — свойство chi-mount-а группы, не домена: push
// смешивает /v1/push (apply/get) и /v1 (push-runs) в одном HumaPushSpecYAML, поэтому
// разбит на две группы. Список specGroups зеркалит топологию buildRouter — это и есть
// будущий drift-guard (агрегатор не должен забыть домен).

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	yaml "gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// bearerSecuritySchemeName — имя securityScheme (http/bearer JWT) в собранной
// спеке. Vьювер /docs читает его для «Try It»; глобальное security-требование
// ссылается на него же. Имя стандартное для bearer-JWT в OpenAPI-экосистеме.
const bearerSecuritySchemeName = "bearerAuth"

// yamlMarshalSchema сериализует *huma.Schema в каноничную YAML-строку для
// побайтового сравнения тел одноимённых схем при merge (детект коллизии гейта б).
func yamlMarshalSchema(s *huma.Schema) (string, error) {
	b, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// specGroup — одна регистрационная группа: chi-префикс mount-а (как в buildRouter)
// + замыкатель, регистрирующий huma-операции этой группы на переданную API.
// prefix="/v1" — операции уже несут полный под-/v1 путь (oracle/augur/herald/…);
// prefix="/v1/<x>" — операции относительны chi-группе r.Route("/<x>").
type specGroup struct {
	prefix   string
	register func(huma.API) error
}

// fullSpecGroups — полный набор регистрационных групп Operator API в топологии
// buildRouter. Источник истины для агрегатора; добавление домена в router без
// строки здесь ловит TestFullSpec_CoversAllRoutes (drift-guard).
//
// Префиксы выверены по router.go (chi-mount каждой группы):
//   - r.Route("/<x>") + относительный Operation.Path  → prefix "/v1/<x>"
//   - группа на /v1 c полным под-/v1 Operation.Path    → prefix "/v1"
//
// Особые случаи:
//   - choir смонтирован на группе /v1/incarnations (Operation.Path = /{name}/choirs/…)
//     → prefix "/v1/incarnations" (НЕ "/v1/choirs").
//   - push разбит: apply/get относительны /v1/push; push-runs — полный /v1.
func fullSpecGroups() []specGroup {
	return []specGroup{
		// Относительные группы r.Route("/<x>").
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
			registerHumaIncarnationFormPrefill(api, stub)
			registerHumaIncarnationHistory(api, stub)
			registerHumaIncarnationRuns(api, stub)
			registerHumaIncarnationRunDetail(api, stub)
			registerHumaIncarnationRun(api, stub)
			registerHumaIncarnationUnlock(api, stub)
			registerHumaIncarnationUpgrade(api, stub)
			registerHumaIncarnationRerunLast(api, stub)
			registerHumaIncarnationCheckDrift(api, stub)
			registerHumaIncarnationDestroy(api, stub)
			registerHumaIncarnationUpdateHosts(api, stub)
			registerHumaIncarnationSetTraits(api, stub)
			return nil
		}},
		// choir смонтирован на группе /v1/incarnations, Operation.Path несёт
		// /{name}/choirs/… → тот же prefix /v1/incarnations.
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

		// Группы на /v1 — Operation.Path уже полный под-/v1 путь.
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
		// GET /v1/cluster — Operation.Path полный под-/v1 (/cluster).
		{"/v1", func(api huma.API) error {
			registerHumaClusterGet(api, handlers.ClusterSpecStub())
			return nil
		}},
		// augur смонтирован через r.Route("/augur") — Operation.Path относителен
		// (/omens, /rites), полный URL = /v1/augur/… (НЕ полный под-/v1 как oracle/
		// herald, которые навешаны прямо на /v1).
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
		// push-runs смонтирован прямо на /v1 (вне r.Route("/push")) — полный путь
		// /push-runs в Operation, отдельная группа от /v1/push apply/get.
		{"/v1", func(api huma.API) error {
			registerHumaPushRunsList(api, handlers.PushSpecStub())
			return nil
		}},

		// auth.* — федеративная аутентификация ВНЕ /v1 (ADR-058): группа на
		// r.Route("/auth"), Operation.Path относителен (/ldap/login) → полный URL
		// /auth/ldap/login. prefix "/auth", чтобы попасть в committed openapi.yaml.
		{"/auth", func(api huma.API) error {
			registerHumaLDAPLogin(api, ldapAuthSpecStub())
			return nil
		}},
		// OIDC-эндпоинты (ADR-058 стадия 2): /auth/oidc/{login,callback}. Отдельная
		// группа от LDAP, чтобы huma-API не делила операции — каждый домен дампит
		// свои пути (LDAP-POST и OIDC-GET не пересекаются по path).
		{"/auth", func(api huma.API) error {
			registerHumaOIDCLogin(api, oidcAuthSpecStub())
			return nil
		}},
	}
}

// schemaCollisionError — два домена дали схему с ОДНИМ именем, но РАЗНЫМ телом.
// При наивном merge один молча перезатёр бы другой (битая спека); pilot-гейт (б)
// детектит это и останавливает сборку (needs_architect: как namespace-ить).
type schemaCollisionError struct {
	name  string
	bodyA string
	bodyB string
}

func (e *schemaCollisionError) Error() string {
	return fmt.Sprintf("schema %q: коллизия имени с РАЗНЫМИ телами между доменами (нельзя молча дедуплицировать)\n--- вариант A ---\n%s\n--- вариант B ---\n%s",
		e.name, e.bodyA, e.bodyB)
}

// pathMethodCollisionError — два домена дали операцию на ОДНОМ полном пути+методе
// после префиксования (pilot-гейт (а)).
type pathMethodCollisionError struct {
	method string
	path   string
}

func (e *pathMethodCollisionError) Error() string {
	return fmt.Sprintf("операция %s %s объявлена дважды после префиксования (коллизия path+method между доменами)", e.method, e.path)
}

// buildFullOpenAPISpec собирает единый huma.OpenAPI-объект из всех регистрационных
// групп через A2-bis. Возвращает ошибку при коллизии path+method (гейт а) или при
// коллизии имени схемы с разным телом (гейт б — needs_architect-сигнал).
//
// Для спеки middleware (audit/RBAC/Toll) НЕ нужен — только операции/схемы: каждая
// группа дампится на «голую» newHumaCadenceAPI (без audit-навески). installHuma-
// ErrorOverride вызывается, чтобы error-response-схемы (HumaProblemError) совпадали
// с served-формой.
func buildFullOpenAPISpec() (*huma.OpenAPI, error) {
	installHumaErrorOverride()

	full := newHumaCadenceAPI(chi.NewRouter()).OpenAPI()
	full.Paths = map[string]*huma.PathItem{}
	if full.Components == nil {
		full.Components = &huma.Components{}
	}

	// bearerAuth securityScheme + глобальное security-требование. Это SCHEMA-ONLY:
	// wire-auth уже в RequireJWT (router.go), здесь лишь декларация для вьювера —
	// RapiDoc «Try It» читает components.securitySchemes + security и шлёт
	// Authorization: Bearer (страница /docs префиллит JWT через setApiKey('bearerAuth')).
	// Глобальное (top-level) требование покрывает ВСЕ операции спеки; все они —
	// /v1 (meta-роуты /healthz/openapi.yaml/docs в спеку не входят), а /v1 целиком
	// за JWT — поэтому per-операционное security не нужно.
	if full.Components.SecuritySchemes == nil {
		full.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	full.Components.SecuritySchemes[bearerSecuritySchemeName] = &huma.SecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  "Archon JWT (Authorization: Bearer <jwt>). Все /v1-операции требуют валидный токен.",
	}
	full.Security = []map[string][]string{{bearerSecuritySchemeName: {}}}
	// Базовая схема Registry full-спеки наполняется ниже из per-группа дампов;
	// сохраняем её карту для merge-детекции коллизий.
	fullSchemas := full.Components.Schemas.Map()

	// Дедупликация tags по имени (Operation.Tags ссылаются на них по строке).
	tagSeen := map[string]struct{}{}
	for _, t := range full.Tags {
		tagSeen[t.Name] = struct{}{}
	}

	for _, g := range fullSpecGroups() {
		// Каждая группа дампится на СВОЮ временную huma.API (изоляция: операции
		// разных групп с одинаковым относительным путём "/" не сталкиваются на
		// одной API). Префиксование при merge разводит их по полным URL.
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

// mergeGroup вливает paths/schemas/tags одной группы (sub) в full, сдвигая
// paths-ключи на prefix. Детектит обе коллизии pilot-гейта.
func mergeGroup(full *huma.OpenAPI, fullSchemas map[string]*huma.Schema, tagSeen map[string]struct{}, prefix string, sub *huma.OpenAPI) error {
	// paths: ключ сдвигаем на prefix; "/" → сам prefix.
	for rel, item := range sub.Paths {
		abs := joinPrefix(prefix, rel)
		dst, exists := full.Paths[abs]
		if !exists {
			full.Paths[abs] = item
			continue
		}
		// Тот же полный путь уже есть от другой группы (напр. /v1/incarnations/{name}
		// от incarnation- и choir-групп при разных под-путях не пересекается, но
		// общий abs возможен) — сливаем операции по методам, НЕ перезатирая item
		// целиком; коллизия одного метода → ошибка гейта (а).
		if err := mergeOps(abs, dst, item); err != nil {
			return err
		}
	}

	// components.schemas: одинаковое имя + идентичное тело → дедуп; иначе коллизия.
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
		// идентичны — дедуп (ничего не делаем).
	}

	// tags: дедуп по имени.
	for _, t := range sub.Tags {
		if _, seen := tagSeen[t.Name]; seen {
			continue
		}
		tagSeen[t.Name] = struct{}{}
		full.Tags = append(full.Tags, t)
	}

	return nil
}

// mergeOps вливает операции src-PathItem в dst по HTTP-методам. Метод, уже занятый
// в dst, — коллизия path+method (гейт а). Покрывает редкий случай одного полного
// пути от двух разных регистрационных групп.
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

// pathItemOps раскладывает PathItem в map[METHOD]*Operation для итерации/детекции.
// nil-item → пустая карта (безопасно для merge-проверки).
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

// joinPrefix приклеивает relative-путь huma-операции к chi-префиксу группы.
// rel=="/" (корень группы) → сам prefix (POST /v1/roles, не /v1/roles/).
func joinPrefix(prefix, rel string) string {
	if rel == "/" {
		return prefix
	}
	return prefix + rel
}

// HumaFullSpecYAML отдаёт единую агрегированную 3.1-спеку всех доменов как YAML-
// строку. Точка входа будущего served-механизма (T4c) и доказательного pilot-теста.
func HumaFullSpecYAML() (string, error) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		return "", err
	}
	// Детерминизм YAML-вывода huma зависит от обхода map (paths/schemas) — для
	// guard-сравнений сериализуем как есть; стабильную сортировку YAML-ключей
	// huma выполняет сам при маршалинге (map-ключи сортируются).
	y, err := spec.YAML()
	if err != nil {
		return "", err
	}
	return string(y), nil
}
