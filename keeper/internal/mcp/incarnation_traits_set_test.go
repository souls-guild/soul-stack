package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// MCP tool keeper.incarnation.traits-set (ADR-060 amend R1) — REST parity
// with PUT /v1/incarnations/{name}/traits. Relocated per-soul → per-incarnation.

func traitsSetRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "traits-setter", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.traits-set"}},
		},
	}
}

// incForTraits is an incFn returning a ready incarnation with the given
// traits/covens (for the full-row FOR UPDATE select in UpdateTraits + scope
// resolution).
func incForTraits(traits map[string]any) func(string) (*incarnation.Incarnation, error) {
	return func(name string) (*incarnation.Incarnation, error) {
		now := time.Now().UTC()
		return &incarnation.Incarnation{
			Name: name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: incarnation.StatusReady,
			State: map[string]any{}, Traits: traits, CreatedAt: now, UpdatedAt: now,
		}, nil
	}
}

func decodeTraitsSetOutput(t *testing.T, resp jsonRPCResponse) incarnationTraitsSetOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationTraitsSetOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func TestIncarnationTraitsSet_InManifest(t *testing.T) {
	e, ok := toolByName("keeper.incarnation.traits-set")
	if !ok {
		t.Fatal("keeper.incarnation.traits-set missing from catalogManifest")
	}
	if e.status != toolStatusImplemented {
		t.Errorf("status = %d, want Implemented", e.status)
	}
	var schema map[string]any
	if err := json.Unmarshal(e.decl.InputSchema, &schema); err != nil {
		t.Fatalf("inputSchema not valid JSON: %v", err)
	}
	if e.decl.OutputSchema == nil {
		t.Error("outputSchema missing")
	}
}

func TestIncarnationTraitsSet_Success(t *testing.T) {
	pool := &fakePool{incFn: incForTraits(map[string]any{"team": "dba"})}
	h, rec := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"env":"prod","az":"a"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsSetOutput(t, resp)
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if len(out.Keys) != 2 || out.Keys[0] != "az" || out.Keys[1] != "env" {
		t.Errorf("keys = %v, want [az env] (sorted)", out.Keys)
	}

	// audit: EventIncarnationTraitsChanged, source=mcp, payload {name, old_keys, new_keys}.
	if len(rec.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.EventType != audit.EventIncarnationTraitsChanged {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	oldKeys, _ := ev.Payload["old_keys"].([]string)
	if len(oldKeys) != 1 || oldKeys[0] != "team" {
		t.Errorf("audit old_keys = %v, want [team]", ev.Payload["old_keys"])
	}
}

func TestIncarnationTraitsSet_InvalidValue(t *testing.T) {
	// A nested trait value is rejected by the domain BEFORE mutation.
	pool := &fakePool{
		incFn:    incForTraits(nil),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called on invalid trait value"},
	}
	h, rec := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"bad":{"nested":1}}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("invalid traits must not write audit")
	}
}

func TestIncarnationTraitsSet_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerFull(t, pool, traitsSetRBAC(), nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"ghost","traits":{"team":"dba"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestIncarnationTraitsSet_RBACForbidden(t *testing.T) {
	// Operator without incarnation.traits-set → deny BEFORE mutation (BeginTx forbidden).
	pool := &fakePool{
		incFn:    incForTraits(nil),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called when RBAC denies"},
	}
	h, rec := newTestHandlerFull(t, pool, nil, nil, nil, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"team":"dba"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Errorf("denied traits-set must not write audit")
	}
}

// --- gate (b): the label being stamped must be one the operator holds (NIM-587) ---
//
// A trait pair is a GRANT, not a description: `trait.<key>` is a live read-side scope
// dimension for incarnations (incScopeColumns.Traits), so stamping `env=prod` hands
// every `trait.env="prod"` role sight of this incarnation. Gate (a) — the forbidden
// case above — asks whether the operator may write to THIS incarnation, which is a
// different question; these are the unit-layer known-bad for gate (b) on this tool.
// They pin the VERDICT and the absence of a write; the SPELLING of a value belongs to
// TestIntegration_TraitWriteGate_AllSurfacesAgree, against a real jsonb.

const traitsSetGateCoven = "dba"

// traitsSetScopedRBAC — a role scoped the way a restricted operator really is: gate
// (a) admits the incarnation through `coven=dba`, and gate (b) is granted by SEPARATE
// permissions constraining trait ALONE.
//
// Separate is not a style choice. rbac.traitsFromPurview counts a pair only from a
// disjunct that constrains trait by itself, so `traits-set on coven=dba and
// trait.env="prod"` would contribute NOTHING to the trait-scope and every case here
// would refuse for the wrong reason — green, and proving the opposite of what it says.
func traitsSetScopedRBAC(traitPerms ...string) *rbactest.Config {
	perms := append([]string{"incarnation.traits-set on coven=" + traitsSetGateCoven}, traitPerms...)
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "traits-setter-scoped", Operators: []string{"archon-alice"}, Permissions: perms},
		},
	}
}

// incForTraitsInCoven — [incForTraits] with the coven gate (a) is scoped on, so a
// scoped operator gets PAST gate (a) and gate (b) is what the case actually exercises.
// Without the coven the tool answers forbidden and the gate below never runs.
func incForTraitsInCoven(traits map[string]any) func(string) (*incarnation.Incarnation, error) {
	base := incForTraits(traits)
	return func(name string) (*incarnation.Incarnation, error) {
		inc, err := base(name)
		if err != nil {
			return nil, err
		}
		inc.Covens = []string{traitsSetGateCoven}
		return inc, nil
	}
}

// TestIncarnationTraitsSet_PairOutsideTraitScope — the operator holds `env=prod` and
// tries to stamp `env=staging`: refused, the refusal NAMES the pair, and the write
// never starts (beginErr would surface as internal-error if it did).
func TestIncarnationTraitsSet_PairOutsideTraitScope(t *testing.T) {
	pool := &fakePool{
		incFn:    incForTraitsInCoven(nil),
		beginErr: errFakeUnexpected{sql: "BeginTx must not be called on an out-of-scope pair"},
	}
	h, rec := newTestHandlerFull(t, pool,
		traitsSetScopedRBAC(`incarnation.traits-set on trait.env="prod"`), nil, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"env":"staging"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if resp.Error.Message != "trait env=staging is outside operator trait-scope" {
		t.Errorf("message = %q, want the refused pair named", resp.Error.Message)
	}
	if len(rec.events) != 0 {
		t.Error("a refused traits-set must not write audit")
	}
	if pool.canonicalizeCalls != 1 {
		t.Errorf("canonicalizeCalls = %d, want 1 (the gate reads the payload as Postgres spells it)",
			pool.canonicalizeCalls)
	}
}

// TestIncarnationTraitsSet_PairInsideTraitScope — the positive control: the SAME
// restricted operator stamping a pair it does hold is admitted. Without this a gate
// that refuses everything would pass the case above.
func TestIncarnationTraitsSet_PairInsideTraitScope(t *testing.T) {
	pool := &fakePool{incFn: incForTraitsInCoven(nil)}
	h, rec := newTestHandlerFull(t, pool,
		traitsSetScopedRBAC(`incarnation.traits-set on trait.env="prod"`), nil, nil, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"env":"prod"}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeTraitsSetOutput(t, resp)
	if len(out.Keys) != 1 || out.Keys[0] != "env" {
		t.Errorf("keys = %v, want [env]", out.Keys)
	}
	if len(rec.events) != 1 {
		t.Errorf("want 1 audit event, got %d", len(rec.events))
	}
}

// TestIncarnationTraitsSet_ScopedOnAnotherDimension — the behavior change NIM-587
// carries for deployed roles, pinned in both directions. An operator scoped only
// `on coven=dba` has an EMPTY trait-scope (a coven disjunct says nothing about traits,
// and mixed disjuncts are dropped fail-closed), so it may no longer stamp ANY pair —
// while an empty payload, which only CLEARS labels and grants nobody anything, still
// passes.
func TestIncarnationTraitsSet_ScopedOnAnotherDimension(t *testing.T) {
	t.Run("stamping a pair is refused", func(t *testing.T) {
		pool := &fakePool{
			incFn:    incForTraitsInCoven(nil),
			beginErr: errFakeUnexpected{sql: "BeginTx must not be called with an empty trait-scope"},
		}
		h, rec := newTestHandlerFull(t, pool, traitsSetScopedRBAC(), nil, nil, nil)
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
			`{"name":"redis-prod","traits":{"team":"dba"}}`)
		if resp.Error == nil {
			t.Fatal("expected error")
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
			t.Errorf("code = %q, want validation-failed", data.Code)
		}
		if len(rec.events) != 0 {
			t.Error("a refused traits-set must not write audit")
		}
	})

	t.Run("clearing the labels still passes", func(t *testing.T) {
		pool := &fakePool{incFn: incForTraitsInCoven(map[string]any{"team": "dba"})}
		h, _ := newTestHandlerFull(t, pool, traitsSetScopedRBAC(), nil, nil, nil)
		resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
			`{"name":"redis-prod"}`)
		if resp.Error != nil {
			t.Fatalf("clearing labels must pass an empty trait-scope, got: %+v", resp.Error)
		}
		if out := decodeTraitsSetOutput(t, resp); len(out.Keys) != 0 {
			t.Errorf("keys = %v, want none (cleared)", out.Keys)
		}
	})
}

// TestIncarnationTraitsSet_NoPurviewResolver — the gate cannot be EVALUATED (no
// resolver wired): fail-closed. internal-error rather than validation-failed because
// the operator did nothing wrong, and above all NO WRITE — waving it through on a
// missing dependency is exactly what the gate exists to prevent.
//
// The handler is built by hand: both newTestHandler helpers wire the resolver now,
// precisely because production always does, so this shape has to be constructed
// deliberately to be tested at all.
//
// The pool deliberately does NOT block BeginTx here. Blocking it would make the tool
// answer internal-error whether the gate ran or not, and the case would pass with the
// gate deleted — proving nothing. Left writable, a missing gate lets the write through
// and the tool answers success, which is the failure this case is for.
func TestIncarnationTraitsSet_NoPurviewResolver(t *testing.T) {
	pool := &fakePool{incFn: incForTraitsInCoven(nil)}
	enf, err := rbactest.NewEnforcer(traitsSetRBAC())
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool: pool, Issuer: &fakeIssuer{}, RBAC: enf, TTLDefault: time.Hour,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB: pool,
		// PurviewResolver deliberately absent — the case under test.
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.traits-set",
		`{"name":"redis-prod","traits":{"env":"prod"}}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("an unevaluable gate must not let the write through")
	}
}
