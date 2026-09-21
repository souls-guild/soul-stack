package mcp

// Guard tests for keeper.soul.forget (NIM-386) — the MCP half of the operator
// path REST reaches with DELETE /v1/souls/{sid}.
//
// The two surfaces share soulforget.Erase, which is what keeps the ORDERING
// from drifting; what they do NOT share is the RBAC check, the audit write and
// the error mapping, and each of those is re-implemented here. A green REST
// suite says nothing about any of them. The catalog count test in
// role_tools_test.go pins 97 tools but not WHICH 97: renaming this tool, or
// swapping it for another, keeps that count exact — so the name is pinned here.

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
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fake pool serving the forget transaction, recording every statement ---

type mcpForgetPool struct {
	status string // "" → ErrNoRows (not found)
	// coven — souls.coven as the per-host RBAC gate reads it before the tool
	// runs (NIM-588). No row (status "") → the coven read says ErrNoRows too.
	coven      []string
	seeds      int64
	tokens     int64
	members    int64
	voices     int64
	statements []string
}

func (p *mcpForgetPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &mcpForgetTx{p: p}, nil
}
func (p *mcpForgetPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("mcpForgetPool.Exec: unexpected")
}
func (p *mcpForgetPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	// soul.CovenBySID — the coven read the RBAC gate does before the forget
	// transaction opens. On the pool, not the tx, so it stays out of
	// `statements`: that list is the forget's own writes and deleted() reads it.
	if strings.Contains(sql, "SELECT coven") && strings.Contains(sql, "FROM souls") {
		if p.status == "" {
			return mcpForgetErrRow{err: pgx.ErrNoRows}
		}
		return mcpForgetCovenRow{coven: p.coven}
	}
	return mcpForgetErrRow{err: errors.New("mcpForgetPool.QueryRow: unexpected")}
}
func (p *mcpForgetPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("mcpForgetPool.Query: unexpected")
}

func (p *mcpForgetPool) deleted() bool {
	for _, s := range p.statements {
		if strings.Contains(s, "DELETE FROM souls") {
			return true
		}
	}
	return false
}

type mcpForgetTx struct {
	pgx.Tx
	p *mcpForgetPool
}

func (t *mcpForgetTx) Commit(context.Context) error   { return nil }
func (t *mcpForgetTx) Rollback(context.Context) error { return nil }

func (t *mcpForgetTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	t.p.statements = append(t.p.statements, sql)
	switch {
	case strings.Contains(sql, "FOR UPDATE"):
		if t.p.status == "" {
			return mcpForgetErrRow{err: pgx.ErrNoRows}
		}
		return mcpForgetStrRow{s: t.p.status}
	case strings.Contains(sql, "incarnation_membership"):
		return mcpForgetCountRow{n: t.p.members}
	case strings.Contains(sql, "incarnation_choir_voices"):
		return mcpForgetCountRow{n: t.p.voices}
	}
	return mcpForgetErrRow{err: errors.New("mcpForgetTx.QueryRow: unexpected SQL: " + sql)}
}

func (t *mcpForgetTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	t.p.statements = append(t.p.statements, sql)
	switch {
	case strings.Contains(sql, "soul_seeds"):
		return mcpForgetTag("UPDATE", t.p.seeds), nil
	case strings.Contains(sql, "bootstrap_tokens"):
		return mcpForgetTag("UPDATE", t.p.tokens), nil
	case strings.Contains(sql, "DELETE FROM souls"):
		return mcpForgetTag("DELETE", 1), nil
	}
	return pgconn.CommandTag{}, errors.New("mcpForgetTx.Exec: unexpected SQL: " + sql)
}

func mcpForgetTag(verb string, n int64) pgconn.CommandTag {
	digits := "0"
	if n > 0 {
		digits = ""
		for v := n; v > 0; v /= 10 {
			digits = string(rune('0'+v%10)) + digits
		}
	}
	return pgconn.NewCommandTag(verb + " " + digits)
}

type mcpForgetErrRow struct{ err error }

func (r mcpForgetErrRow) Scan(...any) error { return r.err }

type mcpForgetStrRow struct{ s string }

func (r mcpForgetStrRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*string); ok {
			*p = r.s
		}
	}
	return nil
}

type mcpForgetCovenRow struct{ coven []string }

func (r mcpForgetCovenRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("mcpForgetCovenRow.Scan: want exactly one dest (coven)")
	}
	p, ok := dest[0].(*[]string)
	if !ok {
		return errors.New("mcpForgetCovenRow.Scan: dest is not *[]string")
	}
	*p = r.coven
	return nil
}

type mcpForgetCountRow struct{ n int64 }

func (r mcpForgetCountRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*int64); ok {
			*p = r.n
		}
	}
	return nil
}

// --- fake teardown ---

type mcpForgetTeardown struct {
	broadcastErr error
	purgeErr     error
	closed       bool
	broadcasts   int
	purged       int64
	streamOpen   bool
}

func (f *mcpForgetTeardown) CloseLocal(string) bool {
	f.closed = true
	return f.streamOpen
}
func (f *mcpForgetTeardown) Broadcast(context.Context, string) (bool, error) {
	f.broadcasts++
	if f.broadcastErr != nil {
		return false, f.broadcastErr
	}
	return true, nil
}
func (f *mcpForgetTeardown) PurgeCache(context.Context, string) (int64, error) {
	if f.purgeErr != nil {
		return 0, f.purgeErr
	}
	return f.purged, nil
}

func forgetterRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "forgetter", Operators: []string{"archon-alice"}, Permissions: []string{"soul.forget"}},
		},
	}
}

// newForgetHandler wires the tool the way the daemon does: SoulDB + SoulTeardown.
func newForgetHandler(t *testing.T, cfg *rbactest.Config, pool *mcpForgetPool, td soulforget.Teardown) (*Handler, *recordingAudit) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB: &fakePool{},
	}
	if pool != nil {
		deps.SoulDB = pool
	}
	deps.SoulTeardown = td
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// TestCatalog_SoulForgetIsPresentByName — the total-count guard is blind to
// substitution: drop this tool and add any other and 97 still holds. The
// operator path the ticket exists to create is a NAME, so the name is what gets
// pinned.
func TestCatalog_SoulForgetIsPresentByName(t *testing.T) {
	const name = "keeper.soul.forget"
	found := false
	for _, d := range listAllTools() {
		if d.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s missing from the tool catalog — the MCP half of the operator path is gone "+
			"while the total count stays exact", name)
	}
	e, ok := toolByName(name)
	if !ok {
		t.Fatalf("%s not declared in catalogManifest", name)
	}
	if e.status != toolStatusImplemented {
		t.Errorf("%s status = %d, want Implemented — a declared-but-stub destructive tool is worse "+
			"than an absent one", name, e.status)
	}
	if e.decl.OutputSchema == nil {
		t.Error("no outputSchema: a client validating the result against the declared schema drops " +
			"every key it does not name, and `warnings` is exactly the key that must survive")
	}
}

// TestSoulForget_Success_WritesAuditAndReleases — the S6 guard for MCP. After
// this call every row the payload names is gone, so `soul.forgotten` is the only
// durable record that the host existed at all.
func TestSoulForget_Success_WritesAuditAndReleases(t *testing.T) {
	// Pairwise-distinct counts on purpose: with `tokens: 1, voices: 1` a reply
	// that reported the Voice count as the bootstrap count would pass, and the
	// two travel side by side all the way from the transaction to the audit row.
	pool := &mcpForgetPool{status: "disconnected", seeds: 2, tokens: 5, members: 3, voices: 7}
	td := &mcpForgetTeardown{streamOpen: true, purged: 11}
	h, rec := newForgetHandler(t, forgetterRBAC(), pool, td)

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out soulForgetOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SID != "host-1.example.com" || out.StatusBefore != "disconnected" {
		t.Errorf("out = %+v, want the SID and the pre-delete status", out)
	}
	if out.SeedsRevoked != 2 || out.BootstrapsBurned != 5 || out.MembershipsSevered != 3 || out.ChoirVoicesRemoved != 7 {
		t.Errorf("counts = %+v, want the measured cascade (2/5/3/7) — an agent told '0 memberships "+
			"severed' for a host on 3 rosters will report a clean removal that emptied three of them", out)
	}
	if !out.LocalStreamClosed || !out.Broadcast || out.CacheKeysPurged != 11 {
		t.Errorf("release = closed:%v broadcast:%v purged:%d, want the real teardown outcome",
			out.LocalStreamClosed, out.Broadcast, out.CacheKeysPurged)
	}
	if out.Warnings == nil {
		t.Error("warnings is null, not [] — an agent cannot tell 'nothing was left holding on' " +
			"from 'the field is missing, probably fine'")
	}
	if !pool.deleted() {
		t.Error("the tool answered success without deleting the row")
	}

	ev := recEvent(rec, audit.EventSoulForgotten)
	if ev == nil {
		t.Fatal("no soul.forgotten audit event — after this call nothing else records that the " +
			"host, its seeds, its memberships and its Voices ever existed")
	}
	if ev.Source != audit.SourceMCP {
		t.Errorf("source = %q, want mcp", ev.Source)
	}
	if ev.Payload["sid"] != "host-1.example.com" || ev.Payload["status_before"] != "disconnected" {
		t.Errorf("payload = %+v, want the SID and the pre-delete status", ev.Payload)
	}
	if ev.Payload["memberships_severed"] != int64(3) || ev.Payload["choir_voices_removed"] != int64(7) {
		t.Errorf("payload cascade counts = %+v, want 3 memberships and 7 Voices", ev.Payload)
	}
}

// TestSoulForget_AuditPayload_SurvivesTheSecretMask — the same guard the REST
// surface carries (handlers/soul_forget_audit_test.go), applied to what this
// tool actually hands the audit writer.
//
// [audit.MaskSecrets] runs over every row before it is stored and matches key
// names by SUBSTRING: a key containing `token` loses its value to
// [audit.MaskedValue] regardless of type. Here that would delete the number
// permanently rather than redact a secret — the rows the payload describes are
// already gone, so there is nothing left to read it back from. The parity that
// matters is not between the two surfaces' payload builders but between what
// the tool answers and what survives in the trail.
func TestSoulForget_AuditPayload_SurvivesTheSecretMask(t *testing.T) {
	pool := &mcpForgetPool{status: "disconnected", seeds: 2, tokens: 1, members: 3, voices: 1}
	h, rec := newForgetHandler(t, forgetterRBAC(), pool, &mcpForgetTeardown{streamOpen: true, purged: 3})

	if resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`); resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	ev := recEvent(rec, audit.EventSoulForgotten)
	if ev == nil {
		t.Fatal("no soul.forgotten audit event")
	}

	masked := audit.MaskSecrets(ev.Payload)
	for k, want := range ev.Payload {
		if got := masked[k]; got == audit.MaskedValue && want != audit.MaskedValue {
			t.Errorf("payload key %q is stored as %q — the tool reported %#v and the audit trail "+
				"keeps nothing; rename the key so it carries no substring the mask matches",
				k, audit.MaskedValue, want)
		}
	}
}

// TestSoulForget_TeardownUnavailable_NothingDeleted — the safety property: if
// the cluster cannot be told, the operation must stop BEFORE the transaction.
// The failure mode this guards is a tool that deletes the row and then reports
// an error, leaving a forgotten host still streaming to another instance.
func TestSoulForget_TeardownUnavailable_NothingDeleted(t *testing.T) {
	pool := &mcpForgetPool{status: "connected", seeds: 2}
	td := &mcpForgetTeardown{broadcastErr: errors.New("dial redis: connection refused")}
	h, rec := newForgetHandler(t, forgetterRBAC(), pool, td)

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
	if resp.Error == nil {
		t.Fatal("the tool reported success although the teardown notice never went out")
	}
	if pool.deleted() {
		t.Error("the row was DELETED with the cluster unreachable — the host is erased from the " +
			"registry while its stream on another Keeper instance stays open")
	}
	if len(pool.statements) != 0 {
		t.Errorf("the transaction ran before the pre-flight succeeded: %v", pool.statements)
	}
	if recEvent(rec, audit.EventSoulForgotten) != nil {
		t.Error("an audit event was written for a forget that did not happen")
	}

	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeTeardownUnavailable {
		t.Errorf("code = %q, want %q", data.Code, mcpCodeTeardownUnavailable)
	}
	// The distinction is the point, not the spelling. `internal-error` is what a
	// broken query returns — a defect the caller cannot act on. This outcome
	// changed nothing and is retryable, and an agent that reads the same code
	// for both either retries a real bug forever or abandons a host it could
	// have forgotten a second later. Machine-readable, not message-readable.
	if data.Code == mcpCodeInternalError {
		t.Error("a retryable no-op is reported with the same code as an internal defect")
	}
	if !strings.Contains(resp.Error.Message, "nothing was deleted") {
		t.Errorf("the error does not say nothing was deleted, so an agent cannot tell a safe retry "+
			"from a partial one: %q", resp.Error.Message)
	}
}

// TestSoulForget_PurgeFailureIsWarnedNotSwallowed — a leaked `soul:<sid>:hb` has
// NO TTL and no rule collects it once the row is gone. Reporting a plain success
// makes it permanently invisible.
func TestSoulForget_PurgeFailureIsWarnedNotSwallowed(t *testing.T) {
	pool := &mcpForgetPool{status: "disconnected", seeds: 1}
	td := &mcpForgetTeardown{streamOpen: true, purgeErr: errors.New("redis went away mid-call")}
	h, rec := newForgetHandler(t, forgetterRBAC(), pool, td)

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
	if resp.Error != nil {
		t.Fatalf("a failed purge must not fail the forget: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out soulForgetOutput
	_ = json.Unmarshal(res.StructuredContent, &out)
	if len(out.Warnings) == 0 {
		t.Fatal("the Redis purge failed and the tool reported a clean success — the leaked key has " +
			"no TTL and nothing will ever collect it")
	}
	ev := recEvent(rec, audit.EventSoulForgotten)
	if ev == nil {
		t.Fatal("no audit event")
	}
	w, _ := ev.Payload["warnings"].([]string)
	if len(w) == 0 {
		t.Error("the warning reached the caller but not the audit trail, which is the only record " +
			"that survives the call")
	}
}

// TestSoulForget_RBACForbidden — `soul.forget` is its own grant. An operator who
// may onboard hosts must not be able to erase them, and a denied call must not
// touch the DB, the teardown, or the audit trail.
func TestSoulForget_RBACForbidden(t *testing.T) {
	creatorOnly := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "onboarder", Operators: []string{"archon-alice"}, Permissions: []string{"soul.create"}},
		},
	}
	pool := &mcpForgetPool{status: "connected"}
	td := &mcpForgetTeardown{}
	h, rec := newForgetHandler(t, creatorOnly, pool, td)

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
	if resp.Error == nil {
		t.Fatal("an operator holding only soul.create forgot a host")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
	if len(pool.statements) != 0 || td.broadcasts != 0 {
		t.Errorf("a denied call reached the DB/teardown: statements=%v broadcasts=%d",
			pool.statements, td.broadcasts)
	}
	if recEvent(rec, audit.EventSoulForgotten) != nil {
		t.Error("audit written on a forbidden call")
	}
}

// TestSoulForget_CovenGrantNarrowsHere_Too — the ADR-004 parity claim for
// NIM-588: `soul.forget on coven=<label>` narrows over MCP exactly as it does
// over DELETE /v1/souls/{sid}.
//
// This is not covered by the REST guards in the api package. The two surfaces
// share soulforget.Erase but NOT the RBAC check — REST resolves the host's
// covens in a router selector, MCP in checkSoulHostScope — so a fix applied to
// one leaves the other exactly as broken as before, and every test on the fixed
// side stays green about it. ADR-004 makes both primary operator interfaces:
// "narrows over OpenAPI, denies over MCP" is not a partial fix, it is a bug for
// whoever uses MCP.
//
// Both directions in one test on purpose. Narrowing that admitted every host
// would satisfy the admit half alone, and the old host-only behaviour would
// satisfy the refuse half alone; only the pair distinguishes a grant that is
// compared from one that is merely fetched or ignored.
func TestSoulForget_CovenGrantNarrowsHere_Too(t *testing.T) {
	for _, form := range covenScopeForms() {
		t.Run(form.name, func(t *testing.T) {
			forgetCovenGrantNarrows(t, form.cfg("soul.forget"))
		})
	}
}

// forgetCovenGrantNarrows is the body of TestSoulForget_CovenGrantNarrowsHere_Too,
// run once per way of expressing the scope (NIM-650): the `on coven=web` suffix
// and the role `default_scope`. The suffix form was all this fixture could
// express until rbactest.Role grew DefaultScope, so the role-default route to
// the same check went untested here.
func forgetCovenGrantNarrows(t *testing.T, webOnly *rbactest.Config) {
	t.Helper()

	t.Run("host in the granted coven is forgotten", func(t *testing.T) {
		pool := &mcpForgetPool{status: "connected", coven: []string{"web"}, seeds: 1}
		h, rec := newForgetHandler(t, webOnly, pool, &mcpForgetTeardown{})

		resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
		if resp.Error != nil {
			t.Fatalf("`soul.forget on coven=web` was refused for a host IN coven web — the MCP surface "+
				"still reads a coven grant as a denial: %+v", resp.Error)
		}
		if !pool.deleted() {
			t.Error("success without a DELETE FROM souls")
		}
		if recEvent(rec, audit.EventSoulForgotten) == nil {
			t.Error("a host was forgotten without `soul.forgotten` — the only record it ever existed")
		}
	})

	t.Run("host in another coven is refused", func(t *testing.T) {
		pool := &mcpForgetPool{status: "connected", coven: []string{"prod"}, seeds: 1}
		td := &mcpForgetTeardown{}
		h, rec := newForgetHandler(t, webOnly, pool, td)

		resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
		if resp.Error == nil {
			t.Fatal("`soul.forget on coven=web` erased a host in coven prod")
		}
		if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
			t.Errorf("code = %q, want forbidden", data.Code)
		}
		if pool.deleted() || td.broadcasts != 0 {
			t.Errorf("a refused call still reached the DB/teardown: statements=%v broadcasts=%d",
				pool.statements, td.broadcasts)
		}
		if recEvent(rec, audit.EventSoulForgotten) != nil {
			t.Error("audit written on a forbidden call")
		}
	})
}

// TestSoulForget_NotFound — 404-equivalent, and no audit for a host that was
// never there.
func TestSoulForget_NotFound(t *testing.T) {
	pool := &mcpForgetPool{} // status "" → ErrNoRows
	h, rec := newForgetHandler(t, forgetterRBAC(), pool, &mcpForgetTeardown{})

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"ghost.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected not-found")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("code = %q, want not-found", data.Code)
	}
	if recEvent(rec, audit.EventSoulForgotten) != nil {
		t.Error("audit written for a host that does not exist")
	}
}

// TestSoulForget_Validation — no `force` argument exists, so a client that
// believes in one must be told, not silently given the plain behaviour under a
// flag that was ignored.
func TestSoulForget_Validation(t *testing.T) {
	cases := []struct {
		name string
		args string
		code string
	}{
		{"missing sid", `{}`, mcpCodeValidationFailed},
		{"empty sid", `{"sid":""}`, mcpCodeValidationFailed},
		{"malformed sid", `{"sid":"not a host"}`, mcpCodeValidationFailed},
		{"unknown force flag", `{"sid":"host-1.example.com","force":true}`, mcpCodeMalformedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := &mcpForgetPool{status: "connected"}
			h, rec := newForgetHandler(t, forgetterRBAC(), pool, &mcpForgetTeardown{})
			resp := callTool(t, h, "archon-alice", "keeper.soul.forget", tc.args)
			if resp.Error == nil {
				t.Fatal("expected a rejection")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.code {
				t.Errorf("code = %q, want %q", data.Code, tc.code)
			}
			if pool.deleted() {
				t.Error("a rejected call still deleted the row")
			}
			if recEvent(rec, audit.EventSoulForgotten) != nil {
				t.Error("audit written on a rejected call")
			}
		})
	}
}

// TestSoulForget_NilSoulDB — a Keeper without the soul DB wired must refuse,
// not panic and not report a forget it could not have performed.
func TestSoulForget_NilSoulDB(t *testing.T) {
	h, rec := newForgetHandler(t, forgetterRBAC(), nil, &mcpForgetTeardown{})

	resp := callTool(t, h, "archon-alice", "keeper.soul.forget", `{"sid":"host-1.example.com"}`)
	if resp.Error == nil {
		t.Fatal("expected an error with no soul DB configured")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error", data.Code)
	}
	if recEvent(rec, audit.EventSoulForgotten) != nil {
		t.Error("audit written with no DB to forget anything in")
	}
}
