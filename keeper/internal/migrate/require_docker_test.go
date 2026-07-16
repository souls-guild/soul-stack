package migrate_test

import (
	"os"
	"testing"
)

// requireDocker is true when CI requires mandatory docker
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Used by TestMain in
// integration_test.go under `//go:build integration`; it lives here without a
// build tag so the unit test below can verify env parsing without starting
// testcontainers.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

// TestRequireDocker verifies case sensitivity and accepted env flag tokens.
// Regression guard: "1" and "true" are the only truthy values; everything else
// (including "TRUE"/"yes"/arbitrary) is falsy.
func TestRequireDocker(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{"TRUE", false}, // case-sensitive
		{"yes", false},
		{"invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER", tt.env)
			if got := requireDocker(); got != tt.want {
				t.Errorf("requireDocker() with env=%q = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}
