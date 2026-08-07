package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// mcpDestroyer — mock of [handlers.DestroyStarter]: captures the teardown
// spec + the number of StartDestroy calls.
type mcpDestroyer struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *mcpDestroyer) StartDestroy(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

func destroyerRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "destroyer", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.destroy"}},
		},
	}
}

// newTestHandlerDestroy assembles a Handler with the full destroy stack
// (destroyer + registry + loader). hasScenario controls whether scenario
// `destroy` is present in the snapshot (mcpLoader.ReadFile).
func newTestHandlerDestroy(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config, destroyer handlers.DestroyStarter, hasScenario bool) (*Handler, *recordingAudit) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc:       svc,
		RBAC:              enf,
		AuditWriter:       rec,
		Logger:            slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB:     pool,
		ScenarioDestroyer: destroyer,
		ServiceRegistry:   &mcpResolver{ok: true},
		ServiceLoader:     &mcpLoader{hasDestroyScenario: hasScenario},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// --- teardown path (allow_destroy=false, scenario present) ---------------

func TestToolsCall_IncarnationDestroy_Teardown_Success(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationDestroyOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("_apply_id not ULID: %q", out.ApplyID)
	}
	if destroyer.calls != 1 || destroyer.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("teardown spec mismatch: %+v (calls=%d)", destroyer.gotSpec, destroyer.calls)
	}
	// Teardown runs against the deployed service version.
	if destroyer.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("teardown ServiceRef.Ref = %q, want v1", destroyer.gotSpec.ServiceRef.Ref)
	}
	// audit destroy_started (source=mcp), force=false.
	if !recHasEvent(rec, audit.EventIncarnationDestroyStarted) {
		t.Errorf("expected destroy_started")
	}
	ev := recEvent(rec, audit.EventIncarnationDestroyStarted)
	if ev == nil || ev.Source != audit.SourceMCP {
		t.Errorf("destroy_started source = %v, want mcp", ev)
	}
	if ev.Payload["force"] != false {
		t.Errorf("destroy_started force = %v, want false", ev.Payload["force"])
	}
}

// --- 422: allow_destroy=false and no scenario ---------------------------

func TestToolsCall_IncarnationDestroy_NoScenario_NoForce(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, false)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
	if destroyer.calls != 0 {
		t.Error("teardown must NOT start (pre-check denial)")
	}
	if len(rec.events) != 0 {
		t.Error("pre-check denial must not write audit")
	}
}

// --- force path (allow_destroy=true, no scenario) → DELETE -----------

func TestToolsCall_IncarnationDestroy_Force_Delete(t *testing.T) {
	pool := &fakePool{
		incFn:      incWithStatus(incarnation.StatusReady),
		memberSIDs: []string{"vm-1.example.com", "vm-2.example.com"},
	}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, false)
	var logs bytes.Buffer
	h.deps.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if destroyer.calls != 0 {
		t.Error("force skips teardown")
	}
	// destroy_started (force=true) + destroy_completed (force-DELETE).
	if !recHasEvent(rec, audit.EventIncarnationDestroyStarted) {
		t.Errorf("expected destroy_started")
	}
	if !recHasEvent(rec, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("expected destroy_completed (force-DELETE)")
	}
	if ev := recEvent(rec, audit.EventIncarnationDestroyStarted); ev == nil || ev.Payload["force"] != true {
		t.Errorf("destroy_started force payload = %v, want true", ev)
	}
	// NIM-395: the tool result must name what force-destroy did NOT release.
	// An agent driving this tool sees only the result — a bare `_apply_id`
	// reads as "cleaned up", while the provisioned VMs are still running and
	// the record that named them is already gone.
	var out struct {
		Structured struct {
			ApplyID    string `json:"_apply_id"`
			Unreleased *struct {
				Provider string   `json:"provider"`
				VMIDs    []string `json:"vm_ids"`
				SIDs     []string `json:"sids"`
			} `json:"unreleased"`
		} `json:"structuredContent"`
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal result: %v; raw=%s", err, raw)
	}
	if out.Structured.Unreleased == nil {
		t.Fatalf("force-destroy tool result carries no `unreleased` block; raw=%s", raw)
	}
	if out.Structured.Unreleased.Provider != "example-dev" || len(out.Structured.Unreleased.VMIDs) != 2 {
		t.Errorf("unreleased = %+v, want example-dev with the two still-running VMs", out.Structured.Unreleased)
	}
	// The member SIDs are a separate dimension: a create that failed before the
	// driver committed vm ids still leaves registered hosts behind, and the
	// membership rows that named them are about to CASCADE away.
	if got := out.Structured.Unreleased.SIDs; len(got) != 2 || got[0] != "vm-1.example.com" || got[1] != "vm-2.example.com" {
		t.Errorf("unreleased.sids = %v, want the two member hosts; raw=%s", got, raw)
	}
	// The `unreleased` block must survive a schema-validating client: a tool that
	// emits a key its own OutputSchema does not declare has it stripped, which is
	// exactly the warning this ticket exists to deliver.
	assertStructuredMatchesOutputSchema(t, "keeper.incarnation.destroy", raw)
	// The tool result is read by the agent that made the call and by nobody else
	// — it is not persisted anywhere an operator can query later. The same list
	// therefore goes to the keeper log at WARN, and it has to be the whole list:
	// a WARN naming fewer resources than were abandoned understates the leak to
	// the one reader who is not the caller.
	warn := logs.String()
	for _, want := range []string{"example-dev", "i-aaa111", "i-bbb222", "vm-1.example.com", "vm-2.example.com"} {
		if !strings.Contains(warn, want) {
			t.Errorf("force-destroy WARN does not name %q — the operator-facing copy that outlives the tool result is short; log=%s",
				want, warn)
		}
	}
}

// TestUnreleasedResourcesOutput_CoversEveryDimension — GUARD, twin of the REST
// one in internal/api: callIncarnationDestroy copies
// [incarnation.UnreleasedResources] field by field, so a fourth kind of
// abandoned resource added to the domain reaches the archive and the audit
// event — both marshal the struct whole — and stops silently here. An agent
// driving this tool would act on a list shorter than the record, and the record
// is in a table with no read API.
//
// assertStructuredMatchesOutputSchema does not cover this: it checks the payload
// against the tool's declared schema, and a dimension missing from BOTH agrees
// with itself.
func TestUnreleasedResourcesOutput_CoversEveryDimension(t *testing.T) {
	domain := mcpJSONFieldNames(t, incarnation.UnreleasedResources{})
	out := mcpJSONFieldNames(t, unreleasedResourcesOutput{})
	if !slices.Equal(domain, out) {
		t.Errorf("unreleasedResourcesOutput names %v, the domain records %v — "+
			"a resource the force-destroy abandoned never reaches the tool result",
			out, domain)
	}
}

// mcpJSONFieldNames — sorted json names of v's fields.
func mcpJSONFieldNames(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s carries no json name — it cannot reach any wire surface",
				rt.Name(), rt.Field(i).Name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// --- force-DELETE no-op (RowsAffected==0) → success, no completed ----

func TestToolsCall_IncarnationDestroy_Force_DeleteNoOp(t *testing.T) {
	pool := &fakePool{
		incFn:     incWithStatus(incarnation.StatusReady),
		deleteTag: pgconn.NewCommandTag("DELETE 0"),
	}
	h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, false)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if recHasEvent(rec, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed must not be written on no-op DELETE")
	}
}

// --- 409: status doesn't allow destroy (applying) ----------------------

func TestToolsCall_IncarnationDestroy_NotDestroyable(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusApplying)}
	destroyer := &mcpDestroyer{}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeIncarnationLocked {
		t.Errorf("data.code = %q, want incarnation-locked", data.Code)
	}
	if destroyer.calls != 0 {
		t.Error("applying must not start teardown")
	}
}

// --- 404: incarnation doesn't exist ------------------------------------

func TestToolsCall_IncarnationDestroy_NotFound(t *testing.T) {
	pool := &fakePool{incFn: func(string) (*incarnation.Incarnation, error) { return nil, pgx.ErrNoRows }}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"ghost","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

// --- RBAC forbidden ---------------------------------------------------

func TestToolsCall_IncarnationDestroy_RBACForbidden(t *testing.T) {
	// RBAC is empty → deny. SelectByName RESOLVES scope (covens ∪ {name}) for
	// the OR-check (mirrors the REST middleware), then the enforcer denies →
	// forbidden. teardown/audit do NOT start on denial.
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	destroyer := &mcpDestroyer{}
	h, rec := newTestHandlerDestroy(t, pool, nil, destroyer, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if destroyer.calls != 0 || len(rec.events) != 0 {
		t.Error("denied destroy must not start teardown / write audit")
	}
}

// --- validation: allow_destroy missing ----------------------------

func TestToolsCall_IncarnationDestroy_MissingAllowDestroy(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	h, _ := newTestHandlerDestroy(t, pool, destroyerRBAC(), &mcpDestroyer{}, true)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// --- 500: destroy not configured ----------------------------------

func TestToolsCall_IncarnationDestroy_NotConfigured(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady)}
	// ScenarioDestroyer is not wired (newTestHandlerFull without a destroyer).
	h, _ := newTestHandlerFull(t, pool, destroyerRBAC(), nil, &mcpResolver{ok: true}, &mcpLoader{hasDestroyScenario: true})

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
		`{"name":"redis-prod","allow_destroy":false}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("data.code = %q, want internal-error", data.Code)
	}
}

// --- helpers ----------------------------------------------------------

func recHasEvent(rec *recordingAudit, et audit.EventType) bool {
	return recEvent(rec, et) != nil
}

func recEvent(rec *recordingAudit, et audit.EventType) *audit.Event {
	for _, ev := range rec.events {
		if ev.EventType == et {
			return ev
		}
	}
	return nil
}
