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
// Narrow fake for [handlers.SoulPool]: handles exactly the SQL queries the
// soul-tools need (Insert souls / Insert bootstrap_tokens / SelectBySID /
// Expire active). Business invariants of the CRUD layer (souls/bootstraptoken)
// are covered by that package's integration tests; this checks TRANSPORT —
// that the tool calls CRUD correctly, maps errors, and encodes output
// (symmetric to roleFakePool).

type soulFakePool struct {
	// soulInsertErr — error returned by INSERT INTO souls (via RETURNING
	// scan). Lets tests inject soul.* sentinels (duplicate / creator-not-found)
	// or a raw pg error.
	soulInsertErr error

	// selectSoul — what SELECT … FROM souls WHERE sid returns (issue-token
	// flow). selectErr takes priority: nil-soul + selectErr → not-found / other.
	selectSoul *soul.Soul
	selectErr  error

	// tokenInsertErr — error from INSERT INTO bootstrap_tokens (RETURNING
	// scan). bootstraptoken.ErrTokenActiveExists → an active token without force.
	tokenInsertErr error

	// expireExpired — what UPDATE bootstrap_tokens … RETURNING token_id
	// (ExpireActiveBySID) returns: true → an active token existed and was
	// invalidated (expired_previous=true); false → none was active
	// (pgx.ErrNoRows branch).
	expireExpired bool

	// beginErr — BeginTx error (transaction failed to open before any writes).
	beginErr error

	// commitErr — Commit error (write succeeded, commit failed). Propagated
	// to soulFakeTx.Commit when the transaction is created.
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
		// ExpireActiveBySID: row exists → expired; no row → ErrNoRows.
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

// soulSelectRow — pgx.Row for SELECT … FROM souls WHERE sid (scanSoul reads
// 11 columns: sid, transport, status, coven, traits, registered_at,
// last_seen_at, last_seen_by_kid, created_by_aid, requested_at, note).
type soulSelectRow struct{ s *soul.Soul }

func (r soulSelectRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.s.SID
	*dest[1].(*string) = string(r.s.Transport)
	*dest[2].(*string) = string(r.s.Status)
	*dest[3].(*[]string) = r.s.Coven
	// traits jsonb (ADR-060): nil Traits → nil bytes (scanSoul → empty map).
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

// newSoulHandler builds a Handler with SoulDB=soulFakePool. soulPool=nil →
// SoulDB stays nil (for nil-guard tests).
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

// soulAdminCfg — RBAC config granting archon-alice soul.create +
// soul.issue-token with no selector restrictions (full grant).
func soulAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "soul-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"soul.create", "soul.issue-token",
			}},
		},
	}
}

// agentSoul / sshSoul — backing rows for the issue-token flow (SelectBySID).
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
	// transport=agent → bootstrap token generated and returned to the client.
	if out.BootstrapToken == "" {
		t.Error("bootstrap_token empty for transport=agent")
	}
	// guard: the raw wire key for expiry is `expires_at` (not the legacy
	// `token_expires_at`). Typed decode maps by tag and would silently
	// swallow a tag-rename regression, so check the raw JSON object instead
	// (bringing back the `token_expires_at` tag in soulCreateOutput → red).
	raw := decodeSoulCreateRaw(t, resp)
	if _, ok := raw["expires_at"]; !ok {
		t.Errorf("raw key expires_at missing from structured output")
	}
	if _, ok := raw["token_expires_at"]; ok {
		t.Errorf("legacy raw key token_expires_at present - rename rolled back")
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
	// transport=ssh: souls row only, no bootstrap token (REST parity:
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
	// archon-alice has no soul.* permissions (empty RBAC → deny all): the
	// insert never runs (Exec/QueryRow would panic on unexpected SQL, but
	// RBAC fires first).
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
	// sid:"" — separate branch from invalid-pattern: the empty field is
	// caught by the first guard (field 'sid' is required), never reaches
	// ValidSID.
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
	// covens with an invalid label → the ValidCoven branch in soul_create.go
	// → validation-failed (before the RBAC check and before any DB write).
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
	// soul.Insert → FK violation on souls_created_by_aid_fk →
	// ErrSoulCreatorNotFound → mapSoulErrorToMCP → validation-failed (REST
	// parity TypeValidationFailed: creator AID missing from the operators
	// registry).
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
	// BeginTx fails → internal-error; write never started, audit empty.
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

// TestSoulCreate_CommitFailNoTokenLeak — security invariant: if Commit fails,
// the transaction rolls back, and the bootstrap token (generated before
// Commit) is NOT returned to the client. Otherwise the client would get a
// token that looks valid but isn't in the DB — a leaked secret for a
// nonexistent record.
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
	// Token is generated before Commit, but the client got an error
	// response, not output — an error response has no Result, so the token
	// didn't leak. Check explicitly.
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
	// force=true + an active token existed → ExpireActiveBySID returns
	// expired=true → audit expired_previous=true, new token issued.
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
	// Without force: INSERT bootstrap_tokens → partial-unique →
	// ErrTokenActiveExists → bootstrap-token-active.
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

// TestSoulIssueToken_RBACDeniesWithHostSelector — KEY security case (review):
// RBAC must be checked with selector {"host": sid}, not nil. The operator
// has soul.issue-token ONLY for host=allowed.example.com; a request for
// host=denied.example.com must be denied, and one for allowed must pass.
// This proves Check is called with the host selector (with a nil context, a
// per-host grant would deny both, or a bare grant would allow both).
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

	// denied host → forbidden, token NOT issued.
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

	// allowed host → success: the same permission grant passes the host
	// match. If Check were called with a nil context, the host-bound grant
	// wouldn't match and this case would also fall into forbidden — it
	// confirms the host=<sid> selector.
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

// decodeSoulCreateRaw decodes structured output into an untyped map — for
// guards on the raw wire key (typed decode maps by json tag and would hide
// a tag rename).
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

// assertNoTokenInAudit — security invariant (review): the plain bootstrap
// token (a secret) must NOT end up in the recorded audit payload under any
// key. Serialize the whole payload to JSON and check the token substring is
// absent.
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

// assertNoTokenNamedKey — issue-token-specific invariant: the payload must
// not carry any key with a `token` substring. The audit secret-mask
// (security fix H1) redacts any such key to `***MASKED***`, so the
// production handler deliberately picks names without it
// (`expired_previous` instead of token_id, etc.); this test guards that
// decision against regression.
func assertNoTokenNamedKey(t *testing.T, ev *audit.Event) {
	t.Helper()
	for k := range ev.Payload {
		if strings.Contains(k, "token") {
			t.Errorf("issue-token audit payload key %q contains 'token' substring (mask risk)", k)
		}
	}
}
