package config

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestSessionTTL_Defaults(t *testing.T) {
	const ninetyDays = 90 * 24 * time.Hour
	for _, c := range []struct {
		name   string
		got    func(time.Duration) (time.Duration, error)
		ttlMax time.Duration
		want   time.Duration
	}{
		{"local absent", (*KeeperAuthLocal)(nil).ResolvedSessionTTL, ninetyDays, 7 * 24 * time.Hour},
		{"local: {}", (&KeeperAuthLocal{}).ResolvedSessionTTL, ninetyDays, 7 * 24 * time.Hour},
		{"ldap omitted", (&KeeperAuthLDAP{}).ResolvedSessionTTL, ninetyDays, 7 * 24 * time.Hour},
		{"oidc omitted", (&KeeperAuthOIDC{}).ResolvedSessionTTL, ninetyDays, 24 * time.Hour},
		{"ldap 3d", (&KeeperAuthLDAP{SessionTTL: "3d"}).ResolvedSessionTTL, ninetyDays, 72 * time.Hour},
		{"oidc 8h", (&KeeperAuthOIDC{SessionTTL: "8h"}).ResolvedSessionTTL, ninetyDays, 8 * time.Hour},
		// A cluster that already runs a ceiling below the default keeps starting.
		{"local omitted under a 48h ttl_max", (&KeeperAuthLocal{}).ResolvedSessionTTL, 48 * time.Hour, 48 * time.Hour},
		{"oidc omitted under a 12h ttl_max", (&KeeperAuthOIDC{}).ResolvedSessionTTL, 12 * time.Hour, 12 * time.Hour},
		// An explicit value is not cut: the range check refuses it instead.
		{"ldap 7d under a 48h ttl_max", (&KeeperAuthLDAP{SessionTTL: "7d"}).ResolvedSessionTTL, 48 * time.Hour, 7 * 24 * time.Hour},
	} {
		got, err := c.got(c.ttlMax)
		if err != nil || got != c.want {
			t.Errorf("%s: %s, %v; want %s", c.name, got, err, c.want)
		}
	}
}

func hasDiag(diags []diag.Diagnostic, level diag.Level, code string) bool {
	for _, d := range diags {
		if d.Level == level && d.Code == code {
			return true
		}
	}
	return false
}

// A session never outlives auth.jwt.ttl_max, and the check resolves ttl_max
// the way `keeper run` does: unset, it follows a longer ttl_default up.
func TestSessionTTL_Range(t *testing.T) {
	for _, c := range []struct {
		name    string
		auth    string
		wantErr bool
	}{
		{"local default", "  local: {}\n", false},
		{"local 30d", "  local: { session_ttl: 30d }\n", false},
		{"local at the default ttl_max", "  local: { session_ttl: 90d }\n", false},
		{"local above the default ttl_max", "  local: { session_ttl: 91d }\n", true},
		{"local below a minute", "  local: { session_ttl: 30s }\n", true},
		{"local above an explicit ttl_max", "  jwt: { ttl_max: 14d }\n  local: { session_ttl: 15d }\n", true},
		{"ttl_max follows a longer ttl_default", "  jwt: { ttl_default: 120d }\n  local: { session_ttl: 100d }\n", false},
		{"omitted under a ttl_max below the default", "  jwt: { ttl_default: 24h, ttl_max: 48h }\n  local: {}\n  ldap: { url: \"ldaps://l:636\" }\n", false},
		{"ldap above ttl_max", "  ldap: { url: \"ldaps://l:636\", session_ttl: 100d }\n", true},
		{"oidc above ttl_max", "  oidc: { issuer: \"https://idp\", client_id: k, redirect_url: \"https://k/cb\", session_ttl: 100d }\n", true},
		{"oidc 7d", "  oidc: { issuer: \"https://idp\", client_id: k, redirect_url: \"https://k/cb\", session_ttl: 7d }\n", false},
	} {
		_, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired+"auth:\n"+c.auth), ValidateOptions{})
		if err != nil {
			t.Fatalf("%s: load: %v", c.name, err)
		}
		if got := hasDiag(diags, diag.LevelError, "session_ttl_out_of_range"); got != c.wantErr {
			t.Errorf("%s: session_ttl_out_of_range = %v, want %v; diags=%v", c.name, got, c.wantErr, diags)
		}
	}
}

func TestSessionTTL_Malformed(t *testing.T) {
	_, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired+"auth:\n  local: { session_ttl: soon }\n"), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hasDiag(diags, diag.LevelError, "duration_invalid") {
		t.Fatalf("want duration_invalid, diags=%v", diags)
	}
	if hasDiag(diags, diag.LevelError, "session_ttl_out_of_range") {
		t.Fatalf("a malformed value is one error, not two: %v", diags)
	}
}

func withTrustedProxies(list string) []byte {
	return []byte(strings.Replace(keeperBaseRequired, "listen:\n", "listen:\n  trusted_proxies: "+list+"\n", 1))
}

func TestTrustedProxies_Load(t *testing.T) {
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", withTrustedProxies(`["10.0.0.0/8", "192.168.1.7", "fd00::/8", "10.1.2.3/8", "::ffff:172.16.0.0/108", "::ffff:10.9.9.9"]`), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("unexpected error: %v", d)
		}
	}
	got, err := cfg.Listen.ResolvedTrustedProxies()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.7/32"),
		netip.MustParsePrefix("fd00::/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("10.9.9.9/32"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestTrustedProxies_Refused(t *testing.T) {
	_, _, diags, err := LoadKeeperFromBytes("keeper.yml", withTrustedProxies(`["ingress.local"]`), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hasDiag(diags, diag.LevelError, "trusted_proxy_invalid") {
		t.Fatalf("a host name is refused: %v", diags)
	}
	_, _, diags, err = LoadKeeperFromBytes("keeper.yml", withTrustedProxies(`["0.0.0.0/0"]`), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hasDiag(diags, diag.LevelWarning, "trusted_proxy_any") || hasDiag(diags, diag.LevelError, "trusted_proxy_invalid") {
		t.Fatalf("trusting every peer warns and loads: %v", diags)
	}
}
