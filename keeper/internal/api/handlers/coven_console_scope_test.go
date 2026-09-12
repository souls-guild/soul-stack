package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
)

// NIM-650: a `coven=`-scoped grant must NARROW the per-host console and Errand
// gates, not refuse them outright. The bug was a context set built as `{host}`
// alone: a condition on a dimension the context does not carry never matches, so
// `soul.console on coven=web` denied every host, including the ones in coven
// web. NIM-588 fixed the three Soul mutations; these are the surfaces it left.
//
// Every case below runs a REAL [rbac.Enforcer], and each is run TWICE — once
// with the scope written on the permission (`soul.console on coven=web`) and
// once with a bare permission under a role whose `default_scope` is `coven=web`.
// The two are different rows in different tables and meet only inside
// effectiveScope; a fixture carrying just one of them proves half the gate.

// covenRow is one souls row as the RBAC gates read it.
type covenRow struct {
	sid    string
	covens []string
}

// covenReader is the souls read surface [soul.HostContextsBySID] and
// [soul.HostContextsBySIDs] resolve through. Hosts absent from `rows` have no
// row at all — a forgotten or forged SID.
type covenReader struct {
	rows     []covenRow
	queryErr error
}

func (c *covenReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("covenReader: Exec not expected")
}

func (c *covenReader) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	sid, _ := args[0].(string)
	for _, r := range c.rows {
		if r.sid == sid {
			return covenScanRow{covens: r.covens}
		}
	}
	return covenScanRow{err: pgx.ErrNoRows}
}

func (c *covenReader) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	want := map[string]bool{}
	if sids, ok := args[0].([]string); ok {
		for _, s := range sids {
			want[s] = true
		}
	}
	out := &covenScanRows{}
	for _, r := range c.rows {
		if want[r.sid] {
			out.rows = append(out.rows, r)
		}
	}
	return out, nil
}

type covenScanRow struct {
	covens []string
	err    error
}

func (r covenScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*[]string)) = r.covens
	return nil
}

type covenScanRows struct {
	rows []covenRow
	idx  int
}

func (r *covenScanRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *covenScanRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*string)) = row.sid
	*(dest[1].(*[]string)) = row.covens
	return nil
}
func (r *covenScanRows) Err() error                                   { return nil }
func (r *covenScanRows) Close()                                       {}
func (r *covenScanRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *covenScanRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *covenScanRows) Values() ([]any, error)                       { return nil, nil }
func (r *covenScanRows) RawValues() [][]byte                          { return nil }
func (r *covenScanRows) Conn() *pgx.Conn                              { return nil }

// covenForm is one of the two ways an operator ends up scoped to a coven. They
// are stored differently and are unified only by rbac.effectiveScope, so a guard
// that pins one of them says nothing about the other.
type covenForm struct {
	name string
	// enforcer grants `perms` narrowed to `coven=web`, to archon-alice.
	enforcer func(t *testing.T, perms ...string) *rbac.Enforcer
}

func covenForms() []covenForm {
	return []covenForm{
		{
			name: "scope on the permission",
			enforcer: func(t *testing.T, perms ...string) *rbac.Enforcer {
				t.Helper()
				scoped := make([]string, len(perms))
				for i, p := range perms {
					scoped[i] = p + " on coven=web"
				}
				return rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"}, Permissions: scoped},
				}})
			},
		},
		{
			name: "default_scope on the role",
			enforcer: func(t *testing.T, perms ...string) *rbac.Enforcer {
				t.Helper()
				return rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
					{Name: "web-ops", Operators: []string{"archon-alice"},
						Permissions: perms, DefaultScope: "coven=web"},
				}})
			},
		},
	}
}

// --- the exec route's `errand.run` gate (router.go: RequirePermissionMulti) ---

// execRouteVerdict runs the real middleware over the real selector for one SID,
// exactly as router.go mounts it, and reports the status.
func execRouteVerdict(t *testing.T, enf *rbac.Enforcer, reader SoulHostContextReader, sid string) int {
	t.Helper()
	mw := middleware.RequirePermissionMulti(enf, "errand", "run", SoulSIDScopeSelector(reader))
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/v1/souls/"+sid+"/exec", http.NoBody)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("sid", sid)
	req = req.WithContext(context.WithValue(
		middleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"}),
		chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestErrandRun_CovenScopeNarrowsTheExecRoute(t *testing.T) {
	t.Parallel()
	reader := &covenReader{rows: []covenRow{
		{sid: "host-web", covens: []string{"web"}},
		{sid: "host-db", covens: []string{"db"}},
	}}
	for _, f := range covenForms() {
		t.Run(f.name, func(t *testing.T) {
			enf := f.enforcer(t, "errand.run")
			if got := execRouteVerdict(t, enf, reader, "host-web"); got != http.StatusOK {
				t.Errorf("host-web (IS in coven web) = %d, want 200 — the scope must NARROW, not refuse", got)
			}
			if got := execRouteVerdict(t, enf, reader, "host-db"); got != http.StatusForbidden {
				t.Errorf("host-db (NOT in coven web) = %d, want 403", got)
			}
		})
	}
}

func TestErrandRun_MultiCovenHostIsAdmittedByAnyOfItsCovens(t *testing.T) {
	t.Parallel()
	// ADR-008: a host carries a LIST. A grant on one of its labels admits it —
	// otherwise the label an operator was given would only work on hosts that
	// carry nothing else.
	reader := &covenReader{rows: []covenRow{{sid: "host-multi", covens: []string{"db", "eu", "web"}}}}
	for _, f := range covenForms() {
		t.Run(f.name, func(t *testing.T) {
			if got := execRouteVerdict(t, f.enforcer(t, "errand.run"), reader, "host-multi"); got != http.StatusOK {
				t.Errorf("multi-coven host = %d, want 200", got)
			}
		})
	}
}

func TestErrandRun_UnknownSIDCannotReachACovenGrant(t *testing.T) {
	t.Parallel()
	// A SID that has no souls row — forged, or forgotten between the token
	// being minted and the call. It must fall back to `{host}` alone, where a
	// `coven=` condition finds no dimension and denies.
	reader := &covenReader{rows: []covenRow{{sid: "host-web", covens: []string{"web"}}}}
	for _, f := range covenForms() {
		t.Run(f.name, func(t *testing.T) {
			if got := execRouteVerdict(t, f.enforcer(t, "errand.run"), reader, "host-ghost"); got != http.StatusForbidden {
				t.Errorf("unknown SID = %d, want 403 — an unreadable coven must fail closed", got)
			}
		})
	}
}

func TestErrandRun_HostScopedGrantIsUnaffected(t *testing.T) {
	t.Parallel()
	// The pre-NIM-588 shape still has to work: `host=` names the dimension that
	// is present in every context, coven or not.
	enf := rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "one-host", Operators: []string{"archon-alice"},
			Permissions: []string{"errand.run on host=host-web"}},
	}})
	reader := &covenReader{rows: []covenRow{
		{sid: "host-web", covens: []string{"web"}},
		{sid: "host-db", covens: []string{"db"}},
	}}
	if got := execRouteVerdict(t, enf, reader, "host-web"); got != http.StatusOK {
		t.Errorf("host-web = %d, want 200", got)
	}
	if got := execRouteVerdict(t, enf, reader, "host-db"); got != http.StatusForbidden {
		t.Errorf("host-db = %d, want 403", got)
	}
}

// --- the exec route's in-handler `soul.console` half (verb-shell modules) ---

func TestErrandExec_CovenScopeNarrowsTheConsoleHalf(t *testing.T) {
	t.Parallel()
	reader := &covenReader{rows: []covenRow{
		{sid: "host-web", covens: []string{"web"}},
		{sid: "host-db", covens: []string{"db"}},
	}}
	for _, f := range covenForms() {
		t.Run(f.name, func(t *testing.T) {
			h := NewErrandHandler(nil, nil, f.enforcer(t, "errand.run", "soul.console"),
				shellgate.New(shellgate.ModeEnforce, nil, nil), reader, nil /*scoper*/, nil)

			if err := h.authorizeShell(context.Background(), "archon-alice", "host-web", "core.cmd.shell"); err != nil {
				t.Errorf("host-web: %v, want nil — `errand.run` already admitted this host one layer "+
					"up, so refusing it here makes the route contradict itself", err)
			}
			if err := h.authorizeShell(context.Background(), "archon-alice", "host-db", "core.cmd.shell"); !isConsoleForbidden(err) {
				t.Errorf("host-db: err = %v, want the console 403", err)
			}
		})
	}
}

func TestErrandExec_ConsoleHalfIsNotConsultedForOrdinaryModules(t *testing.T) {
	t.Parallel()
	// The coven read lives INSIDE the gate's probe closure on purpose: an
	// ordinary Errand must not pay a round-trip for a check that never runs.
	reader := &covenReader{queryErr: errors.New("the reader must not be touched")}
	h := NewErrandHandler(nil, nil, rbactest.MustEnforcer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "web-ops", Operators: []string{"archon-alice"},
			Permissions: []string{"errand.run on coven=web"}},
	}}), shellgate.New(shellgate.ModeEnforce, nil, nil), reader, nil /*scoper*/, nil)

	if err := h.authorizeShell(context.Background(), "archon-alice", "host-db", "core.pkg.present"); err != nil {
		t.Fatalf("a read-safe module was refused by the console gate: %v", err)
	}
}

// --- the Voyage batch (kind=command over a resolved target) ---

func TestVoyageCommand_CovenScopeNarrowsTheBatch(t *testing.T) {
	t.Parallel()
	reader := &covenReader{rows: []covenRow{
		{sid: "host-web-1", covens: []string{"web"}},
		{sid: "host-web-2", covens: []string{"eu", "web"}},
		{sid: "host-db", covens: []string{"db"}},
	}}
	for _, f := range covenForms() {
		t.Run(f.name, func(t *testing.T) {
			h := &VoyageHandler{
				enforcer:   f.enforcer(t, "errand.run", "soul.console"),
				soulReader: reader,
				gate:       shellgate.New(shellgate.ModeEnforce, nil, nil),
			}
			err := h.authorizeShellErr(context.Background(), "archon-alice", "core.cmd.shell",
				[]string{"host-web-1", "host-web-2"})
			if err != nil {
				t.Errorf("a batch entirely inside coven web was refused: %v", err)
			}
			// All-or-nothing still holds: one host outside the coven sinks the
			// batch rather than silently trimming it.
			err = h.authorizeShellErr(context.Background(), "archon-alice", "core.cmd.shell",
				[]string{"host-web-1", "host-db"})
			if !isConsoleForbidden(err) {
				t.Errorf("mixed batch: err = %v, want the console 403", err)
			}
			if d, ok := AsProblemDetails(err); ok && !strings.Contains(d.Detail, "host-db") {
				t.Errorf("detail = %q, want it to name the uncovered host", d.Detail)
			}
		})
	}
}
