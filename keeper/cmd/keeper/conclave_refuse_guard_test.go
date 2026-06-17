package main

import (
	"strings"
	"testing"
)

// TestDecideConclaveSinglePath — чистое решение refuse-guard-а soul-shedding
// (Finding-A, ADR-027(h)). Mock Conclave-count = liveCount-аргумент: опасна
// ровно конфигурация acolytes=0 + liveCount>1 без opt-out → refuse; opt-out
// превращает её в warn; acolytes>0 (любой count) и единственный инстанс
// (count<=1) → ok.
func TestDecideConclaveSinglePath(t *testing.T) {
	tests := []struct {
		name        string
		acolytes    int
		liveCount   int
		allowUnsafe bool
		want        conclaveSinglePathDecision
	}{
		// --- опасная конфигурация: acolytes=0 + другие живые инстансы ---
		{"acolytes=0, 2 live, no opt-out → refuse", 0, 2, false, conclaveSinglePathRefuse},
		{"acolytes=0, 5 live, no opt-out → refuse", 0, 5, false, conclaveSinglePathRefuse},
		{"acolytes=0, 2 live, opt-out → warn", 0, 2, true, conclaveSinglePathWarn},
		{"acolytes=0, 5 live, opt-out → warn", 0, 5, true, conclaveSinglePathWarn},

		// --- единственный инстанс (count<=1): run-goroutine-путь штатен ---
		{"acolytes=0, 1 live → ok", 0, 1, false, conclaveSinglePathOK},
		{"acolytes=0, 0 live → ok", 0, 0, false, conclaveSinglePathOK},
		{"acolytes=0, 1 live, opt-out irrelevant → ok", 0, 1, true, conclaveSinglePathOK},

		// --- acolytes>0 (work-queue): cross-keeper-зависания нет при любом count ---
		{"acolytes=1, 1 live → ok", 1, 1, false, conclaveSinglePathOK},
		{"acolytes=4, 9 live → ok", 4, 9, false, conclaveSinglePathOK},
		{"acolytes=4, 9 live, opt-out irrelevant → ok", 4, 9, true, conclaveSinglePathOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideConclaveSinglePath(tt.acolytes, tt.liveCount, tt.allowUnsafe)
			if got != tt.want {
				t.Errorf("decideConclaveSinglePath(acolytes=%d, live=%d, unsafe=%v) = %v, want %v",
					tt.acolytes, tt.liveCount, tt.allowUnsafe, got, tt.want)
			}
		})
	}
}

// TestConclaveRefuseMessage — refuse-сообщение содержит число живых инстансов,
// маркер отказа и обе подсказки починки (acolytes>0 + opt-out-флаг).
func TestConclaveRefuseMessage(t *testing.T) {
	msg := conclaveRefuseMessage(3)
	for _, want := range []string{
		"3 живых",
		"refusing to start",
		"keeper.acolytes>0",
		"allow_unsafe_single_path_multi_keeper",
		"KEEPER_ALLOW_UNSAFE_MULTI_KEEPER",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("conclaveRefuseMessage(3) = %q, want substring %q", msg, want)
		}
	}
}

// TestConclaveRefuseGuardOptOutEnv — env-OR opt-out: даже при выключенном
// конфиг-флаге KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=truthy превращает refuse в warn
// (паттерн KEEPER_INITIALIZE, ADR-013(d)). Воспроизводим резолв allowUnsafe из
// setupConclaveRefuseGuard на чистом решении.
func TestConclaveRefuseGuardOptOutEnv(t *testing.T) {
	const key = "KEEPER_ALLOW_UNSAFE_MULTI_KEEPER"
	const acolytes, liveCount = 0, 2 // опасная конфигурация

	t.Run("env unset, cfg false → refuse", func(t *testing.T) {
		allowUnsafe := false || envTruthy(key) // cfg-флаг = false
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathRefuse {
			t.Errorf("got %v, want refuse", got)
		}
	})

	t.Run("env=true, cfg false → warn (старт продолжается)", func(t *testing.T) {
		t.Setenv(key, "true")
		allowUnsafe := false || envTruthy(key)
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathWarn {
			t.Errorf("got %v, want warn", got)
		}
	})

	t.Run("env=garbage, cfg false → refuse (мусор не включает opt-out)", func(t *testing.T) {
		t.Setenv(key, "garbage")
		allowUnsafe := false || envTruthy(key)
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathRefuse {
			t.Errorf("got %v, want refuse", got)
		}
	})

	t.Run("cfg=true, env unset → warn", func(t *testing.T) {
		allowUnsafe := true || envTruthy(key) // cfg-флаг = true
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathWarn {
			t.Errorf("got %v, want warn", got)
		}
	})
}
