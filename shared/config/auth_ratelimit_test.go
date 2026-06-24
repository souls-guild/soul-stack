package config

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestKeeperAuth_LoginRateLimitDefaults — nil-блок auth.rate_limit резолвится к
// дефолтам и enabled=true (default-ON, footgun-guard, HIGH-3).
func TestKeeperAuth_LoginRateLimitDefaults(t *testing.T) {
	var a *KeeperAuth
	if !a.LoginRateLimitEnabled() {
		t.Errorf("nil auth → login rate-limit enabled by default")
	}
	rate, burst, threshold, window, backoff := a.ResolvedLoginRateLimit()
	if rate != DefaultAuthLoginRLRate || burst != DefaultAuthLoginRLBurst {
		t.Errorf("rate/burst = %v/%d, want defaults %v/%d", rate, burst, DefaultAuthLoginRLRate, DefaultAuthLoginRLBurst)
	}
	if threshold != DefaultAuthLoginLockoutThreshold {
		t.Errorf("threshold = %d, want default %d", threshold, DefaultAuthLoginLockoutThreshold)
	}
	if window != DefaultAuthLoginLockoutWindow || backoff != DefaultAuthLoginLockoutBackoff {
		t.Errorf("window/backoff = %v/%v, want defaults", window, backoff)
	}
}

// TestKeeperAuth_LoginRateLimitExplicit — заданные поля переопределяют дефолты;
// duration-поля парсятся.
func TestKeeperAuth_LoginRateLimitExplicit(t *testing.T) {
	disabled := false
	a := &KeeperAuth{RateLimit: &KeeperAuthLoginRateLimit{
		Enabled:          &disabled,
		Rate:             2,
		Burst:            7,
		LockoutThreshold: 9,
		LockoutWindow:    "30m",
		LockoutBackoff:   "1h",
	}}
	if a.LoginRateLimitEnabled() {
		t.Errorf("explicit enabled=false must disable")
	}
	rate, burst, threshold, window, backoff := a.ResolvedLoginRateLimit()
	if rate != 2 || burst != 7 || threshold != 9 {
		t.Errorf("rate/burst/threshold = %v/%d/%d, want 2/7/9", rate, burst, threshold)
	}
	if window != 30*time.Minute || backoff != time.Hour {
		t.Errorf("window/backoff = %v/%v, want 30m/1h", window, backoff)
	}
}

// TestKeeperAuth_LoginRateLimitBadDuration — невалидный duration в lockout_window
// → semantic-ERROR (duration_invalid, тот же checkDuration).
func TestKeeperAuth_LoginRateLimitBadDuration(t *testing.T) {
	src := keeperBaseRequired + `auth:
  rate_limit:
    lockout_window: "not-a-duration"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected error for invalid lockout_window duration")
	}
}
