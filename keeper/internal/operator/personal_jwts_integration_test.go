//go:build integration

// Integration tests for personal tokens (migration 129, ADR-014 amendment
// NIM-1052) against real Postgres: the CHECK accepts the kind, and
// IssuePersonalJWT's bounds — the credential that may ask, the lifetime ceiling
// and the OIDC sign-in cut-off, the active and the daily counts — hold on the
// real rows, including under concurrent requests for the last free slot.

package operator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// syncIssuer — a concurrency-safe stand-in for jwt.Issuer.
type syncIssuer struct {
	mu    sync.Mutex
	calls int
}

func (s *syncIssuer) Issue(id, aid string, _ []string, _ time.Duration, _ bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return "jwt-" + aid + "-" + id, nil
}

func personalService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Pool: integrationPool, Issuer: &syncIssuer{}, RBAC: &fakeRBAC{},
		TTLDefault: 24 * time.Hour, TTLMax: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func seedOperatorWithMethod(t *testing.T, aid string, m AuthMethod) {
	t.Helper()
	if err := Insert(context.Background(), integrationPool, &Operator{AID: aid, DisplayName: aid, AuthMethod: m, CreatedVia: string(m)}); err != nil {
		t.Fatalf("seed %q: %v", aid, err)
	}
}

// sessionOf mints a sign-in for aid — the credential a person issues personal
// tokens from.
func sessionOf(t *testing.T, aid string) string {
	t.Helper()
	return mintFor(t, aid, JWTKindSession, "", "").JWTID
}

func issuePersonal(s *Service, aid, callerJWTID string, ttl time.Duration, label string) (*IssueTokenResult, error) {
	return s.IssuePersonalJWT(context.Background(), IssuePersonalJWTInput{
		AID: aid, CallerJWTID: callerJWTID, TTL: ttl, Label: label,
		Client: ClientMeta{IP: "10.0.0.9", UserAgent: "itest/personal"},
	})
}

func TestIntegration_PersonalJWT_RowIsTheOwnersOwn(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	s := personalService(t)

	res, err := issuePersonal(s, "alice", sessionOf(t, "alice"), 90*24*time.Hour, "backup script")
	if err != nil {
		t.Fatalf("IssuePersonalJWT: %v", err)
	}
	recs, _, err := ListJWTs(context.Background(), integrationPool, "alice", JWTFilter{Kinds: []JWTKind{JWTKindPersonal}}, 0, 50)
	if err != nil || len(recs) != 1 {
		t.Fatalf("personal rows = %d, err %v; want 1", len(recs), err)
	}
	got := recs[0]
	if got.JWTID != res.JWTID || got.Label != "backup script" || got.CreatedByAID == nil || *got.CreatedByAID != "alice" ||
		got.CreatedIP != "10.0.0.9" || got.ExpiresAt.Sub(got.CreatedAt) != 90*24*time.Hour {
		t.Errorf("personal row = %+v", got)
	}
}

// A personal token cannot mint its successor; a sign-in, an issued token and a
// token minted before accounting can. A `jti` with no row is refused rather
// than read as "not personal".
func TestIntegration_PersonalJWT_WhichCredentialMayAsk(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	seedActiveOperator(t, "bob", nil)
	s := personalService(t)

	first, err := issuePersonal(s, "alice", sessionOf(t, "alice"), time.Hour, "")
	if err != nil {
		t.Fatalf("from a session: %v", err)
	}
	if _, err := issuePersonal(s, "alice", first.JWTID, time.Hour, ""); !errors.Is(err, ErrPersonalJWTFromPersonal) {
		t.Errorf("from a personal token: err = %v, want ErrPersonalJWTFromPersonal", err)
	}
	if _, err := issuePersonal(s, "alice", mintFor(t, "alice", JWTKindIssued, "bob", "").JWTID, time.Hour, ""); err != nil {
		t.Errorf("from an issued token: %v", err)
	}
	if _, err := issuePersonal(s, "alice", "", time.Hour, ""); err != nil {
		t.Errorf("from a token minted before migration 127: %v", err)
	}
	if _, err := issuePersonal(s, "alice", "01JNOR0WF0RTH1S0000000000Z", time.Hour, ""); !errors.Is(err, ErrPersonalJWTFromPersonal) {
		t.Errorf("from an unaccounted jti: err = %v, want ErrPersonalJWTFromPersonal", err)
	}
	if _, err := issuePersonal(s, "alice", sessionOf(t, "bob"), time.Hour, ""); !errors.Is(err, ErrPersonalJWTFromPersonal) {
		t.Errorf("with another operator's jti: err = %v, want ErrPersonalJWTFromPersonal", err)
	}
}

// OIDC groups are reconciled only at sign-in, so an OIDC operator's personal
// token lives no longer than a sign-in; LDAP and local operators get ttl_max.
func TestIntegration_PersonalJWT_Ceiling(t *testing.T) {
	resetOperators(t)
	seedOperatorWithMethod(t, "olga", AuthMethodOIDC)
	seedOperatorWithMethod(t, "lena", AuthMethodLDAP)
	seedActiveOperator(t, "jack", nil)
	s := personalService(t)

	if _, err := issuePersonal(s, "olga", sessionOf(t, "olga"), 24*time.Hour+time.Second, ""); !errors.Is(err, ErrInvalidTTL) {
		t.Errorf("OIDC above the sign-in lifetime: err = %v, want ErrInvalidTTL", err)
	}
	// Asked for the whole sign-in lifetime from a sign-in with an hour left: cut
	// to expire with the sign-in.
	signIn := mintFor(t, "olga", JWTKindSession, "", "")
	res, err := issuePersonal(s, "olga", signIn.JWTID, 24*time.Hour, "")
	if err != nil {
		t.Fatalf("OIDC at the sign-in lifetime: %v", err)
	}
	if res.ExpiresAt.After(signIn.ExpiresAt) || res.TTL > time.Hour {
		t.Errorf("OIDC token expires %s (ttl %s), after its sign-in's %s", res.ExpiresAt, res.TTL, signIn.ExpiresAt)
	}
	// The terms keep the same one-second margin the issue does.
	if terms, err := s.PersonalJWTTerms(context.Background(), "olga", signIn.JWTID); err != nil || terms.ExpiresBy == nil || terms.ExpiresBy.After(signIn.ExpiresAt.Add(-time.Second)) {
		t.Errorf("terms from the sign-in = %+v, %v; want a cut-off at least a second before %s", terms, err, signIn.ExpiresAt)
	}
	// From an issued token there is no sign-in to cut to: the ceiling alone.
	if res, err := issuePersonal(s, "olga", mintFor(t, "olga", JWTKindIssued, "", "").JWTID, 24*time.Hour, ""); err != nil || res.TTL != 24*time.Hour {
		t.Errorf("OIDC from an issued token: %v", err)
	}
	// An LDAP sign-in is not cut: its groups are re-read in the background.
	if res, err := issuePersonal(s, "lena", sessionOf(t, "lena"), 30*24*time.Hour, ""); err != nil || res.TTL != 30*24*time.Hour {
		t.Errorf("LDAP from a sign-in: %v", err)
	}
	for _, aid := range []string{"lena", "jack"} {
		if res, err := issuePersonal(s, aid, sessionOf(t, aid), 90*24*time.Hour, ""); err != nil || res.TTL != 90*24*time.Hour {
			t.Errorf("%s at ttl_max: err %v", aid, err)
		}
		if _, err := issuePersonal(s, aid, sessionOf(t, aid), 90*24*time.Hour+time.Second, ""); !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("%s above ttl_max: err = %v, want ErrInvalidTTL", aid, err)
		}
	}

	for aid, want := range map[string]time.Duration{"olga": 24 * time.Hour, "lena": 90 * 24 * time.Hour, "jack": 90 * 24 * time.Hour} {
		terms, err := s.PersonalJWTTerms(context.Background(), aid, "")
		if err != nil || terms.TTLMax != want || terms.Limit != MaxActivePersonalJWTs || terms.DailyLimit != MaxPersonalJWTsPerDay || terms.ExpiresBy != nil {
			t.Errorf("PersonalJWTTerms(%s) = %+v, %v; want ttl_max %s", aid, terms, err, want)
		}
	}
}

// Revoked and expired personal tokens free their slot; the other kinds never
// take one.
func TestIntegration_PersonalJWT_LimitCountsActiveOnly(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	s := personalService(t)
	ctx := context.Background()
	session := sessionOf(t, "alice")
	mintFor(t, "alice", JWTKindIssued, "", "")

	var ids []string
	for i := range MaxActivePersonalJWTs {
		res, err := issuePersonal(s, "alice", session, time.Hour, fmt.Sprintf("t%d", i))
		if err != nil {
			t.Fatalf("personal #%d: %v", i, err)
		}
		ids = append(ids, res.JWTID)
	}
	if _, err := issuePersonal(s, "alice", session, time.Hour, "one too many"); !errors.Is(err, ErrPersonalJWTLimit) {
		t.Fatalf("at the limit: err = %v, want ErrPersonalJWTLimit", err)
	}

	if _, err := RevokeJWT(ctx, integrationPool, "alice", ids[0], "alice"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operator_jwts SET created_at = NOW() - INTERVAL '2 hours', expires_at = NOW() - INTERVAL '1 hour' WHERE jwt_id = $1`, ids[1]); err != nil {
		t.Fatalf("expire: %v", err)
	}
	for i := range 2 {
		if _, err := issuePersonal(s, "alice", session, time.Hour, ""); err != nil {
			t.Fatalf("after freeing two slots, #%d: %v", i, err)
		}
	}
	if _, err := issuePersonal(s, "alice", session, time.Hour, ""); !errors.Is(err, ErrPersonalJWTLimit) {
		t.Errorf("full again: err = %v, want ErrPersonalJWTLimit", err)
	}
}

// Requests racing for the free slots get exactly as many as are free.
func TestIntegration_PersonalJWT_LimitHoldsUnderConcurrency(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	s := personalService(t)
	session := sessionOf(t, "alice")
	const taken = MaxActivePersonalJWTs - 3
	for range taken {
		if _, err := issuePersonal(s, "alice", session, time.Hour, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	const racers = 12
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		granted int
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := issuePersonal(s, "alice", session, time.Hour, "")
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case !errors.Is(err, ErrPersonalJWTLimit):
				t.Errorf("racer: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != MaxActivePersonalJWTs-taken {
		t.Errorf("granted %d of %d racing requests, want exactly the %d free slots", granted, racers, MaxActivePersonalJWTs-taken)
	}
	if c, err := CountPersonalJWTs(context.Background(), integrationPool, "alice"); err != nil || c.Active != MaxActivePersonalJWTs {
		t.Errorf("active personal tokens = %d (%v), want %d", c.Active, err, MaxActivePersonalJWTs)
	}
}

func TestIntegration_ListJWTs_Filters(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	ctx := context.Background()
	// Apart in time, so newest-first is one order; `issued` is moved two hours
	// back below and becomes the oldest.
	session := mintFor(t, "alice", JWTKindSession, "", "")
	time.Sleep(5 * time.Millisecond)
	issued := mintFor(t, "alice", JWTKindIssued, "", "Service Desk prod")
	time.Sleep(5 * time.Millisecond)
	personal := mintFor(t, "alice", JWTKindPersonal, "alice", "backup SCRIPT 100%")
	time.Sleep(5 * time.Millisecond)
	revoked := mintFor(t, "alice", JWTKindPersonal, "alice", "old laptop")
	if _, err := RevokeJWT(ctx, integrationPool, "alice", revoked.JWTID, "alice"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operator_jwts SET created_at = NOW() - INTERVAL '2 hours', expires_at = NOW() - INTERVAL '1 hour' WHERE jwt_id = $1`, issued.JWTID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	seedActiveOperator(t, "bob", nil)
	mintFor(t, "bob", JWTKindPersonal, "bob", "backup script")

	for name, c := range map[string]struct {
		f    JWTFilter
		want []string
	}{
		"everything":                {JWTFilter{}, []string{revoked.JWTID, personal.JWTID, session.JWTID, issued.JWTID}},
		"personal":                  {JWTFilter{Kinds: []JWTKind{JWTKindPersonal}}, []string{revoked.JWTID, personal.JWTID}},
		"session or issued":         {JWTFilter{Kinds: []JWTKind{JWTKindSession, JWTKindIssued}}, []string{session.JWTID, issued.JWTID}},
		"active":                    {JWTFilter{State: JWTStateActive}, []string{personal.JWTID, session.JWTID}},
		"revoked":                   {JWTFilter{State: JWTStateRevoked}, []string{revoked.JWTID}},
		"expired":                   {JWTFilter{State: JWTStateExpired}, []string{issued.JWTID}},
		"label, any case":           {JWTFilter{Q: "script 100%"}, []string{personal.JWTID}},
		"label wildcard is literal": {JWTFilter{Q: "%"}, []string{personal.JWTID}},
		"combined":                  {JWTFilter{Kinds: []JWTKind{JWTKindPersonal}, State: JWTStateActive, Q: "backup"}, []string{personal.JWTID}},
	} {
		t.Run(name, func(t *testing.T) {
			recs, total, err := ListJWTs(ctx, integrationPool, "alice", c.f, 0, 50)
			if err != nil {
				t.Fatalf("ListJWTs: %v", err)
			}
			got := make([]string, 0, len(recs))
			for _, r := range recs {
				got = append(got, r.JWTID)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) || total != len(c.want) {
				t.Errorf("ids = %v (total %d), want %v", got, total, c.want)
			}
		})
	}
	if _, _, err := ListJWTs(ctx, integrationPool, "alice", JWTFilter{State: "dormant"}, 0, 50); !errors.Is(err, ErrInvalidJWTFilter) {
		t.Errorf("unknown state: err = %v, want ErrInvalidJWTFilter", err)
	}
}

// A revoke frees an active slot but not a daily one: an issue-then-revoke loop
// stops at MaxPersonalJWTsPerDay.
func TestIntegration_PersonalJWT_DailyLimit(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	s := personalService(t)
	ctx := context.Background()
	session := sessionOf(t, "alice")

	for i := range MaxPersonalJWTsPerDay {
		res, err := issuePersonal(s, "alice", session, time.Hour, "")
		if err != nil {
			t.Fatalf("issue #%d: %v", i, err)
		}
		if _, err := RevokeJWT(ctx, integrationPool, "alice", res.JWTID, "alice"); err != nil {
			t.Fatalf("revoke #%d: %v", i, err)
		}
	}
	if _, err := issuePersonal(s, "alice", session, time.Hour, ""); !errors.Is(err, ErrPersonalJWTDailyLimit) {
		t.Fatalf("after %d issued and revoked: err = %v, want ErrPersonalJWTDailyLimit", MaxPersonalJWTsPerDay, err)
	}
	if c, _ := CountPersonalJWTs(ctx, integrationPool, "alice"); c.Active != 0 || c.LastDay != MaxPersonalJWTsPerDay {
		t.Errorf("counts = %+v, want 0 active and %d in the last day", c, MaxPersonalJWTsPerDay)
	}

	// A day later the slots are free again.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE operator_jwts SET created_at = created_at - INTERVAL '25 hours', expires_at = expires_at - INTERVAL '25 hours' WHERE aid = 'alice' AND kind = 'personal'`); err != nil {
		t.Fatalf("age the rows: %v", err)
	}
	if _, err := issuePersonal(s, "alice", session, time.Hour, ""); err != nil {
		t.Errorf("a day later: %v", err)
	}
}

// The Reaper keeps a personal row 24 hours after it was created however short
// its token lived, so the daily count sees it; other kinds keep the
// max_age-only rule.
func TestIntegration_PersonalJWT_PurgeKeepsADay(t *testing.T) {
	resetOperators(t)
	seedActiveOperator(t, "alice", nil)
	ctx := context.Background()
	insert := func(id, kind string, createdAgo time.Duration) {
		t.Helper()
		if _, err := integrationPool.Exec(ctx, `
INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at, revoked_at)
VALUES ($1, 'alice', $2, NOW() - $3::interval, NOW() - $3::interval + INTERVAL '1 minute', NOW() - $3::interval)`,
			id, kind, createdAgo.String()); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// 23h: inside the daily window, so held however long ago it expired.
	insert("01JPERS0NA1Y0VNG0000000000", "personal", 23*time.Hour)
	insert("01JPERS0NA10DD000000000000", "personal", 25*time.Hour)
	insert("01J1SSVEDY0VNG000000000000", "issued", 2*time.Hour)

	var deleted int64
	if err := integrationPool.QueryRow(ctx, `SELECT purge_operator_jwts('1 hour'::interval, 100)`).Scan(&deleted); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var left []string
	rows, err := integrationPool.Query(ctx, `SELECT jwt_id FROM operator_jwts ORDER BY jwt_id`)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		left = append(left, id)
	}
	rows.Close()
	if deleted != 2 || len(left) != 1 || left[0] != "01JPERS0NA1Y0VNG0000000000" {
		t.Errorf("purged %d, left %v; want the 23h-old personal row kept and the other two purged", deleted, left)
	}
}
