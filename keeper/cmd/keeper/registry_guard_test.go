package main

import (
	"os"
	"strings"
	"testing"
)

// TestGuardOperatorsRegistry покрывает чистое решение restart-семантики
// ADR-013(d): отказ старта при пустом реестре без initialize, иначе proceed.
func TestGuardOperatorsRegistry(t *testing.T) {
	tests := []struct {
		name        string
		n           int64
		initialize  bool
		wantProceed bool
		wantPending bool
		wantRefuse  bool // ожидается непустой refuseMsg
	}{
		{"empty, no initialize → refuse", 0, false, false, false, true},
		{"empty, initialize → bootstrap-pending", 0, true, true, true, false},
		{"non-empty, no initialize → ready", 3, false, true, false, false},
		{"non-empty, initialize → ready", 3, true, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proceed, refuseMsg, pending := guardOperatorsRegistry(tt.n, tt.initialize)
			if proceed != tt.wantProceed {
				t.Errorf("proceed = %v, want %v", proceed, tt.wantProceed)
			}
			if pending != tt.wantPending {
				t.Errorf("pending = %v, want %v", pending, tt.wantPending)
			}
			if tt.wantRefuse {
				if refuseMsg == "" {
					t.Fatalf("refuseMsg empty, want non-empty")
				}
				if !strings.Contains(refuseMsg, "refusing to start") {
					t.Errorf("refuseMsg = %q, want substring %q", refuseMsg, "refusing to start")
				}
			} else if refuseMsg != "" {
				t.Errorf("refuseMsg = %q, want empty (proceed=%v)", refuseMsg, proceed)
			}
		})
	}
}

// TestEnvTruthy — truthy-парсинг KEEPER_INITIALIZE (ADR-013(d)): валидные
// boolean-формы → true, пустая/мусорная строка → false.
func TestEnvTruthy(t *testing.T) {
	const key = "KEEPER_INITIALIZE_TEST"
	tests := []struct {
		raw  string
		set  bool // false = переменная не выставлена
		want bool
	}{
		{"true", true, true},
		{"1", true, true},
		{"T", true, true},
		{"TRUE", true, true},
		{"false", true, false},
		{"0", true, false},
		{"", true, false},
		{"garbage", true, false},
		{"", false, false}, // переменная не выставлена вовсе
	}
	for _, tt := range tests {
		name := tt.raw
		if !tt.set {
			name = "(unset)"
		}
		t.Run(name, func(t *testing.T) {
			if tt.set {
				t.Setenv(key, tt.raw)
			} else {
				os.Unsetenv(key)
			}
			if got := envTruthy(key); got != tt.want {
				t.Errorf("envTruthy(%q=%q,set=%v) = %v, want %v", key, tt.raw, tt.set, got, tt.want)
			}
		})
	}
}
