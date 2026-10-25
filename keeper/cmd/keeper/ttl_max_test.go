package main

import (
	"testing"
	"time"
)

func TestResolveTTLMax(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		ttlDefault time.Duration
		want       time.Duration
		wantErr    bool
	}{
		{"unset takes 90 days", "", 24 * time.Hour, 90 * 24 * time.Hour, false},
		// A cluster that ran ttl_default above 90 days before the key existed must
		// keep starting: an unset ceiling follows the default up.
		{"unset follows a longer ttl_default", "", 120 * 24 * time.Hour, 120 * 24 * time.Hour, false},
		{"explicit", "30d", 24 * time.Hour, 30 * 24 * time.Hour, false},
		{"explicit equal to the default", "24h", 24 * time.Hour, 24 * time.Hour, false},
		{"explicit below the default", "1h", 24 * time.Hour, 0, true},
		{"malformed", "soon", 24 * time.Hour, 0, true},
	}
	for _, c := range cases {
		got, err := resolveTTLMax(c.raw, c.ttlDefault)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: resolveTTLMax(%q, %s) = %s, %v; want %s, err=%v", c.name, c.raw, c.ttlDefault, got, err, c.want, c.wantErr)
		}
	}
}
