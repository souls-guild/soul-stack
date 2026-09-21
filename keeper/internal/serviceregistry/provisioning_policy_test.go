package serviceregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// === B5 case 4: ParseProvisioningMethods (config-error on empty) ===

func TestParseProvisioningMethods_Valid(t *testing.T) {
	cases := []struct {
		csv  string
		want []string
	}{
		{"user,ldap,oidc", []string{"ldap", "oidc", "user"}},
		{" User , LDAP ", []string{"ldap", "user"}}, // trim + lowercase + dedup
		{"user,user,user", []string{"user"}},        // dedup
		{"oidc,,,user", []string{"oidc", "user"}},   // empty elements dropped
	}
	for _, c := range cases {
		set, err := ParseProvisioningMethods(c.csv)
		if err != nil {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want nil", c.csv, err)
			continue
		}
		got := sortedSet(set)
		if !equalStrs(got, c.want) {
			t.Errorf("ParseProvisioningMethods(%q) = %v, want %v", c.csv, got, c.want)
		}
	}
}

// TestParseProvisioningMethods_Empty — empty/whitespace-only/commas-only → config-
// error ErrEmptyProvisioningMethods (anti-lockout).
func TestParseProvisioningMethods_Empty(t *testing.T) {
	for _, csv := range []string{"", "   ", ",  ,", ",,,"} {
		_, err := ParseProvisioningMethods(csv)
		if !errors.Is(err, ErrEmptyProvisioningMethods) {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want ErrEmptyProvisioningMethods", csv, err)
		}
	}
}

// TestParseProvisioningMethods_InvalidMethod — a method outside {user,ldap,oidc}
// (including bootstrap/system, which CANNOT be set in the policy) → ErrInvalidProvisioningMethod.
func TestParseProvisioningMethods_InvalidMethod(t *testing.T) {
	for _, csv := range []string{"bootstrap", "system", "user,bootstrap", "saml", "ldap,unknown"} {
		_, err := ParseProvisioningMethods(csv)
		if !errors.Is(err, ErrInvalidProvisioningMethod) {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want ErrInvalidProvisioningMethod", csv, err)
		}
	}
}

// === B5 case 4: PoolSource.Load on an empty key → error (snapshot not published) ===

// settingPool — a fake ExecQueryRower for PoolSource.Load: ListServices gives an
// empty catalog, GetSetting returns a preset value by key (or ErrNoRows).
type settingPool struct {
	// values keyed by keeper_settings; a missing key → pgx.ErrNoRows.
	values map[string]string
}

func (p settingPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p settingPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// GetSetting: SELECT ... FROM keeper_settings WHERE key = $1.
	if len(args) == 1 {
		key, _ := args[0].(string)
		v, ok := p.values[key]
		if !ok {
			return settingScanRow{err: pgx.ErrNoRows}
		}
		return settingScanRow{key: key, value: v}
	}
	return settingScanRow{err: pgx.ErrNoRows}
}

func (p settingPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	// ListServices: empty catalog.
	return &emptyRows{}, nil
}

type settingScanRow struct {
	key, value string
	err        error
}

func (r settingScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	// keeper_settings: key, value, updated_by_aid, updated_at.
	if len(dest) >= 4 {
		*dest[0].(*string) = r.key
		*dest[1].(*string) = r.value
		// dest[2] (*updatedByAID) and dest[3] (*time.Time) — left as zero.
		if tp, ok := dest[3].(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

type emptyRows struct{ pgx.Rows }

func (*emptyRows) Next() bool { return false }
func (*emptyRows) Err() error { return nil }
func (*emptyRows) Close()     {}

// TestPoolSourceLoad_ProvisioningKeyAbsent — no key → policy not set
// (nil-map, everything allowed). B5 case 7.
func TestPoolSourceLoad_ProvisioningKeyAbsent(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{}}}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap.provisioningMethods != nil {
		t.Errorf("provisioningMethods = %v, want nil (key absent → all allowed)", snap.provisioningMethods)
	}
}

// TestPoolSourceLoad_ProvisioningKeyValid — a set valid key parses into a set.
func TestPoolSourceLoad_ProvisioningKeyValid(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{
		SettingProvisioningAllowedMethods: "user,ldap",
	}}}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !snap.provisioningMethods["user"] || !snap.provisioningMethods["ldap"] || snap.provisioningMethods["oidc"] {
		t.Errorf("provisioningMethods = %v, want {user,ldap}", snap.provisioningMethods)
	}
}

// TestPoolSourceLoad_ProvisioningKeyEmpty — the key IS SET but empty → Load returns
// an error (a broken snapshot is NOT published; at startup NewHolder → fatal, anti-lockout).
// B5 case 4.
func TestPoolSourceLoad_ProvisioningKeyEmpty(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{
		SettingProvisioningAllowedMethods: "  , ,",
	}}}
	if _, err := src.Load(context.Background()); !errors.Is(err, ErrEmptyProvisioningMethods) {
		t.Fatalf("Load on empty key err=%v, want ErrEmptyProvisioningMethods", err)
	}
}

// === B5 cases 5/7: Holder getters ===

// snapProv — a snapshot with a set policy (set non-nil) or without one (nil).
func snapProv(methods map[string]bool) *Snapshot {
	return &Snapshot{services: map[string]ServiceEntry{}, provisioningMethods: methods}
}

// TestHolder_ProvisioningMethodAllowed_BootstrapAlwaysTrue — bootstrap/system
// always pass, even under policy {} / without them. B5 case 5.
func TestHolder_ProvisioningMethodAllowed_BootstrapAlwaysTrue(t *testing.T) {
	// A policy without user/ldap/oidc is impossible (empty set = config-error), but
	// even under a restrictive {oidc} bootstrap/system must pass.
	src := &fakeSnapSource{snap: snapProv(map[string]bool{"oidc": true})}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	for _, m := range []string{"bootstrap", "system"} {
		if !h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = false, want true (never gated)", m)
		}
	}
	// user/ldap under policy {oidc} — forbidden.
	for _, m := range []string{"user", "ldap"} {
		if h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = true under policy {oidc}, want false", m)
		}
	}
	if !h.ProvisioningMethodAllowed("oidc") {
		t.Error("ProvisioningMethodAllowed(oidc) = false under policy {oidc}, want true")
	}
}

// TestHolder_ProvisioningPolicy_DefaultAllowAll — policy not set (nil-map) →
// all methods allowed, GET-projection policy_set=false. B5 case 7.
func TestHolder_ProvisioningPolicy_DefaultAllowAll(t *testing.T) {
	src := &fakeSnapSource{snap: snapProv(nil)}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	for _, m := range []string{"user", "ldap", "oidc", "bootstrap"} {
		if !h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = false when policy not set, want true (back-compat)", m)
		}
	}
	methods, set := h.ProvisioningPolicy()
	if set {
		t.Errorf("ProvisioningPolicy set=true, want false (policy not set)")
	}
	if methods != nil {
		t.Errorf("ProvisioningPolicy methods=%v, want nil", methods)
	}
}

// TestHolder_ProvisioningPolicy_Set — a set policy is returned as a sorted
// list + set=true.
func TestHolder_ProvisioningPolicy_Set(t *testing.T) {
	src := &fakeSnapSource{snap: snapProv(map[string]bool{"user": true, "ldap": true})}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	methods, set := h.ProvisioningPolicy()
	if !set {
		t.Fatal("ProvisioningPolicy set=false, want true")
	}
	if !equalStrs(methods, []string{"ldap", "user"}) {
		t.Errorf("ProvisioningPolicy methods=%v, want [ldap user]", methods)
	}
}

// TestHolder_ProvisioningMethodAllowed_NilReceiver — nil-Holder → true (gate not
// configured, back-compat).
func TestHolder_ProvisioningMethodAllowed_NilReceiver(t *testing.T) {
	var h *Holder
	if !h.ProvisioningMethodAllowed("user") {
		t.Error("nil-Holder ProvisioningMethodAllowed(user) = false, want true (back-compat)")
	}
}

// --- helpers ---

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
