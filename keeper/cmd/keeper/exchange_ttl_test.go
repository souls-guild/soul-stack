package main

import (
	"testing"
	"time"
)

// TestClampExchangeTTL — floor auth.jwt.exchange_ttl: значение ниже минимума
// поднимается до 1m, на/выше — без изменений (NIM-77).
func TestClampExchangeTTL(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"ниже floor", 5 * time.Second, minExchangeTTL},
		{"ровно floor", 1 * time.Minute, 1 * time.Minute},
		{"дефолт выше floor", 10 * time.Minute, 10 * time.Minute},
		{"ноль поднимается", 0, minExchangeTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampExchangeTTL(tc.in); got != tc.want {
				t.Errorf("clampExchangeTTL(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
