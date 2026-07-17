package main

import (
	"testing"
	"time"
)

// TestClampExchangeTTL — floor auth.jwt.exchange_ttl: a value below the minimum
// is raised to 1m, at/above stays unchanged (NIM-77).
func TestClampExchangeTTL(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"below floor", 5 * time.Second, minExchangeTTL},
		{"exactly floor", 1 * time.Minute, 1 * time.Minute},
		{"default above floor", 10 * time.Minute, 10 * time.Minute},
		{"zero raised", 0, minExchangeTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampExchangeTTL(tc.in); got != tc.want {
				t.Errorf("clampExchangeTTL(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
