// Aggregate structural guard against audit-regression recurrence (S6-bridge history).
//
// PROBLEM. Audit of every mutating /v1 route is protected by a SCATTER of per-domain
// tests (*_RecordsOnSuccess / *_NoAudit in huma_<domain>_test.go). There is NO single
// invariant "set of write routes ⊆ set of audit-covered routes" → a new write domain
// with no audit wiring builds silently and passes the build (exactly how S6 got its
// critical regression: write route exists, no audit emit, no per-domain test yet).
//
// DETECTION MECHANISM — a declarative registry (option "c" of the spec), NOT structural
// inspection of the chi chain. Why NOT structural:
//
//   - audit on huma domains is wired via api.UseMiddleware(humaAuditMiddleware) INSIDE
//     huma.API (newHumaAuditAPI / newHuma<Domain>API), NOT as chi middleware. chi.Walk
//     hands back ONLY the route's chi chain as its 4th arg — the huma audit wiring is
//     not in it. Structurally "is audit middleware wired" is invisible from the chi tree.
//   - some write routes write self-audit INSIDE the handler (*Typed → auditW.Write),
//     with no middleware at all (incarnation rerun/destroy/…, choir, voyage, cadence
//     patch/delete/…). A structural "middleware is wired" check would falsely fail them.
//   - and the key S6 lesson: "middleware wired" ≠ "audit writes". A node in the chain
//     does NOT prove the write (the S6 bridge intercepted the ResponseWriter BEFORE the
//     recorder — middleware was present, the write silently lost). The declarative
//     registry forces the engineer, when adding a write route, to CONSCIOUSLY list it in
//     auditedWriteRoutes (bound to the event types a per-domain *_RecordsOnSuccess test
//     already proves are written) OR in writeRoutesNoAudit with a rationale. Any
//     unaccounted write route goes red.
//
// SOURCE OF THE FULL SET OF write routes — buildFullOpenAPISpec() (NOT collectRoutes):
// the full spec holds ALL domains, including opt-in (voyage/cadence/herald/push/errand/
// choir/audit), which the drift router mounts with handler=nil. collectRoutes would not
// see them → a hole in the guard's coverage.
//
// TWO-LEVEL DEFENCE (this test + per-domain): here — "write route is NOT forgotten in
// the audit topology" (set completeness); per-domain *_RecordsOnSuccess — "event is
// actually written on 2xx" (the S6 invariant "writes, not just wired"). One without the
// other is leaky: the registry without per-domain misses a silent middleware; per-domain
// without the registry misses an entirely uncovered new route.

package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// auditedRoute — declaration of audit coverage for one write route. events — the set of
// event types, ONE of which the route writes on success (>1 — kind-dependent choice, as in
// voyage scenario/command). Binding to the audit.Event* constants ties the registry to the
// per-domain *_RecordsOnSuccess tests (which prove those events are actually written).
// note — explanation for the non-trivial (self-audit / kind-dependent) cases.
type auditedRoute struct {
	events []audit.EventType
	note   string
}

// auditedWriteRoutes — write routes, EACH of which must write an audit event on success.
// Source — router.go (buildRouter topology); adding a write route without a line here
// goes red.
//
// Class A (middleware-audit, ADR-054 variant B): audit is written by humaAuditMiddleware
// wired by newHuma<Domain>API(evt). Class B (self-audit): audit is written by the handler
// itself inside *Typed (newHumaCadenceAPI, no middleware). For guard completeness they are
// indistinguishable — both must emit and both are here.
var auditedWriteRoutes = map[route]auditedRoute{
	// operators (middleware-audit).
	{http.MethodPost, "/v1/operators"}:                   {events: []audit.EventType{audit.EventOperatorCreated}},
	{http.MethodPost, "/v1/operators/{aid}/revoke"}:      {events: []audit.EventType{audit.EventOperatorRevoked}},
	{http.MethodPost, "/v1/operators/{aid}/issue-token"}: {events: []audit.EventType{audit.EventOperatorTokenIssued}},

	// auth (self-audit inside the handler: login writes operator.login after issuing the
	// JWT, OUTSIDE /v1, with no middleware-audit wiring — ADR-058). provision (operator.
	// provisioned) is written by the Mapper, not the endpoint — this route has one login event.
	{http.MethodPost, "/auth/ldap/login"}: {events: []audit.EventType{audit.EventOperatorLogin}, note: "self-audit: handler writes operator.login после выпуска JWT (ADR-058, вне /v1)"},

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

	// incarnations — MIXED audit class (middleware create/run/unlock/upgrade +
	// self-audit rerun/check-drift/destroy/update-hosts), all must emit.
	{http.MethodPost, "/v1/incarnations"}:                             {events: []audit.EventType{audit.EventIncarnationCreated}},
	{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}"}: {events: []audit.EventType{audit.EventIncarnationScenarioStarted}},
	{http.MethodPost, "/v1/incarnations/{name}/unlock"}:               {events: []audit.EventType{audit.EventIncarnationUnlocked}},
	{http.MethodPost, "/v1/incarnations/{name}/upgrade"}:              {events: []audit.EventType{audit.EventIncarnationUpgradeStarted}},
	{http.MethodPost, "/v1/incarnations/{name}/rerun-last"}:           {events: []audit.EventType{audit.EventIncarnationRerunLast}, note: "self-audit: handler writes внутри RerunLastTyped"},
	{http.MethodPost, "/v1/incarnations/{name}/check-drift"}:          {events: []audit.EventType{audit.EventIncarnationDriftChecked}, note: "self-audit: handler writes внутри CheckDriftTyped"},
	{http.MethodDelete, "/v1/incarnations/{name}"}:                    {events: []audit.EventType{audit.EventIncarnationDestroyStarted}, note: "self-audit: destroy_started writes service-слой incarnation.Destroy"},
	{http.MethodPatch, "/v1/incarnations/{name}/hosts"}:               {events: []audit.EventType{audit.EventIncarnationHostsUpdated}, note: "self-audit: handler writes внутри UpdateHostsTyped"},
	{http.MethodPut, "/v1/incarnations/{name}/traits"}:                {events: []audit.EventType{audit.EventIncarnationTraitsChanged}, note: "self-audit: handler writes внутри SetTraitsTyped"},
	{http.MethodPost, "/v1/incarnations/{name}/secrets/reveal"}:       {events: []audit.EventType{audit.EventIncarnationSecretRevealed}, note: "self-audit после ReadKV"},

	// choir (self-audit inside *Typed via writeAuditCtx).
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

	// provisioning-policy (middleware-audit; PUT mutating, GET — read). ADR-058 Part B.
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

	// push (middleware-audit; apply mutating, GET — read).
	{http.MethodPost, "/v1/push/apply"}: {events: []audit.EventType{audit.EventPushApplied}},

	// push-providers (middleware-audit).
	{http.MethodPost, "/v1/push-providers"}:          {events: []audit.EventType{audit.EventPushProviderCreated}},
	{http.MethodPut, "/v1/push-providers/{name}"}:    {events: []audit.EventType{audit.EventPushProviderUpdated}},
	{http.MethodDelete, "/v1/push-providers/{name}"}: {events: []audit.EventType{audit.EventPushProviderDeleted}},

	// providers + profiles — Cloud CRUD (middleware-audit, ADR-017). No update
	// (Provider/Profile are immutable).
	{http.MethodPost, "/v1/providers"}:          {events: []audit.EventType{audit.EventProviderCreated}},
	{http.MethodDelete, "/v1/providers/{name}"}: {events: []audit.EventType{audit.EventProviderDeleted}},
	{http.MethodPost, "/v1/profiles"}:           {events: []audit.EventType{audit.EventProfileCreated}},
	{http.MethodDelete, "/v1/profiles/{name}"}:  {events: []audit.EventType{audit.EventProfileDeleted}},

	// heralds + tidings (middleware-audit).
	{http.MethodPost, "/v1/heralds"}:          {events: []audit.EventType{audit.EventHeraldCreated}},
	{http.MethodPut, "/v1/heralds/{name}"}:    {events: []audit.EventType{audit.EventHeraldUpdated}},
	{http.MethodDelete, "/v1/heralds/{name}"}: {events: []audit.EventType{audit.EventHeraldDeleted}},
	{http.MethodPost, "/v1/tidings"}:          {events: []audit.EventType{audit.EventTidingCreated}},
	{http.MethodPut, "/v1/tidings/{name}"}:    {events: []audit.EventType{audit.EventTidingUpdated}},
	{http.MethodDelete, "/v1/tidings/{name}"}: {events: []audit.EventType{audit.EventTidingDeleted}},

	// errands (cancel — middleware-audit; POST exec lives under /v1/souls/{sid}/exec).
	{http.MethodDelete, "/v1/errands/{errand_id}"}: {events: []audit.EventType{audit.EventTypeErrandCancelled}},

	// voyages — KIND-dependent self-audit (RBAC-by-kind, ADR-043 §6): create writes
	// scenario_run.started OR command_run.invoked by recipe kind; cancel —
	// scenario_run.cancelled OR command_run.cancelled. preview — read-like (see
	// writeRoutesNoAudit).
	{http.MethodPost, "/v1/voyages"}:        {events: []audit.EventType{audit.EventScenarioRunStarted, audit.EventCommandRunInvoked}, note: "self-audit, kind-зависимый: scenario→started / command→invoked"},
	{http.MethodDelete, "/v1/voyages/{id}"}: {events: []audit.EventType{audit.EventScenarioRunCancelled, audit.EventCommandRunCancelled}, note: "self-audit, kind-зависимый: scenario/command cancelled"},

	// cadences — self-audit inside *Typed. enable/disable write cadence.updated
	// (toggle via update-event; no separate enabled/disabled events).
	{http.MethodPost, "/v1/cadences"}:              {events: []audit.EventType{audit.EventCadenceCreated}, note: "self-audit: handler writes внутри CreateTyped (kind-permission внутри handler, ADR-046 §7)"},
	{http.MethodPatch, "/v1/cadences/{id}"}:        {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit"},
	{http.MethodDelete, "/v1/cadences/{id}"}:       {events: []audit.EventType{audit.EventCadenceDeleted}, note: "self-audit"},
	{http.MethodPost, "/v1/cadences/{id}/enable"}:  {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit: enable/disable пишут cadence.updated (toggle)"},
	{http.MethodPost, "/v1/cadences/{id}/disable"}: {events: []audit.EventType{audit.EventCadenceUpdated}, note: "self-audit: enable/disable пишут cadence.updated (toggle)"},
}

// writeRoutesNoAudit — DELIBERATE exceptions: write routes that get NO audit, each with
// a rationale WHY (otherwise an exception = a hole, not a decision). Empty = "no write
// route without audit"; any addition here is an explicit architectural decision under review.
var writeRoutesNoAudit = map[route]string{
	// POST /v1/voyages/preview — dry-resolve scope WITHOUT creating a Voyage and WITHOUT
	// mutating state (ADR-043 amendment §4). POST by HTTP method, but read-like by semantics
	// (changes nothing) → audit deliberately not written (parity with GET-read). This is the
	// ONLY write-method-without-mutation in the API; kept explicit so the guard does not
	// demand an audit event it has none of by design.
	{http.MethodPost, "/v1/voyages/preview"}: "ADR-043 amendment §4: dry-resolve scope без withздания Voyage и без мутации withстояния — read-like POST, audit onмеренbut не writesся (паритет GET-read)",

	// POST /v1/modules/{name}/form-prep — resolver of source catalogs for a module's UI
	// form (ADR-045 S3): from incarnation_hosts/choir it returns live SIDs for autocomplete
	// of the Run→Command form. POST by HTTP method (carries a filter body), but a read-only
	// resolve by semantics — mutates nothing (router.go: "no audit, soul.list / service.list
	// pattern"). audit deliberately not written.
	{http.MethodPost, "/v1/modules/{name}/form-prep"}: "ADR-045 S3: read-only-резолв source-каталогов UI-формы (живые SID-ы), без мутации withстояния — audit onмеренbut не writesся (паттерн soul.list/service.list)",

	// POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill — day-2
	// pre-fill of the scenario UI form from incarnation.state (docs/input.md). POST by
	// HTTP method (carries an optional body-ref), but a read-only resolve by semantics —
	// reads the state of a single incarnation, mutates nothing. Permission
	// incarnation.get (read pattern). audit deliberately not written.
	{http.MethodPost, "/v1/incarnations/{name}/scenarios/{scenario}/form-prefill"}: "day-2 pre-fill формы from incarnation.state (docs/input.md): read-only-резолв одbutй инкарonции, без мутации — audit onмеренbut не writesся (паттерн get/module.form-prep)",
}

// writeMethods — HTTP methods considered mutating by the guard.
var writeMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// TestAuditCompleteness_AllWriteRoutesCovered — aggregate declarative guard.
//
// Takes the FULL set of write routes from buildFullOpenAPISpec (all domains, incl.
// opt-in) and asserts: every write route is either in auditedWriteRoutes (must write
// audit, event confirmed by per-domain *_RecordsOnSuccess) or in writeRoutesNoAudit
// (a deliberate exception with a rationale). Any write route outside both sets = a new
// write route with no explicit audit decision → goes red (an S6 recurrence won't slip by).
//
// Also catches the reverse drift: a registry entry with no matching write route in the
// spec (a stale declaration — the registry must mirror the topology).
func TestAuditCompleteness_AllWriteRoutesCovered(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Full set of the spec's write routes.
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
		t.Fatal("in compiled spec нет ни одbutго write routeа — спека пуста or write-методы не распозonны?")
	}

	// (1) Every write route in the spec is covered by exactly one of the two registries.
	var uncovered []string
	for r := range specWrites {
		ar, audited := auditedWriteRoutes[r]
		_, exempt := writeRoutesNoAudit[r]
		switch {
		case audited && exempt:
			t.Errorf("write route %s одbutвременbut в auditedWriteRoutes и writeRoutesNoAudit — реестры обязаны быть непересекающимися", r)
		case audited && len(ar.events) == 0:
			t.Errorf("write route %s в auditedWriteRoutes без едиbutго event-типа — binding к audit.Event* обязательon (связь с per-domain *_RecordsOnSuccess)", r)
		case !audited && !exempt:
			uncovered = append(uncovered, r.String())
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("WRITE-РОУТ БЕЗ РЕШЕНИЯ ПО AUDIT — %d (рецидив S6: butвый мутирующий роут без audit-onвески прошёл бы молча):\n  %s\n"+
			"→ внеси КАЖДЫЙ в auditedWriteRoutes (привязав к audit.Event*, который per-domain *_RecordsOnSuccess-тест toказывает пишущимся) "+
			"ИЛИ в writeRoutesNoAudit с обосbutванием ПОЧЕМУ audit не нalreadyн.",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}

	// (2) Reverse drift: a registry declaration with no real write route in the spec.
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
		t.Errorf("УСТАРЕВШАЯ ДЕКЛАРАЦИЯ — %d записей в audit-реестрах без withответствующits write routeа в спеке (реестр обязан зеркалить топологию buildRouter):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}

	t.Logf("guard: %d write routeов покрыто (%d audited, %d оwithзonнных no-audit исключений)",
		len(specWrites), len(auditedWriteRoutes), len(writeRoutesNoAudit))
}
