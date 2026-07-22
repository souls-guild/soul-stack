package config

import "testing"

// TestResolveInputValues_FormatEnforce — runtime-enforces a value against each
// canonical format (docs/input.md → "Allowed format values"). Format guard: for
// each, a valid value passes and an invalid one is rejected. Edge cases (hostname
// with a dot = fqdn; fqdn without a dot; ipv4 out-of-range; duration with an
// unknown suffix; semver without patch; uri without a scheme) — as separate table
// rows.
func TestResolveInputValues_FormatEnforce(t *testing.T) {
	cases := []struct {
		name   string
		format string
		value  string
		ok     bool
	}{
		// hostname — RFC1123 label WITHOUT dots.
		{"hostname_ok", "hostname", "redis-01", true},
		{"hostname_single_label", "hostname", "redis", true},
		{"hostname_with_dot_is_fqdn", "hostname", "redis-01.prod.local", false},
		{"hostname_leading_dash", "hostname", "-redis", false},
		{"hostname_trailing_dash", "hostname", "redis-", false},
		{"hostname_empty_label_chars", "hostname", "redis_01", false},

		// fqdn — ≥2 RFC1123 labels separated by dots.
		{"fqdn_ok", "fqdn", "redis-01.prod.example.local", true},
		{"fqdn_two_labels", "fqdn", "redis.local", true},
		{"fqdn_no_dot_fails", "fqdn", "redis-01", false},
		{"fqdn_trailing_dot_empty_label", "fqdn", "redis.local.", false},
		{"fqdn_bad_label", "fqdn", "redis..local", false},

		// ipv4.
		{"ipv4_ok", "ipv4", "10.0.0.1", true},
		{"ipv4_zero", "ipv4", "0.0.0.0", true},
		{"ipv4_octet_overflow", "ipv4", "999.1.1.1", false},
		{"ipv4_is_ipv6", "ipv4", "2001:db8::1", false},
		{"ipv4_garbage", "ipv4", "not-an-ip", false},

		// ipv6.
		{"ipv6_ok", "ipv6", "2001:db8::1", true},
		{"ipv6_loopback", "ipv6", "::1", true},
		{"ipv6_is_ipv4", "ipv6", "10.0.0.1", false},
		{"ipv6_garbage", "ipv6", "zzzz::1", false},

		// cidr.
		{"cidr_ok_v4", "cidr", "10.0.0.0/24", true},
		{"cidr_ok_v6", "cidr", "2001:db8::/32", true},
		{"cidr_no_prefix", "cidr", "10.0.0.0", false},
		{"cidr_bad_prefix", "cidr", "10.0.0.0/99", false},

		// email.
		{"email_ok", "email", "ops@example.com", true},
		{"email_no_at", "email", "ops.example.com", false},
		{"email_named_form_rejected", "email", "Ops <ops@example.com>", false},
		{"email_no_domain", "email", "ops@", false},

		// uri.
		{"uri_ok_https", "uri", "https://example.com/path", true},
		{"uri_ok_with_port", "uri", "https://vault.internal:8200", true},
		{"uri_no_scheme", "uri", "example.com/path", false},
		{"uri_relative", "uri", "/just/a/path", false},

		// uuid.
		{"uuid_ok", "uuid", "550e8400-e29b-41d4-a716-446655440000", true},
		{"uuid_uppercase", "uuid", "550E8400-E29B-41D4-A716-446655440000", true},
		{"uuid_no_dashes", "uuid", "550e8400e29b41d4a716446655440000", false},
		{"uuid_too_short", "uuid", "550e8400-e29b-41d4-a716", false},

		// semver.
		{"semver_ok", "semver", "1.4.2", true},
		{"semver_prerelease", "semver", "2.0.0-rc1", true},
		{"semver_build_meta", "semver", "1.0.0+build.5", true},
		{"semver_two_parts_fails", "semver", "1.2", false},
		{"semver_leading_v", "semver", "v1.2.3", false},
		{"semver_leading_zero", "semver", "01.2.3", false},

		// duration — Go duration syntax.
		{"duration_seconds", "duration", "30s", true},
		{"duration_minutes", "duration", "5m", true},
		{"duration_compound", "duration", "1h30m", true},
		{"duration_bad_suffix", "duration", "5x", false},
		{"duration_no_unit", "duration", "5", false},

		// sid — regression: was already enforced before, not broken.
		{"sid_ok", "sid", "redis-01.prod.example.local", true},
		{"sid_uppercase_rejected", "sid", "Redis-01", false},
		{"sid_underscore_rejected", "sid", "redis_01", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := schemaFromInput(t, "host:\n  type: string\n  format: "+tc.format+"\n")
			_, err := ResolveInputValues(schema, map[string]any{"host": tc.value})
			if tc.ok && err != nil {
				t.Fatalf("format=%s value=%q: expected to pass, got %v", tc.format, tc.value, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("format=%s value=%q: expected validation error, passed", tc.format, tc.value)
			}
		})
	}
}

// TestResolveInputValues_FormatExprSkipped — an expression value (${...}) is not
// validated against format at its own level: the final form appears after the
// render phase (docs/input.md → "Expression values"). Symmetric to pattern/enum.
func TestResolveInputValues_FormatExprSkipped(t *testing.T) {
	schema := schemaFromInput(t, "host:\n  type: string\n  format: ipv4\n")
	got, err := ResolveInputValues(schema, map[string]any{"host": "${ soulprint.self.primary_ip }"})
	if err != nil {
		t.Fatalf("expression must not be validated against format: %v", err)
	}
	if got["host"] != "${ soulprint.self.primary_ip }" {
		t.Errorf("got=%#v", got)
	}
}

// TestResolveInputValues_FormatArrayItems — format enforce is applied per-element
// via items (validateValueAt recursion). Example from docs:
// array[items.format=email].
func TestResolveInputValues_FormatArrayItems(t *testing.T) {
	schema := schemaFromInput(t, "users:\n  type: array\n  items:\n    type: string\n    format: email\n")
	if _, err := ResolveInputValues(schema, map[string]any{"users": []any{"a@b.ru", "bad-email"}}); err == nil {
		t.Fatal("expected error: invalid email element of array")
	}
	if _, err := ResolveInputValues(schema, map[string]any{"users": []any{"a@b.ru", "c@d.ru"}}); err != nil {
		t.Fatalf("valid email elements must pass: %v", err)
	}
}
