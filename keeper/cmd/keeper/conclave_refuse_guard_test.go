package main

import (
	"strings"
	"testing"
)

// TestDecideConclaveSinglePath -- pure decision for the soul-shedding
// refuse-guard (Finding-A, ADR-027(h)). Mock Conclave-count = liveCount arg:
// exactly the acolytes=0 + liveCount>1 without opt-out configuration is
// dangerous -> refuse; opt-out turns it into warn; acolytes>0 (any count)
// and a single instance (count<=1) -> ok.
func TestDecideConclaveSinglePath(t *testing.T) {
	tests := []struct {
		name        string
		acolytes    int
		liveCount   int
		allowUnsafe bool
		want        conclaveSinglePathDecision
	}{
		// --- dangerous configuration: acolytes=0 + other live instances ---
		{"acolytes=0, 2 live, no opt-out → refuse", 0, 2, false, conclaveSinglePathRefuse},
		{"acolytes=0, 5 live, no opt-out → refuse", 0, 5, false, conclaveSinglePathRefuse},
		{"acolytes=0, 2 live, opt-out → warn", 0, 2, true, conclaveSinglePathWarn},
		{"acolytes=0, 5 live, opt-out → warn", 0, 5, true, conclaveSinglePathWarn},

		// --- single instance (count<=1): run-goroutine path is fine ---
		{"acolytes=0, 1 live → ok", 0, 1, false, conclaveSinglePathOK},
		{"acolytes=0, 0 live → ok", 0, 0, false, conclaveSinglePathOK},
		{"acolytes=0, 1 live, opt-out irrelevant → ok", 0, 1, true, conclaveSinglePathOK},

		// --- acolytes>0 (work-queue): no cross-keeper hang at any count ---
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

// TestConclaveRefuseMessage -- the refuse message contains the number of
// live instances, the refusal marker, and both fix hints (acolytes>0 +
// opt-out flag).
func TestConclaveRefuseMessage(t *testing.T) {
	msg := conclaveRefuseMessage(3)
	for _, want := range []string{
		"3 live",
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

// TestConclaveRefuseGuardOptOutEnv -- env-OR opt-out: even with the config
// flag off, KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=truthy turns refuse into warn
// (KEEPER_INITIALIZE pattern, ADR-013(d)). Reproduces the allowUnsafe
// resolution from setupConclaveRefuseGuard on the pure decision.
func TestConclaveRefuseGuardOptOutEnv(t *testing.T) {
	const key = "KEEPER_ALLOW_UNSAFE_MULTI_KEEPER"
	const acolytes, liveCount = 0, 2 // dangerous configuration

	t.Run("env unset, cfg false → refuse", func(t *testing.T) {
		allowUnsafe := false || envTruthy(key) // cfg flag = false
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathRefuse {
			t.Errorf("got %v, want refuse", got)
		}
	})

	t.Run("env=true, cfg false → warn (start continues)", func(t *testing.T) {
		t.Setenv(key, "true")
		allowUnsafe := false || envTruthy(key)
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathWarn {
			t.Errorf("got %v, want warn", got)
		}
	})

	t.Run("env=garbage, cfg false → refuse (garbage does not enable opt-out)", func(t *testing.T) {
		t.Setenv(key, "garbage")
		allowUnsafe := false || envTruthy(key)
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathRefuse {
			t.Errorf("got %v, want refuse", got)
		}
	})

	t.Run("cfg=true, env unset → warn", func(t *testing.T) {
		allowUnsafe := true || envTruthy(key) // cfg flag = true
		if got := decideConclaveSinglePath(acolytes, liveCount, allowUnsafe); got != conclaveSinglePathWarn {
			t.Errorf("got %v, want warn", got)
		}
	})
}
