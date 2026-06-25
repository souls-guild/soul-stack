// Агрегатный structural guard против рецидива audit-регрессии (история S6-моста).
//
// ПРОБЛЕМА. Аудит каждого мутирующего /v1-роута защищён РОССЫПЬЮ per-domain тестов
// (*_RecordsOnSuccess / *_NoAudit в huma_<домен>_test.go). Единого инварианта
// «множество write-роутов ⊆ множество audit-покрытых роутов» НЕТ → новый write-
// домен БЕЗ audit-навески собирается молча и проходит сборку (именно так дал
// крит-регрессию S6: write-роут есть, audit-emit нет, per-domain теста ещё нет).
//
// МЕХАНИЗМ ДЕТЕКЦИИ — декларативный реестр (вариант «в» ТЗ), а НЕ структурная
// инспекция chi-цепочки. Почему НЕ structural:
//
//   - audit на huma-доменах навешивается через api.UseMiddleware(humaAuditMiddleware)
//     ВНУТРИ huma.API (newHumaAuditAPI / newHuma<Domain>API), а НЕ как chi-middleware.
//     chi.Walk отдаёт 4-м аргументом ТОЛЬКО chi-цепочку роута — audit-навеска huma
//     в неё не попадает. Структурно «есть ли audit-middleware» из chi-дерева не видно.
//   - часть write-роутов пишут self-audit ВНУТРИ handler-а (*Typed → auditW.Write),
//     вообще без middleware (incarnation rerun/destroy/…, choir, voyage, cadence
//     patch/delete/…). Structural-проверка «middleware навешан» их бы ложно завалила.
//   - и ключевой урок S6: «middleware навешан» ≠ «audit пишет». Наличие узла в цепочке
//     НЕ доказывает запись (S6-bridge перехватывал ResponseWriter ДО рекордера —
//     middleware был, запись молча терялась). Декларативный реестр заставляет инженера
//     при добавлении write-роута ОСОЗНАННО внести его в auditedWriteRoutes (привязав к
//     event-типам, которые per-domain *_RecordsOnSuccess-тест уже доказывает пишущимися)
//     ИЛИ в writeRoutesNoAudit с обоснованием. Любой не учтённый write-роут краснит.
//
// ИСТОЧНИК ПОЛНОГО МНОЖЕСТВА write-роутов — buildFullOpenAPISpec() (а НЕ collectRoutes):
// full-spec содержит ВСЕ домены, включая opt-in (voyage/cadence/herald/push/errand/
// choir/audit), которые drift-router монтирует с handler=nil. collectRoutes их не
// увидел бы → дыра в покрытии guard-а.
//
// ДВУХУРОВНЕВАЯ ЗАЩИТА (этот тест + per-domain): здесь — «write-роут НЕ забыт в
// audit-топологии» (полнота множества); per-domain *_RecordsOnSuccess — «event
// реально пишется на 2xx» (S6-инвариант «пишет, не только навешан»). Один без
// другого дыряв: реестр без per-domain не ловит молчащий middleware; per-domain без
// реестра не ловит совсем не покрытый новый роут.

package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// auditedRoute — декларация audit-покрытия одного write-роута. events — множество
// event-типов, ОДИН из которых роут пишет на успехе (>1 — kind-зависимый выбор, как
// voyage scenario/command). Привязка к константам audit.Event* связывает реестр с
// per-domain *_RecordsOnSuccess-тестами (те доказывают, что эти event-ы реально
// пишутся). note — пояснение к нетривиальным (self-audit / kind-зависимым) случаям.
type auditedRoute struct {
	events []audit.EventType
	note   string
}

// auditedWriteRoutes — write-роуты, КАЖДЫЙ из которых обязан писать audit-event на
// успехе. Источник — router.go (топология buildRouter); добавление write-роута без
// строки тут краснит тест.
//
// Класс A (middleware-audit, вариант B ADR-054): audit пишет humaAuditMiddleware,
// навешанный newHuma<Domain>API(evt). Класс B (self-audit): audit пишет сам handler
// внутри *Typed (newHumaCadenceAPI, без middleware). Для guard-полноты они
// неразличимы — оба обязаны emit-ить и оба здесь.
var auditedWriteRoutes = map[route]auditedRoute{
	// operators (middleware-audit).
	{http.MethodPost, "/v1/operators"}:                   {events: []audit.EventType{audit.EventOperatorCreated}},
	{http.MethodPost, "/v1/operators/{aid}/revoke"}:      {events: []audit.EventType{audit.EventOperatorRevoked}},
	{http.MethodPost, "/v1/operators/{aid}/issue-token"}: {events: []audit.EventType{audit.EventOperatorTokenIssued}},

	// auth (self-audit внутри handler-а: login пишет operator.login после выпуска
	// JWT, ВНЕ /v1, без middleware-audit-навески — ADR-058). provision (operator.
	// provisioned) пишет Mapper, не endpoint — у этого роута одно login-событие.
	{http.MethodPost, "/auth/ldap/login"}: {events: []audit.EventType{audit.EventOperatorLogin}, note: "self-audit: handler пишет operator.login после выпуска JWT (ADR-058, вне /v1)"},

	// roles (middleware-audit).
	{http.MethodPost, "/v1/roles"}:                          {events: []audit.EventType{audit.EventRoleCreated}},
	{http.MethodDelete, "/v1/roles/{name}"}:                 {events: []audit.EventType{audit.EventRoleDeleted}},
	{http.MethodPatch, "/v1/roles/{name}/permissions"}:      {events: []audit.EventType{audit.EventRolePermissionsUpdated}},
	{http.MethodPost, "/v1/roles/{name}/operators"}:         {events: []audit.EventType{audit.EventRoleOperatorGranted}},
	{http.MethodDelete, "/v1/roles/{name}/operators/{aid}"}: {events: []audit.EventType{audit.EventRoleOperatorRevoked}},

	// synods (middleware-audit).
	{http.MethodPost, "/v1/synods"}:                            {events: []audit.EventType{audit.EventSynodCreated}},
	{http.MethodPatch, "/v1/synods/{name}"}:                    {events: []audit.EventType{audit.EventSynodUpdated}},
	{http.MethodDelete, "/v1/synods/{name}"}:                   {events: []audit.EventType{audit.EventSynodDeleted}},
	{http.MethodPost, "/v1/synods/{name}/operators"}:           {events: []audit.EventType{audit.EventSynodOperatorAdded}},
	{http.MethodDelete, "/v1/synods/{name}/operators/{aid}"}:   {events: []audit.EventType{audit.EventSynodOperatorRemoved}},
	{http.MethodPost, "/v1/synods/{name}/roles"}:               {events: []audit.EventType{audit.EventSynodRoleGranted}},
	{http.MethodDelete, "/v1/synods/{name}/roles/{role_name}"}: {events: []audit.EventType{audit.EventSynodRoleRevoked}},

	// incarnations — MIXED audit-класс (middleware create/run/unlock/upgrade +
	// self-audit rerun/check-drift/destroy/update-hosts), все обязаны emit-ить.
	{http.MethodPost, "/v1/incarnations"}:                             {events: []audit.EventType{audit.EventIncarnationCreated}},
	{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}"}: {events: []audit.EventType{audit.EventIncarnationScenarioStarted}},
	{http.MethodPost, "/v1/incarnations/{name}/unlock"}:               {events: []audit.EventType{audit.EventIncarnationUnlocked}},
	{http.MethodPost, "/v1/incarnations/{name}/upgrade"}:              {events: []audit.EventType{audit.EventIncarnationUpgradeStarted}},
	{http.MethodPost, "/v1/incarnations/{name}/rerun-create"}:         {events: []audit.EventType{audit.EventIncarnationCreateRerun}, note: "self-audit: handler пишет внутри RerunCreateTyped"},
	{http.MethodPost, "/v1/incarnations/{name}/check-drift"}:          {events: []audit.EventType{audit.EventIncarnationDriftChecked}, note: "self-audit: handler пишет внутри CheckDriftTyped"},
	{http.MethodDelete, "/v1/incarnations/{name}"}:                    {events: []audit.EventType{audit.EventIncarnationDestroyStarted}, note: "self-audit: destroy_started пишет service-слой incarnation.Destroy"},
	{http.MethodPatch, "/v1/incarnations/{name}/hosts"}:               {events: []audit.EventType{audit.EventIncarnationHostsUpdated}, note: "self-audit: handler пишет внутри UpdateHostsTyped"},

	// choir (self-audit внутри *Typed через writeAuditCtx).
	{http.MethodPost, "/v1/incarnations/{name}/choirs"}:                        {events: []audit.EventType{audit.EventChoirCreated}, note: "self-audit"},
	{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}"}:              {events: []audit.EventType{audit.EventChoirDeleted}, note: "self-audit"},
	{http.MethodPost, "/v1/incarnations/{name}/choirs/{choir}/voices"}:         {events: []audit.EventType{audit.EventChoirVoiceAdded}, note: "self-audit"},
	{http.MethodDelete, "/v1/incarnations/{name}/choirs/{choir}/voices/{sid}"}: {events: []audit.EventType{audit.EventChoirVoiceRemoved}, note: "self-audit"},

	// souls (middleware-audit; exec → errand.invoked, middleware + dispatcher).
	{http.MethodPost, "/v1/souls"}:                   {events: []audit.EventType{audit.EventSoulCreated}},
	{http.MethodPost, "/v1/souls/coven"}:             {events: []audit.EventType{audit.EventSoulCovenChanged}},
	{http.MethodPost, "/v1/souls/traits"}:            {events: []audit.EventType{audit.EventSoulTraitsChanged}},
	{http.MethodPost, "/v1/souls/{sid}/issue-token"}: {events: []audit.EventType{audit.EventSoulTokenIssued}},
	{http.MethodPut, "/v1/souls/{sid}/ssh-target"}:   {events: []audit.EventType{audit.EventSoulSshTargetUpdated}},
	{http.MethodPost, "/v1/souls/{sid}/exec"}:        {events: []audit.EventType{audit.EventTypeErrandInvoked}},

	// plugins/sigils (middleware-audit).
	{http.MethodPost, "/v1/plugins/sigils"}:                            {events: []audit.EventType{audit.EventPluginAllowed}},
	{http.MethodDelete, "/v1/plugins/sigils/{namespace}/{name}/{ref}"}: {events: []audit.EventType{audit.EventPluginRevoked}},

	// sigil/keys (middleware-audit).
	{http.MethodPost, "/v1/sigil/keys"}:                  {events: []audit.EventType{audit.EventSigilKeyIntroduced}},
	{http.MethodPost, "/v1/sigil/keys/{key_id}/primary"}: {events: []audit.EventType{audit.EventSigilKeyPrimarySet}},
	{http.MethodDelete, "/v1/sigil/keys/{key_id}"}:       {events: []audit.EventType{audit.EventSigilKeyRetired}},

	// services (middleware-audit).
	{http.MethodPost, "/v1/services"}:          {events: []audit.EventType{audit.EventServiceRegistered}},
	{http.MethodPatch, "/v1/services/{name}"}:  {events: []audit.EventType{audit.EventServiceUpdated}},
	{http.MethodDelete, "/v1/services/{name}"}: {events: []audit.EventType{audit.EventServiceDeregistered}},

	// provisioning-policy (middleware-audit; PUT мутирующий, GET — read). ADR-058 Часть B.
	{http.MethodPut, "/v1/provisioning-policy"}: {events: []audit.EventType{audit.EventProvisioningPolicyChanged}},

	// augur (middleware-audit).
	{http.MethodPost, "/v1/augur/omens"}:          {events: []audit.EventType{audit.EventOmenCreated}},
	{http.MethodDelete, "/v1/augur/omens/{name}"}: {events: []audit.EventType{audit.EventOmenRevoked}},
	{http.MethodPost, "/v1/augur/rites"}:          {events: []audit.EventType{audit.EventRiteCreated}},
	{http.MethodDelete, "/v1/augur/rites/{id}"}:   {events: []audit.EventType{audit.EventRiteRevoked}},

	// oracle (middleware-audit).
	{http.MethodPost, "/v1/vigils"}:           {events: []audit.EventType{audit.EventVigilCreated}},
	{http.MethodDelete, "/v1/vigils/{name}"}:  {events: []audit.EventType{audit.EventVigilDeleted}},
	{http.MethodPost, "/v1/decrees"}:          {events: []audit.EventType{audit.EventDecreeCreated}},
	{http.MethodDelete, "/v1/decrees/{name}"}: {events: []audit.EventType{audit.EventDecreeDeleted}},

	// push (middleware-audit; apply мутирующий, GET — read).
	{http.MethodPost, "/v1/push/apply"}: {events: []audit.EventType{audit.EventPushApplied}},

	// push-providers (middleware-audit).
	{http.MethodPost, "/v1/push-providers"}:          {events: []audit.EventType{audit.EventPushProviderCreated}},
	{http.MethodPut, "/v1/push-providers/{name}"}:    {events: []audit.EventType{audit.EventPushProviderUpdated}},
	{http.MethodDelete, "/v1/push-providers/{name}"}: {events: []audit.EventType{audit.EventPushProviderDeleted}},

	// heralds + tidings (middleware-audit).
	{http.MethodPost, "/v1/heralds"}:          {events: []audit.EventType{audit.EventHeraldCreated}},
	{http.MethodPut, "/v1/heralds/{name}"}:    {events: []audit.EventType{audit.EventHeraldUpdated}},
	{http.MethodDelete, "/v1/heralds/{name}"}: {events: []audit.EventType{audit.EventHeraldDeleted}},
	{http.MethodPost, "/v1/tidings"}:          {events: []audit.EventType{audit.EventTidingCreated}},
	{http.MethodPut, "/v1/tidings/{name}"}:    {events: []audit.EventType{audit.EventTidingUpdated}},
	{http.MethodDelete, "/v1/tidings/{name}"}: {events: []audit.EventType{audit.EventTidingDeleted}},

	// errands (cancel — middleware-audit; POST exec живёт под /v1/souls/{sid}/exec).
	{http.MethodDelete, "/v1/errands/{errand_id}"}: {events: []audit.EventType{audit.EventTypeErrandCancelled}},

	// voyages — KIND-зависимый self-audit (RBAC-by-kind, ADR-043 §6): create пишет
	// scenario_run.started ИЛИ command_run.invoked по kind рецепта; cancel —
	// scenario_run.cancelled ИЛИ command_run.cancelled. preview — read-like (см.
	// writeRoutesNoAudit).
	{http.MethodPost, "/v1/voyages"}:        {events: []audit.EventType{audit.EventScenarioRunStarted, audit.EventCommandRunInvoked}, note: "self-audit, kind-зависимый: scenario→started / command→invoked"},
	{http.MethodDelete, "/v1/voyages/{id}"}: {events: []audit.EventType{audit.EventScenarioRunCancelled, audit.EventCommandRunCancelled}, note: "self-audit, kind-зависимый: scenario/command cancelled"},

	// cadences — self-audit внутри *Typed. enable/disable пишут cadence.updated
	// (toggle через update-event, отдельных enabled/disabled-event нет).
	{http.MethodPost, "/v1/cadences"}:              {events: []audit.EventType{audit.EventCadenceCreated}, note: "self-audit: handler пишет внутри CreateTyped (kind-permission внутри handler, ADR-046 §7)"},
	{http.MethodPatch, "/v1/cadences/{id}"}:        {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit"},
	{http.MethodDelete, "/v1/cadences/{id}"}:       {events: []audit.EventType{audit.EventCadenceDeleted}, note: "self-audit"},
	{http.MethodPost, "/v1/cadences/{id}/enable"}:  {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit: enable/disable пишут cadence.updated (toggle)"},
	{http.MethodPost, "/v1/cadences/{id}/disable"}: {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit: enable/disable пишут cadence.updated (toggle)"},
}

// writeRoutesNoAudit — ОСОЗНАННЫЕ исключения: write-роуты, которым audit НЕ положен,
// каждый с обоснованием ПОЧЕМУ (иначе исключение = дыра, а не решение). Пусто = «нет
// write-роута без audit»; любое добавление сюда — явное архитектурное решение под ревью.
var writeRoutesNoAudit = map[route]string{
	// POST /v1/voyages/preview — dry-resolve scope БЕЗ создания Voyage и БЕЗ мутации
	// состояния (ADR-043 amendment §4). POST по HTTP-методу, но read-like по семантике
	// (ничего не меняет) → audit не пишется намеренно (паритет GET-read). Это
	// ЕДИНСТВЕННЫЙ write-метод-без-мутации в API; держим явно, чтобы guard не требовал
	// от него audit-event, которого по дизайну нет.
	{http.MethodPost, "/v1/voyages/preview"}: "ADR-043 amendment §4: dry-resolve scope без создания Voyage и без мутации состояния — read-like POST, audit намеренно не пишется (паритет GET-read)",

	// POST /v1/modules/{name}/form-prep — резолвер source-каталогов UI-формы модуля
	// (ADR-045 S3): по incarnation_hosts/choir отдаёт живые SID-ы для автокомплита
	// формы Run→Command. POST по HTTP-методу (несёт тело-фильтр), но read-only-резолв
	// по семантике — ничего не мутирует (router.go: «Без audit, паттерн soul.list /
	// service.list»). audit намеренно не пишется.
	{http.MethodPost, "/v1/modules/{name}/form-prep"}: "ADR-045 S3: read-only-резолв source-каталогов UI-формы (живые SID-ы), без мутации состояния — audit намеренно не пишется (паттерн soul.list/service.list)",

	// POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill — day-2
	// pre-fill UI-формы сценария из incarnation.state (docs/input.md). POST по
	// HTTP-методу (несёт опц. тело-ref), но read-only-резолв по семантике —
	// читает state одной инкарнации, ничего не мутирует. Permission
	// incarnation.get (паттерн read). audit намеренно не пишется.
	{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}/form-prefill"}: "day-2 pre-fill формы из incarnation.state (docs/input.md): read-only-резолв одной инкарнации, без мутации — audit намеренно не пишется (паттерн get/module.form-prep)",
}

// writeMethods — HTTP-методы, считающиеся мутирующими для guard-а.
var writeMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// TestAuditCompleteness_AllWriteRoutesCovered — агрегатный declarative guard.
//
// Берёт ПОЛНОЕ множество write-роутов из buildFullOpenAPISpec (все домены, вкл.
// opt-in) и утверждает: каждый write-роут либо в auditedWriteRoutes (обязан писать
// audit, event подтверждается per-domain *_RecordsOnSuccess), либо в writeRoutesNoAudit
// (осознанное исключение с обоснованием). Любой write-роут вне обоих множеств = новый
// write-роут без явного решения по audit → краснит (рецидив S6 не пройдёт молча).
//
// Заодно ловит обратный дрейф: запись в реестре, которой больше нет соответствующего
// write-роута в спеке (устаревшая декларация — реестр должен зеркалить топологию).
func TestAuditCompleteness_AllWriteRoutesCovered(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Полное множество write-роутов спеки.
	specWrites := map[route]struct{}{}
	for path, item := range spec.Paths {
		for method := range pathItemOps(item) {
			if _, isWrite := writeMethods[method]; !isWrite {
				continue
			}
			specWrites[route{method: method, path: normalizePath(path)}] = struct{}{}
		}
	}
	if len(specWrites) == 0 {
		t.Fatal("в собранной спеке нет ни одного write-роута — спека пуста или write-методы не распознаны?")
	}

	// (1) Каждый write-роут спеки покрыт ровно одним из двух реестров.
	var uncovered []string
	for r := range specWrites {
		ar, audited := auditedWriteRoutes[r]
		_, exempt := writeRoutesNoAudit[r]
		switch {
		case audited && exempt:
			t.Errorf("write-роут %s одновременно в auditedWriteRoutes и writeRoutesNoAudit — реестры обязаны быть непересекающимися", r)
		case audited && len(ar.events) == 0:
			t.Errorf("write-роут %s в auditedWriteRoutes без единого event-типа — привязка к audit.Event* обязательна (связь с per-domain *_RecordsOnSuccess)", r)
		case !audited && !exempt:
			uncovered = append(uncovered, r.String())
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("WRITE-РОУТ БЕЗ РЕШЕНИЯ ПО AUDIT — %d (рецидив S6: новый мутирующий роут без audit-навески прошёл бы молча):\n  %s\n"+
			"→ внеси КАЖДЫЙ в auditedWriteRoutes (привязав к audit.Event*, который per-domain *_RecordsOnSuccess-тест доказывает пишущимся) "+
			"ИЛИ в writeRoutesNoAudit с обоснованием ПОЧЕМУ audit не нужен.",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}

	// (2) Обратный дрейф: декларация в реестре без реального write-роута в спеке.
	var stale []string
	for r := range auditedWriteRoutes {
		if _, ok := specWrites[r]; !ok {
			stale = append(stale, "auditedWriteRoutes: "+r.String())
		}
	}
	for r := range writeRoutesNoAudit {
		if _, ok := specWrites[r]; !ok {
			stale = append(stale, "writeRoutesNoAudit: "+r.String())
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("УСТАРЕВШАЯ ДЕКЛАРАЦИЯ — %d записей в audit-реестрах без соответствующего write-роута в спеке (реестр обязан зеркалить топологию buildRouter):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}

	t.Logf("guard: %d write-роутов покрыто (%d audited, %d осознанных no-audit исключений)",
		len(specWrites), len(auditedWriteRoutes), len(writeRoutesNoAudit))
}
