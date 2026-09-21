// Completeness guard over the RBAC perimeter of the Operator API (NIM-843).
//
// PROBLEM. Three structural guards already walk the route tree and none of them
// is about authorization: TestFullSpec_CoversAllRoutes proves every chi route is
// in the OpenAPI spec, TestAuditCompleteness_AllWriteRoutesCovered proves every
// write route is classified for audit, TestRevokedOperator_401OnEveryAuthenticated
// Route proves a revoked Archon is refused everywhere. Nothing asserted that a
// route carries an RBAC gate at all. Mount a new domain in a bare `r.Group(...)`
// instead of `r.With(RequirePermission(...))` and every test stayed green while
// the route was reachable by any authenticated Archon — and "this route has no
// chi gate" would not stand out in review, because six routes are mounted that
// way on purpose.
//
// That is the shape this repository found three times in one week — NIM-811/826
// (CEL activation roots), NIM-817/824 (wire→native projections) — and each time
// the fix was a TABLE, not a list: a row per member, each carrying a written
// reason, checked against what actually runs. A list has nowhere to write an
// untruth, so nothing can catch one.
//
// THE TABLE is [rbacRouteGates]: one row per route, declaring the ordered set of
// questions the route's gate puts to the enforcer, or declaring that it asks
// none and WHY. The two halves are separately falsifiable:
//
//	(1) completeness — the key set of the table equals the route set (every
//	    route of the assembled spec, plus every route the production router
//	    mounts). A new route lands in neither registry → red. A row whose route
//	    is gone → red.
//	(2) truthfulness — the declared questions are the questions that RUN. The
//	    probe is an enforcer that records every Check/HoldsAction and grants
//	    nothing, behind the REAL buildRouter: the refusal names the permission
//	    the gate asked for, so a row claiming `soul.forget` on a route gated by
//	    `soul.list` goes red, and so does a row claiming a gate on a route that
//	    has none. The row cannot lie about the gate because it is not the row
//	    being read — it is the gate being run.
//
// WHY the question and not just the permission: `check:` and `holds:` are
// different gates with different semantics (ADR-047 §d). RequirePermission runs
// the scope-aware [rbac.Enforcer.Check] and denies a scoped operator whose
// context did not match; RequireAction runs the existence-only HoldsAction and
// leaves the narrowing to the handler. NIM-421 found those two answering
// differently to the same revoked Archon, so "which gate" is load-bearing and a
// table that recorded only the permission name could not tell the two apart.
//
// WHAT IT DOES NOT CLAIM, written down because a guard believed past its reach is
// worse than no guard:
//
//   - In-handler authorization. For a route that decides inside the handler
//     (voyage RBAC-by-kind, the run-events SSE stream) the row says so and names
//     both the permissions and the test that pins them; the handlers here are spec
//     stubs, so this file cannot watch them ask. Its claim for those rows is the
//     narrow one it can make: NO gate runs in the chain, so the row is not hiding
//     one.
//   - A SECOND chain gate behind the first. The probe grants nothing, so every
//     request stops at the first refusal and the recording ends there — a chain of
//     two gates is indistinguishable from its first. No /v1 route stacks two
//     today (the cadence toggles record two questions from ONE RequireAnyPermission
//     gate), and the MCP guard, where three tools really do ask twice, carries the
//     second pass that reads past the first question
//     (TestRBACCompleteness_ToolGateBeyondTheFirstQuestion, which grants each row's
//     own questions so the next one is reachable). When a /v1 route grows a second
//     gate, that pass is the thing to copy here.
//   - The MCP listener's own mux. `/mcp`, `/mcp/events` and its 404 live on a
//     separate listener with separate auth (NIM-551) and are in neither table:
//     every tool behind `/mcp` is covered by the MCP guard, and `/mcp/events` is
//     NIM-858.
//   - A new METHOD on a path already collapsed to [anyMethod].

package api

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// rbacGate — the authorization decision bound to one route.
//
// asks — the ordered questions the chi chain puts to the enforcer before the
// handler runs, spelled `check:<resource>.<action>` for the scope-aware gates
// ([apimiddleware.RequirePermission], RequirePermissionMulti, RequireAnyPermission)
// and `holds:<resource>.<action>` for the existence gate
// ([apimiddleware.RequireAction]). More than one entry = an OR-gate, tried in
// order, and the refusal names the first.
//
// why — mandatory when asks is empty: who authorizes instead, or why nothing
// does. A route with no gate is a legitimate answer (health, the self-describing
// catalogs, in-handler RBAC-by-kind); a route with no gate and no row is the
// defect. The difference between the two is this field.
type rbacGate struct {
	asks []string
	why  string
}

// anyMethod — the method of a row standing for a chi catch-all pattern.
//
// One row per pattern rather than per method: the `/v1/*` fallback is registered
// under all nine methods and `/docs/assets/*` and `/ui/*` under GET alone, and in
// every case the count of registrations is an artefact of how the mount was
// written while the authorization decision is one. Collapsing them keeps the table
// describing decisions; it also means a NEW method on a wildcard path is not a new
// row, which is the one thing this collapse gives up.
const anyMethod = "*"

// rbacRouteGates — every route of the Operator API and what authorizes it.
//
// Source of the route set: the assembled spec (buildFullOpenAPISpec — all
// domains, including the opt-in ones) ∪ chi.Walk over the production router.
// Both, because neither alone is complete: the spec misses the console
// WebSocket and everything outside /v1, the walk misses a domain whose handler
// is nil in this test's wiring.
var rbacRouteGates = map[route]rbacGate{
	// --- outside /v1: public or authentication-only, by design ---
	{http.MethodGet, "/healthz"}:            {why: "liveness probe, deliberately public (router.go § Health/Meta): it answers before a JWT exists and carries no operator data — gating it would make the cluster unmonitorable by its own orchestrator"},
	{http.MethodGet, "/readyz"}:             {why: "readiness probe, deliberately public for the same reason as /healthz: the load balancer that calls it holds no Archon identity"},
	{http.MethodGet, "/docs"}:               {why: "ADR-054 doc-viewer mechanism A: the RapiDoc shell is public static with no API description in it; the description arrives only when the page fetches /openapi.json, which IS behind a JWT"},
	{anyMethod, "/docs/assets/*"}:           {why: "ADR-054 doc-viewer mechanism A: RapiDoc's own JS/CSS, public static like the shell that loads it"},
	{http.MethodGet, "/openapi.yaml"}:       {why: "authenticated (RequireJWT + RejectRevoked) but deliberately NOT RBAC-gated: the spec describes the shape of the surface, never its contents, and every Archon needs it to call anything. NIM-421 moved it from public to authenticated; per-permission filtering of a static document was not attempted"},
	{http.MethodGet, "/openapi.json"}:       {why: "same document as /openapi.yaml in the form the /docs viewer fetches; same decision"},
	{http.MethodGet, "/auth/methods"}:       {why: "pre-authentication (ADR-058): the login form asks which methods exist before any Archon exists, so there is no subject to authorize"},
	{http.MethodPost, "/auth/token"}:        {why: "pre-authentication (NIM-77): exchanges a verified session cookie for a short Bearer. The cookie IS the authorization; an RBAC gate would need the token this route issues"},
	{http.MethodPost, "/auth/ldap/login"}:   {why: "pre-authentication (ADR-058): credentials in, JWT out. Throttled by AuthLoginLimit rather than authorized"},
	{http.MethodGet, "/auth/oidc/login"}:    {why: "pre-authentication (ADR-058 stage 2): redirects to the IdP, carries no subject yet"},
	{http.MethodGet, "/auth/oidc/callback"}: {why: "pre-authentication (ADR-058 stage 2): the IdP's code lands here and is exchanged for a JWT — this route establishes the identity an RBAC gate would need"},

	// --- the embedded UI (ADR-055) ---
	{http.MethodGet, "/ui"}: {why: "a redirect to /ui/, mounted beside /docs and public for the same reason: ADR-055 protects the API, not the static shell that calls it. Every byte it serves is the same for every Archon, and the SPA is useless until it authenticates against /v1"},
	{anyMethod, "/ui/*"}:    {why: "the go:embed SPA bundle — JS/CSS/index.html, public static like /docs/assets/*. It is mounted only when web_ui_enabled is on, which is why this guard builds its router with that toggle ON: a route behind a config boolean is still a route, and one added beside these would otherwise be in neither half of the universe"},

	// --- the /v1 404 fallback ---
	{anyMethod, "/v1/*"}: {why: "the catch-all 404 behind the auth chain (router.go, end of the /v1 group). It reaches no handler and touches no state — it exists so an unknown path answers 'no such endpoint' to a live Archon and 401 to anyone else"},

	// --- the self-describing catalogs: deliberately authentication-only ---
	{http.MethodGet, "/v1/permissions"}:    {why: "architect verdict (router.go §/v1/permissions): requiring a permission to read the catalog of permissions is chicken-and-egg — the UI needs the real names to assign any. Static content from rbac.catalog.go, identical for every Archon, and a revoked one is still refused by RejectRevoked (NIM-421)"},
	{http.MethodGet, "/v1/event-types"}:    {why: "same chicken-and-egg verdict as /v1/permissions: the static catalog of event types a Tiding can subscribe to (herald/eventtypes.go), identical for every Archon"},
	{http.MethodGet, "/v1/herald-types"}:   {why: "same chicken-and-egg verdict: the static catalog of Herald channel types and their config fields"},
	{http.MethodGet, "/v1/me/permissions"}: {why: "reads the CALLER's own effective permissions, AID taken from the claims and never from the query — so the only thing it can disclose is what the caller already holds. A gate here would be a permission to know one's own permissions"},

	// --- authorization inside the handler: no chi gate, by construction ---
	{http.MethodGet, "/v1/incarnations/{id}/runs/{apply_id}/events"}: {why: "ADR-068 §A3: the rule is 'the run's initiator OR incarnation.get/history', and an initiator may hold neither permission — not expressible as an existence gate, which is why there is no chi one. authorizeRunEventsSSE decides it per stream (huma_incarnation_runevents.go), pinned by TestAuthorizeRunEventsSSE_Matrix and TestRunEventsSSE_Forbidden*, and re-decided for the life of the stream by TestRunEventsStillAuthorized_Matrix (NIM-844)"},
	{http.MethodPost, "/v1/voyages"}:                                 {why: "ADR-043 §6 RBAC-by-kind: the permission is incarnation.run for a scenario recipe and errand.run for a command one, and the kind is only visible in the body — so VoyageHandler.CreateTyped picks and checks it, through resolveScenarioScopeErr / the command half. Pinned by handlers.TestVoyageCreate_ScenarioRBACDenied and TestVoyageCreate_CommandRBACDenied"},
	{http.MethodPost, "/v1/voyages/preview"}:                         {why: "ADR-043 amendment §4: the same RBAC-by-kind as create, over the same body, through the same resolveScenarioScopeErr — preview resolves the scope the create would use, so it must not be reachable with less. Pinned by handlers.TestVoyagePreview_ScenarioRBACDenied_403"},
	{http.MethodDelete, "/v1/voyages/{id}"}:                          {why: "ADR-043 §6 RBAC-by-kind, kind read from the stored row rather than a body: scenario→incarnation.run, command→errand.run, checked in VoyageHandler.CancelTyped after voyage.SelectByID. Since NIM-841/842/846 a disjunction of the two rights is asked BEFORE the row is read, so 403-vs-404 no longer tells an Archon holding neither whether a guessed ULID is a real run; the by-kind check still follows. handlers.TestVoyageCancel_RBACDenied pins the scenario branch only — the command→errand.run half of cancel is asserted nowhere"},

	// --- operators (ADR-014) ---
	{http.MethodGet, "/v1/operators"}:                    {asks: []string{"check:operator.list"}},
	{http.MethodPost, "/v1/operators"}:                   {asks: []string{"check:operator.create"}},
	{http.MethodGet, "/v1/operators/{aid}"}:              {asks: []string{"check:operator.list"}},
	{http.MethodPost, "/v1/operators/{aid}/issue-token"}: {asks: []string{"check:operator.issue-token"}},
	{http.MethodPost, "/v1/operators/{aid}/revoke"}:      {asks: []string{"check:operator.revoke"}},

	// --- audit ---
	{http.MethodGet, "/v1/audit"}: {asks: []string{"check:audit.read"}},

	// --- roles / synods (RBAC CRUD) ---
	{http.MethodGet, "/v1/roles"}:                              {asks: []string{"check:role.list"}},
	{http.MethodPost, "/v1/roles"}:                             {asks: []string{"check:role.create"}},
	{http.MethodDelete, "/v1/roles/{name}"}:                    {asks: []string{"check:role.delete"}},
	{http.MethodPatch, "/v1/roles/{name}/permissions"}:         {asks: []string{"check:role.update"}},
	{http.MethodPost, "/v1/roles/{name}/operators"}:            {asks: []string{"check:role.grant-operator"}},
	{http.MethodDelete, "/v1/roles/{name}/operators/{aid}"}:    {asks: []string{"check:role.revoke-operator"}},
	{http.MethodGet, "/v1/synods"}:                             {asks: []string{"check:synod.list"}},
	{http.MethodPost, "/v1/synods"}:                            {asks: []string{"check:synod.create"}},
	{http.MethodPatch, "/v1/synods/{name}"}:                    {asks: []string{"check:synod.update"}},
	{http.MethodDelete, "/v1/synods/{name}"}:                   {asks: []string{"check:synod.delete"}},
	{http.MethodPost, "/v1/synods/{name}/operators"}:           {asks: []string{"check:synod.add-operator"}},
	{http.MethodDelete, "/v1/synods/{name}/operators/{aid}"}:   {asks: []string{"check:synod.remove-operator"}},
	{http.MethodPost, "/v1/synods/{name}/roles"}:               {asks: []string{"check:synod.grant-role"}},
	{http.MethodDelete, "/v1/synods/{name}/roles/{role_name}"}: {asks: []string{"check:synod.revoke-role"}},

	// --- incarnations (ADR-047 §d: reads take the existence gate, writes the
	// scope-aware one — a scoped operator must not be cut off from their own
	// list before the handler can narrow it) ---
	{http.MethodGet, "/v1/incarnations"}:                                         {asks: []string{"holds:incarnation.list"}},
	{http.MethodPost, "/v1/incarnations"}:                                        {asks: []string{"check:incarnation.create"}},
	{http.MethodPost, "/v1/incarnations/resolve-id"}:                             {asks: []string{"check:incarnation.create"}},
	{http.MethodGet, "/v1/incarnations/{id}"}:                                    {asks: []string{"holds:incarnation.get"}},
	{http.MethodDelete, "/v1/incarnations/{id}"}:                                 {asks: []string{"check:incarnation.destroy"}},
	{http.MethodGet, "/v1/incarnations/{id}/history"}:                            {asks: []string{"holds:incarnation.history"}},
	{http.MethodPut, "/v1/incarnations/{id}/label"}:                              {asks: []string{"check:incarnation.label-set"}},
	{http.MethodGet, "/v1/incarnations/{id}/members"}:                            {asks: []string{"check:incarnation.get"}},
	{http.MethodPost, "/v1/incarnations/{id}/members"}:                           {asks: []string{"check:incarnation.bind-member"}},
	{http.MethodDelete, "/v1/incarnations/{id}/members/{sid}"}:                   {asks: []string{"check:incarnation.unbind-member"}},
	{http.MethodPost, "/v1/incarnations/{id}/rerun-last"}:                        {asks: []string{"check:incarnation.rerun-last"}},
	{http.MethodGet, "/v1/incarnations/{id}/runs"}:                               {asks: []string{"holds:incarnation.history"}},
	{http.MethodGet, "/v1/incarnations/{id}/runs/{apply_id}"}:                    {asks: []string{"holds:incarnation.history"}},
	{http.MethodGet, "/v1/incarnations/{id}/runs/{apply_id}/tasks"}:              {asks: []string{"holds:incarnation.history"}},
	{http.MethodPost, "/v1/incarnations/{id}/scenarios/{scenario}"}:              {asks: []string{"check:incarnation.run"}},
	{http.MethodPost, "/v1/incarnations/{id}/scenarios/{scenario}/form-prefill"}: {asks: []string{"holds:incarnation.get"}},
	{http.MethodPost, "/v1/incarnations/{id}/secrets/reveal"}:                    {asks: []string{"check:incarnation.view-secrets"}},
	{http.MethodGet, "/v1/incarnations/{id}/secrets/revealable"}:                 {asks: []string{"holds:incarnation.view-secrets"}},
	{http.MethodGet, "/v1/incarnations/{id}/telemetry"}:                          {asks: []string{"holds:incarnation.get"}},
	{http.MethodPut, "/v1/incarnations/{id}/traits"}:                             {asks: []string{"check:incarnation.traits-set"}},
	{http.MethodPost, "/v1/incarnations/{id}/unlock"}:                            {asks: []string{"check:incarnation.unlock"}},
	{http.MethodPost, "/v1/incarnations/{id}/upgrade"}:                           {asks: []string{"check:incarnation.upgrade"}},
	{http.MethodGet, "/v1/incarnations/{id}/upgrade-paths"}:                      {asks: []string{"holds:incarnation.upgrade"}},
	{http.MethodGet, "/v1/runs"}:                                                 {asks: []string{"holds:incarnation.history"}},
	{http.MethodGet, "/v1/runs/stats"}:                                           {asks: []string{"holds:incarnation.history"}},
	{http.MethodGet, "/v1/deprecations"}:                                         {asks: []string{"holds:incarnation.list"}},

	// --- choirs (ADR-044) ---
	{http.MethodGet, "/v1/incarnations/{id}/choirs"}:                         {asks: []string{"check:choir.list"}},
	{http.MethodPost, "/v1/incarnations/{id}/choirs"}:                        {asks: []string{"check:choir.create"}},
	{http.MethodDelete, "/v1/incarnations/{id}/choirs/{choir}"}:              {asks: []string{"check:choir.delete"}},
	{http.MethodGet, "/v1/incarnations/{id}/choirs/{choir}/voices"}:          {asks: []string{"check:choir.list"}},
	{http.MethodPost, "/v1/incarnations/{id}/choirs/{choir}/voices"}:         {asks: []string{"check:choir.add-voice"}},
	{http.MethodDelete, "/v1/incarnations/{id}/choirs/{choir}/voices/{sid}"}: {asks: []string{"check:choir.remove-voice"}},

	// --- souls. `soul.get` does not exist (rbac.md § Souls): a per-host read
	// takes the same soul.list existence gate as the collection. ---
	{http.MethodGet, "/v1/souls"}:                    {asks: []string{"holds:soul.list"}},
	{http.MethodPost, "/v1/souls"}:                   {asks: []string{"check:soul.create"}},
	{http.MethodGet, "/v1/souls/stats"}:              {asks: []string{"holds:soul.list"}},
	{http.MethodPost, "/v1/souls/coven"}:             {asks: []string{"check:soul.coven-assign"}},
	{http.MethodPost, "/v1/souls/traits"}:            {asks: []string{"holds:soul.traits-assign"}},
	{http.MethodGet, "/v1/souls/{sid}"}:              {asks: []string{"holds:soul.list"}},
	{http.MethodDelete, "/v1/souls/{sid}"}:           {asks: []string{"check:soul.forget"}},
	{http.MethodGet, "/v1/souls/{sid}/history"}:      {asks: []string{"holds:soul.list"}},
	{http.MethodPost, "/v1/souls/{sid}/issue-token"}: {asks: []string{"check:soul.issue-token"}},
	{http.MethodGet, "/v1/souls/{sid}/soulprint"}:    {asks: []string{"holds:soul.list"}},
	{http.MethodPut, "/v1/souls/{sid}/ssh-target"}:   {asks: []string{"check:soul.ssh-target-update"}},
	{http.MethodGet, "/v1/souls/{sid}/telemetry"}:    {asks: []string{"holds:soul.list"}},
	{http.MethodPost, "/v1/souls/{sid}/exec"}:        {asks: []string{"check:errand.run"}},

	// --- errands (ADR-033). The EXISTENCE gate since NIM-841/842/846: the
	// scope-aware form denied a host-scoped role its own Errands outright, so the
	// narrowing moved into the store (errand.Store) and the chain now only asks
	// whether the right is held at all — the same ADR-047 §d shape as the
	// incarnation and soul reads. ---
	{http.MethodGet, "/v1/errands"}:                {asks: []string{"holds:errand.list"}},
	{http.MethodGet, "/v1/errands/{errand_id}"}:    {asks: []string{"holds:errand.list"}},
	{http.MethodDelete, "/v1/errands/{errand_id}"}: {asks: []string{"holds:errand.cancel"}},

	// --- console (ADR-0074). The socket and the recordings take the EXISTENCE
	// gate: neither names a host at upgrade/listing time, and a scope-aware
	// check with an absent host dimension fails closed on exactly the
	// `host=`-scoped roles the feature serves (ADR-047 §g G1). The per-host
	// scope is re-applied inside the handler. ---
	{http.MethodGet, "/v1/console"}:                                {asks: []string{"holds:soul.console"}},
	{http.MethodGet, "/v1/console/recordings"}:                     {asks: []string{"holds:soul.console"}},
	{http.MethodGet, "/v1/console/recordings/{recording_id}"}:      {asks: []string{"holds:soul.console"}},
	{http.MethodGet, "/v1/console/recordings/{recording_id}/cast"}: {asks: []string{"holds:soul.console"}},

	// --- cluster ---
	{http.MethodGet, "/v1/cluster"}: {asks: []string{"holds:soul.list"}},

	// --- plugins / sigil keys ---
	{http.MethodGet, "/v1/plugins/sigils"}:               {asks: []string{"check:plugin.list"}},
	{http.MethodPost, "/v1/plugins/sigils"}:              {asks: []string{"check:plugin.allow"}},
	{http.MethodDelete, "/v1/plugins/sigils/{alias}"}:    {asks: []string{"check:plugin.revoke"}},
	{http.MethodGet, "/v1/sigil/keys"}:                   {asks: []string{"check:sigil.key-list"}},
	{http.MethodPost, "/v1/sigil/keys"}:                  {asks: []string{"check:sigil.key-introduce"}},
	{http.MethodDelete, "/v1/sigil/keys/{key_id}"}:       {asks: []string{"check:sigil.key-retire"}},
	{http.MethodPost, "/v1/sigil/keys/{key_id}/primary"}: {asks: []string{"check:sigil.key-set-primary"}},

	// --- services. Every read facet of a service reuses service.list: there is
	// no per-facet permission (rbac.md § Services). ---
	{http.MethodGet, "/v1/services"}:                   {asks: []string{"check:service.list"}},
	{http.MethodPost, "/v1/services"}:                  {asks: []string{"check:service.register"}},
	{http.MethodGet, "/v1/services/{id}"}:              {asks: []string{"check:service.list"}},
	{http.MethodPatch, "/v1/services/{id}"}:            {asks: []string{"check:service.update"}},
	{http.MethodDelete, "/v1/services/{id}"}:           {asks: []string{"check:service.deregister"}},
	{http.MethodPut, "/v1/services/{id}/label"}:        {asks: []string{"check:service.label-set"}},
	{http.MethodGet, "/v1/services/{id}/compat"}:       {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/dependencies"}: {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/directives"}:   {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/refs"}:         {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/scenarios"}:    {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/state-schema"}: {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/services/{id}/telemetry"}:    {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/modules"}:                    {asks: []string{"check:service.list"}},
	{http.MethodGet, "/v1/modules/{name}"}:             {asks: []string{"check:service.list"}},
	// form-prep resolves the catalogs a Run→Command form offers, so it is gated
	// by the right to run rather than the right to read the module (ADR-045 S3).
	{http.MethodPost, "/v1/modules/{name}/form-prep"}: {asks: []string{"check:incarnation.run"}},

	// --- provisioning policy / settings ---
	{http.MethodGet, "/v1/provisioning-policy"}: {asks: []string{"check:provisioning.read"}},
	{http.MethodPut, "/v1/provisioning-policy"}: {asks: []string{"check:provisioning.update"}},
	{http.MethodGet, "/v1/settings"}:            {asks: []string{"check:setting.read"}},
	{http.MethodPut, "/v1/settings/{key}"}:      {asks: []string{"check:setting.update"}},
	{http.MethodDelete, "/v1/settings/{key}"}:   {asks: []string{"check:setting.delete"}},

	// --- augur (ADR-025) / oracle (ADR-030) ---
	{http.MethodGet, "/v1/augur/omens"}:            {asks: []string{"check:omen.list"}},
	{http.MethodPost, "/v1/augur/omens"}:           {asks: []string{"check:omen.create"}},
	{http.MethodGet, "/v1/augur/omens/{id}"}:       {asks: []string{"check:omen.list"}},
	{http.MethodDelete, "/v1/augur/omens/{id}"}:    {asks: []string{"check:omen.delete"}},
	{http.MethodPut, "/v1/augur/omens/{id}/label"}: {asks: []string{"check:omen.label-set"}},
	{http.MethodGet, "/v1/augur/rites"}:            {asks: []string{"check:rite.list"}},
	{http.MethodPost, "/v1/augur/rites"}:           {asks: []string{"check:rite.create"}},
	{http.MethodDelete, "/v1/augur/rites/{id}"}:    {asks: []string{"check:rite.delete"}},
	{http.MethodGet, "/v1/vigils"}:                 {asks: []string{"check:vigil.list"}},
	{http.MethodPost, "/v1/vigils"}:                {asks: []string{"check:vigil.create"}},
	{http.MethodGet, "/v1/vigils/{id}"}:            {asks: []string{"check:vigil.list"}},
	{http.MethodDelete, "/v1/vigils/{id}"}:         {asks: []string{"check:vigil.delete"}},
	{http.MethodPut, "/v1/vigils/{id}/label"}:      {asks: []string{"check:vigil.label-set"}},
	{http.MethodGet, "/v1/decrees"}:                {asks: []string{"check:decree.list"}},
	{http.MethodPost, "/v1/decrees"}:               {asks: []string{"check:decree.create"}},
	{http.MethodGet, "/v1/decrees/{id}"}:           {asks: []string{"check:decree.list"}},
	{http.MethodDelete, "/v1/decrees/{id}"}:        {asks: []string{"check:decree.delete"}},
	{http.MethodPut, "/v1/decrees/{id}/label"}:     {asks: []string{"check:decree.label-set"}},

	// --- push (agentless delivery) ---
	{http.MethodPost, "/v1/push/apply"}:               {asks: []string{"check:push.apply"}},
	{http.MethodGet, "/v1/push/{apply_id}"}:           {asks: []string{"check:push.read"}},
	{http.MethodGet, "/v1/push-runs"}:                 {asks: []string{"check:incarnation.history"}},
	{http.MethodGet, "/v1/push-providers"}:            {asks: []string{"check:push-provider.list"}},
	{http.MethodPost, "/v1/push-providers"}:           {asks: []string{"check:push-provider.create"}},
	{http.MethodGet, "/v1/push-providers/{id}"}:       {asks: []string{"check:push-provider.read"}},
	{http.MethodPut, "/v1/push-providers/{id}"}:       {asks: []string{"check:push-provider.update"}},
	{http.MethodDelete, "/v1/push-providers/{id}"}:    {asks: []string{"check:push-provider.delete"}},
	{http.MethodPut, "/v1/push-providers/{id}/label"}: {asks: []string{"check:push-provider.label-set"}},

	// --- heralds / tidings (ADR-052) ---
	{http.MethodGet, "/v1/heralds"}:            {asks: []string{"check:herald.list"}},
	{http.MethodPost, "/v1/heralds"}:           {asks: []string{"check:herald.create"}},
	{http.MethodGet, "/v1/heralds/{id}"}:       {asks: []string{"check:herald.read"}},
	{http.MethodPut, "/v1/heralds/{id}"}:       {asks: []string{"check:herald.update"}},
	{http.MethodDelete, "/v1/heralds/{id}"}:    {asks: []string{"check:herald.delete"}},
	{http.MethodPut, "/v1/heralds/{id}/label"}: {asks: []string{"check:herald.label-set"}},
	{http.MethodGet, "/v1/tidings"}:            {asks: []string{"check:tiding.list"}},
	{http.MethodPost, "/v1/tidings"}:           {asks: []string{"check:tiding.create"}},
	{http.MethodGet, "/v1/tidings/{id}"}:       {asks: []string{"check:tiding.read"}},
	{http.MethodPut, "/v1/tidings/{id}"}:       {asks: []string{"check:tiding.update"}},
	{http.MethodDelete, "/v1/tidings/{id}"}:    {asks: []string{"check:tiding.delete"}},
	{http.MethodPut, "/v1/tidings/{id}/label"}: {asks: []string{"check:tiding.label-set"}},

	// --- voyages: reads only. The three write routes are RBAC-by-kind above. ---
	{http.MethodGet, "/v1/voyages"}:              {asks: []string{"check:incarnation.history"}},
	{http.MethodGet, "/v1/voyages/{id}"}:         {asks: []string{"check:incarnation.history"}},
	{http.MethodGet, "/v1/voyages/{id}/targets"}: {asks: []string{"check:incarnation.history"}},

	// --- cadences (ADR-046). enable/disable are OR-gates: the granular right
	// first, then the backcompat cadence.update, so a role written before the
	// split does not lose the toggle (amendment 2026-06-02). ---
	{http.MethodGet, "/v1/cadences"}:               {asks: []string{"check:cadence.list"}},
	{http.MethodPost, "/v1/cadences"}:              {asks: []string{"check:cadence.create"}},
	{http.MethodGet, "/v1/cadences/{id}"}:          {asks: []string{"check:cadence.list"}},
	{http.MethodPatch, "/v1/cadences/{id}"}:        {asks: []string{"check:cadence.update"}},
	{http.MethodDelete, "/v1/cadences/{id}"}:       {asks: []string{"check:cadence.delete"}},
	{http.MethodPost, "/v1/cadences/{id}/enable"}:  {asks: []string{"check:cadence.enable", "check:cadence.update"}},
	{http.MethodPost, "/v1/cadences/{id}/disable"}: {asks: []string{"check:cadence.disable", "check:cadence.update"}},
	{http.MethodGet, "/v1/cadences/{id}/runs"}:     {asks: []string{"check:incarnation.history"}},
}

// --- the probe ---

// rbacAsk — one recorded question, in the table's spelling.
type rbacAsk string

func scopedAsk(resource, action string) rbacAsk { return rbacAsk("check:" + resource + "." + action) }
func existenceAsk(resource, action string) rbacAsk {
	return rbacAsk("holds:" + resource + "." + action)
}

// recordingRBAC wraps an enforcer and records every question put to it, in
// order. Embedding the interface rather than reimplementing it keeps the other
// half of [RBACProvider] — the purview and roster surfaces the handlers use —
// answering exactly as production does.
type recordingRBAC struct {
	RBACProvider
	mu   sync.Mutex
	asks []rbacAsk
}

func (r *recordingRBAC) Check(aid, resource, action string, ctx map[string]string) error {
	r.mu.Lock()
	r.asks = append(r.asks, scopedAsk(resource, action))
	r.mu.Unlock()
	return r.RBACProvider.Check(aid, resource, action, ctx)
}

func (r *recordingRBAC) HoldsAction(aid, resource, action string) bool {
	r.mu.Lock()
	r.asks = append(r.asks, existenceAsk(resource, action))
	r.mu.Unlock()
	return r.RBACProvider.HoldsAction(aid, resource, action)
}

// take returns the questions recorded since the last call and resets.
func (r *recordingRBAC) take() []rbacAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asks
	r.asks = nil
	return out
}

const rbacGateAID = "archon-gate-sweep"

// zeroGrantRBAC — an enforcer for an Archon who holds a role and nothing in it.
// Not "an unknown AID": an unknown subject could be refused by some other
// branch, and the refusal this sweep reads must be the gate's own
// [rbac.ErrPermissionDenied], which is what names the permission.
func zeroGrantRBAC(t *testing.T) *recordingRBAC {
	t.Helper()
	enf, err := rbac.NewEnforcerFromSnapshot(&rbac.Snapshot{
		Roles:      map[string][]string{"holds-nothing": {}},
		Membership: map[string][]string{rbacGateAID: {"holds-nothing"}},
		Revoked:    map[string]time.Time{},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return &recordingRBAC{RBACProvider: enf}
}

// walkedGateRoutes — every route the production router mounts, with the
// catch-all patterns collapsed onto [anyMethod] and a concrete request path
// beside each pattern (path params filled with a literal; no request under this
// sweep is meant to reach a handler that cares).
type walkedRoute struct {
	r        route
	concrete string
}

func walkGateRoutes(t *testing.T, h http.Handler) []walkedRoute {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter returned %T, does not implement chi.Routes", h)
	}
	var out []walkedRoute
	seen := map[route]struct{}{}
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern = normalizePath(pattern)
		r := route{method: strings.ToUpper(method), path: pattern}
		concrete := strings.ReplaceAll(pattern, "/*", "/no-such-endpoint")
		for {
			open := strings.Index(concrete, "{")
			if open < 0 {
				break
			}
			closing := strings.Index(concrete[open:], "}")
			if closing < 0 {
				break
			}
			concrete = concrete[:open] + "x" + concrete[open+closing+1:]
		}
		if _, dup := seen[r]; dup {
			return nil
		}
		seen[r] = struct{}{}
		out = append(out, walkedRoute{r: r, concrete: concrete})
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].r.String() < out[j].r.String() })
	return out
}

// tableKey maps a walked route onto its row: a catch-all pattern is one
// decision under nine method registrations, so all nine read one row.
func tableKey(r route) route {
	if strings.HasSuffix(r.path, wildcardSuffix) {
		r.method = anyMethod
	}
	return r
}

// TestRBACCompleteness_EveryRouteDeclaresItsAuthorization — half (1), the
// completeness invariant and the reason this file exists. Mount a route without
// touching authorization and it lands here, in neither direction of the table.
//
// The route universe is the union of two independently incomplete sources: the
// assembled spec (all domains, including the opt-in ones a given wiring leaves
// nil) and chi.Walk over the router (everything outside /v1 and the console
// WebSocket, neither of which carries an OpenAPI operation).
func TestRBACCompleteness_EveryRouteDeclaresItsAuthorization(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	universe := map[route]struct{}{}
	for path, item := range spec.Paths {
		for method := range pathItemOps(item) {
			universe[tableKey(route{method: method, path: normalizePath(path)})] = struct{}{}
		}
	}
	h := revokedGateRouterWebUI(t, zeroGrantRBAC(t))
	for _, w := range walkGateRoutes(t, h) {
		universe[tableKey(w.r)] = struct{}{}
	}
	if len(universe) < 150 {
		t.Fatalf("route universe is %d routes, expected ~170 — a handler is nil and its whole subtree never mounted, so this sweep would pass over a domain it never saw", len(universe))
	}

	var undeclared []string
	for r := range universe {
		if _, ok := rbacRouteGates[r]; !ok {
			undeclared = append(undeclared, r.String())
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("ROUTE WITH NO AUTHORIZATION DECISION — %d (NIM-843: this is how a route reachable by any authenticated Archon arrives unnoticed):\n  %s\n"+
			"-> add EACH one to rbacRouteGates, either with the questions its gate asks (`check:<resource>.<action>` for RequirePermission*, `holds:…` for RequireAction) "+
			"or with `why:` stating who authorizes it instead. A route with no gate is an allowed answer; a route with no row is not.",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}

	var stale []string
	for r := range rbacRouteGates {
		if _, ok := universe[r]; !ok {
			stale = append(stale, r.String())
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("STALE ROW — %d entries in rbacRouteGates with no such route in the spec or the router (the table must mirror the topology, or it starts describing a perimeter that moved):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}

	// Every declared question must name a permission the catalog actually has,
	// and every gateless row must say why. A row is the place an untruth gets
	// written; these two checks are what make the cheap ones impossible.
	var bad []string
	gateless := 0
	for r, g := range rbacRouteGates {
		if len(g.asks) == 0 {
			gateless++
			if len(strings.TrimSpace(g.why)) < 40 {
				bad = append(bad, r.String()+": no gate and no substantive `why` — state who authorizes instead, or why nothing needs to")
			}
			continue
		}
		for _, a := range g.asks {
			kind, perm, ok := strings.Cut(a, ":")
			switch {
			case !ok || (kind != "check" && kind != "holds"):
				bad = append(bad, r.String()+": question "+a+" must be spelled check:<resource>.<action> or holds:<resource>.<action>")
			default:
				if _, known := rbac.AllowedPermissions[perm]; !known {
					bad = append(bad, r.String()+": question "+a+" names "+perm+", which is not in rbac.AllowedPermissions — a gate on a permission nobody can hold denies everyone")
				}
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("MALFORMED ROW — %d:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}

	t.Logf("guard: %d routes declared (%d gated, %d deliberately gateless)", len(rbacRouteGates), len(rbacRouteGates)-gateless, gateless)
}

// TestRBACCompleteness_DeclaredGateIsTheGateThatRuns — half (2), and the half a
// list cannot have. Every row is read back off the REAL buildRouter: the
// questions recorded are the questions declared, in order, and a gate that
// declared a permission refuses naming that permission.
//
// This is what catches the row that lies — NIM-826's fifth hole was a row whose
// stated reason was false, found only because there was a row to falsify. Delete
// a RequirePermission wrapper from router.go and the route's row still claims
// the gate while the probe records nothing: red, per route, by name.
func TestRBACCompleteness_DeclaredGateIsTheGateThatRuns(t *testing.T) {
	rec := zeroGrantRBAC(t)
	h := revokedGateRouterWebUI(t, rec)
	token := revokedGateToken(t, rbacGateAID)

	probed := 0
	for _, w := range walkGateRoutes(t, h) {
		g, ok := rbacRouteGates[tableKey(w.r)]
		if !ok {
			continue // reported by the completeness half; nothing to verify against
		}
		probed++

		rec.take()
		resp, panicked := serveRecovering(h, w.r.method, w.concrete, token)
		got := rec.take()

		want := make([]rbacAsk, 0, len(g.asks))
		for _, a := range g.asks {
			want = append(want, rbacAsk(a))
		}
		if !sameAsks(got, want) {
			t.Errorf("%s: gate asked %v, table declares %v — the row does not describe the gate that runs",
				w.r, got, want)
			continue
		}
		if len(want) == 0 {
			// A gateless row is proven by the empty recording above. What the
			// handler does next is its own domain's test; here a panic into a
			// stub is the expected shape and says nothing either way.
			continue
		}
		if panicked {
			t.Errorf("%s: reached the handler despite declaring gate %v — the gate ran and let a zero-permission Archon through", w.r, want)
			continue
		}
		if resp.Code != http.StatusForbidden {
			t.Errorf("%s = %d for an Archon holding nothing, want 403 (gate %v); body=%s",
				w.r, resp.Code, want, resp.Body.String())
			continue
		}
		_, perm, _ := strings.Cut(g.asks[0], ":")
		if !strings.Contains(resp.Body.String(), "lacks required permission "+perm) {
			t.Errorf("%s: 403 does not name %s — the route is gated by something else, or the refusal does not tell the operator which grant is missing; body=%s",
				w.r, perm, resp.Body.String())
		}
	}
	if probed < 150 {
		t.Fatalf("probed only %d routes (expected ~170) — the walk lost a subtree and the sweep is quietly covering less than it claims", probed)
	}
	t.Logf("guard: %d routes read back off the live router", probed)
}

func sameAsks(got, want []rbacAsk) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
