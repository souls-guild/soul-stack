package migrate_test

import (
	"os"
	"testing"
)

// requireDocker — true, если CI требует обязательного docker-а
// (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true). Используется TestMain
// в integration_test.go под `//go:build integration`; здесь живёт без
// build-tag, чтобы unit-тест ниже мог проверить env-разбор без
// поднятия testcontainers.
func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

// TestRequireDocker — case-sensitivity и допустимые токены env-флага.
// Регрессионная защита: «1» и «true» — единственные truthy-значения;
// прочее (включая «TRUE»/«yes»/произвольное) — falsy.
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
