// Completeness guard over the RBAC perimeter of the MCP surface (NIM-843).
//
// PROBLEM, and it is worse here than on REST. [Handler.handleToolsCall]
// dispatches every implemented tool through a hand-written switch, and each
// target function carries its own `h.deps.RBAC.Check(...)` written out by hand in
// its own file. There is no middleware, so there is no chain to inspect and no
// single place a check could be hung; the permission of a tool is a line of code
// inside it. Nothing enumerated the dispatch table against the checks, so a
// tool added with a `case` and no check would ship green — and MCP is a PRIMARY
// operator surface (ADR-004), not a wrapper over REST, so that tool is a whole
// perimeter of its own.
//
// THE TABLE is [rbacToolGates]: one row per declared tool — implemented or stub
// — carrying the ordered questions it puts to the RBAC surface, the minimal
// arguments that carry a call as far as the gate, or a written reason why it asks
// nothing. Same two halves as the REST guard (rbac_route_gate_guard_test.go):
//
//	(1) completeness — the key set equals the key set of [catalogManifest], and
//	    each row's `stub` flag equals the manifest's status. A new tool → red. A
//	    stub promoted to implemented → red, so its row has to declare the gate
//	    it now needs rather than inherit "a stub answers before dispatch".
//	(2) truthfulness — the declared questions are the questions that RUN. Every
//	    tool is called against an RBAC surface that records and denies, and the
//	    recording is compared to the row. A row claiming `service.update` on a
//	    tool that checks `service.list` goes red; a row claiming any check at all
//	    on a tool that makes none goes red.
//
// EVERY DEPENDENCY IN THE PROBE IS A TRAP. The services are zero-value or
// panicking stubs, because a tool must authorize BEFORE it touches one. A tool
// that reads its store first reaches a trap and panics, and the guard reports
// that as the least-disclosure defect it is rather than as a test error. This is
// also why the table carries `args`: a tool that validates its arguments before
// asking (the ten `label-set` tools do, via callLabelSet) has to be given enough
// of a body to get past validation, or the probe would read "asks nothing" off
// an argument error and call it a hole.
//
// THREE SPELLINGS, because there are three ways to ask here:
//
//	check:<resource>.<action>   — [PermissionChecker.Check], the scope-aware gate
//	holds:<resource>.<action>   — HoldsAction, the existence gate
//	purview:<resource>.<action> — ResolvePurview + [rbac.Purview.Holds], which is
//	                              how the bulk trait write asks (soul_traits_assign.go:
//	                              the same Purview that supplies its scope answers
//	                              whether the right is held at all, NIM-128)
//
// The distinction is kept because it is the difference the REST side got wrong
// once already (NIM-421: a scope-aware check and an existence gate answering a
// revoked Archon differently), and a table that recorded only the permission
// name could not tell them apart.

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// toolGate — the authorization decision bound to one MCP tool.
//
// asks — the ordered questions the tool puts to the RBAC surface (see the three
// spellings in the file header). args — the minimal arguments that carry the
// call as far as the gate, for the tools that validate before asking; empty
// means `{}` is enough. why — mandatory when asks is empty. stub — the manifest
// declares the tool unimplemented, so tools/call refuses it before dispatch.
type toolGate struct {
	asks []string
	// alsoAsks — the questions the tool asks only AFTER everything in asks is
	// satisfied. A probe that grants nothing stops at the first denial and is
	// blind to a SECOND gate inside the same tool, so these are read back with a
	// separate pass that grants `asks` and nothing else
	// ([TestRBACCompleteness_ToolGateBeyondTheFirstQuestion]). Three tools declare
	// one today. The reason the field exists is the first of them: the console gate
	// over a verb-shell Errand (NIM-197) was deletable with the single-pass guard
	// still green.
	alsoAsks []string
	// orGate marks asks as ALTERNATIVES rather than a sequence: the tool tries them
	// in order and the first that passes ends the question. The distinction is not
	// cosmetic — a sequence means every one of them must be held, a disjunction means
	// any one of them opens the tool — and the two passes see it differently: granting
	// nothing records every alternative (all denied), while granting them records only
	// the first. Without the marker the second pass would read a satisfied disjunction
	// as a question that disappeared.
	orGate bool
	args   string
	why    string
	stub   bool
}

// rbacToolGates — every tool in [catalogManifest] and what authorizes it.
var rbacToolGates = map[string]toolGate{
	// --- operators (ADR-014) ---
	"keeper.operator.create":      {asks: []string{"check:operator.create"}, args: `{"aid":"archon-probe","display_name":"probe"}`},
	"keeper.operator.revoke":      {asks: []string{"check:operator.revoke"}, args: `{"aid":"archon-probe"}`},
	"keeper.operator.issue-token": {asks: []string{"check:operator.issue-token"}, args: `{"aid":"archon-probe"}`},

	// --- roles / synods ---
	"keeper.role.create":           {asks: []string{"check:role.create"}},
	"keeper.role.delete":           {asks: []string{"check:role.delete"}},
	"keeper.role.list":             {asks: []string{"check:role.list"}},
	"keeper.role.update":           {asks: []string{"check:role.update"}},
	"keeper.role.grant-operator":   {asks: []string{"check:role.grant-operator"}},
	"keeper.role.revoke-operator":  {asks: []string{"check:role.revoke-operator"}},
	"keeper.synod.create":          {asks: []string{"check:synod.create"}},
	"keeper.synod.update":          {asks: []string{"check:synod.update"}},
	"keeper.synod.delete":          {asks: []string{"check:synod.delete"}},
	"keeper.synod.list":            {asks: []string{"check:synod.list"}},
	"keeper.synod.add-operator":    {asks: []string{"check:synod.add-operator"}},
	"keeper.synod.remove-operator": {asks: []string{"check:synod.remove-operator"}},
	"keeper.synod.grant-role":      {asks: []string{"check:synod.grant-role"}},
	"keeper.synod.revoke-role":     {asks: []string{"check:synod.revoke-role"}},

	// --- incarnations. Unlike REST, every single-incarnation tool reaches the
	// scope-aware Check (checkIncarnationScope), including the reads: MCP has no
	// chi middleware, so there is no existence gate to stand in front of it. ---
	"keeper.incarnation.create":     {asks: []string{"check:incarnation.create"}, args: `{"id":"inc-probe","service":"svc-probe"}`},
	"keeper.incarnation.get":        {asks: []string{"check:incarnation.get"}, args: `{"id":"inc-probe"}`},
	"keeper.incarnation.list":       {asks: []string{"check:incarnation.list"}},
	"keeper.incarnation.history":    {asks: []string{"check:incarnation.history"}, args: `{"id":"inc-probe"}`},
	"keeper.incarnation.run":        {asks: []string{"check:incarnation.run"}, args: `{"id":"inc-probe","scenario":"restart"}`},
	"keeper.incarnation.unlock":     {asks: []string{"check:incarnation.unlock"}, args: `{"id":"inc-probe","reason":"gate probe"}`},
	"keeper.incarnation.upgrade":    {asks: []string{"check:incarnation.upgrade"}, args: `{"id":"inc-probe","to_version":"2"}`},
	"keeper.incarnation.rerun-last": {asks: []string{"check:incarnation.rerun-last"}, args: `{"id":"inc-probe","reason":"gate probe"}`},
	"keeper.incarnation.destroy":    {asks: []string{"check:incarnation.destroy"}, args: `{"id":"inc-probe","allow_destroy":true}`},
	"keeper.incarnation.label-set":  {asks: []string{"check:incarnation.label-set"}, args: `{"id":"inc-probe"}`},
	"keeper.incarnation.traits-set": {asks: []string{"check:incarnation.traits-set"}, args: `{"id":"inc-probe","traits":{}}`},
	// The three membership tools ask a SECOND question after their own: the roster
	// is narrowed to the caller's soul visibility through the Purview, so a host
	// the caller cannot see is not bindable, unbindable or listable
	// (incarnation_members.go). `members` declares it too, but this guard cannot
	// read it back — the probe's roster scan reaches a trap first, and that shows
	// up in the trap list the second pass logs.
	"keeper.incarnation.bind-member": {
		asks:     []string{"check:incarnation.bind-member"},
		alsoAsks: []string{"purview:soul.list"},
		args:     `{"id":"inc-probe","sids":["host-1.example.com"]}`,
	},
	"keeper.incarnation.unbind-member": {
		asks:     []string{"check:incarnation.unbind-member"},
		alsoAsks: []string{"purview:soul.list"},
		args:     `{"id":"inc-probe","sid":"host-1.example.com"}`,
	},
	"keeper.incarnation.members": {asks: []string{"check:incarnation.get"}, args: `{"id":"inc-probe"}`},

	// --- souls. run-command is the one soul tool not paired with soul.<action>:
	// a non-interactive shell is the console right (ADR-0074). ---
	"keeper.soul.create":            {asks: []string{"check:soul.create"}},
	"keeper.soul.issue-token":       {asks: []string{"check:soul.issue-token"}, args: `{"sid":"host-1.example.com"}`},
	"keeper.soul.forget":            {asks: []string{"check:soul.forget"}, args: `{"sid":"host-1.example.com"}`},
	"keeper.soul.coven-assign":      {asks: []string{"check:soul.coven-assign"}, args: `{"mode":"append","label":"web","selector":{"sids":["host-1.example.com"]}}`},
	"keeper.soul.ssh-target.update": {asks: []string{"check:soul.ssh-target-update"}, args: `{"sid":"host-1.example.com","ssh_port":22,"ssh_user":"root","soul_path":"/usr/bin/soul"}`},
	"keeper.soul.run-command":       {asks: []string{"check:soul.console"}, args: `{"sid":"host-1.example.com","command":"id"}`},
	// The bulk trait write asks through the Purview rather than through Check:
	// a trait pair IS a scope dimension (NIM-128), so the same projection that
	// supplies gate (a)'s scope answers whether the right is held at all
	// (holdsTraitsAssign, soul_traits_assign.go).
	"keeper.soul.traits-assign": {asks: []string{"purview:soul.traits-assign"}, args: `{"mode":"merge","traits":{"k":"v"},"selector":{"sids":["host-1.example.com"]}}`},
	"keeper.soul.list":          {stub: true, why: "declared in the manifest but not implemented (M0.7.a): tools/call refuses it on the status flag before dispatch, so there is no code to authorize. Implementing it must also give this row its gate — the stub flag is checked against the manifest, so the promotion cannot be silent"},

	// --- errands (ADR-033) ---
	// `errand.run` is not sufficient for a verb-shell module: `core.exec.run` and
	// `core.cmd.shell` carry an arbitrary command line, so the same context set
	// must also satisfy `soul.console` (ADR-0074 amendment, NIM-197 — the sibling
	// keeper.soul.run-command always required it, and this closed the way around).
	// The probe names a verb-shell module on purpose; with an ordinary module the
	// second question is not asked and this row would be unfalsifiable. What this
	// row does NOT pin is the ENFORCEMENT: `shellgate` defaults to ModeWarn
	// (`console.errand_shell_gate` is optional), and warn mode still calls the check
	// and then discards its denial — so the recording is identical either way. That
	// the denial is acted on is shellgate's own claim, not this table's.
	"keeper.soul.errand.run": {
		asks:     []string{"check:errand.run"},
		alsoAsks: []string{"check:soul.console"},
		args:     `{"sid":"host-1.example.com","module":"core.exec.run"}`,
	},
	// The three read/cancel tools ask through the PURVIEW since NIM-841/842/846:
	// the right's existence and the scope it carries come back in one answer, and
	// the scope is then pushed into the store's query instead of being applied to
	// rows already fetched. REST asks the same three with its existence gate and
	// narrows in the same store.
	"keeper.errand.list":   {asks: []string{"purview:errand.list"}},
	"keeper.errand.get":    {asks: []string{"purview:errand.list"}, args: `{"errand_id":"11111111-1111-1111-1111-111111111111"}`},
	"keeper.errand.cancel": {asks: []string{"purview:errand.cancel"}, args: `{"errand_id":"11111111-1111-1111-1111-111111111111"}`},

	// --- plugins / sigil keys ---
	"keeper.plugin.allow":          {asks: []string{"check:plugin.allow"}},
	"keeper.plugin.revoke":         {asks: []string{"check:plugin.revoke"}},
	"keeper.plugin.list":           {asks: []string{"check:plugin.list"}},
	"keeper.sigil.key.introduce":   {asks: []string{"check:sigil.key-introduce"}},
	"keeper.sigil.key.list":        {asks: []string{"check:sigil.key-list"}},
	"keeper.sigil.key.set-primary": {asks: []string{"check:sigil.key-set-primary"}},
	"keeper.sigil.key.retire":      {asks: []string{"check:sigil.key-retire"}},

	// --- services ---
	"keeper.service.register":   {asks: []string{"check:service.register"}},
	"keeper.service.update":     {asks: []string{"check:service.update"}},
	"keeper.service.label-set":  {asks: []string{"check:service.label-set"}, args: `{"id":"svc-probe"}`},
	"keeper.service.list":       {asks: []string{"check:service.list"}},
	"keeper.service.deregister": {asks: []string{"check:service.deregister"}},

	// --- settings (ADR-0073). list reads with setting.read — the same right REST
	// mounts on GET /v1/settings, not a list-specific one. ---
	"keeper.setting.list":   {asks: []string{"check:setting.read"}},
	"keeper.setting.update": {asks: []string{"check:setting.update"}},
	"keeper.setting.delete": {asks: []string{"check:setting.delete"}},

	// --- augur (ADR-025) / oracle (ADR-030) ---
	"keeper.augur.omen.create":       {asks: []string{"check:omen.create"}},
	"keeper.augur.omen.list":         {asks: []string{"check:omen.list"}},
	"keeper.augur.omen.label-set":    {asks: []string{"check:omen.label-set"}, args: `{"id":"omen-probe"}`},
	"keeper.augur.omen.delete":       {asks: []string{"check:omen.delete"}},
	"keeper.augur.rite.create":       {asks: []string{"check:rite.create"}},
	"keeper.augur.rite.list":         {asks: []string{"check:rite.list"}},
	"keeper.augur.rite.delete":       {asks: []string{"check:rite.delete"}},
	"keeper.oracle.vigil.create":     {asks: []string{"check:vigil.create"}},
	"keeper.oracle.vigil.list":       {asks: []string{"check:vigil.list"}},
	"keeper.oracle.vigil.label-set":  {asks: []string{"check:vigil.label-set"}, args: `{"id":"11111111-1111-1111-1111-111111111111"}`},
	"keeper.oracle.vigil.delete":     {asks: []string{"check:vigil.delete"}},
	"keeper.oracle.decree.create":    {asks: []string{"check:decree.create"}},
	"keeper.oracle.decree.list":      {asks: []string{"check:decree.list"}},
	"keeper.oracle.decree.label-set": {asks: []string{"check:decree.label-set"}, args: `{"id":"11111111-1111-1111-1111-111111111111"}`},
	"keeper.oracle.decree.delete":    {asks: []string{"check:decree.delete"}},

	// --- push ---
	"keeper.push.apply":              {asks: []string{"check:push.apply"}},
	"keeper.push.cleanup":            {stub: true, why: "declared in the manifest but not implemented (a separate slice): tools/call refuses it on the status flag before dispatch, so there is no code to authorize. The stub flag is checked against the manifest, so implementing it forces this row to declare a gate"},
	"keeper.push-provider.create":    {asks: []string{"check:push-provider.create"}, args: `{"id":"pp-probe","params":{}}`},
	"keeper.push-provider.update":    {asks: []string{"check:push-provider.update"}, args: `{"id":"pp-probe"}`},
	"keeper.push-provider.delete":    {asks: []string{"check:push-provider.delete"}, args: `{"id":"pp-probe"}`},
	"keeper.push-provider.list":      {asks: []string{"check:push-provider.list"}},
	"keeper.push-provider.read":      {asks: []string{"check:push-provider.read"}, args: `{"id":"pp-probe"}`},
	"keeper.push-provider.label-set": {asks: []string{"check:push-provider.label-set"}, args: `{"id":"pp-probe"}`},

	// --- heralds / tidings (ADR-052) ---
	"keeper.herald.create":    {asks: []string{"check:herald.create"}},
	"keeper.herald.update":    {asks: []string{"check:herald.update"}},
	"keeper.herald.label-set": {asks: []string{"check:herald.label-set"}, args: `{"id":"herald-probe"}`},
	"keeper.herald.delete":    {asks: []string{"check:herald.delete"}},
	"keeper.herald.list":      {asks: []string{"check:herald.list"}},
	"keeper.herald.read":      {asks: []string{"check:herald.read"}},
	"keeper.tiding.create":    {asks: []string{"check:tiding.create"}},
	"keeper.tiding.update":    {asks: []string{"check:tiding.update"}},
	"keeper.tiding.label-set": {asks: []string{"check:tiding.label-set"}, args: `{"id":"tiding-probe"}`},
	"keeper.tiding.delete":    {asks: []string{"check:tiding.delete"}},
	"keeper.tiding.list":      {asks: []string{"check:tiding.list"}},
	"keeper.tiding.read":      {asks: []string{"check:tiding.read"}},

	// --- voyages (ADR-043). start/list/get go through the REST VoyageHandler and
	// reach its gate here; cancel cannot, and says so. ---
	"keeper.voyage.start": {asks: []string{"check:incarnation.run"}, args: `{"kind":"scenario","scenario_name":"restart","target":{"incarnations":["inc-probe"]}}`},
	// The two reads narrow the returned runs to the caller's soul visibility through
	// the Purview after the history check (NIM-841/842/846), the same second question
	// the errand reads ask for themselves.
	"keeper.voyage.list": {asks: []string{"check:incarnation.history"}, alsoAsks: []string{"purview:soul.list"}},
	"keeper.voyage.get": {
		asks:     []string{"check:incarnation.history"},
		alsoAsks: []string{"purview:soul.list"},
		args:     `{"voyage_id":"01J0000000000000000000000Z"}`,
	},
	// Cancel asks the DISJUNCTION of the two by-kind rights before reading the row —
	// NIM-841/842/846 closed a 403-vs-404 enumeration hole that way — which is also
	// why this row can be read back at all: the older code reached its gate only
	// after a row this probe cannot supply. The by-kind choice still happens after
	// that read, so what is verified here is the pre-check; handlers.
	// TestVoyageCancel_RBACDenied pins the scenario branch of the choice.
	"keeper.voyage.cancel": {
		asks:   []string{"check:incarnation.run", "check:errand.run"},
		orGate: true,
		args:   `{"voyage_id":"01J0000000000000000000000Z"}`,
	},
}

// --- the probe ---

// toolGateAID is the probe's Archon.
const toolGateAID = "archon-tool-gate-sweep"

// toolAsk — one recorded question, in the table's spelling.
type toolAsk string

// recordingRBAC records every question put to the RBAC surface and denies every
// one of them. Embedding [*rbac.Enforcer] keeps the rest of the surface
// (RolesOf, CovenScope, the purview projection) answering as production does,
// over a snapshot that grants nothing.
type recordingRBAC struct {
	*rbac.Enforcer
	mu   sync.Mutex
	asks []toolAsk
}

func (r *recordingRBAC) record(kind, resource, action string) {
	r.mu.Lock()
	r.asks = append(r.asks, toolAsk(kind+":"+resource+"."+action))
	r.mu.Unlock()
}

func (r *recordingRBAC) Check(aid, resource, action string, ctx map[string]string) error {
	r.record("check", resource, action)
	return r.Enforcer.Check(aid, resource, action, ctx)
}

func (r *recordingRBAC) HoldsAction(aid, resource, action string) bool {
	r.record("holds", resource, action)
	return r.Enforcer.HoldsAction(aid, resource, action)
}

func (r *recordingRBAC) ResolvePurview(aid, resource, action string) rbac.Purview {
	r.record("purview", resource, action)
	return r.Enforcer.ResolvePurview(aid, resource, action)
}

// take returns the questions recorded since the last call, with consecutive
// repeats of one question collapsed: an OR-Check over several scope contexts
// (checkIncarnationScope) asks the same question per candidate, and the table
// records WHICH question, not how many contexts it was tried against.
func (r *recordingRBAC) take() []toolAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw := r.asks
	r.asks = nil
	out := make([]toolAsk, 0, len(raw))
	for _, a := range raw {
		if len(out) > 0 && out[len(out)-1] == a {
			continue
		}
		out = append(out, a)
	}
	return out
}

// --- trap dependencies: reaching one before asking RBAC is the defect ---

type trapVoyageStore struct{ *fakePool }

func (trapVoyageStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("voyage CopyFrom reached before authorization")
}

func (trapVoyageStore) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("voyage BeginTx reached before authorization")
}

type trapVoyageScenarioResolver struct{}

func (trapVoyageScenarioResolver) ResolveIncarnations(context.Context, handlers.VoyageScenarioFilter) ([]string, error) {
	panic("voyage scenario resolver reached before authorization")
}

type trapVoyageCommandResolver struct{}

func (trapVoyageCommandResolver) ResolveSIDs(context.Context, handlers.VoyageCommandFilter) ([]string, error) {
	panic("voyage command resolver reached before authorization")
}

func (trapVoyageCommandResolver) ResolveSIDsInScope(context.Context, handlers.VoyageCommandFilter, soulpurview.Scope) (handlers.ScopedSIDs, error) {
	panic("voyage command resolver reached before authorization")
}

// toolGateHandler — a Handler with every optional dependency non-nil, because a
// nil one short-circuits tools/call with "not configured" before the permission
// check and would read as "this tool asks nothing".
func toolGateHandler(t *testing.T) (*Handler, *recordingRBAC) {
	t.Helper()
	return toolGateHandlerGranting(t, nil)
}

// toolGateHandlerGranting is [toolGateHandler] with `granted` held by the probe's
// AID and nothing else — for reading the questions a tool asks AFTER its first one
// is satisfied. The first pass grants nothing and therefore stops at the first
// denial, which is blind to a SECOND gate in the same tool.
func toolGateHandlerGranting(t *testing.T, granted []string) (*Handler, *recordingRBAC) {
	t.Helper()
	return toolGateHandlerWith(t, granted, &fakePool{})
}

// toolGateHandlerDeeper is [toolGateHandlerGranting] over a pool that ANSWERS
// instead of reporting no rows: an incarnation row and a membership roster.
//
// The second pass needs it. Three member tools ask their scope-aware question and
// only then narrow the roster through the Purview, and with an empty pool
// `SelectByID` returns no-such-row and the tool returns before the second question
// is ever asked — so the pass would have read "no second question" off a fake.
func toolGateHandlerDeeper(t *testing.T, granted []string) (*Handler, *recordingRBAC) {
	t.Helper()
	return toolGateHandlerWith(t, granted, &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) {
			return &incarnation.Incarnation{
				ID:      name,
				Service: "svc-probe",
				Status:  incarnation.StatusReady,
			}, nil
		},
		memberSIDs: []string{"host-1.example.com"},
	})
}

func toolGateHandlerWith(t *testing.T, granted []string, pool *fakePool) (*Handler, *recordingRBAC) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// A role that holds `granted` and nothing else — and, when granted is empty, a
	// role that holds nothing at all rather than no role. "No roles" is a
	// different state that some code paths can answer differently, and the
	// refusal this sweep reads must be the ordinary permission denial.
	cfg := &rbactest.Config{Roles: []rbactest.Role{
		{Name: "probe", Operators: []string{toolGateAID}, Permissions: granted},
	}}
	base, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("rbactest.NewEnforcer: %v", err)
	}
	rec := &recordingRBAC{Enforcer: base}

	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool: pool, Issuer: &fakeIssuer{}, RBAC: rec, TTLDefault: time.Hour, Logger: logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}
	rbacSvc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool, Logger: logger})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	svcReg, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool, Logger: logger})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	augurSvc, err := augur.NewService(augur.ServiceDeps{Pool: pool, Logger: logger})
	if err != nil {
		t.Fatalf("augur.NewService: %v", err)
	}
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("oracle.NewWhereEvaluator: %v", err)
	}
	oracleSvc, err := oracle.NewService(oracle.ServiceDeps{Pool: pool, Where: where, Logger: logger})
	if err != nil {
		t.Fatalf("oracle.NewService: %v", err)
	}
	ppSvc, err := pushprovider.NewService(pushprovider.ServiceDeps{Pool: pool, Logger: logger})
	if err != nil {
		t.Fatalf("pushprovider.NewService: %v", err)
	}
	heraldSvc, err := herald.NewService(herald.ServiceDeps{Pool: pool, Logger: logger})
	if err != nil {
		t.Fatalf("herald.NewService: %v", err)
	}

	h, err := NewHandler(HandlerDeps{
		OperatorSvc:     opSvc,
		RBAC:            rec,
		PurviewResolver: rec,
		RBACRoles:       rbacSvc,
		ServiceSvc:      svcReg,
		AugurSvc:        augurSvc,
		OracleSvc:       oracleSvc,
		PushProviderSvc: ppSvc,
		HeraldSvc:       heraldSvc,
		// Zero-value services: never reached, because the permission check comes
		// first. If one is reached it nil-derefs, and the sweep reports the panic
		// as the ordering defect it is.
		SigilSvc:               &sigil.Service{},
		SigilKeySvc:            &sigil.KeyService{},
		Settings:               &handlers.SettingsHandler{},
		PushRun:                &pushorch.PushRun{},
		ErrandDispatcher:       &errand.Dispatcher{},
		ErrandStore:            errand.NewStore(pool),
		ShellGate:              shellgate.New(shellgate.ModeEnforce, nil, nil),
		ConsolePlaneEnabled:    func() bool { return true },
		VoyageDB:               trapVoyageStore{pool},
		VoyageScenarioResolver: trapVoyageScenarioResolver{},
		VoyageCommandResolver:  trapVoyageCommandResolver{},
		ScenarioRunner:         &mcpStarter{},
		ServiceRegistry:        &mcpResolver{ok: true},
		ServiceLoader:          &mcpLoader{},
		ScenarioDestroyer:      &mcpDestroyer{},
		AuditWriter:            &recordingAudit{},
		Logger:                 logger,
		IncarnationDB:          pool,
		SoulDB:                 pool,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// TestRBACCompleteness_EveryToolDeclaresItsAuthorization — half (1). Add a
// `case` to the dispatch switch and a manifest entry without touching
// authorization, and the tool lands here with no row.
func TestRBACCompleteness_EveryToolDeclaresItsAuthorization(t *testing.T) {
	manifest := map[string]bool{} // name -> is a stub
	for _, e := range catalogManifest {
		manifest[e.decl.Name] = e.status == toolStatusStub
	}
	if len(manifest) < 90 {
		t.Fatalf("catalogManifest holds %d tools, expected ~97 — the sweep would pass over a domain it never saw", len(manifest))
	}

	var undeclared, stale, mismatched []string
	for name, isStub := range manifest {
		g, ok := rbacToolGates[name]
		if !ok {
			undeclared = append(undeclared, name)
			continue
		}
		if g.stub != isStub {
			mismatched = append(mismatched, name+": row says stub="+boolWord(g.stub)+", catalogManifest says stub="+boolWord(isStub))
		}
	}
	for name := range rbacToolGates {
		if _, ok := manifest[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	sort.Strings(mismatched)

	if len(undeclared) > 0 {
		t.Errorf("TOOL WITH NO AUTHORIZATION DECISION — %d (NIM-843: the dispatch switch is hand-written and every check is hand-written inside its own tool, so a tool without one ships green):\n  %s\n"+
			"-> add EACH one to rbacToolGates with the questions it asks (`check:`/`holds:`/`purview:` + <resource>.<action>) and, if it validates arguments before asking, the minimal `args` that reach the gate.",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("STALE ROW — %d entries in rbacToolGates naming no tool in catalogManifest:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
	if len(mismatched) > 0 {
		t.Errorf("STUB FLAG DISAGREES WITH THE MANIFEST — %d. A stub is refused before dispatch and so declares no gate; promoting it to implemented must declare one:\n  %s",
			len(mismatched), strings.Join(mismatched, "\n  "))
	}

	var bad []string
	gateless := 0
	for name, g := range rbacToolGates {
		if len(g.asks) == 0 {
			gateless++
			if len(strings.TrimSpace(g.why)) < 40 {
				bad = append(bad, name+": declares no question and no substantive `why` — state who authorizes instead, or why nothing does")
			}
			continue
		}
		for _, a := range g.asks {
			kind, perm, ok := strings.Cut(a, ":")
			switch {
			case !ok || (kind != "check" && kind != "holds" && kind != "purview"):
				bad = append(bad, name+": question "+a+" must be spelled check:/holds:/purview: + <resource>.<action>")
			default:
				if _, known := rbac.AllowedPermissions[perm]; !known {
					bad = append(bad, name+": question "+a+" names "+perm+", which is not in rbac.AllowedPermissions — a gate on a permission nobody can hold denies everyone")
				}
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("MALFORMED ROW — %d:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}

	t.Logf("guard: %d tools declared (%d with a gate, %d gateless)", len(rbacToolGates), len(rbacToolGates)-gateless, gateless)
}

// TestRBACCompleteness_DeclaredToolGateIsTheGateThatRuns — half (2). Each tool
// is called against an RBAC surface that records and denies; the recording is
// the row, or the row is wrong.
func TestRBACCompleteness_DeclaredToolGateIsTheGateThatRuns(t *testing.T) {
	h, rec := toolGateHandler(t)
	claims := &jwt.Claims{}
	claims.Subject = toolGateAID

	names := make([]string, 0, len(rbacToolGates))
	for _, e := range catalogManifest {
		names = append(names, e.decl.Name)
	}
	sort.Strings(names)

	verified := 0
	for _, name := range names {
		g, ok := rbacToolGates[name]
		if !ok {
			continue // reported by the completeness half
		}
		args := g.args
		if args == "" {
			args = `{}`
		}
		params, err := json.Marshal(toolsCallParams{Name: name, Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: marshal params: %v", name, err)
		}

		rec.take()
		resp, panicked := callToolRecovering(h, claims, params)
		got := rec.take()

		want := make([]toolAsk, 0, len(g.asks))
		for _, a := range g.asks {
			want = append(want, toolAsk(a))
		}
		if panicked && len(got) == 0 {
			t.Errorf("%s: reached a dependency before asking RBAC anything (the probe's services are traps) — the tool authorizes after it touches state", name)
			continue
		}
		if !sameToolAsks(got, want) {
			t.Errorf("%s: asked %v, table declares %v — the row does not describe the code that runs (arguments used: %s)", name, got, want, args)
			continue
		}
		if len(want) == 0 {
			continue // a gateless row is proven by the empty recording
		}
		if code := toolErrorCode(t, resp); code != mcpCodeForbidden {
			t.Errorf("%s: asked %v and was denied, but answered code=%q instead of %q — the gate ran and its answer was dropped",
				name, want, code, mcpCodeForbidden)
			continue
		}
		verified++
	}
	if verified < 90 {
		t.Fatalf("verified only %d tool gates (expected ~94) — the sweep is covering less than it claims", verified)
	}
	t.Logf("guard: %d tool gates read back off the live dispatcher", verified)
}

// TestRBACCompleteness_ToolGateBeyondTheFirstQuestion — the second pass, and the
// one that can see a gate standing behind another.
//
// The main sweep grants nothing, so every tool is refused at its first question
// and the recording stops there: a tool asking A and then B looks exactly like a
// tool asking only A. Here each row's own `asks` are GRANTED and nothing else is,
// so the tool proceeds to whatever it asks next, and the recording must be the
// row in full — `asks` followed by `alsoAsks`. Delete the console gate from
// keeper.soul.errand.run and this goes red naming the question that stopped being
// asked; the main sweep would not notice.
//
// Run for every gated row, not only the ones declaring `alsoAsks`: an UNDECLARED
// second question is the case worth catching, and it can only be caught by asking
// every row.
func TestRBACCompleteness_ToolGateBeyondTheFirstQuestion(t *testing.T) {
	names := make([]string, 0, len(rbacToolGates))
	for name := range rbacToolGates {
		names = append(names, name)
	}
	sort.Strings(names)

	claims := &jwt.Claims{}
	claims.Subject = toolGateAID

	checked := 0
	var stoppedAtTrap []string
	for _, name := range names {
		g := rbacToolGates[name]
		if len(g.asks) == 0 {
			continue // nothing to satisfy, so nothing can stand behind it
		}
		granted := make([]string, 0, len(g.asks))
		for _, a := range g.asks {
			_, perm, _ := strings.Cut(a, ":")
			granted = append(granted, perm)
		}
		h, rec := toolGateHandlerDeeper(t, granted)

		args := g.args
		if args == "" {
			args = `{}`
		}
		params, err := json.Marshal(toolsCallParams{Name: name, Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: marshal params: %v", name, err)
		}
		rec.take()
		_, panicked := callToolRecovering(h, claims, params)
		got := rec.take()

		// A disjunction ends at its first satisfied alternative, so granting the
		// whole row makes the rest of it disappear — correctly.
		declared := g.asks
		if g.orGate {
			declared = g.asks[:1]
		}
		want := make([]toolAsk, 0, len(declared)+len(g.alsoAsks))
		for _, a := range declared {
			want = append(want, toolAsk(a))
		}
		for _, a := range g.alsoAsks {
			want = append(want, toolAsk(a))
		}
		if !sameToolAsks(got, want) {
			t.Errorf("%s: with %v granted the tool asked %v, table declares %v — a question stands behind the first one that no row mentions (or one it mentions is gone). panicked=%v",
				name, granted, got, want, panicked)
			continue
		}
		checked++
		if panicked {
			// The tool authorized and then reached a trap dependency, which is
			// the ordering this file wants — but it also means the pass saw only
			// as far as that dependency. Counted rather than hidden: it is the
			// measure of how much of the surface this second claim covers.
			stoppedAtTrap = append(stoppedAtTrap, name)
		}
	}
	if checked < 90 {
		t.Fatalf("read back only %d rows past their first question (expected ~94)", checked)
	}
	sort.Strings(stoppedAtTrap)

	// The blind spots are a declared set, not a log line. A tool that starts
	// stopping at a trap has become unreadable past its first question, and a
	// silently growing list of those is how this pass would quietly stop covering
	// the surface it claims — the same failure the table itself exists to prevent.
	want := make([]string, 0, len(toolGateTrapStopped))
	for name := range toolGateTrapStopped {
		want = append(want, name)
	}
	sort.Strings(want)
	if strings.Join(stoppedAtTrap, ",") != strings.Join(want, ",") {
		t.Errorf("the set of tools this pass cannot read past changed:\n  got  %v\n  want %v\n"+
			"-> a tool that now stops at a trap is no longer covered past its first question; add it to toolGateTrapStopped WITH the reason, or give the probe the dependency it needs.",
			stoppedAtTrap, want)
	}
	t.Logf("guard: %d rows read back past their first question; %d declared unreadable past it", checked, len(want))
}

// toolGateTrapStopped — the tools whose post-gate step reaches a trap dependency
// before returning, so [TestRBACCompleteness_ToolGateBeyondTheFirstQuestion]
// cannot see a question standing behind that step. Each one IS confirmed to
// authorize first (it got past its gate to reach the trap at all), which is the
// ordering this file wants; what is unknown is only whether something else is
// asked afterwards.
var toolGateTrapStopped = map[string]string{
	"keeper.incarnation.label-set": "reaches the label write after checking incarnation.label-set",
	"keeper.incarnation.members":   "the roster scan trips the fake pool after checking incarnation.get; its purview narrowing (incarnation_members.go) is therefore declared in prose on that row rather than read back",
	"keeper.plugin.list":           "reaches the zero-value sigil.Service after checking plugin.list",
	"keeper.setting.list":          "reaches the zero-value SettingsHandler after checking setting.read",
	"keeper.sigil.key.introduce":   "reaches the zero-value sigil.KeyService after checking sigil.key-introduce",
	"keeper.sigil.key.list":        "reaches the zero-value sigil.KeyService after checking sigil.key-list",
	"keeper.voyage.start":          "reaches the trap voyage resolver after the REST handler's by-kind check",
}

// TestRBACCompleteness_StubToolIsRefusedBeforeDispatch pins the one claim the
// stub rows rest on: an unimplemented tool never reaches code that could
// authorize, so "declares no gate" is the whole truth about it.
func TestRBACCompleteness_StubToolIsRefusedBeforeDispatch(t *testing.T) {
	h, rec := toolGateHandler(t)
	claims := &jwt.Claims{}
	claims.Subject = toolGateAID

	stubs := 0
	for name, g := range rbacToolGates {
		if !g.stub {
			continue
		}
		stubs++
		params, err := json.Marshal(toolsCallParams{Name: name, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("%s: marshal params: %v", name, err)
		}
		rec.take()
		resp, panicked := callToolRecovering(h, claims, params)
		if panicked {
			t.Errorf("%s: a stub tool reached code that panicked — it is no longer refused before dispatch", name)
			continue
		}
		if got := rec.take(); len(got) != 0 {
			t.Errorf("%s: a stub tool asked %v — it is executing, and its row must declare that gate instead of `stub`", name, got)
		}
		if code := toolErrorCode(t, resp); code != mcpCodeNotImplemented {
			t.Errorf("%s: stub answered code=%q, want %q", name, code, mcpCodeNotImplemented)
		}
	}
	if stubs == 0 {
		t.Skip("no stub tools left in the manifest")
	}
}

// callToolRecovering runs one tools/call and reports whether it panicked. A
// panic is a trap dependency reached before authorization, which the caller
// reports rather than letting it abort the binary.
func callToolRecovering(h *Handler, claims *jwt.Claims, params json.RawMessage) (resp jsonRPCResponse, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	resp, _ = h.handleToolsCall(context.Background(), claims, jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(1), Method: "tools/call", Params: params,
	})
	return resp, false
}

// toolErrorCode pulls the MCP code out of the JSON-RPC error's data payload.
func toolErrorCode(t *testing.T, resp jsonRPCResponse) string {
	t.Helper()
	if resp.Error == nil {
		return ""
	}
	te, ok := resp.Error.Data.(mcpToolError)
	if !ok {
		return ""
	}
	return te.Code
}

func sameToolAsks(got, want []toolAsk) bool {
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

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
