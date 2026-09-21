// Package ldap — LDAP authentication of operators (Archon) via search-bind +
// group-search. ADR-058(c) (LDAP part accepted).
//
// Flow (ADR-058(c)):
//  1. POST /auth/ldap/login {username, password} over HTTPS.
//  2. connect via LDAPS or StartTLS (plaintext LDAP forbidden by config).
//  3. search-bind: the service account looks up the user DN via user_filter →
//     re-bind with that DN + the entered password (password check).
//  4. group-search (group_filter) → []groups.
//  5. return auth.ExternalIdentity (Subject=user-DN, AID=derived from
//     aid_attr, Groups=...) — role mapping is done by auth.Mapper higher up
//     the stack.
//
// Security (ADR-058(g), "security first"):
//   - TLS is mandatory (ldaps:// or StartTLS), otherwise New rejects the config;
//   - username is escaped with ldap.EscapeFilter before being substituted into
//     the filter (anti-injection);
//   - any failure reason (bad bind, wrong entry, IdP error) is sanitized into
//     auth.ErrAuthFailed — nothing leaks outward (anti-oracle); details go
//     only to the debug log, without password/bind creds.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// BindMode — the LDAP bind mode (ADR-058(c), decision #7). MVP — search only.
type BindMode string

const (
	// BindModeSearch — the service account looks up the user DN, then
	// re-binds with the user's password. The only mode stage 1 supports.
	BindModeSearch BindMode = "search"
)

// defaultAIDAttr — the LDAP attribute AID is derived from when aid_attr is
// not set (ADR-058: default `uid`). uid was chosen over mail because it's
// shorter, more stable (mail can change/get reassigned), and is almost
// always present in the person/inetOrgPerson schema.
const defaultAIDAttr = "uid"

// defaultGroupAttr — default group attribute → name used for the role map.
const defaultGroupAttr = "cn"

// defaultTimeout — default connect/bind/search timeout.
const defaultTimeout = 10 * time.Second

// Config — resolved configuration for the LDAP authenticator. Secrets
// (BindPassword) are already resolved from Vault at load time (ADR-058(e))
// and arrive here as plaintext values, NOT as *_ref. TLSCA is a resolved
// CA bundle.
type Config struct {
	URL                string   // ldaps://host:636 | ldap://host:389 (latter requires StartTLS)
	StartTLS           bool     // StartTLS over ldap:// (mutually exclusive with ldaps://)
	TLSCA              []byte   // resolved CA bundle for LDAPS (optional)
	InsecureSkipVerify bool     // dev-only (semantic-WARN)
	BindMode           BindMode // search (MVP)
	BindDN             string   // service-account DN (search mode)
	BindPassword       string   // resolved from bind_password_ref (search mode)
	BaseDN             string   // search root
	UserFilter         string   // (uid=%s) — %s = escaped username
	GroupFilter        string   // (member=%s) — %s = escaped user-DN
	GroupAttr          string   // group attribute → name for the role map (default cn)
	AIDAttr            string   // attribute → AID (default uid)
	TimeoutSeconds     int      // connect/bind/search timeout (default 10s)
}

// conn — narrow subset of *ldapv3.Conn needed by Authenticate. The interface
// lets unit tests swap in a fake conn without a real LDAP server.
type conn interface {
	Bind(username, password string) error
	Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// dialFunc — connection factory. Defaults to [dialLDAP] (real go-ldap);
// tests swap in a fake. Returns an already TLS-protected connection
// (ldaps:// — TLS at dial; ldap://+StartTLS — StartTLS is raised internally).
type dialFunc func(ctx context.Context, cfg Config, tlsCfg *tls.Config) (conn, error)

// Authenticator performs LDAP search-bind authentication (ADR-058(c)).
type Authenticator struct {
	cfg    Config
	tlsCfg *tls.Config
	dial   dialFunc
	logger *slog.Logger
}

// New constructs an LDAP authenticator from a resolved config.
//
// Validates security invariants (TLS required, ldaps-vs-StartTLS,
// search ⇒ BindDN+BindPassword) — defense-in-depth on top of the config
// layer's semantic validation (config can be built programmatically,
// bypassing YAML validation).
func New(cfg Config, logger *slog.Logger) (*Authenticator, error) {
	isLDAPS := strings.HasPrefix(cfg.URL, "ldaps://")
	isPlainLDAP := strings.HasPrefix(cfg.URL, "ldap://")

	if !isLDAPS && !(isPlainLDAP && cfg.StartTLS) {
		return nil, fmt.Errorf("auth/ldap: plaintext LDAP forbidden: url %q must be ldaps:// or ldap:// with start_tls", cfg.URL)
	}
	if isLDAPS && cfg.StartTLS {
		return nil, errors.New("auth/ldap: ldaps:// and start_tls are mutually exclusive")
	}
	mode := cfg.BindMode
	if mode == "" {
		mode = BindModeSearch
	}
	if mode != BindModeSearch {
		return nil, fmt.Errorf("auth/ldap: unsupported bind_mode %q (only %q in stage 1)", cfg.BindMode, BindModeSearch)
	}
	if cfg.BindDN == "" || cfg.BindPassword == "" {
		return nil, errors.New("auth/ldap: bind_mode=search requires bind_dn and bind_password")
	}
	if cfg.BaseDN == "" {
		return nil, errors.New("auth/ldap: base_dn is required")
	}
	if cfg.UserFilter == "" {
		return nil, errors.New("auth/ldap: user_filter is required")
	}

	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{cfg: cfg, tlsCfg: tlsCfg, dial: dialLDAP, logger: logger}, nil
}

// buildTLSConfig assembles *tls.Config: ServerName from the URL's host,
// RootCAs from TLSCA (if set), InsecureSkipVerify (dev-only).
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	t := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator opt-out (dev-only, semantic-WARN); default false
		ServerName:         hostFromURL(cfg.URL),
	}
	if len(cfg.TLSCA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.TLSCA) {
			return nil, errors.New("auth/ldap: tls.ca_ref does not contain a valid PEM certificate")
		}
		t.RootCAs = pool
	}
	return t, nil
}

// hostFromURL extracts the host (no scheme or port) for tls.ServerName.
func hostFromURL(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "ldaps://"), "ldap://")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Method implements auth.Authenticator: returns "ldap" (ADR-058(a)).
func (a *Authenticator) Method() string { return "ldap" }

// Authenticate performs search-bind + group-search and returns the external
// identity. Any error outward is auth.ErrAuthFailed (anti-oracle); details go
// only to the debug log, without password/bind creds.
func (a *Authenticator) Authenticate(ctx context.Context, username, password string) (auth.ExternalIdentity, error) {
	// An empty password is a common source of "unauthenticated bind" (RFC
	// 4513 §5.1.2): some servers treat a bind with an empty password as an
	// anonymous success. Reject explicitly.
	if username == "" || password == "" {
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	c, err := a.dial(ctx, a.cfg, a.tlsCfg)
	if err != nil {
		a.debugFail("dial", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	defer func() { _ = c.Close() }()

	// 1. bind the service account.
	if err := c.Bind(a.cfg.BindDN, a.cfg.BindPassword); err != nil {
		a.debugFail("service bind", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 2. search user (escaped username — anti-injection).
	aidAttr := a.aidAttr()
	userReq := &ldapv3.SearchRequest{
		BaseDN:       a.cfg.BaseDN,
		Scope:        ldapv3.ScopeWholeSubtree,
		DerefAliases: ldapv3.NeverDerefAliases,
		SizeLimit:    2, // >1 → ambiguous, reject
		TimeLimit:    a.timeoutSeconds(),
		Filter:       fmt.Sprintf(a.cfg.UserFilter, ldapv3.EscapeFilter(username)),
		Attributes:   []string{aidAttr},
	}
	res, err := c.Search(userReq)
	if err != nil {
		a.debugFail("user search", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	if len(res.Entries) != 1 {
		a.logger.Debug("auth/ldap: user search did not return exactly one entry",
			slog.Int("entries", len(res.Entries)))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	userDN := res.Entries[0].DN
	aid := strings.ToLower(res.Entries[0].GetAttributeValue(aidAttr))
	if aid == "" {
		a.logger.Debug("auth/ldap: aid attribute empty", slog.String("aid_attr", aidAttr))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 3. re-bind as user-DN + the entered password (password check).
	if err := c.Bind(userDN, password); err != nil {
		a.debugFail("user bind", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 4. group-search (optional — only if group_filter is set).
	groups, err := a.searchGroups(c, userDN)
	if err != nil {
		a.debugFail("group search", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	return auth.ExternalIdentity{
		Subject:  userDN,
		AID:      aid,
		Username: username,
		Groups:   groups,
	}, nil
}

// searchGroups looks up the user's groups via group_filter (member=<userDN>)
// and collects names via GroupAttr. Empty group_filter → no groups (Mapper
// will reject with ErrNoRoleMapping).
func (a *Authenticator) searchGroups(c conn, userDN string) ([]string, error) {
	if a.cfg.GroupFilter == "" {
		return nil, nil
	}
	groupAttr := a.groupAttr()
	req := &ldapv3.SearchRequest{
		BaseDN:       a.cfg.BaseDN,
		Scope:        ldapv3.ScopeWholeSubtree,
		DerefAliases: ldapv3.NeverDerefAliases,
		TimeLimit:    a.timeoutSeconds(),
		Filter:       fmt.Sprintf(a.cfg.GroupFilter, ldapv3.EscapeFilter(userDN)),
		Attributes:   []string{groupAttr},
	}
	res, err := c.Search(req)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if v := e.GetAttributeValue(groupAttr); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

func (a *Authenticator) aidAttr() string {
	if a.cfg.AIDAttr != "" {
		return a.cfg.AIDAttr
	}
	return defaultAIDAttr
}

func (a *Authenticator) groupAttr() string {
	if a.cfg.GroupAttr != "" {
		return a.cfg.GroupAttr
	}
	return defaultGroupAttr
}

func (a *Authenticator) timeoutSeconds() int {
	if a.cfg.TimeoutSeconds > 0 {
		return a.cfg.TimeoutSeconds
	}
	return int(defaultTimeout / time.Second)
}

// debugFail logs the failure reason at debug level. NEVER logs the password
// or bind creds — only the stage tag and error text (go-ldap doesn't put
// passwords into bind error text).
func (a *Authenticator) debugFail(stage string, err error) {
	a.logger.Debug("auth/ldap: authentication failed",
		slog.String("stage", stage), slog.Any("error", err))
}

// dialLDAP — the real connection factory. For ldaps:// — TLS at dial; for
// ldap://+StartTLS — plain dial + StartTLS. Timeout comes from cfg.
func dialLDAP(_ context.Context, cfg Config, tlsCfg *tls.Config) (conn, error) {
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	var (
		c   *ldapv3.Conn
		err error
	)
	if strings.HasPrefix(cfg.URL, "ldaps://") {
		c, err = ldapv3.DialURL(cfg.URL, ldapv3.DialWithTLSConfig(tlsCfg))
	} else {
		c, err = ldapv3.DialURL(cfg.URL)
	}
	if err != nil {
		return nil, err
	}
	c.SetTimeout(timeout)

	if cfg.StartTLS {
		if err := c.StartTLS(tlsCfg); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("start_tls: %w", err)
		}
	}
	return c, nil
}

// compile-time assertion: *Authenticator implements auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)

// compile-time assertion: *ldapv3.Conn satisfies the narrow conn interface.
var _ conn = (*ldapv3.Conn)(nil)
