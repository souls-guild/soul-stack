package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// scriptedConn answers the user search with a fixed result and the group
// search with fixed groups; it records the filters it was asked.
type scriptedConn struct {
	bindErr   error
	users     []*ldapv3.Entry
	userErr   error
	groups    []*ldapv3.Entry
	filters   []string
	bindCalls []bindCall
}

func (c *scriptedConn) Bind(dn, password string) error {
	c.bindCalls = append(c.bindCalls, bindCall{dn, password})
	return c.bindErr
}

func (c *scriptedConn) Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error) {
	c.filters = append(c.filters, req.Filter)
	if strings.HasPrefix(req.Filter, "(member=") {
		return &ldapv3.SearchResult{Entries: c.groups}, nil
	}
	if c.userErr != nil {
		return nil, c.userErr
	}
	return &ldapv3.SearchResult{Entries: c.users}, nil
}

func (c *scriptedConn) Close() error { return nil }

func openScripted(t *testing.T, c *scriptedConn) (*Directory, error) {
	t.Helper()
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.dial = func(context.Context, Config, *tls.Config) (conn, error) { return c, nil }
	return a.OpenDirectory(context.Background())
}

func TestDirectory_GroupsOfAnExistingUser(t *testing.T) {
	c := &scriptedConn{
		users:  []*ldapv3.Entry{mkEntry("uid=Alice,ou=people,dc=example,dc=com", "uid", "Alice")},
		groups: []*ldapv3.Entry{mkEntry("cn=keeper-ops,dc=example,dc=com", "cn", "keeper-ops")},
	}
	d, err := openScripted(t, c)
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	groups, err := d.Groups("alice")
	if err != nil || len(groups) != 1 || groups[0] != "keeper-ops" {
		t.Fatalf("Groups = %v, %v; want [keeper-ops]", groups, err)
	}
	if len(c.bindCalls) != 1 || c.bindCalls[0].dn != "cn=svc,dc=example,dc=com" {
		t.Errorf("binds = %v, want only the service account — no user password exists here", c.bindCalls)
	}
	if c.filters[0] != "(&(uid=alice)(uid=*))" {
		t.Errorf("user filter = %q, want the aid attribute matched against the AID under user_filter", c.filters[0])
	}
}

// ★ Whatever user_filter excludes a login cannot find, and neither can the
// sync: a disabled AD account is "not found" and loses its group roles,
// instead of being found by its uid and keeping them on every live token.
func TestDirectory_UserFilterConstraintsApply(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.UserFilter = "(&(objectClass=user)(sAMAccountName=%s)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"
	got := a.lookupFilter("alice")
	want := "(&(uid=alice)(&(objectClass=user)(sAMAccountName=*)(!(userAccountControl:1.2.840.113556.1.4.803:=2))))"
	if got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
	if _, err := ldapv3.CompileFilter(got); err != nil {
		t.Errorf("composed filter does not compile: %v", err)
	}
}

// user_filter is filled as a login fills it, through fmt.Sprintf: an indexed
// verb that logs in fine must not reach the directory literally and turn
// every operator into "not found".
func TestDirectory_UserFilterIsFilledLikeALogin(t *testing.T) {
	a, err := New(validSearchConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.UserFilter = "(|(uid=%[1]s)(mail=%[1]s))"
	if got, want := a.lookupFilter("alice"), "(&(uid=alice)(|(uid=*)(mail=*)))"; got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
}

func TestDirectory_AIDIsEscapedInTheFilter(t *testing.T) {
	c := &scriptedConn{}
	d, err := openScripted(t, c)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Groups("a*)(uid=*")
	if c.filters[0] != "(&(uid="+ldapv3.EscapeFilter("a*)(uid=*")+")(uid=*))" {
		t.Errorf("filter = %q, want the AID escaped", c.filters[0])
	}
}

// ★ Only an empty answer is "not found". The sync drops roles on that answer
// and only that one, so every failure must come back as something else.
func TestDirectory_OnlyAnEmptyAnswerIsNotFound(t *testing.T) {
	two := []*ldapv3.Entry{mkEntry("uid=a,dc=x", "uid", "alice"), mkEntry("uid=b,dc=x", "uid", "alice")}
	for name, c := range map[string]*scriptedConn{
		"empty":      {},
		"ambiguous":  {users: two},
		"search err": {userErr: errors.New("LDAP Result Code 32 \"No Such Object\"")},
		"mismatch":   {users: []*ldapv3.Entry{mkEntry("uid=bob,dc=x", "uid", "bob")}},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := openScripted(t, c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.Groups("alice")
			if err == nil {
				t.Fatal("Groups succeeded")
			}
			if got, want := errors.Is(err, ErrUserNotFound), name == "empty"; got != want {
				t.Errorf("errors.Is(%v, ErrUserNotFound) = %v, want %v", err, got, want)
			}
		})
	}
}

func TestDirectory_ServiceBindFailureIsAnError(t *testing.T) {
	_, err := openScripted(t, &scriptedConn{bindErr: errors.New("LDAP Result Code 49 \"Invalid Credentials\"")})
	if err == nil || errors.Is(err, ErrUserNotFound) {
		t.Fatalf("OpenDirectory = %v, want a bind error", err)
	}
	if strings.Contains(err.Error(), "svc-secret") {
		t.Errorf("error leaks the bind password: %v", err)
	}
}
