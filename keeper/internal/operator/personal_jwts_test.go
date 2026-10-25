package operator

import (
	"errors"
	"testing"
	"time"
)

func TestResolvePersonalTTL(t *testing.T) {
	const day = 24 * time.Hour
	s := newServiceTTL(t, activeOperatorPool(), &fakeIssuer{}, 8*time.Hour, 90*day)
	s.oidcSessionTTL = day

	cases := []struct {
		name    string
		method  AuthMethod
		ttl     time.Duration
		want    time.Duration
		wantErr bool
	}{
		{"omitted takes the default", AuthMethodJWT, 0, 8 * time.Hour, false},
		{"local at ttl_max", AuthMethodJWT, 90 * day, 90 * day, false},
		{"local above ttl_max", AuthMethodJWT, 90*day + time.Second, 0, true},
		{"LDAP gets ttl_max", AuthMethodLDAP, 90 * day, 90 * day, false},
		{"OIDC at the sign-in lifetime", AuthMethodOIDC, day, day, false},
		{"OIDC above the sign-in lifetime", AuthMethodOIDC, day + time.Second, 0, true},
		{"below the minimum", AuthMethodJWT, 30 * time.Second, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.resolvePersonalTTL(c.ttl, c.method, 0)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidTTL) {
					t.Fatalf("err = %v, want ErrInvalidTTL", err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got %s, %v; want %s", got, err, c.want)
			}
		})
	}
}

// An omitted ttl takes the default only while the default fits the operator's
// ceiling; a sign-in lifetime shorter than ttl_default caps it.
func TestResolvePersonalTTL_DefaultCappedAtTheCeiling(t *testing.T) {
	s := newServiceTTL(t, activeOperatorPool(), &fakeIssuer{}, 24*time.Hour, 90*24*time.Hour)
	s.oidcSessionTTL = time.Hour
	if got, err := s.resolvePersonalTTL(0, AuthMethodOIDC, 0); err != nil || got != time.Hour {
		t.Errorf("got %s, %v; want 1h", got, err)
	}
}

// A sign-in lifetime longer than ttl_max does not raise an OIDC operator's
// ceiling above everyone else's.
func TestPersonalTTLMax_NeverAboveTTLMax(t *testing.T) {
	s := newServiceTTL(t, activeOperatorPool(), &fakeIssuer{}, time.Hour, 48*time.Hour)
	s.oidcSessionTTL = 72 * time.Hour
	if got := s.personalTTLMax(AuthMethodOIDC); got != 48*time.Hour {
		t.Errorf("OIDC ceiling = %s, want ttl_max 48h", got)
	}
}

func TestNewService_OIDCSessionTTL(t *testing.T) {
	base := ServiceDeps{Pool: activeOperatorPool(), Issuer: &fakeIssuer{}, RBAC: &fakeRBAC{}, TTLDefault: 2 * time.Hour}
	s, err := NewService(base)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if s.oidcSessionTTL != 2*time.Hour {
		t.Errorf("omitted OIDCSessionTTL = %s, want ttl_default 2h", s.oidcSessionTTL)
	}
	base.OIDCSessionTTL = -time.Second
	if _, err := NewService(base); err == nil {
		t.Error("a negative OIDCSessionTTL was accepted")
	}
}

// A token asked for from an OIDC sign-in is cut to expire with that sign-in;
// a request beyond the sign-in lifetime is still refused rather than cut.
func TestResolvePersonalTTL_CutToTheOIDCSignIn(t *testing.T) {
	s := newServiceTTL(t, activeOperatorPool(), &fakeIssuer{}, 24*time.Hour, 90*24*time.Hour)
	for name, c := range map[string]struct {
		ttl, left, want time.Duration
		wantErr         bool
	}{
		"asked for the sign-in lifetime": {24 * time.Hour, 3 * time.Hour, 3*time.Hour - time.Second, false},
		"omitted":                        {0, 90 * time.Minute, 90*time.Minute - time.Second, false},
		"shorter than what is left":      {time.Hour, 3 * time.Hour, time.Hour, false},
		"beyond the sign-in lifetime":    {24*time.Hour + time.Second, 3 * time.Hour, 0, true},
		"the sign-in is about to end":    {time.Hour, 30 * time.Second, 0, true},
		"a short token, sign-in ending":  {time.Minute, 50 * time.Second, 0, true},
		"no sign-in (another kind)":      {24 * time.Hour, 0, 24 * time.Hour, false},
		"a fraction of a second left":    {24 * time.Hour, 3*time.Hour + 500*time.Millisecond, 3*time.Hour - time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.resolvePersonalTTL(c.ttl, AuthMethodOIDC, c.left)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidTTL) {
					t.Fatalf("err = %v, want ErrInvalidTTL", err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got %s, %v; want %s", got, err, c.want)
			}
		})
	}
}
