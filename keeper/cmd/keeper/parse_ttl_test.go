package main

import (
	"strings"
	"testing"
	"time"
)

// TestParseTTL_Bootstrap covers parseTTL for cfg.Auth.JWT.TTLBootstrap
// (PM-decision M0.5c #3: default 720h for an empty string).
func TestParseTTL_Bootstrap(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr string // substring; "" = expects nil error
	}{
		{"empty defaults to 720h", "", 720 * time.Hour, ""},
		{"1h ok", "1h", time.Hour, ""},
		{"24h ok", "24h", 24 * time.Hour, ""},
		{"invalid string", "not-a-duration", 0, "invalid auth.jwt.ttl_bootstrap"},
		{"negative duration", "-1h", 0, "must be positive"},
		{"zero duration", "0s", 0, "must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTTL(tt.raw, "auth.jwt.ttl_bootstrap", 720*time.Hour)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseTTL(%q): unexpected err %v", tt.raw, err)
				}
				if got != tt.want {
					t.Errorf("parseTTL(%q) = %v, want %v", tt.raw, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseTTL(%q): expected error, got nil", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestParseTTL_Default -- sanity-check for the second call site
// (ttl_default), confirms the message context reflects the field name.
func TestParseTTL_Default(t *testing.T) {
	if _, err := parseTTL("not-a-duration", "auth.jwt.ttl_default", 24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "invalid auth.jwt.ttl_default") {
		t.Errorf("err = %v, want \"invalid auth.jwt.ttl_default\" prefix", err)
	}
	d, err := parseTTL("", "auth.jwt.ttl_default", 24*time.Hour)
	if err != nil {
		t.Fatalf("empty default: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("default = %v, want 24h", d)
	}
}
