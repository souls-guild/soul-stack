package api

// Guard tests for DELETE /v1/souls/{sid} on the huma REST surface (NIM-386).
//
// The route destroys rows nobody can get back, and `soul.forgotten` is the ONLY
// durable record that the host, its seeds, its memberships and its Voices ever
// existed — every row the payload describes is gone by the time it is written.
// A route that silently stopped writing that event would still return a
// perfectly correct 200: nothing else in the system would notice. So the audit
// write is asserted on success and its ABSENCE asserted on every rejection.
//
// The 503 path is asserted twice over, because the status code alone is the
// weaker half of the claim: the operator seeing "unavailable" is only safe if
// nothing was deleted. That is invisible from the wire, so the fake pool
// records every statement and the test reads it back.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fake pool: serves the forget transaction and RECORDS what it was asked ---

type fgPool struct {
	status     string // souls.status under FOR UPDATE; "" → pgx.ErrNoRows (404)
	seeds      int64  // rows the soul_seeds UPDATE reports
	tokens     int64  // rows the bootstrap_tokens UPDATE reports
	members    int64
	voices     int64
	statements []string // every SQL that reached the tx, in order
}

func (p *fgPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return &fgTx{p: p}, nil }

func (p *fgPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fgPool.Exec: unexpected")
}
func (p *fgPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return hSoulErrRow{err: errors.New("fgPool.QueryRow: unexpected")}
}
func (p *fgPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fgPool.Query: unexpected")
}

// deleted reports whether the souls row was actually removed.
func (p *fgPool) deleted() bool {
	for _, s := range p.statements {
		if strings.Contains(s, "DELETE FROM souls") {
			return true
		}
	}
	return false
}

type fgTx struct {
	pgx.Tx
	p *fgPool
}

func (t *fgTx) Commit(context.Context) error   { return nil }
func (t *fgTx) Rollback(context.Context) error { return nil }

func (t *fgTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	t.p.statements = append(t.p.statements, sql)
	switch {
	case strings.Contains(sql, "FOR UPDATE"):
		if t.p.status == "" {
			return hSoulErrRow{err: pgx.ErrNoRows}
		}
		return hSoulStaticRow{vals: []any{t.p.status}}
	case strings.Contains(sql, "incarnation_membership"):
		return fgCountRow{n: t.p.members}
	case strings.Contains(sql, "incarnation_choir_voices"):
		return fgCountRow{n: t.p.voices}
	}
	return hSoulErrRow{err: errors.New("fgTx.QueryRow: unexpected SQL: " + sql)}
}

func (t *fgTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	t.p.statements = append(t.p.statements, sql)
	switch {
	case strings.Contains(sql, "soul_seeds"):
		return fgTag("UPDATE", t.p.seeds), nil
	case strings.Contains(sql, "bootstrap_tokens"):
		return fgTag("UPDATE", t.p.tokens), nil
	case strings.Contains(sql, "DELETE FROM souls"):
		return fgTag("DELETE", 1), nil
	}
	return pgconn.CommandTag{}, errors.New("fgTx.Exec: unexpected SQL: " + sql)
}

func fgTag(verb string, n int64) pgconn.CommandTag {
	return pgconn.NewCommandTag(verb + " " + itoa64(n))
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type fgCountRow struct{ n int64 }

func (r fgCountRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*int64); ok {
			*p = r.n
		}
	}
	return nil
}

// --- fake teardown ---

type fgTeardown struct {
	broadcastErr  error
	purgeErr      error
	closed        bool
	broadcasts    int
	purged        int64
	streamPresent bool
}

func (f *fgTeardown) CloseLocal(string) bool {
	f.closed = true
	return f.streamPresent
}

func (f *fgTeardown) Broadcast(context.Context, string) (bool, error) {
	f.broadcasts++
	if f.broadcastErr != nil {
		return false, f.broadcastErr
	}
	return true, nil
}

func (f *fgTeardown) PurgeCache(context.Context, string) (int64, error) {
	if f.purgeErr != nil {
		return 0, f.purgeErr
	}
	return f.purged, nil
}

// forgetRouter mounts DELETE /v1/souls/{sid} exactly as router.go does — the
// scope-aware RequirePermission on `soul.forget` plus the audit-writing huma
// API. Anything less would test the handler, not the route.
func forgetRouter(t *testing.T, enforcer hSoulEnforcer, auditW audit.Writer, soulH *handlers.SoulHandler) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/souls", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "soul", "forget", handlers.SoulSIDSelector)).
				Group(func(r chi.Router) {
					registerHumaSoulForget(newHumaSoulAPI(r, auditW, audit.EventSoulForgotten, nil), soulH)
				})
		})
	})
	return r
}

func forgetDelete(t *testing.T, r *chi.Mux, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/souls/"+sid, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestHumaAudit_SoulForget_RecordsOnSuccess — the S6 guard for the one route
// whose subject no longer exists afterwards. If the audit write is lost, the
// only trace of the host and everything that cascaded with it is lost with it.
func TestHumaAudit_SoulForget_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	// Every count is a DIFFERENT number, deliberately. Five fields carried from
	// the transaction through the reply into the audit payload is five chances
	// to wire one to another's value, and a fixture that says `tokens: 1,
	// voices: 1` cannot tell those two apart — the swapped version passes.
	pool := &fgPool{status: "disconnected", seeds: 2, tokens: 5, members: 3, voices: 7}
	td := &fgTeardown{streamPresent: true, purged: 11}
	h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)

	rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: true}, auditCap, h), "host-1.example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	assertAuditWritten(t, auditCap, audit.EventSoulForgotten, map[string]any{
		"sid":                  "host-1.example.com",
		"status_before":        "disconnected",
		"seeds_revoked":        int64(2),
		"bootstraps_burned":    int64(5),
		"memberships_severed":  int64(3),
		"choir_voices_removed": int64(7),
		"local_stream_closed":  true,
		"broadcast":            true,
		"cache_keys_purged":    int64(11),
	})

	// The counts must be the MEASURED ones, not zeros: an audit trail that
	// records "0 memberships severed" for a host that was on three rosters is
	// worse than no record — it actively misleads the person reading it.
	ev := auditCap.Events()[0]
	if ev.Payload["memberships_severed"] == int64(0) && pool.members != 0 {
		t.Error("the audit event reports 0 memberships severed for a host that was on 3 rosters")
	}
	if _, ok := ev.Payload["warnings"]; !ok {
		t.Error("audit payload has no `warnings` key — an operator reading the trail cannot tell " +
			"'forgotten and released' from 'forgotten, and something is still holding on'")
	}
}

// TestHumaAudit_SoulForget_WarningsSurviveIntoTheAuditTrail — a partly released
// host is not a plain success, and the audit event is the only durable place
// that fact can live. Dropping the warnings here turns "the heartbeat key leaked"
// into something nobody can find afterwards.
func TestHumaAudit_SoulForget_WarningsSurviveIntoTheAuditTrail(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := &fgPool{status: "connected", seeds: 1}
	td := &fgTeardown{streamPresent: true, purgeErr: errors.New("redis went away mid-call")}
	h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)

	rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: true}, auditCap, h), "host-1.example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a failed purge is a warning, not a failed forget", rec.Code)
	}
	if len(auditCap.Events()) == 0 {
		t.Fatal("no audit event for a successful forget")
	}
	warnings, _ := auditCap.Events()[0].Payload["warnings"].([]string)
	if len(warnings) == 0 {
		t.Fatal("the Redis purge failed and the audit event carries no warning — the leaked " +
			"`soul:<sid>:hb` key has no TTL and nothing will ever collect it, and the trail says success")
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if w, ok := body["warnings"].([]any); !ok || len(w) == 0 {
		t.Errorf("200 body warnings = %v, want the unreleased resource named to the caller too", body["warnings"])
	}
}

// TestHumaSoulForget_TeardownUnavailable_503_AndNothingDeleted — the two halves
// of the promise. 503 alone would be satisfied by a route that deleted the row
// and then failed; the second assertion is the one that matters.
func TestHumaSoulForget_TeardownUnavailable_503_AndNothingDeleted(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := &fgPool{status: "connected", seeds: 2, tokens: 1, members: 2, voices: 1}
	td := &fgTeardown{broadcastErr: errors.New("dial redis: connection refused")}
	h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)

	rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: true}, auditCap, h), "host-1.example.com")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — the cluster could not be told, which is 'come back in "+
			"a moment', not a bug (body=%s)", rec.Code, rec.Body.String())
	}
	if pool.deleted() {
		t.Error("the row was DELETED although the teardown notice never went out — the host is " +
			"erased from the registry while its stream on another Keeper instance stays open, " +
			"and the operator was handed an error that says nothing happened")
	}
	if len(pool.statements) != 0 {
		t.Errorf("the transaction ran at all before the pre-flight succeeded: %v", pool.statements)
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("an audit event was written for a forget that did not happen: %+v", auditCap.Events()[0])
	}
	if !strings.Contains(rec.Body.String(), "nothing was deleted") {
		t.Errorf("the 503 body does not tell the operator that nothing was deleted, so they cannot "+
			"tell a safe retry from a partial one: %s", rec.Body.String())
	}

	// The status code is shared with Toll's degraded mode, so 503 on its own
	// does not identify what happened — the `type` URN does, and it is what a
	// client or dashboard branches on.
	var prob struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("body is not problem+json: %v (%s)", err, rec.Body.String())
	}
	if prob.Type != problem.TypeTeardownUnavailable {
		t.Errorf("type = %q, want %q", prob.Type, problem.TypeTeardownUnavailable)
	}
	if prob.Type == problem.TypeClusterDegraded {
		t.Error("a failed Redis publish is reported as Toll's cluster-degraded flag: every client " +
			"branching on the type is told mass Soul churn tripped the cluster, and anyone who " +
			"investigates goes looking for a Toll audit event that was never written")
	}
}

// TestHumaAudit_SoulForget_NoAudit_OnRBACDeny — a refused call is not a forget.
func TestHumaAudit_SoulForget_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := &fgPool{status: "connected"}
	td := &fgTeardown{}
	h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)

	rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: false}, auditCap, h), "host-1.example.com")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written on a 403: %+v", auditCap.Events()[0])
	}
	if td.broadcasts != 0 || len(pool.statements) != 0 {
		t.Errorf("a denied operator still reached the teardown/DB: broadcasts=%d statements=%v",
			td.broadcasts, pool.statements)
	}
}

// TestHumaAudit_SoulForget_NoAudit_OnNotFound — 404 must not leave a
// `soul.forgotten` behind for a host that was never there.
func TestHumaAudit_SoulForget_NoAudit_OnNotFound(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	pool := &fgPool{} // status "" → ErrNoRows
	h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, &fgTeardown{}, nil)

	rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: true}, auditCap, h), "ghost.example.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit written for a host that does not exist: %+v", auditCap.Events()[0])
	}
}

// TestHumaSoulForget_ForgettingIsLegalInEveryState — the design decision the
// user settled: ONE verb, no state gate. A `connected` host is forgotten and its
// stream torn down; a gate would put the operator back where the ticket started,
// with a row only SQL can remove.
func TestHumaSoulForget_ForgettingIsLegalInEveryState(t *testing.T) {
	for _, status := range []string{"pending", "connected", "disconnected", "revoked", "expired", "destroyed"} {
		t.Run(status, func(t *testing.T) {
			auditCap := &auditCaptureWriter{}
			pool := &fgPool{status: status, seeds: 1}
			td := &fgTeardown{streamPresent: true}
			h := handlers.NewSoulHandlerWithTeardown(pool, hSoulScoper{unrestricted: true}, nil, td, nil)

			rec := forgetDelete(t, forgetRouter(t, hSoulEnforcer{allow: true}, auditCap, h), "host-1.example.com")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %q → %d, want 200: a state gate is exactly the dead end NIM-386 "+
					"exists to remove (body=%s)", status, rec.Code, rec.Body.String())
			}
			if !pool.deleted() {
				t.Errorf("status %q: nothing was deleted on a 200", status)
			}
			if !td.closed {
				t.Errorf("status %q: the local stream was never closed — a host forgotten while "+
					"connected keeps talking to a Keeper with no record of it", status)
			}
			if got := auditCap.Events()[0].Payload["status_before"]; got != status {
				t.Errorf("audit status_before = %v, want %q — the trail must say whether the "+
					"operator erased a dead host or a live one", got, status)
			}
		})
	}
}
