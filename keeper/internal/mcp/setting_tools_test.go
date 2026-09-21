package mcp

// Transport guards of the setting.*-tools (ADR-0073). The gate itself lives in
// handlers.SettingsHandler and is covered where it lives; here the question is
// whether MCP reaches it with the same rules — permission first, catalog
// admission, and an audit record only on a write that actually happened.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// settingFakePool records what reached `keeper_settings`.
type settingFakePool struct {
	writes  int
	deletes int
}

func (p *settingFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM keeper_settings") {
		p.deletes++
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *settingFakePool) QueryRow(context.Context, string, ...any) pgx.Row {
	p.writes++
	return settingFakeRow{}
}

func (p *settingFakePool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, &svcRegErr{"settingFakePool.Query unused"}
}

type settingFakeRow struct{}

func (settingFakeRow) Scan(dest ...any) error {
	for _, d := range dest {
		if tp, ok := d.(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

type settingFakeOverlay struct{}

func (settingFakeOverlay) Values() map[string]any         { return nil }
func (settingFakeOverlay) Overlay() []config.OverlayEntry { return nil }
func (settingFakeOverlay) Refresh(context.Context) error  { return nil }

// newSettingToolHandler wires the tools over a REAL config.Store on the golden
// keeper.yml, so the cross-field dry-run merge in the write-gate is the
// production one.
func newSettingToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *settingFakePool) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc:   opSvc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        logger,
		IncarnationDB: &fakePool{},
	}
	if pool != nil {
		svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool, Logger: logger})
		if err != nil {
			t.Fatalf("serviceregistry.NewService: %v", err)
		}
		deps.Settings = handlers.NewSettingsHandler(settingToolStore(t), settingFakeOverlay{}, svc, logger)
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

func settingToolStore(t *testing.T) *config.Store[config.KeeperConfig] {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../../examples/keeper/keeper.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, diags, err := config.LoadKeeperStore(path, config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load fixture: err=%v diags=%v", err, diags)
	}
	return store
}

func settingAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "settings-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"setting.read", "setting.update", "setting.delete",
			}},
		},
	}
}

// settingsReaderCfg — read-only: the operator can look at the catalog but not
// change a cluster-wide tunable.
func settingsReaderCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "settings-reader", Operators: []string{"archon-alice"}, Permissions: []string{
				"setting.read",
			}},
		},
	}
}

func callSettingTool(t *testing.T, h *Handler, aid, tool, args string) jsonRPCResponse {
	t.Helper()
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"` + tool + `","arguments":` + args + `}`),
	}
	resp, _ := h.Dispatch(context.Background(), claims(aid), req)
	return resp
}

func TestSettingTools_InManifest(t *testing.T) {
	for _, name := range []string{"keeper.setting.list", "keeper.setting.update", "keeper.setting.delete"} {
		e, ok := toolByName(name)
		if !ok {
			t.Errorf("%s missing from catalogManifest", name)
			continue
		}
		if e.status != toolStatusImplemented {
			t.Errorf("%s status = %d, want Implemented", name, e.status)
		}
	}
}

// The catalog an agent needs to construct a valid value: type, bounds, default,
// the effective value and where it came from.
func TestSettingList_ReturnsTheCatalog(t *testing.T) {
	h, rec := newSettingToolHandler(t, settingAdminCfg(), &settingFakePool{})

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.list", `{}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	body := string(resp.Result)
	for _, want := range []string{`"key":"cfg_toll_threshold"`, `"type":"duration"`, `"source":"`, `"bounds":"`} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog missing %s; body=%s", want, body)
		}
	}
	// Reads are not audited (the provisioning.read precedent).
	if len(rec.events) != 0 {
		t.Errorf("a read was audited (%d events)", len(rec.events))
	}
}

func TestSettingUpdate_WritesAndAudits(t *testing.T) {
	pool := &settingFakePool{}
	h, rec := newSettingToolHandler(t, settingAdminCfg(), pool)

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.update",
		`{"key":"cfg_reaper_interval","value":"120m"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if pool.writes != 1 {
		t.Errorf("writes = %d, want 1", pool.writes)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventSettingUpdated {
		t.Fatalf("audit events = %+v", rec.events)
	}
	if rec.events[0].Source != audit.SourceMCP {
		t.Errorf("audit source = %q, want mcp", rec.events[0].Source)
	}
	// The duration is normalized by the same field-registry both transports use.
	if got := rec.events[0].Payload["value"]; got != "2h" {
		t.Errorf("audited value = %v, want 2h", got)
	}
}

// Admission is enumerated — a key outside the registry does not exist, on this
// transport either.
func TestSettingUpdate_UnknownKeyIsNotFound(t *testing.T) {
	pool := &settingFakePool{}
	h, rec := newSettingToolHandler(t, settingAdminCfg(), pool)

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.update",
		`{"key":"cfg_nope","value":"1"}`)
	if resp.Error == nil {
		t.Fatal("unknown key accepted")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want %s", data.Code, mcpCodeNotFound)
	}
	if pool.writes != 0 || len(rec.events) != 0 {
		t.Errorf("an unregistered key reached Postgres/audit")
	}
}

// The write-gate is the handler's, so MCP inherits it: a value that is in-bounds
// per field but breaks a cross-field invariant is refused before the write.
func TestSettingUpdate_CrossFieldViolationIsRejected(t *testing.T) {
	pool := &settingFakePool{}
	h, rec := newSettingToolHandler(t, settingAdminCfg(), pool)

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.update",
		`{"key":"cfg_cadence_scheduler_poll_floor","value":"10m"}`)
	if resp.Error == nil {
		t.Fatal("poll_floor=10m accepted against poll_ceiling=1m")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want %s", data.Code, mcpCodeValidationFailed)
	}
	if pool.writes != 0 || len(rec.events) != 0 {
		t.Errorf("a rejected value reached Postgres/audit")
	}
}

// Editing a cluster-wide tunable is its own privilege: setting.read alone is
// not enough, and the refusal reaches Postgres as nothing at all.
func TestSettingUpdate_WithoutPermissionIsForbidden(t *testing.T) {
	pool := &settingFakePool{}
	h, rec := newSettingToolHandler(t, settingsReaderCfg(), pool)

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.update",
		`{"key":"cfg_toll_threshold","value":"0.5"}`)
	if resp.Error == nil {
		t.Fatal("update accepted without setting.update")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want %s", data.Code, mcpCodeForbidden)
	}
	if pool.writes != 0 {
		t.Errorf("keeper_settings written without the permission")
	}
	if len(rec.events) != 0 {
		t.Errorf("audit recorded for a forbidden update (%d events)", len(rec.events))
	}
}

func TestSettingDelete_WithoutPermissionIsForbidden(t *testing.T) {
	pool := &settingFakePool{}
	h, _ := newSettingToolHandler(t, settingsReaderCfg(), pool)

	resp := callSettingTool(t, h, "archon-alice", "keeper.setting.delete",
		`{"key":"cfg_toll_threshold"}`)
	if resp.Error == nil {
		t.Fatal("delete accepted without setting.delete")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want %s", data.Code, mcpCodeForbidden)
	}
	if pool.deletes != 0 {
		t.Errorf("a row was deleted without the permission")
	}
}

// Under the break-glass switch the overlay is unwired: the tools still dispatch,
// they just report that there is nothing to talk to (the ServiceSvc pattern).
func TestSettingTools_NotConfigured(t *testing.T) {
	h, _ := newSettingToolHandler(t, settingAdminCfg(), nil)

	for _, tc := range []struct{ tool, args string }{
		{"keeper.setting.list", `{}`},
		{"keeper.setting.update", `{"key":"cfg_toll_threshold","value":"0.5"}`},
		{"keeper.setting.delete", `{"key":"cfg_toll_threshold"}`},
	} {
		resp := callSettingTool(t, h, "archon-alice", tc.tool, tc.args)
		if resp.Error == nil {
			t.Errorf("%s: succeeded with no settings store", tc.tool)
			continue
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
			t.Errorf("%s: code = %q, want %s", tc.tool, data.Code, mcpCodeInternalError)
		}
	}
}
