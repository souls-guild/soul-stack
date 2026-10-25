package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func activeOperatorPool() *svcPool {
	return &svcPool{selectFn: func(aid string) (*Operator, error) {
		return &Operator{AID: aid, DisplayName: aid, AuthMethod: AuthMethodJWT}, nil
	}}
}

func newServiceTTL(t *testing.T, pool ServicePool, iss JWTIssuer, ttlDefault, ttlMax time.Duration) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{Pool: pool, Issuer: iss, RBAC: &fakeRBAC{}, TTLDefault: ttlDefault, TTLMax: ttlMax})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// IssueToken accounts the token before it is signed: the row carries the id the
// token is signed under, its kind, label, issuer and the minting client.
func TestIssueToken_RecordsTheToken(t *testing.T) {
	pool := activeOperatorPool()
	s := newServiceTTL(t, pool, &fakeIssuer{}, time.Hour, 90*24*time.Hour)

	res, err := s.IssueToken(context.Background(), IssueTokenInput{
		AID: "sd-integration", CallerAID: "archon-alice", TTL: 2160 * time.Hour, Label: "Service Desk prod",
		Client: ClientMeta{IP: "10.1.2.3", UserAgent: "curl/8"},
	})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if !ValidJWTID(res.JWTID) {
		t.Fatalf("JWTID = %q, want a ULID", res.JWTID)
	}
	if res.TTL != 2160*time.Hour {
		t.Errorf("TTL = %s, want the requested 2160h", res.TTL)
	}
	if len(pool.jwtInserts) != 1 {
		t.Fatalf("operator_jwts inserts = %d, want 1", len(pool.jwtInserts))
	}
	row := pool.jwtInserts[0]
	want := []any{res.JWTID, "sd-integration", string(JWTKindIssued), "Service Desk prod"}
	for i, w := range want {
		if row[i] != w {
			t.Errorf("insert arg %d = %v, want %v", i, row[i], w)
		}
	}
	created, expires := row[4].(time.Time), row[5].(time.Time)
	if got := expires.Sub(created); got != 2160*time.Hour {
		t.Errorf("row lifetime = %s, want 2160h", got)
	}
	if !expires.Equal(res.ExpiresAt) {
		t.Errorf("row expires_at %s != reply expires_at %s", expires, res.ExpiresAt)
	}
	if row[6] != "archon-alice" || row[7] != "10.1.2.3" || row[8] != "curl/8" {
		t.Errorf("created_by/ip/ua = %v/%v/%v", row[6], row[7], row[8])
	}
}

func TestIssueToken_TTLBounds(t *testing.T) {
	cases := []struct {
		name    string
		ttl     time.Duration
		wantTTL time.Duration
		wantErr bool
	}{
		{"omitted takes the default", 0, time.Hour, false},
		{"at the ceiling", 90 * 24 * time.Hour, 90 * 24 * time.Hour, false},
		{"above the ceiling", 90*24*time.Hour + time.Second, 0, true},
		{"below the minimum", 30 * time.Second, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := activeOperatorPool()
			iss := &fakeIssuer{}
			s := newServiceTTL(t, pool, iss, time.Hour, 90*24*time.Hour)
			res, err := s.IssueToken(context.Background(), IssueTokenInput{AID: "archon-bob", CallerAID: "archon-alice", TTL: c.ttl})
			if c.wantErr {
				if !errors.Is(err, ErrInvalidTTL) {
					t.Fatalf("err = %v, want ErrInvalidTTL", err)
				}
				if len(pool.jwtInserts) != 0 || iss.calls != 0 {
					t.Errorf("a refused ttl still recorded %d rows and signed %d tokens", len(pool.jwtInserts), iss.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			if res.TTL != c.wantTTL {
				t.Errorf("TTL = %s, want %s", res.TTL, c.wantTTL)
			}
		})
	}
}

// A bad ttl or label refuses create before the operator row is written: a
// refused call must not leave an operator behind without a token.
func TestCreate_BadTokenTermsRefuseBeforeInsert(t *testing.T) {
	for name, in := range map[string]CreateInput{
		"ttl":   {AID: "archon-bob", CallerAID: "archon-alice", TTL: 365 * 24 * time.Hour},
		"label": {AID: "archon-bob", CallerAID: "archon-alice", Label: "line\nbreak"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := &svcPool{}
			s := newServiceTTL(t, pool, &fakeIssuer{}, time.Hour, 90*24*time.Hour)
			if _, err := s.Create(context.Background(), in); err == nil {
				t.Fatal("Create accepted bad token terms")
			}
			if pool.insertCalls != 0 {
				t.Errorf("operator inserted %d times before the token terms were checked", pool.insertCalls)
			}
		})
	}
}

func TestMintJWT_RowBeforeSignature(t *testing.T) {
	t.Run("insert fails: nothing is signed", func(t *testing.T) {
		pool := &svcPool{jwtErr: errors.New("db down")}
		iss := &fakeIssuer{}
		_, err := MintJWT(context.Background(), pool, iss, MintInput{AID: "a1", TTL: time.Hour, Kind: JWTKindSession})
		if err == nil || iss.calls != 0 {
			t.Fatalf("err = %v, signed %d tokens; want an error and no token", err, iss.calls)
		}
	})
	t.Run("signing fails: the row exists, no token does", func(t *testing.T) {
		pool := &svcPool{}
		iss := &fakeIssuer{err: errors.New("sign")}
		if _, err := MintJWT(context.Background(), pool, iss, MintInput{AID: "a1", TTL: time.Hour, Kind: JWTKindSession}); err == nil {
			t.Fatal("MintJWT reported success after the signature failed")
		}
		if len(pool.jwtInserts) != 1 {
			t.Errorf("inserts = %d, want the row written first", len(pool.jwtInserts))
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		if _, err := MintJWT(context.Background(), &svcPool{}, &fakeIssuer{}, MintInput{AID: "a1", TTL: time.Hour, Kind: "personal"}); err == nil {
			t.Fatal("MintJWT accepted a kind the CHECK constraint does not")
		}
	})
}

func TestParseTTL(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 0, "2160h": 2160 * time.Hour, "90d": 90 * 24 * time.Hour, "30m": 30 * time.Minute} {
		got, err := ParseTTL(raw)
		if err != nil || got != want {
			t.Errorf("ParseTTL(%q) = %s, %v; want %s", raw, got, err, want)
		}
	}
	for _, raw := range []string{"soon", "-1h", "0s", "1d2h"} {
		if _, err := ParseTTL(raw); !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("ParseTTL(%q) err = %v, want ErrInvalidTTL", raw, err)
		}
	}
}

func TestValidJWTLabel(t *testing.T) {
	if !ValidJWTLabel("") || !ValidJWTLabel("Service Desk prod · stage") || !ValidJWTLabel(strings.Repeat("я", MaxJWTLabelLen)) {
		t.Error("a valid label was refused")
	}
	for _, bad := range []string{strings.Repeat("x", MaxJWTLabelLen+1), "a\x00b", "a\nb", "\xff"} {
		if ValidJWTLabel(bad) {
			t.Errorf("ValidJWTLabel(%q) = true", bad)
		}
	}
}

func TestNewService_TTLMaxBelowDefaultRefused(t *testing.T) {
	_, err := NewService(ServiceDeps{Pool: &svcPool{}, Issuer: &fakeIssuer{}, RBAC: &fakeRBAC{}, TTLDefault: 24 * time.Hour, TTLMax: time.Hour})
	if err == nil {
		t.Fatal("NewService accepted ttl_max below ttl_default")
	}
}

func TestRevokeJWT_RefusesAMalformedIDWithoutAQuery(t *testing.T) {
	pool := &svcPool{}
	s := newServiceTTL(t, pool, &fakeIssuer{}, time.Hour, time.Hour)
	if _, err := s.RevokeJWT(context.Background(), RevokeJWTInput{AID: "archon-bob", JWTID: "not-a-ulid", CallerAID: "archon-alice"}); !errors.Is(err, ErrJWTNotFound) {
		t.Errorf("err = %v, want ErrJWTNotFound", err)
	}
}

// One write per token per interval, however many requests present it.
func TestJWTUseRecorder_ThrottlesPerToken(t *testing.T) {
	r := NewJWTUseRecorder(&svcPool{}, nil)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	const id = "01JTESTJT1000000000000000A"
	r.RecordUse(id, "10.0.0.1", "ua")
	r.RecordUse(id, "10.0.0.1", "ua")
	r.RecordUse("not-a-ulid", "10.0.0.1", "ua")
	if got := len(r.queue); got != 1 {
		t.Fatalf("queued = %d, want 1 (second use throttled, malformed id ignored)", got)
	}
	now = now.Add(JWTUseInterval)
	r.RecordUse(id, "10.0.0.2", "ua")
	if got := len(r.queue); got != 2 {
		t.Errorf("queued = %d after the interval, want 2", got)
	}
}

type countingExec struct {
	svcPool
	mu      sync.Mutex
	touches int
}

func (c *countingExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE operator_jwts") {
		c.mu.Lock()
		c.touches++
		c.mu.Unlock()
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// Requests present tokens concurrently while Run drains: no race, no lost
// write for a token the throttle let through, one write per token.
func TestJWTUseRecorder_ConcurrentUse(t *testing.T) {
	db := &countingExec{}
	r := NewJWTUseRecorder(db, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	ids := []string{"01JTESTJT1000000000000000A", "01JTESTJT2000000000000000B", "01JTESTJT3000000000000000C"}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.RecordUse(ids[i%len(ids)], "10.0.0.1", "ua")
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		db.mu.Lock()
		n := db.touches
		db.mu.Unlock()
		if n == len(ids) || time.Now().After(deadline) {
			if n != len(ids) {
				t.Errorf("touches = %d, want one per token (%d)", n, len(ids))
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
