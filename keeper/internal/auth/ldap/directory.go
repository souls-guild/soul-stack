package ldap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// ErrUserNotFound — the directory answered the lookup and holds no entry for
// the AID. Distinct from every other failure on purpose: the background group
// sync drops an operator's group roles on this answer and only on this one, so
// an outage must never read as "not found" (ADR-058, NIM-1053).
var ErrUserNotFound = errors.New("auth/ldap: no directory entry for aid")

// Directory is one service-bound connection for reading the groups of
// operators who are not logging in, so without their passwords — the
// background group sync (ADR-058, NIM-1053). Not safe for concurrent use.
type Directory struct {
	a *Authenticator
	c conn
}

// OpenDirectory dials and binds the service account. Errors are returned as
// they are, not sanitized: there is no login to answer and no oracle to deny,
// and the operator reading the sync status needs the reason.
func (a *Authenticator) OpenDirectory(ctx context.Context) (*Directory, error) {
	c, err := a.dial(ctx, a.cfg, a.tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("auth/ldap: dial: %w", err)
	}
	if err := c.Bind(a.cfg.BindDN, a.cfg.BindPassword); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("auth/ldap: service bind: %w", err)
	}
	return &Directory{a: a, c: c}, nil
}

// Groups returns the groups of the entry whose aid attribute equals aid — the
// inverse of how a login derives the AID — read with the group search a login
// runs, though bound as the service account rather than as the user.
// [ErrUserNotFound] when the search succeeds with no entry; more than one
// entry, or one whose attribute does not fold back to aid, is an error rather
// than a guess.
//
// The search is ANDed with user_filter, its %s widened to a presence test, so
// every constraint a login applies — a disabled account excluded, an
// objectClass — applies here too: an entry a login would refuse is "not found",
// and offboarding by disabling an account takes the group roles with it.
func (d *Directory) Groups(aid string) ([]string, error) {
	attr := d.a.aidAttr()
	res, err := d.c.Search(&ldapv3.SearchRequest{
		BaseDN:       d.a.cfg.BaseDN,
		Scope:        ldapv3.ScopeWholeSubtree,
		DerefAliases: ldapv3.NeverDerefAliases,
		SizeLimit:    2,
		TimeLimit:    d.a.timeoutSeconds(),
		Filter:       d.a.lookupFilter(aid),
		Attributes:   []string{attr},
	})
	if err != nil {
		return nil, fmt.Errorf("auth/ldap: user search: %w", err)
	}
	switch n := len(res.Entries); {
	case n == 0:
		return nil, ErrUserNotFound
	case n > 1:
		return nil, fmt.Errorf("auth/ldap: user search: %d entries carry %s=%q", n, attr, aid)
	}
	entry := res.Entries[0]
	if got := strings.ToLower(entry.GetAttributeValue(attr)); got != aid {
		return nil, fmt.Errorf("auth/ldap: user search: entry %q carries %s=%q, not %q", entry.DN, attr, got, aid)
	}
	groups, err := d.a.searchGroups(d.c, entry.DN)
	if err != nil {
		return nil, fmt.Errorf("auth/ldap: group search: %w", err)
	}
	return groups, nil
}

// Close releases the connection.
func (d *Directory) Close() error { return d.c.Close() }

// lookupFilter finds an entry by its AID under the login's own constraints.
// user_filter is filled the way a login fills it — fmt.Sprintf, so `%[1]s`
// and `%%` mean what they mean there — with `*` for the username.
func (a *Authenticator) lookupFilter(aid string) string {
	return fmt.Sprintf("(&(%s=%s)%s)", a.aidAttr(), ldapv3.EscapeFilter(aid), fmt.Sprintf(a.cfg.UserFilter, "*"))
}
