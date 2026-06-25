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
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- soul fake pool ---
//
// Узкий fake под [handlers.SoulPool]: гоняет именно те SQL-запросы, что нужны
// soul-tools-ам (Insert souls / Insert bootstrap_tokens / SelectBySID / Expire
// active). Бизнес-инварианты CRUD-слоя (souls/bootstraptoken) покрыты
// integration-тестами соответствующих пакетов; здесь проверяется ТРАНСПОРТ —
// что tool правильно зовёт CRUD, маппит ошибки и кодирует output (симметрично
// roleFakePool).

type soulFakePool struct {
	// soulInsertErr — ошибка, которую вернёт INSERT INTO souls (через RETURNING-
	// scan). Позволяет инжектить sentinel-ы soul.* (duplicate / creator-not-found)
	// или сырую pg-ошибку.
	soulInsertErr error

	// selectSoul — что вернёт SELECT … FROM souls WHERE sid (issue-token-флоу).
	// selectErr приоритетнее: nil-soul + selectErr → not-found / прочее.
	selectSoul *soul.Soul
	selectErr  error

	// tokenInsertErr — ошибка INSERT INTO bootstrap_tokens (RETURNING-scan).
	// bootstraptoken.ErrTokenActiveExists → активный токен без force.
	tokenInsertErr error

	// expireExpired — что вернёт UPDATE bootstrap_tokens … RETURNING token_id
	// (ExpireActiveBySID): true → активный токен был и инвалидирован
	// (expired_previous=true); false → активного не было (pgx.ErrNoRows-ветка).
	expireExpired bool

	// beginErr — ошибка BeginTx (сбой открытия транзакции до любых записей).
	beginErr error

	// commitErr — ошибка Commit (запись прошла, фиксация сорвалась). Проброс в
	// soulFakeTx.Commit при создании транзакции.
	commitErr error
}

func (p *soulFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errFakeUnexpected{sql: sql}
}

func (p *soulFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO souls"):
		if p.soulInsertErr != nil {
			return errRow{err: p.soulInsertErr}
		}
		// RETURNING registered_at, requested_at.
		return staticRow{values: []any{time.Time{}, time.Time{}}}
	case strings.Contains(sql, "INSERT INTO bootstrap_tokens"):
		if p.tokenInsertErr != nil {
			return errRow{err: p.tokenInsertErr}
		}
		// RETURNING token_id, created_at.
		return staticRow{values: []any{"token-uuid", time.Time{}}}
	case strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid"):
		if p.selectErr != nil {
			return errRow{err: p.selectErr}
		}
		return soulSelectRow{s: p.selectSoul}
	case strings.Contains(sql, "UPDATE bootstrap_tokens") && strings.Contains(sql, "RETURNING token_id"):
		// ExpireActiveBySID: строка есть → expired; нет строки → ErrNoRows.
		if p.expireExpired {
			return staticRow{values: []any{"expired-token-uuid"}}
		}
		return errRow{err: pgx.ErrNoRows}
	}
	return errRow{err: errFakeUnexpected{sql: sql}}
}

func (p *soulFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return nil, errFakeUnexpected{sql: sql}
}

func (p *soulFakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return &soulFakeTx{pool: p, commitErr: p.commitErr}, nil
}

type soulFakeTx struct {
	pool      *soulFakePool
	commitErr error
}

func (t *soulFakeTx) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, a...)
}
func (t *soulFakeTx) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, a...)
}
func (t *soulFakeTx) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, a...)
}
func (t *soulFakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *soulFakeTx) Commit(_ context.Context) error            { return t.commitErr }
func (t *soulFakeTx) Rollback(_ context.Context) error          { return nil }
func (t *soulFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("soulFakeTx.CopyFrom: unexpected")
}
func (t *soulFakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("soulFakeTx.SendBatch: unexpected")
}
func (t *soulFakeTx) LargeObjects() pgx.LargeObjects { panic("soulFakeTx.LargeObjects: unexpected") }
func (t *soulFakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("soulFakeTx.Prepare: unexpected")
}
func (t *soulFakeTx) Conn() *pgx.Conn { return nil }

// soulSelectRow — pgx.Row для SELECT … FROM souls WHERE sid (scanSoul читает
// 11 колонок: sid, transport, status, coven, traits, registered_at,
// last_seen_at, last_seen_by_kid, created_by_aid, requested_at, note).
type soulSelectRow struct{ s *soul.Soul }

func (r soulSelectRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.s.SID
	*dest[1].(*string) = string(r.s.Transport)
	*dest[2].(*string) = string(r.s.Status)
	*dest[3].(*[]string) = r.s.Coven
	// traits jsonb (ADR-060): nil Traits → nil-bytes (scanSoul → пустой map).
	if len(r.s.Traits) > 0 {
		b, err := json.Marshal(r.s.Traits)
		if err != nil {
			return err
		}
		*dest[4].(*[]byte) = b
	} else {
		*dest[4].(*[]byte) = nil
	}
	*dest[5].(*time.Time) = r.s.RegisteredAt
	*dest[6].(**time.Time) = r.s.LastSeenAt
	*dest[7].(**string) = r.s.LastSeenByKID
	*dest[8].(**string) = r.s.CreatedByAID
	*dest[9].(**time.Time) = r.s.RequestedAt
	*dest[10].(**string) = nil
	return nil
}

// --- harness ---

// newSoulHandler собирает Handler с SoulDB=soulFakePool. soulPool=nil → SoulDB
// остаётся nil (для проверки nil-guard).
func newSoulHandler(t *testing.T, rbacCfg *rbactest.Config, soulPool *soulFakePool) (*Handler, *recordingAudit) {
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
	if soulPool != nil {
		deps.SoulDB = soulPool
	}

	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// soulAdminCfg — конфиг RBAC, дающий archon-alice soul.create + soul.issue-token
// без селекторных ограничений (full grant).
func soulAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "soul-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"soul.create", "soul.issue-token",
			}},
		},
	}
}

// agentSoul / sshSoul — backing-строки для issue-token-флоу (SelectBySID).
func agentSoul() *soul.Soul {
	return &soul.Soul{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending}
}

func sshSoul() *soul.Soul {
	return &soul.Soul{SID: "web-01.example.com", Transport: soul.TransportSSH, Status: soul.StatusPending}
}

// --- tests: manifest ---

func TestSoulTools_InManifest(t *testing.T) {
	want := []string{"keeper.soul.create", "keeper.soul.issue-token"}
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

// --- tests: nil-guard ---

func TestSoulTools_NilGuard(t *testing.T) {
	h, _ := newSoulHandler(t, soulAdminCfg(), nil) // SoulDB == nil
	cases := []struct {
		tool string
		args string
	}{
		{"keeper.soul.create", `{"sid":"web-01.example.com","transport":"agent"}`},
		{"keeper.soul.issue-token", `{"sid":"web-01.example.com"}`},
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

// --- tests: soul.create ---

func TestSoulCreate_Success(t *testing.T) {
	h, rec := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent","covens":["prod"]}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeSoulCreateOutput(t, resp)
	if out.SID != "web-01.example.com" {
		t.Errorf("sid = %q", out.SID)
	}
	if out.Transport != "agent" {
		t.Errorf("transport = %q, want agent", out.Transport)
	}
	if out.Status != string(soul.StatusPending) {
		t.Errorf("status = %q, want pending", out.Status)
	}
	if out.CreatedByAID != "archon-alice" {
		t.Errorf("created_by_aid = %q", out.CreatedByAID)
	}
	// transport=agent → bootstrap-токен сгенерирован и возвращён клиенту.
	if out.BootstrapToken == "" {
		t.Error("bootstrap_token empty for transport=agent")
	}
	// guard: raw wire-ключ срока истечения — `expires_at` (не legacy
	// `token_expires_at`). Типизированный декод маппит по тегу и регрессию
	// переименования тега бы проглотил, поэтому проверяем сырой JSON-объект
	// (вернуть тег `token_expires_at` в soulCreateOutput → красный).
	raw := decodeSoulCreateRaw(t, resp)
	if _, ok := raw["expires_at"]; !ok {
		t.Errorf("raw-ключ expires_at отсутствует в structured output")
	}
	if _, ok := raw["token_expires_at"]; ok {
		t.Errorf("legacy raw-ключ token_expires_at присутствует — переименование откатилось")
	}

	ev := requireSingleAudit(t, rec, string(audit.EventSoulCreated))
	if ev.Payload["sid"] != "web-01.example.com" {
		t.Errorf("audit sid = %v", ev.Payload["sid"])
	}
	if ev.Payload["transport"] != "agent" {
		t.Errorf("audit transport = %v", ev.Payload["transport"])
	}
	if ev.Payload["token_issued"] != true {
		t.Errorf("audit token_issued = %v, want true", ev.Payload["token_issued"])
	}
	assertNoTokenInAudit(t, rec, out.BootstrapToken)
}

func TestSoulCreate_SSHNoToken(t *testing.T) {
	// transport=ssh: только souls-row, без bootstrap-токена (паритет REST
	// issueToken := transport == agent). Audit token_issued=false.
	h, rec := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"ssh"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeSoulCreateOutput(t, resp)
	if out.BootstrapToken != "" {
		t.Errorf("bootstrap_token = %q, want empty for transport=ssh", out.BootstrapToken)
	}
	if out.TokenExpiresAt != "" {
		t.Errorf("token_expires_at = %q, want empty for transport=ssh", out.TokenExpiresAt)
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulCreated))
	if ev.Payload["token_issued"] != false {
		t.Errorf("audit token_issued = %v, want false", ev.Payload["token_issued"])
	}
}

func TestSoulCreate_RBACForbidden(t *testing.T) {
	// archon-alice без soul.*-permissions (пустой RBAC → deny all): вставка не
	// выполняется (Exec/QueryRow паникнули бы на unexpected SQL, но RBAC раньше).
	h, rec := newSoulHandler(t, nil, &soulFakePool{
		soulInsertErr: errFakeUnexpected{sql: "soul.create must NOT insert when RBAC denies"},
	})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("denied create must not write audit")
	}
}

func TestSoulCreate_Duplicate(t *testing.T) {
	// INSERT INTO souls → 23505 → ErrSoulAlreadyExists → soul-already-exists.
	pool := &soulFakePool{soulInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "souls_pkey"}}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeSoulExists {
		t.Errorf("code = %q, want soul-already-exists", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed create must not write audit")
	}
}

func TestSoulCreate_InvalidSID(t *testing.T) {
	h, _ := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"WEB_01","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

func TestSoulCreate_EmptySID(t *testing.T) {
	// sid:"" — отдельная ветка от invalid-pattern: пустое поле ловится первым
	// guard-ом (field 'sid' is required), не доходит до ValidSID.
	h, rec := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed create must not write audit")
	}
}

func TestSoulCreate_InvalidCoven(t *testing.T) {
	// covens с невалидной меткой → ветка ValidCoven в soul_create.go →
	// validation-failed (до RBAC-check и до любой DB-записи).
	h, rec := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent","covens":["BAD COVEN"]}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed create must not write audit")
	}
}

func TestSoulCreate_CreatorAIDNotFound(t *testing.T) {
	// soul.Insert → FK-violation по souls_created_by_aid_fk →
	// ErrSoulCreatorNotFound → mapSoulErrorToMCP → validation-failed (REST-паритет
	// TypeValidationFailed: AID создателя отсутствует в реестре operators).
	pool := &soulFakePool{soulInsertErr: soul.ErrSoulCreatorNotFound}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed create must not write audit")
	}
}

func TestSoulCreate_BeginTxFail(t *testing.T) {
	// BeginTx сорвался → internal-error; запись не начиналась, audit пуст.
	pool := &soulFakePool{beginErr: errors.New("begin tx failed")}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("begin-fail create must not write audit")
	}
}

// TestSoulCreate_CommitFailNoTokenLeak — security-инвариант: при сбое Commit
// транзакция откатывается, и bootstrap-токен (сгенерированный до Commit) НЕ
// возвращается клиенту. Иначе клиент получил бы валидный по виду токен, которого
// нет в БД (или, наоборот, утёкший секрет несуществующей записи).
func TestSoulCreate_CommitFailNoTokenLeak(t *testing.T) {
	pool := &soulFakePool{commitErr: errors.New("commit failed")}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
	// Токен сгенерирован до Commit, но клиенту вернулась ошибка, а не output —
	// у error-ответа нет Result, значит токен не утёк. Проверяем явно.
	if resp.Result != nil {
		t.Fatalf("commit-fail must return error only, got result: %s", resp.Result)
	}
	if len(rec.events) != 0 {
		t.Error("commit-fail create must not write audit")
	}
}

func TestSoulCreate_UnknownArg(t *testing.T) {
	h, _ := newSoulHandler(t, soulAdminCfg(), &soulFakePool{})
	resp := callTool(t, h, "archon-alice", "keeper.soul.create",
		`{"sid":"web-01.example.com","transport":"agent","extra":1}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeMalformedRequest {
		t.Errorf("code = %q, want malformed-request", data.Code)
	}
}

// --- tests: soul.issue-token ---

func TestSoulIssueToken_Success(t *testing.T) {
	pool := &soulFakePool{selectSoul: agentSoul()}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"web-01.example.com"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeSoulIssueTokenOutput(t, resp)
	if out.SID != "web-01.example.com" {
		t.Errorf("sid = %q", out.SID)
	}
	if out.BootstrapToken == "" {
		t.Error("bootstrap_token empty")
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulTokenIssued))
	if ev.Payload["force"] != false {
		t.Errorf("audit force = %v, want false", ev.Payload["force"])
	}
	if ev.Payload["expired_previous"] != false {
		t.Errorf("audit expired_previous = %v, want false", ev.Payload["expired_previous"])
	}
	assertNoTokenInAudit(t, rec, out.BootstrapToken)
	assertNoTokenNamedKey(t, ev)
}

func TestSoulIssueToken_ForceExpiresPrevious(t *testing.T) {
	// force=true + активный токен был → ExpireActiveBySID returns expired=true →
	// audit expired_previous=true, новый токен выписан.
	pool := &soulFakePool{selectSoul: agentSoul(), expireExpired: true}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"web-01.example.com","force":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	out := decodeSoulIssueTokenOutput(t, resp)
	if out.BootstrapToken == "" {
		t.Error("bootstrap_token empty after force-reissue")
	}
	ev := requireSingleAudit(t, rec, string(audit.EventSoulTokenIssued))
	if ev.Payload["force"] != true {
		t.Errorf("audit force = %v, want true", ev.Payload["force"])
	}
	if ev.Payload["expired_previous"] != true {
		t.Errorf("audit expired_previous = %v, want true", ev.Payload["expired_previous"])
	}
	assertNoTokenInAudit(t, rec, out.BootstrapToken)
	assertNoTokenNamedKey(t, ev)
}

func TestSoulIssueToken_ActiveWithoutForce(t *testing.T) {
	// Без force: INSERT bootstrap_tokens → partial-unique → ErrTokenActiveExists
	// → bootstrap-token-active.
	pool := &soulFakePool{
		selectSoul:     agentSoul(),
		tokenInsertErr: &pgconn.PgError{Code: "23505", ConstraintName: "bootstrap_tokens_active_by_sid_idx"},
	}
	h, rec := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"web-01.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeBootstrapTokenActive {
		t.Errorf("code = %q, want bootstrap-token-active", data.Code)
	}
	if len(rec.events) != 0 {
		t.Error("failed issue-token must not write audit")
	}
}

func TestSoulIssueToken_NotFound(t *testing.T) {
	// SelectBySID → ErrSoulNotFound → not-found.
	pool := &soulFakePool{selectErr: soul.ErrSoulNotFound}
	h, _ := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"ghost.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
}

func TestSoulIssueToken_SSHReject(t *testing.T) {
	// transport=ssh → validation-failed (REST TypeValidationFailed: bootstrap
	// tokens only for transport=agent).
	pool := &soulFakePool{selectSoul: sshSoul()}
	h, _ := newSoulHandler(t, soulAdminCfg(), pool)
	resp := callTool(t, h, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"web-01.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Errorf("code = %q, want validation-failed", data.Code)
	}
}

// TestSoulIssueToken_RBACDeniesWithHostSelector — КЛЮЧЕВОЙ security-кейс
// (review): RBAC должен проверяться с селектором {"host": sid}, а не nil.
// Оператор имеет soul.issue-token ТОЛЬКО для host=allowed.example.com; запрос
// на host=denied.example.com должен быть отклонён, а на allowed — пройти.
// Это доказывает, что Check вызывается именно с host-селектором (при nil-
// контексте per-host-grant отклонил бы оба, либо bare-grant пропустил бы оба).
func TestSoulIssueToken_RBACDeniesWithHostSelector(t *testing.T) {
	cfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{
				Name:        "soul-host-op",
				Operators:   []string{"archon-alice"},
				Permissions: []string{"soul.issue-token on host=allowed.example.com"},
			},
		},
	}

	// denied host → forbidden, токен НЕ выписывается.
	deniedPool := &soulFakePool{selectSoul: &soul.Soul{
		SID: "denied.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
	}}
	hDenied, recDenied := newSoulHandler(t, cfg, deniedPool)
	resp := callTool(t, hDenied, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"denied.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden error for denied host")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("denied host code = %q, want forbidden", data.Code)
	}
	if len(recDenied.events) != 0 {
		t.Error("denied issue-token must not write audit")
	}

	// allowed host → success: тот же permission-grant пропускает host-match.
	// Если бы Check звался с nil-контекстом, host-bound grant НЕ матчился бы и
	// этот кейс тоже упал бы в forbidden — он подтверждает host=<sid> селектор.
	allowedPool := &soulFakePool{selectSoul: &soul.Soul{
		SID: "allowed.example.com", Transport: soul.TransportAgent, Status: soul.StatusPending,
	}}
	hAllowed, _ := newSoulHandler(t, cfg, allowedPool)
	resp = callTool(t, hAllowed, "archon-alice", "keeper.soul.issue-token",
		`{"sid":"allowed.example.com"}`)
	if resp.Error != nil {
		t.Fatalf("allowed host: unexpected error: %+v", resp.Error)
	}
}

// --- decode / security helpers ---

func decodeSoulCreateOutput(t *testing.T, resp jsonRPCResponse) soulCreateOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out soulCreateOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

// decodeSoulCreateRaw декодит structured output в нетипизированный map —
// для guard-ов по сырому wire-ключу (типизированный decode маппит по json-тегу
// и переименование тега бы спрятал).
func decodeSoulCreateRaw(t *testing.T, resp jsonRPCResponse) map[string]any {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func decodeSoulIssueTokenOutput(t *testing.T, resp jsonRPCResponse) soulIssueTokenOutput {
	t.Helper()
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out soulIssueTokenOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

// assertNoTokenInAudit — security-инвариант (review): plain bootstrap-токен
// (секрет) НЕ должен попасть в записанный audit-payload ни под одним ключом.
// Сериализуем весь payload в JSON и проверяем отсутствие подстроки токена.
func assertNoTokenInAudit(t *testing.T, rec *recordingAudit, token string) {
	t.Helper()
	if token == "" {
		t.Fatal("assertNoTokenInAudit: token is empty, cannot verify masking")
	}
	for _, ev := range rec.events {
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			t.Fatalf("marshal audit payload: %v", err)
		}
		if strings.Contains(string(raw), token) {
			t.Fatalf("bootstrap token leaked into audit payload: %s", raw)
		}
	}
}

// assertNoTokenNamedKey — issue-token-специфичный инвариант: payload не должен
// нести ни одного ключа с `token`-substring. audit secret-mask (security-fix
// H1) редактирует любой такой ключ в `***MASKED***`, поэтому production-handler
// сознательно выбирает имена без него (`expired_previous` вместо token_id и
// т.п.); тест охраняет это решение от регрессии.
func assertNoTokenNamedKey(t *testing.T, ev *audit.Event) {
	t.Helper()
	for k := range ev.Payload {
		if strings.Contains(k, "token") {
			t.Errorf("issue-token audit payload key %q contains 'token' substring (mask risk)", k)
		}
	}
}
