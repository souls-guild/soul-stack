package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// MCP-граница sigil.key.*-tools (R3-S7): проверяется ТРАНСПОРТ — диспетч,
// permission-проверка, маппинг sentinel-ов, audit, паритет с REST. Бизнес-логика
// (key-gen, Vault-write, CRUD) — пакет sigil; здесь только MCP-граница.

const skValidKeyID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// --- fakes под sigil.KeyService ---

type skVault struct{ err error }

func (v skVault) WriteKV(context.Context, string, map[string]any) error { return v.err }

type skPool struct{ tx *skTx }

func (p *skPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *skPool) QueryRow(context.Context, string, ...any) pgx.Row { return skErrRow{pgx.ErrNoRows} }
func (p *skPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("skPool.Query unused")
}
func (p *skPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return p.tx, nil }

type skErrRow struct{ err error }

func (r skErrRow) Scan(...any) error { return r.err }

type skTx struct {
	pgx.Tx
	insertID   int64
	insertTime time.Time
}

func (tx *skTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (tx *skTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO sigil_signing_keys") {
		return skInsertRow{id: tx.insertID, at: tx.insertTime}
	}
	return skErrRow{pgx.ErrNoRows}
}
func (tx *skTx) Commit(context.Context) error   { return nil }
func (tx *skTx) Rollback(context.Context) error { return nil }

type skInsertRow struct {
	id int64
	at time.Time
}

func (r skInsertRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.id
	*(dest[1].(*time.Time)) = r.at
	return nil
}

// newSigilKeyHandler собирает Handler с SigilKeySvc над fake pool/vault.
// withKeySvc=false → SigilKeySvc остаётся nil (для nil-guard).
func newSigilKeyHandler(t *testing.T, rbacCfg *rbactest.Config, withKeySvc bool) (*Handler, *recordingAudit) {
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
	if withKeySvc {
		svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{
			Pool:  &skPool{tx: &skTx{insertID: 1, insertTime: time.Unix(1700000000, 0).UTC()}},
			Vault: skVault{},
		})
		if err != nil {
			t.Fatalf("sigil.NewKeyService: %v", err)
		}
		deps.SigilKeySvc = svc
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// sigilKeyAdminCfg — RBAC, дающий archon-alice все sigil.key-*-permissions.
func sigilKeyAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "sigil-key-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"sigil.key-introduce", "sigil.key-retire", "sigil.key-list", "sigil.key-set-primary",
			}},
		},
	}
}

func TestSigilKeyTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.sigil.key.introduce", "keeper.sigil.key.list",
		"keeper.sigil.key.set-primary", "keeper.sigil.key.retire",
	}
	for _, name := range want {
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

func TestSigilKeyTools_NilGuard(t *testing.T) {
	h, _ := newSigilKeyHandler(t, sigilKeyAdminCfg(), false) // SigilKeySvc == nil
	cases := []struct{ tool, args string }{
		{"keeper.sigil.key.introduce", `{}`},
		{"keeper.sigil.key.list", `{}`},
		{"keeper.sigil.key.set-primary", `{"key_id":"` + skValidKeyID + `"}`},
		{"keeper.sigil.key.retire", `{"key_id":"` + skValidKeyID + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

func TestSigilKeyTools_RBACForbidden(t *testing.T) {
	// archon-alice без sigil.key-*-permissions (пустой RBAC → deny all).
	h, rec := newSigilKeyHandler(t, nil, true)
	resp := callTool(t, h, "archon-alice", "keeper.sigil.key.introduce", `{"make_primary":true}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied introduce must not write audit")
	}
}

func TestSigilKeyIntroduce_Success_NoPrivateKey(t *testing.T) {
	h, rec := newSigilKeyHandler(t, sigilKeyAdminCfg(), true)
	resp := callTool(t, h, "archon-alice", "keeper.sigil.key.introduce", `{"make_primary":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	var out sigilKeyIntroduceOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(out.KeyID) != 64 {
		t.Errorf("key_id len = %d, want 64", len(out.KeyID))
	}
	if !strings.Contains(out.PubkeyPEM, "BEGIN PUBLIC KEY") {
		t.Errorf("pubkey_pem не SPKI: %q", out.PubkeyPEM)
	}
	// SECURITY: ни одно поле output-а не содержит приватник.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Errorf("private key leaked into output: %s", raw)
	}
	// Audit sigil.key-introduced записан с key_id, без приватника.
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	if rec.events[0].EventType != "sigil.key-introduced" {
		t.Errorf("event = %q, want sigil.key-introduced", rec.events[0].EventType)
	}
}

func TestSigilKeyTools_ValidationBadKeyID(t *testing.T) {
	h, _ := newSigilKeyHandler(t, sigilKeyAdminCfg(), true)
	for _, tool := range []string{"keeper.sigil.key.set-primary", "keeper.sigil.key.retire"} {
		t.Run(tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tool, `{"key_id":"NOTHEX"}`)
			if resp.Error == nil {
				t.Fatal("expected validation error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
				t.Errorf("code = %q, want validation-failed", data.Code)
			}
		})
	}
}
