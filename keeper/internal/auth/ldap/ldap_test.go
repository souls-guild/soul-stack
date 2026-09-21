package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// fakeConn is a stand-in LDAP connection for unit tests (no real LDAP).
// It records all bind credentials so tests can verify the password is
// NOT logged/leaked and cover bad-bind scenarios.
type fakeConn struct {
	// userEntry is the entry returned by the user search (nil → 0 entries).
	userEntry *ldapv3.Entry
	// groupEntries are the entries returned by the group search.
	groupEntries []*ldapv3.Entry
	// userPassword is the "correct" password for the user bind; re-bind with a different one → fail.
	userPassword string
	// servicePassword is the "correct" password for the service bind.
	servicePassword string

	bindCalls []bindCall
	searches  []*ldapv3.SearchRequest
	closed    bool
}

type bindCall struct{ dn, password string }

func (f *fakeConn) Bind(dn, password string) error {
	f.bindCalls = append(f.bindCalls, bindCall{dn, password})
	// service-account bind
	if f.userEntry != nil && dn == f.userEntry.DN {
		if password != f.userPassword {
			return errors.New("LDAP Result Code 49 \"Invalid Credentials\"")
		}
		return nil
	}
	if password != f.servicePassword {
		return errors.New("LDAP Result Code 49 \"Invalid Credentials\"")
	}
	return nil
}

func (f *fakeConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	f.searches = append(f.searches, req)
	// Group search by member/group_filter — distinguished by whether userDN is present in the filter.
	if f.userEntry != nil && strings.Contains(req.Filter, escapeForTest(f.userEntry.DN)) {
		return &ldapv3.SearchResult{Entries: f.groupEntries}, nil
	}
	if f.userEntry == nil {
		return &ldapv3.SearchResult{}, nil
	}
	return &ldapv3.SearchResult{Entries: []*ldapv3.Entry{f.userEntry}}, nil
}

func (f *fakeConn) Close() error { f.closed = true; return nil }

func escapeForTest(s string) string { return ldapv3.EscapeFilter(s) }

func mkEntry(dn, attr, val string) *ldapv3.Entry {
	return &ldapv3.Entry{DN: dn, Attributes: []*ldapv3.EntryAttribute{{Name: attr, Values: []string{val}}}}
}

// validSearchConfig is a valid search-bind config (ldaps://).
func validSearchConfig() Config {
	return Config{
		URL:          "ldaps://ldap.example.com:636",
		BindMode:     BindModeSearch,
		BindDN:       "cn=svc,dc=example,dc=com",
		BindPassword: "svc-secret",
		BaseDN:       "dc=example,dc=com",
		UserFilter:   "(uid=%s)",
		GroupFilter:  "(member=%s)",
		GroupAttr:    "cn",
		AIDAttr:      "uid",
	}
}

// withFakeDial replaces the authenticator's dial factory to return the given conn.
func withFakeDial(a *Authenticator, fc *fakeConn) {
	a.dial = func(_ context.Context, _ Config, _ *tls.Config) (conn, error) { return fc, nil }
}

// --- (1) TLS-required ---

func TestNew_PlaintextRejected(t *testing.T) {
	cfg := validSearchConfig()
	cfg.URL = "ldap://ldap.example.com:389" // without StartTLS
	if _, err := New(cfg, nil); err == nil {
		t.Fatalf("expected New to reject plaintext ldap:// without start_tls")
	}
}

func TestNew_StartTLSAccepted(t *testing.T) {
	cfg := validSearchConfig()
	cfg.URL = "ldap://ldap.example.com:389"
	cfg.StartTLS = true
	if _, err := New(cfg, nil); err != nil {
		t.Fatalf("ldap:// + start_tls must be accepted, got %v", err)
	}
}

func TestNew_LDAPSStartTLSConflict(t *testing.T) {
	cfg := validSearchConfig()
	cfg.StartTLS = true // ldaps:// + start_tls
	if _, err := New(cfg, nil); err == nil {
		t.Fatalf("expected New to reject ldaps:// + start_tls (mutually exclusive)")
	}
}

func TestNew_SearchRequiresBindCreds(t *testing.T) {
	cfg := validSearchConfig()
	cfg.BindPassword = ""
	if _, err := New(cfg, nil); err == nil {
		t.Fatalf("expected New to require bind_password for search-bind")
	}
}

// --- (2) Password / bind creds do not leak into the error ---

func TestAuthenticate_NoPasswordLeakInError(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fc := &fakeConn{
		userEntry:       mkEntry("uid=alice,dc=example,dc=com", "uid", "alice"),
		userPassword:    "correct-horse",
		servicePassword: "svc-secret",
	}
	withFakeDial(a, fc)

	_, gotErr := a.Authenticate(context.Background(), "alice", "WRONG-PASSWORD")
	if !errors.Is(gotErr, auth.ErrAuthFailed) {
		t.Fatalf("bad password must map to auth.ErrAuthFailed, got %v", gotErr)
	}
	msg := gotErr.Error()
	for _, secret := range []string{"WRONG-PASSWORD", "svc-secret", "correct-horse"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error message leaked secret %q: %q", secret, msg)
		}
	}
}

// --- happy path: search-bind returns an identity with derived AID + groups ---

func TestAuthenticate_HappySearchBind(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fc := &fakeConn{
		userEntry:       mkEntry("uid=alice,dc=example,dc=com", "uid", "alice"),
		groupEntries:    []*ldapv3.Entry{mkEntry("cn=ops,dc=example,dc=com", "cn", "ops")},
		userPassword:    "correct-horse",
		servicePassword: "svc-secret",
	}
	withFakeDial(a, fc)

	ext, err := a.Authenticate(context.Background(), "alice", "correct-horse")
	if err != nil {
		t.Fatalf("Authenticate: unexpected error %v", err)
	}
	if ext.AID != "alice" {
		t.Errorf("AID = %q, want alice (derived from aid_attr=uid)", ext.AID)
	}
	if ext.Subject != "uid=alice,dc=example,dc=com" {
		t.Errorf("Subject = %q, want user-DN", ext.Subject)
	}
	if len(ext.Groups) != 1 || ext.Groups[0] != "ops" {
		t.Errorf("Groups = %v, want [ops]", ext.Groups)
	}
	if !fc.closed {
		t.Errorf("connection must be closed (defer Close)")
	}
}

// --- username is escaped (anti-injection) ---

func TestAuthenticate_UsernameEscapedInFilter(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fc := &fakeConn{
		userEntry:       mkEntry("uid=x,dc=example,dc=com", "uid", "alice"),
		userPassword:    "p",
		servicePassword: "svc-secret",
	}
	withFakeDial(a, fc)

	// injection attempt: `*)(uid=*` must be escaped to a literal.
	_, _ = a.Authenticate(context.Background(), "*)(uid=*", "p")
	if len(fc.searches) == 0 {
		t.Fatalf("expected at least one search")
	}
	filter := fc.searches[0].Filter
	if strings.Contains(filter, "*)(uid=*") {
		t.Fatalf("raw injection payload leaked into filter unescaped: %q", filter)
	}
}

// --- wrong entry count (0 or >1) → ErrAuthFailed ---

func TestAuthenticate_NoUserEntry(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fc := &fakeConn{servicePassword: "svc-secret"} // userEntry=nil → 0 entries
	withFakeDial(a, fc)
	if _, err := a.Authenticate(context.Background(), "ghost", "p"); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("missing user must map to auth.ErrAuthFailed, got %v", err)
	}
}

// --- empty password is rejected before bind (anti unauthenticated-bind) ---

func TestAuthenticate_EmptyPasswordRejected(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fc := &fakeConn{servicePassword: "svc-secret"}
	withFakeDial(a, fc)
	if _, err := a.Authenticate(context.Background(), "alice", ""); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("empty password must be rejected, got %v", err)
	}
	if len(fc.bindCalls) != 0 {
		t.Fatalf("empty password must short-circuit before any bind, got %d binds", len(fc.bindCalls))
	}
}
