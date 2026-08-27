package mcp

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
)

// Coven-scoped grants over the MCP execution pair (NIM-650): `errand.run` on
// keeper.soul.errand.run, and `soul.console` on both that tool's verb-shell half
// and keeper.soul.run-command.
//
// Separate from the REST guards in the api package on purpose. The two surfaces
// share nothing on this path but the enforcer: REST resolves the host's Covens
// in a router selector, MCP in [Handler.hostContexts]. A fix applied to one
// leaves the other exactly as broken, with every test on the fixed side green
// about it. ADR-004 makes OpenAPI and MCP equally primary — "narrows over
// OpenAPI, denies over MCP" is not a partial fix.
//
// Every case runs under BOTH ways an operator ends up coven-scoped: the
// permission's own `on coven=web` suffix and the role's `default_scope`. They
// are different columns and reach [rbac.effectiveScope] by different routes; a
// guard written with one alone proves nothing about the other.
//
// An ALLOWED call lands on the nil-guard (internal-error) — no dispatcher is
// wired — and a refusal is `forbidden`. The two are never confusable, and that
// difference IS the assertion.

const (
	covenWebSID  = "web-01.example.com"
	covenProdSID = "db-01.example.com"
	covenBothSID = "edge-01.example.com"
)

// covenPool — SoulDB fake serving the one column the gate reads.
// byHost: SID → Coven labels; a SID absent from the map has no row at all
// (ErrNoRows), which is how an unknown host reaches the gate.
type covenPool struct {
	byHost map[string][]string
	reads  int
}

func (p *covenPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), nil
}

func (p *covenPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.reads++
	sid, _ := args[0].(string)
	labels, ok := p.byHost[sid]
	if !ok {
		return covenMissingRow{}
	}
	return covenLabelRow{labels: labels}
}

func (p *covenPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (p *covenPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, pgx.ErrTxClosed
}

type covenLabelRow struct{ labels []string }

func (r covenLabelRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if out, ok := dest[0].(*[]string); ok {
			*out = r.labels
		}
	}
	return nil
}

type covenMissingRow struct{}

func (covenMissingRow) Scan(...any) error { return pgx.ErrNoRows }

func covenTestPool() *covenPool {
	return &covenPool{byHost: map[string][]string{
		covenWebSID:  {"web"},
		covenProdSID: {"prod"},
		covenBothSID: {"prod", "web"},
	}}
}

// covenScopeForm — the two routes a coven scope takes to the resolver.
type covenScopeForm struct {
	name string
	cfg  func(perms ...string) *rbactest.Config
}

func covenScopeForms() []covenScopeForm {
	return []covenScopeForm{
		{
			name: "scope on the permission",
			cfg: func(perms ...string) *rbactest.Config {
				scoped := make([]string, len(perms))
				for i, p := range perms {
					scoped[i] = p + " on coven=web"
				}
				return &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"}, Permissions: scoped},
				}}
			},
		},
		{
			name: "default_scope on the role",
			cfg: func(perms ...string) *rbactest.Config {
				return &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"},
						Permissions: perms, DefaultScope: "coven=web"},
				}}
			},
		},
	}
}

// newCovenScopeHandler wires the tools the way the daemon does for this path:
// RBAC + SoulDB + an enforcing ShellGate, no dispatcher.
func newCovenScopeHandler(t *testing.T, cfg *rbactest.Config, pool *covenPool) *Handler {
	t.Helper()
	enf, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	deps := HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enf,
		AuditWriter:   &recordingAudit{},
		Logger:        logger,
		IncarnationDB: &fakePool{},
		ShellGate:     shellgate.New(shellgate.ModeEnforce, nil, nil),
	}
	if pool != nil {
		deps.SoulDB = pool
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func errandArgsJSON(sid, module string) string {
	return `{"sid":"` + sid + `","module":"` + module + `","input":{"cmd":"uptime"}}`
}

// toolCode runs the tool and returns the semantic error code. Every call here
// errors: allowed → internal-error (no dispatcher), refused → forbidden.
func toolCode(t *testing.T, h *Handler, tool, args string) string {
	t.Helper()
	resp := callTool(t, h, "archon-alice", tool, args)
	if resp.Error == nil {
		t.Fatal("expected an error response (no dispatcher is wired)")
	}
	return mustToolErrorData(t, resp.Error.Data).Code
}

// TestMCPRunCommand_CovenScopeNarrows — `soul.console on coven=web` opens a
// shell on a host IN coven web and on no other. Before NIM-650 the MCP gate
// built the `{host}` context alone, so the coven condition could not be
// satisfied by any host and the grant denied everything it named.
//
// Both directions in one test on purpose: narrowing that admitted every host
// would satisfy the admit half alone, and the old refuse-everything behaviour
// would satisfy the refuse half alone. Only the pair distinguishes a grant that
// is compared from one that is merely fetched, or ignored.
func TestMCPRunCommand_CovenScopeNarrows(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			h := newCovenScopeHandler(t, form.cfg("soul.console"), covenTestPool())

			if code := toolCode(t, h, "keeper.soul.run-command",
				runCommandArgsJSON(covenWebSID)); code != mcpCodeInternalError {
				t.Errorf("host IN coven web: code = %q, want internal-error — a `soul.console` "+
					"scoped to coven=web is still read as a denial over MCP", code)
			}
			if code := toolCode(t, h, "keeper.soul.run-command",
				runCommandArgsJSON(covenProdSID)); code != mcpCodeForbidden {
				t.Errorf("host in coven prod: code = %q, want forbidden — the scope admitted a "+
					"host outside it", code)
			}
		})
	}
}

// TestMCPRunCommand_MultiCovenHostIsAdmittedByAnyOfItsCovens — ADR-008: a host
// carries a LIST of labels, and an rbac.Permission holds one value per dimension,
// so the check is an OR over one context per label. A gate that collapsed the
// list to its first element would refuse edge-01 here (its first label is prod)
// while every single-coven case above stayed green.
func TestMCPRunCommand_MultiCovenHostIsAdmittedByAnyOfItsCovens(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			h := newCovenScopeHandler(t, form.cfg("soul.console"), covenTestPool())

			if code := toolCode(t, h, "keeper.soul.run-command",
				runCommandArgsJSON(covenBothSID)); code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error — a host in {prod, web} was refused by a "+
					"grant on coven=web, so only one of its labels reached the check", code)
			}
		})
	}
}

// TestMCPRunCommand_UnknownSIDCannotReachACovenGrant — a SID with no row yields
// the `{host}` context alone, and a `coven=` condition is not satisfied by a
// context that has no coven dimension. Without this a caller could name any
// string and be judged by a grant written for real hosts.
func TestMCPRunCommand_UnknownSIDCannotReachACovenGrant(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			h := newCovenScopeHandler(t, form.cfg("soul.console"), covenTestPool())

			if code := toolCode(t, h, "keeper.soul.run-command",
				runCommandArgsJSON("ghost.example.com")); code != mcpCodeForbidden {
				t.Errorf("code = %q, want forbidden — an unregistered SID passed a coven-scoped "+
					"grant, so the scope is satisfied by a host nobody has ever seen", code)
			}
		})
	}
}

// TestMCPErrandRun_CovenScopeNarrowsBothRights — the verb-shell path needs
// `errand.run` AND `soul.console`, and NIM-650 has to make the coven scope work
// on both. A fix that reached only the console half would leave the tool refusing
// in-coven hosts at the earlier check, with the console guard above still green.
func TestMCPErrandRun_CovenScopeNarrowsBothRights(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			h := newCovenScopeHandler(t, form.cfg("errand.run", "soul.console"), covenTestPool())

			if code := toolCode(t, h, "keeper.soul.errand.run",
				errandArgsJSON(covenWebSID, "core.cmd.shell")); code != mcpCodeInternalError {
				t.Errorf("host IN coven web: code = %q, want internal-error", code)
			}
			if code := toolCode(t, h, "keeper.soul.errand.run",
				errandArgsJSON(covenProdSID, "core.cmd.shell")); code != mcpCodeForbidden {
				t.Errorf("host in coven prod: code = %q, want forbidden", code)
			}
		})
	}
}

// TestMCPErrandRun_OrdinaryModuleNarrowsOnErrandRunAlone — the `errand.run` half
// on its own: a coven-scoped grant with no `soul.console` at all runs an ordinary
// module in its coven and nowhere else. This is the half a console-only fix would
// have left denying every call.
func TestMCPErrandRun_OrdinaryModuleNarrowsOnErrandRunAlone(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			h := newCovenScopeHandler(t, form.cfg("errand.run"), covenTestPool())

			if code := toolCode(t, h, "keeper.soul.errand.run",
				errandArgsJSON(covenWebSID, "core.http.probe")); code != mcpCodeInternalError {
				t.Errorf("host IN coven web: code = %q, want internal-error", code)
			}
			if code := toolCode(t, h, "keeper.soul.errand.run",
				errandArgsJSON(covenProdSID, "core.http.probe")); code != mcpCodeForbidden {
				t.Errorf("host in coven prod: code = %q, want forbidden", code)
			}
		})
	}
}

// TestMCPErrandRun_TheTwoRightsStayIndependentUnderAScope — ADR-0074(c) holds
// with a coven in play: an unscoped `errand.run` does not carry a coven-scoped
// `soul.console` past its own boundary. The ordinary module runs anywhere, the
// verb-shell one only inside the console grant's coven. A gate that ORed the two
// rights, or checked the console half against the wrong context set, would let
// core.cmd.shell through on db-01 here.
func TestMCPErrandRun_TheTwoRightsStayIndependentUnderAScope(t *testing.T) {
	h := newCovenScopeHandler(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "mixed", Operators: []string{"archon-alice"},
			Permissions: []string{"errand.run", "soul.console on coven=web"}},
	}}, covenTestPool())

	if code := toolCode(t, h, "keeper.soul.errand.run",
		errandArgsJSON(covenProdSID, "core.http.probe")); code != mcpCodeInternalError {
		t.Errorf("ordinary module on db-01: code = %q, want internal-error — an unscoped "+
			"errand.run was narrowed by an unrelated console scope", code)
	}
	if code := toolCode(t, h, "keeper.soul.errand.run",
		errandArgsJSON(covenProdSID, "core.cmd.shell")); code != mcpCodeForbidden {
		t.Errorf("verb-shell module on db-01: code = %q, want forbidden — `soul.console on "+
			"coven=web` opened a shell on a host in coven prod", code)
	}
	if code := toolCode(t, h, "keeper.soul.errand.run",
		errandArgsJSON(covenWebSID, "core.cmd.shell")); code != mcpCodeInternalError {
		t.Errorf("verb-shell module on web-01: code = %q, want internal-error", code)
	}
}

// TestMCPErrandRun_CovensAreReadOncePerCall — the contexts are resolved once and
// reused by both rights. Two reads would not merely cost a round-trip: they could
// return different label sets for the same host mid-call, and the two halves of
// one authorization decision would then be answering about different hosts.
func TestMCPErrandRun_CovensAreReadOncePerCall(t *testing.T) {
	pool := covenTestPool()
	h := newCovenScopeHandler(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "web-ops", Operators: []string{"archon-alice"},
			Permissions: []string{"errand.run on coven=web", "soul.console on coven=web"}},
	}}, pool)

	if code := toolCode(t, h, "keeper.soul.errand.run",
		errandArgsJSON(covenWebSID, "core.cmd.shell")); code != mcpCodeInternalError {
		t.Fatalf("code = %q, want internal-error", code)
	}
	if pool.reads != 1 {
		t.Errorf("coven reads = %d, want 1 — both rights must be judged against the same "+
			"resolved context set", pool.reads)
	}
}

// TestMCPCovenGate_NilSoulDBKeepsHostScopedGrantsWorking — a Keeper without the
// soul pool wired falls back to the `{host}` context alone. Host-scoped and bare
// grants keep working; a coven-scoped one fails closed rather than opening up.
func TestMCPCovenGate_NilSoulDBKeepsHostScopedGrantsWorking(t *testing.T) {
	h := newCovenScopeHandler(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "one-host", Operators: []string{"archon-alice"},
			Permissions: []string{"soul.console on host=" + covenWebSID}},
	}}, nil)

	if code := toolCode(t, h, "keeper.soul.run-command",
		runCommandArgsJSON(covenWebSID)); code != mcpCodeInternalError {
		t.Errorf("own host: code = %q, want internal-error — the coven work broke a plain "+
			"host-scoped grant on a Keeper with no soul pool", code)
	}
	if code := toolCode(t, h, "keeper.soul.run-command",
		runCommandArgsJSON(covenProdSID)); code != mcpCodeForbidden {
		t.Errorf("another host: code = %q, want forbidden", code)
	}

	scoped := newCovenScopeHandler(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "web-ops", Operators: []string{"archon-alice"},
			Permissions: []string{"soul.console on coven=web"}},
	}}, nil)
	if code := toolCode(t, scoped, "keeper.soul.run-command",
		runCommandArgsJSON(covenWebSID)); code != mcpCodeForbidden {
		t.Errorf("coven grant without a soul pool: code = %q, want forbidden — an unreadable "+
			"coven must fail closed, not admit", code)
	}
}
