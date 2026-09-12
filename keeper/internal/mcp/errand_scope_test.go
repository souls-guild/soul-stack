package mcp

// Scope guards for the errand MCP tools (NIM-841) — the twins of the REST
// guards in api/handlers/errand_scope_test.go.
//
// The two facades are the point. `keeper.errand.list` and `keeper.errand.get`
// used to call RBAC.Check and then read the store unfiltered, so closing the
// REST hole alone would have left the same fleet-wide stdout one JSON-RPC call
// away. These tests use a REAL enforcer rather than a stub: the rule under test
// is that a scoped grant narrows instead of denying, and only the real
// ResolvePurview knows the difference between a bare permission and a scoped one.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// errandScopedCfg grants `errand.list`/`errand.cancel` narrowed to one coven —
// the spelling the NoSelector gate used to reject outright, which is why the
// right had only two settings: nothing, or the whole fleet.
func errandScopedCfg(coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{{
			Name:      "dev-ops",
			Operators: []string{"archon-alice"},
			Permissions: []string{
				"errand.list on coven=" + coven,
				"errand.cancel on coven=" + coven,
			},
		}},
	}
}

// mcpErrandPool records every statement and serves one row per id.
type mcpErrandPool struct {
	rows map[string][]any

	lastSQL  string
	lastArgs []any
}

func (p *mcpErrandPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("mcpErrandPool.Exec: unexpected")
}

func (p *mcpErrandPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	p.lastSQL, p.lastArgs = sql, args
	if strings.Contains(sql, "COUNT(*)") {
		return mcpErrandRow{values: []any{0}}
	}
	if len(args) == 1 {
		if v, ok := p.rows[args[0].(string)]; ok {
			return mcpErrandRow{values: v}
		}
	}
	return mcpErrandRow{err: pgx.ErrNoRows}
}

func (p *mcpErrandPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	p.lastSQL, p.lastArgs = sql, args
	return &mcpErrandRows{}, nil
}

type mcpErrandRow struct {
	values []any
	err    error
}

func (r mcpErrandRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch dd := d.(type) {
		case *string:
			*dd = r.values[i].(string)
		case *int:
			*dd = r.values[i].(int)
		case *bool:
			*dd = r.values[i].(bool)
		case *time.Time:
			*dd = r.values[i].(time.Time)
		case *[]byte:
			if r.values[i] != nil {
				*dd = r.values[i].([]byte)
			}
		case *[]string:
			if r.values[i] != nil {
				*dd = r.values[i].([]string)
			}
		}
	}
	return nil
}

type mcpErrandRows struct{}

func (*mcpErrandRows) Next() bool                    { return false }
func (*mcpErrandRows) Scan(...any) error             { return nil }
func (*mcpErrandRows) Err() error                    { return nil }
func (*mcpErrandRows) Close()                        {}
func (*mcpErrandRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (*mcpErrandRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (*mcpErrandRows) Values() ([]any, error) { return nil, nil }
func (*mcpErrandRows) RawValues() [][]byte    { return nil }
func (*mcpErrandRows) Conn() *pgx.Conn        { return nil }

// mcpErrandScanRow — the 20 joined scan columns (see errand.selectColumns).
func mcpErrandScanRow(id, sid string, covens []string) []any {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	return []any{
		id, sid, "core.cmd.shell", []byte(nil), string(errand.StatusRunning),
		nil, "root-only secret", "", false, false,
		nil, "", []byte(nil), "archon-alice", "kid-1",
		now, nil, now,
		covens, []byte(nil),
	}
}

// TestMCPErrandList_PurviewIsPushedIntoTheQuery — the tool must narrow in SQL,
// not in Go. The assertion is on the statement because the fake replays a fixed
// page either way: "the response was empty" proves nothing about the filter.
func TestMCPErrandList_PurviewIsPushedIntoTheQuery(t *testing.T) {
	pool := &mcpErrandPool{rows: map[string][]any{}}
	h, _, _ := newTestHandler(t, &fakePool{}, errandScopedCfg("dev"))
	h.deps.ErrandStore = errand.NewStore(pool)

	resp := callTool(t, h, "archon-alice", "keeper.errand.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("errand.list: %+v", resp.Error)
	}
	if !strings.Contains(pool.lastSQL, "s.coven && $") {
		t.Errorf("list SQL carries no coven predicate: %q", pool.lastSQL)
	}
	if !strings.Contains(pool.lastSQL, "LEFT JOIN souls") {
		t.Errorf("list SQL does not join souls: %q", pool.lastSQL)
	}
}

// TestMCPErrandGet_OutOfScopeIsNotFound — the MCP twin of the REST fold. An
// errand on a host outside the purview must answer exactly as a missing one, or
// the tool becomes an existence oracle over the fleet's command history.
func TestMCPErrandGet_OutOfScopeIsNotFound(t *testing.T) {
	pool := &mcpErrandPool{rows: map[string][]any{
		"ERR-PROD": mcpErrandScanRow("ERR-PROD", "prod-db-01", []string{"prod"}),
	}}
	h, _, _ := newTestHandler(t, &fakePool{}, errandScopedCfg("dev"))
	h.deps.ErrandStore = errand.NewStore(pool)

	resp := callTool(t, h, "archon-alice", "keeper.errand.get", `{"errand_id":"ERR-PROD"}`)
	if resp.Error == nil {
		t.Fatal("an out-of-scope errand was returned to a coven-scoped operator")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want %q — a distinct refusal confirms the errand exists",
			data.Code, mcpCodeNotFound)
	}
}

// TestMCPErrandGet_InScopeIsServed — the narrowing admits the hosts the operator
// IS entitled to. Without this the guard above passes on a tool that refuses
// everything.
func TestMCPErrandGet_InScopeIsServed(t *testing.T) {
	pool := &mcpErrandPool{rows: map[string][]any{
		"ERR-DEV": mcpErrandScanRow("ERR-DEV", "dev-web-01", []string{"dev"}),
	}}
	h, _, _ := newTestHandler(t, &fakePool{}, errandScopedCfg("dev"))
	h.deps.ErrandStore = errand.NewStore(pool)

	resp := callTool(t, h, "archon-alice", "keeper.errand.get", `{"errand_id":"ERR-DEV"}`)
	if resp.Error != nil {
		t.Fatalf("a coven-scoped operator lost their own coven's errand: %+v", resp.Error)
	}
}

// TestMCPErrandCancel_OutOfScopeIsNotFound — `errand.cancel` is a write, and the
// host it reaches is only knowable from the row. The refusal is the uniform
// not-found for the same anti-enumeration reason as the read.
func TestMCPErrandCancel_OutOfScopeIsNotFound(t *testing.T) {
	pool := &mcpErrandPool{rows: map[string][]any{
		"ERR-PROD": mcpErrandScanRow("ERR-PROD", "prod-db-01", []string{"prod"}),
	}}
	h, _, _ := newTestHandler(t, &fakePool{}, errandScopedCfg("dev"))
	h.deps.ErrandStore = errand.NewStore(pool)
	h.deps.ErrandDispatcher = mcpCancelDispatcher(t)

	resp := callTool(t, h, "archon-alice", "keeper.errand.cancel", `{"errand_id":"ERR-PROD"}`)
	if resp.Error == nil {
		t.Fatal("a coven-scoped operator cancelled a command on a host outside their coven")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want %q", data.Code, mcpCodeNotFound)
	}
}

// mcpCancelDispatcher — a dispatcher that FAILS if it is reached. The scope
// refusal under test must happen before dispatch, so an assertion on the error
// code alone could be satisfied by a cancel that ran and then reported not-found.
func mcpCancelDispatcher(t *testing.T) *errand.Dispatcher {
	t.Helper()
	d, err := errand.NewDispatcher(errand.Deps{
		Store:    mcpUnreachableStore{t},
		Outbound: mcpUnreachableStore{t},
		ApplyBus: mcpUnreachableStore{t},
		Logger:   slog.New(slog.DiscardHandler),
		KID:      "kid-1",
	})
	if err != nil {
		t.Fatalf("errand.NewDispatcher: %v", err)
	}
	return d
}

// mcpUnreachableStore satisfies the dispatcher's store/outbound/bus surfaces and
// fails the test on any call: the cancel path must not get this far.
type mcpUnreachableStore struct{ t *testing.T }

func (s mcpUnreachableStore) reached(what string) {
	s.t.Helper()
	s.t.Errorf("the dispatcher was reached (%s) — the scope check ran too late", what)
}

func (s mcpUnreachableStore) Insert(context.Context, errand.Row) error {
	s.reached("Insert")
	return nil
}

func (s mcpUnreachableStore) Get(context.Context, string) (*errand.Row, error) {
	s.reached("Get")
	return nil, errand.ErrNotFound
}

func (s mcpUnreachableStore) MarkTerminal(context.Context, string, errand.TerminalUpdate) (bool, error) {
	s.reached("MarkTerminal")
	return false, nil
}

func (s mcpUnreachableStore) SweepOrphanRunning(context.Context, string, time.Duration, string) ([]string, error) {
	return nil, nil
}

func (s mcpUnreachableStore) SendErrand(context.Context, string, *keeperv1.ErrandRequest) error {
	s.reached("SendErrand")
	return nil
}

func (s mcpUnreachableStore) SendCancelErrand(context.Context, string, string) error {
	s.reached("SendCancelErrand")
	return nil
}

func (s mcpUnreachableStore) Subscribe(context.Context, string) <-chan applybus.Event {
	return make(chan applybus.Event)
}

func (s mcpUnreachableStore) SubscribeWithBridge(context.Context, string, bool) <-chan applybus.Event {
	return make(chan applybus.Event)
}
