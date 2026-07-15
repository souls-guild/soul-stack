package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func newRBAC(t *testing.T, cfg *rbactest.Config) *rbac.Enforcer {
	t.Helper()
	e, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	return e
}

// withClaims puts claims into the context directly — we do not go through the RequireJWT
// middleware, testing only the RBAC layer.
func withClaims(r *http.Request, subject string) *http.Request {
	c := &keeperjwt.Claims{Subject: subject}
	return r.WithContext(context.WithValue(r.Context(), claimsCtxKey{}, c))
}

func TestRequirePermission_Allow(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})
	h := RequirePermission(e, "operator", "create", NoSelector)(next)

	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-alice")
	h.ServeHTTP(rec, req)

	if !called {
		t.Errorf("next handler should be called on allow")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("Code = %d, want 201", rec.Code)
	}
}

func TestRequirePermission_Deny(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "ro", Operators: []string{"archon-bob"}, Permissions: []string{"soul.list"}},
		},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("next should NOT be called on deny")
	})
	h := RequirePermission(e, "operator", "create", NoSelector)(next)

	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/operators", nil), "archon-bob")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestRequirePermission_NoClaims500(t *testing.T) {
	// If RequireJWT did not run — this is a chain misconfiguration: 500, not 401.
	e := newRBAC(t, nil)
	h := RequirePermission(e, "operator", "create", NoSelector)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("next should NOT be called when claims missing")
		}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/operators", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", rec.Code)
	}
}

func TestRequirePermission_SelectorPassesContext(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{
				Name:        "db-op",
				Operators:   []string{"archon-db"},
				Permissions: []string{"incarnation.create on service=redis"},
			},
		},
	})
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	extractor := func(_ *http.Request) map[string]string {
		return map[string]string{"service": "redis"}
	}
	h := RequirePermission(e, "incarnation", "create", extractor)(next)

	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations", nil), "archon-db")
	h.ServeHTTP(rec, req)

	if !called {
		t.Errorf("next should be called when context matches selector")
	}

	// And the reverse — the context does not match the selector.
	called = false
	extractor2 := func(_ *http.Request) map[string]string {
		return map[string]string{"service": "postgres"}
	}
	h2 := RequirePermission(e, "incarnation", "create", extractor2)(next)
	rec2 := httptest.NewRecorder()
	req2 := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations", nil), "archon-db")
	h2.ServeHTTP(rec2, req2)
	if called {
		t.Errorf("next should NOT be called on selector mismatch")
	}
	if rec2.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want 403", rec2.Code)
	}
}

// --- RequirePermissionMulti (ADR-008 amendment a, per-Coven incarnation scope) ---

// incCovenContexts replicates the expansion of an incarnation's coven scope into a set of
// per-candidate contexts (covens ∪ {name}), as the handler extractor does. Duplicated here
// deliberately — the middleware test checks EXACTLY the OR-Check decision over a set of
// contexts, without importing handlers (a circular dependency); context construction is
// tested in handlers/incarnation_test.go.
func incCovenContexts(name, service string, covens []string) []map[string]string {
	seen := map[string]struct{}{}
	cand := []string{}
	add := func(c string) {
		if c == "" {
			return
		}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		cand = append(cand, c)
	}
	for _, c := range covens {
		add(c)
	}
	add(name)
	out := make([]map[string]string, 0, len(cand))
	for _, c := range cand {
		out = append(out, map[string]string{"incarnation": name, "service": service, "coven": c})
	}
	return out
}

// runMulti runs RequirePermissionMulti with a fixed set of contexts and returns
// (allowed, statusCode).
func runMulti(t *testing.T, e *rbac.Enforcer, subject, resource, action string, contexts []map[string]string) (bool, int) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	ext := func(_ *http.Request) []map[string]string { return contexts }
	h := RequirePermissionMulti(e, resource, action, ext)(next)
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/incarnations/x", nil), subject)
	h.ServeHTTP(rec, req)
	return called, rec.Code
}

func TestRequirePermissionMulti_CovenScope_Match(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "prod-runner", Operators: []string{"archon-prod"}, Permissions: []string{"incarnation.run on coven=prod"}},
	}})
	// incarnation declared covens=[prod] → context coven=prod present → allow.
	allowed, code := runMulti(t, e, "archon-prod", "incarnation", "run",
		incCovenContexts("redis-main", "redis", []string{"prod"}))
	if !allowed || code != http.StatusOK {
		t.Errorf("coven=prod role должна матчить inc covens=[prod]; allowed=%v code=%d", allowed, code)
	}
}

func TestRequirePermissionMulti_CovenScope_NoMatch(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "prod-runner", Operators: []string{"archon-prod"}, Permissions: []string{"incarnation.run on coven=prod"}},
	}})
	// incarnation declared covens=[dev] → no coven=prod → deny.
	allowed, code := runMulti(t, e, "archon-prod", "incarnation", "run",
		incCovenContexts("redis-dev", "redis", []string{"dev"}))
	if allowed || code != http.StatusForbidden {
		t.Errorf("coven=prod role НЕ должна матчить inc covens=[dev]; allowed=%v code=%d", allowed, code)
	}
}

func TestRequirePermissionMulti_ServiceScope_Match(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "redis-admin", Operators: []string{"archon-r"}, Permissions: []string{"incarnation.* on service=redis"}},
	}})
	allowed, code := runMulti(t, e, "archon-r", "incarnation", "run",
		incCovenContexts("redis-x", "redis", []string{"any"}))
	if !allowed || code != http.StatusOK {
		t.Errorf("service=redis role должна матчить inc сервиса redis; allowed=%v code=%d", allowed, code)
	}
	// service=postgres incarnation — does not match.
	allowed2, code2 := runMulti(t, e, "archon-r", "incarnation", "run",
		incCovenContexts("pg-x", "postgres", []string{"any"}))
	if allowed2 || code2 != http.StatusForbidden {
		t.Errorf("service=redis role НЕ должна матчить inc сервиса postgres; allowed=%v code=%d", allowed2, code2)
	}
}

func TestRequirePermissionMulti_NameAsCoven_Match(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "named", Operators: []string{"archon-n"}, Permissions: []string{"incarnation.* on coven=redis-prod"}},
	}})
	// covens is empty — but the incarnation name = redis-prod is the root Coven label.
	allowed, code := runMulti(t, e, "archon-n", "incarnation", "upgrade",
		incCovenContexts("redis-prod", "redis", nil))
	if !allowed || code != http.StatusOK {
		t.Errorf("coven=<name> должна матчить incarnation с этим именем; allowed=%v code=%d", allowed, code)
	}
}

func TestRequirePermissionMulti_Negative_DevCannotTouchProd(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "dev-op", Operators: []string{"archon-dev"}, Permissions: []string{
			"incarnation.run on coven=dev",
			"incarnation.destroy on coven=dev",
		}},
	}})
	for _, action := range []string{"run", "destroy"} {
		allowed, code := runMulti(t, e, "archon-dev", "incarnation", action,
			incCovenContexts("redis-prod", "redis", []string{"prod"}))
		if allowed || code != http.StatusForbidden {
			t.Errorf("coven=dev оператор НЕ должен %s prod-incarnation; allowed=%v code=%d", action, allowed, code)
		}
	}
}

func TestRequirePermissionMulti_Negative_DevCannotCreateProd(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "dev-creator", Operators: []string{"archon-dev"}, Permissions: []string{"incarnation.create on coven=dev"}},
	}})
	// create incarnation name=redis-prod covens=[prod] → candidates {prod, redis-prod},
	// coven=dev is not among them → deny.
	allowed, code := runMulti(t, e, "archon-dev", "incarnation", "create",
		incCovenContexts("redis-prod", "redis", []string{"prod"}))
	if allowed || code != http.StatusForbidden {
		t.Errorf("coven=dev оператор НЕ должен создать incarnation с covens=[prod]; allowed=%v code=%d", allowed, code)
	}
	// But within its own scope (covens=[dev]) — it can.
	allowed2, code2 := runMulti(t, e, "archon-dev", "incarnation", "create",
		incCovenContexts("redis-dev", "redis", []string{"dev"}))
	if !allowed2 || code2 != http.StatusOK {
		t.Errorf("coven=dev оператор должен создать incarnation covens=[dev]; allowed=%v code=%d", allowed2, code2)
	}
}

func TestRequirePermissionMulti_BarePermission_NoRegression(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "runner", Operators: []string{"archon-bare"}, Permissions: []string{"incarnation.run"}},
	}})
	// bare incarnation.run (without on) — ignores context, passes on any inc.
	allowed, code := runMulti(t, e, "archon-bare", "incarnation", "run",
		incCovenContexts("redis-prod", "redis", []string{"prod"}))
	if !allowed || code != http.StatusOK {
		t.Errorf("bare incarnation.run должна проходить (регресс); allowed=%v code=%d", allowed, code)
	}
}

func TestRequirePermissionMulti_Wildcard_NoRegression(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "admin", Operators: []string{"archon-root"}, Permissions: []string{"*"}},
	}})
	allowed, code := runMulti(t, e, "archon-root", "incarnation", "destroy",
		incCovenContexts("anything", "redis", []string{"prod"}))
	if !allowed || code != http.StatusOK {
		t.Errorf("* должна проходить любую incarnation-операцию (регресс); allowed=%v code=%d", allowed, code)
	}
}

func TestRequirePermissionMulti_EmptyContexts_BareOnly(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "scoped", Operators: []string{"archon-s"}, Permissions: []string{"incarnation.run on coven=prod"}},
		{Name: "bare", Operators: []string{"archon-b"}, Permissions: []string{"incarnation.run"}},
	}})
	// An empty set (the extractor landed no data: 404 / broken body) →
	// fail-closed for scoped, pass for bare.
	allowedScoped, codeScoped := runMulti(t, e, "archon-s", "incarnation", "run", nil)
	if allowedScoped || codeScoped != http.StatusForbidden {
		t.Errorf("scoped роль при пустом наборе → deny; allowed=%v code=%d", allowedScoped, codeScoped)
	}
	allowedBare, codeBare := runMulti(t, e, "archon-b", "incarnation", "run", nil)
	if !allowedBare || codeBare != http.StatusOK {
		t.Errorf("bare роль при пустом наборе → allow; allowed=%v code=%d", allowedBare, codeBare)
	}
}

func TestRequirePermissionMulti_NoClaims_500(t *testing.T) {
	e := newRBAC(t, nil)
	h := RequirePermissionMulti(e, "incarnation", "run", func(_ *http.Request) []map[string]string { return nil })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("next should NOT be called when claims missing")
		}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/x", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", rec.Code)
	}
}

// --- RequireAnyPermission (any-of <resource>.<action>, backcompat grant) ---
//
// Gates cadence.enable/disable with an OR over rights: enable requires
// `cadence.enable` OR `cadence.update`, disable — `cadence.disable` OR
// `cadence.update`. cadence.update stays valid for the toggle (roles with the old
// right do not lose access).

// runAny runs RequireAnyPermission(resource, actions...) with NoSelector and returns
// (allowed, statusCode).
func runAny(t *testing.T, e *rbac.Enforcer, subject, resource string, actions ...string) (bool, int) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := RequireAnyPermission(e, resource, actions, NoSelector)(next)
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodPost, "/v1/cadences/x/enable", nil), subject)
	h.ServeHTTP(rec, req)
	return called, rec.Code
}

func TestRequireAnyPermission_EnableGrantsEnableNotDisable(t *testing.T) {
	// A role with cadence.enable → can /enable, can NOT /disable.
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "enabler", Operators: []string{"archon-e"}, Permissions: []string{"cadence.enable"}},
	}})
	if allowed, code := runAny(t, e, "archon-e", "cadence", "enable", "update"); !allowed || code != http.StatusOK {
		t.Errorf("cadence.enable должна допускать enable; allowed=%v code=%d", allowed, code)
	}
	if allowed, code := runAny(t, e, "archon-e", "cadence", "disable", "update"); allowed || code != http.StatusForbidden {
		t.Errorf("cadence.enable НЕ должна допускать disable; allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAnyPermission_DisableGrantsDisableNotEnable(t *testing.T) {
	// A role with cadence.disable → can /disable, can NOT /enable.
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "disabler", Operators: []string{"archon-d"}, Permissions: []string{"cadence.disable"}},
	}})
	if allowed, code := runAny(t, e, "archon-d", "cadence", "disable", "update"); !allowed || code != http.StatusOK {
		t.Errorf("cadence.disable должна допускать disable; allowed=%v code=%d", allowed, code)
	}
	if allowed, code := runAny(t, e, "archon-d", "cadence", "enable", "update"); allowed || code != http.StatusForbidden {
		t.Errorf("cadence.disable НЕ должна допускать enable; allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAnyPermission_UpdateBackcompat(t *testing.T) {
	// A role with the old cadence.update → can BOTH /enable AND /disable (backcompat).
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "updater", Operators: []string{"archon-u"}, Permissions: []string{"cadence.update"}},
	}})
	if allowed, code := runAny(t, e, "archon-u", "cadence", "enable", "update"); !allowed || code != http.StatusOK {
		t.Errorf("cadence.update должна допускать enable (backcompat); allowed=%v code=%d", allowed, code)
	}
	if allowed, code := runAny(t, e, "archon-u", "cadence", "disable", "update"); !allowed || code != http.StatusOK {
		t.Errorf("cadence.update должна допускать disable (backcompat); allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAnyPermission_NoneDenied(t *testing.T) {
	// A role without cadence.enable/disable/update → 403 on both.
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "reader", Operators: []string{"archon-r"}, Permissions: []string{"cadence.list"}},
	}})
	if allowed, code := runAny(t, e, "archon-r", "cadence", "enable", "update"); allowed || code != http.StatusForbidden {
		t.Errorf("cadence.list НЕ должна допускать enable; allowed=%v code=%d", allowed, code)
	}
	if allowed, code := runAny(t, e, "archon-r", "cadence", "disable", "update"); allowed || code != http.StatusForbidden {
		t.Errorf("cadence.list НЕ должна допускать disable; allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAnyPermission_NoClaims_500(t *testing.T) {
	e := newRBAC(t, nil)
	h := RequireAnyPermission(e, "cadence", []string{"enable", "update"}, NoSelector)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("next should NOT be called when claims missing")
		}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/x/enable", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", rec.Code)
	}
}

// --- RequireAction (existence gate for read endpoints, ADR-047 §d amendment) ---
//
// runAction runs RequireAction(resource, action) and returns (allowed, statusCode).
// Subject="" → claims are not put into the context (a missing-claims check).
func runAction(t *testing.T, e *rbac.Enforcer, subject, resource, action string) (bool, int) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := RequireAction(e, resource, action)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/souls", nil)
	if subject != "" {
		req = withClaims(req, subject)
	}
	h.ServeHTTP(rec, req)
	return called, rec.Code
}

// A holder of the action (of any scope) → the gate lets it into the handler. A scoped
// operator, whom RequirePermission/Check would cut off on an empty context, passes.
func TestRequireAction_ScopedHolder_PassesToHandler(t *testing.T) {
	for _, perm := range []string{
		"soul.list",
		"soul.list on coven=prod",
		`soul.list on regex='^web-'`,
		`soul.list on soulprint='soulprint.self.os.family == "debian"'`,
		`soul.list on state='state.redis_version == "8.0"'`,
		"*",
	} {
		e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
			{Name: "ro", Operators: []string{"archon-ro"}, Permissions: []string{perm}},
		}})
		allowed, code := runAction(t, e, "archon-ro", "soul", "list")
		if !allowed || code != http.StatusOK {
			t.Errorf("perm=%q: gate должен пускать держателя; allowed=%v code=%d", perm, allowed, code)
		}
	}
}

func TestRequireAction_NonHolder_403(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "other", Operators: []string{"archon-x"}, Permissions: []string{"operator.create"}},
	}})
	allowed, code := runAction(t, e, "archon-x", "soul", "list")
	if allowed || code != http.StatusForbidden {
		t.Errorf("оператор без soul.list → 403; allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAction_NoClaims_500(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "ro", Operators: []string{"archon-ro"}, Permissions: []string{"soul.list"}},
	}})
	allowed, code := runAction(t, e, "", "soul", "list")
	if allowed || code != http.StatusInternalServerError {
		t.Errorf("missing claims → 500 (конфиг-ошибка chain-а); allowed=%v code=%d", allowed, code)
	}
}

func TestRequireAction_ForbiddenContentType(t *testing.T) {
	e := newRBAC(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "other", Operators: []string{"archon-x"}, Permissions: []string{"operator.create"}},
	}})
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("next should NOT be called on deny")
	})
	h := RequireAction(e, "soul", "list")(next)
	rec := httptest.NewRecorder()
	req := withClaims(httptest.NewRequest(http.MethodGet, "/v1/souls", nil), "archon-x")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q (problem+json)", got, problem.ContentType)
	}
}
