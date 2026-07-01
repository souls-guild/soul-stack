package main

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveCertRotatorConfig_DayDurations — guard на major-фикс: порог/джиттер
// правила rotate_due_certs пишутся convention `<N>d` (как у всех reaper-правил) и
// обязаны парситься через config.ParseDuration, а не stdlib time.ParseDuration
// (последняя не знает суффикс `d` → Threshold=0 → правило МОЛЧА не ротирует при
// enabled:true, тихий security-сбой).
func TestResolveCertRotatorConfig_DayDurations(t *testing.T) {
	seven := 7
	cfg := &config.KeeperConfig{
		Vault: config.KeeperVault{PKIMount: "pki/soulstack", PKIRole: "service-tls"},
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:             true,
					RotateThreshold:     "30d",
					RotateJitter:        "7d",
					MaxRotationsPerTick: &seven,
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)

	if out.Threshold != 30*24*time.Hour {
		t.Errorf("Threshold = %v, want 720h (30d)", out.Threshold)
	}
	if out.JitterWindow != 7*24*time.Hour {
		t.Errorf("JitterWindow = %v, want 168h (7d)", out.JitterWindow)
	}
	if out.MaxRotationsPerTick != 7 {
		t.Errorf("MaxRotationsPerTick = %d, want 7", out.MaxRotationsPerTick)
	}
	if out.DefaultPKIMount != "pki/soulstack" || out.DefaultPKIRole != "service-tls" {
		t.Errorf("PKI mount/role = %q/%q, want pki/soulstack/service-tls", out.DefaultPKIMount, out.DefaultPKIRole)
	}
}

// TestResolveCertRotatorConfig_HourDurations — stdlib-совместимая форма `720h`
// продолжает работать (config.ParseDuration делегирует time.ParseDuration без
// суффикса `d`).
func TestResolveCertRotatorConfig_HourDurations(t *testing.T) {
	cfg := &config.KeeperConfig{
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:         true,
					RotateThreshold: "720h",
					RotateJitter:    "168h",
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)

	if out.Threshold != 720*time.Hour {
		t.Errorf("Threshold = %v, want 720h", out.Threshold)
	}
	if out.JitterWindow != 168*time.Hour {
		t.Errorf("JitterWindow = %v, want 168h", out.JitterWindow)
	}
}

// TestResolveCertRotatorConfig_InvalidThreshold — кривой формат порога не должен
// молча схлопывать Threshold в 0 без следа: invalid остаётся Threshold=0 (правило
// не ротирует), но факт должен быть залогирован warn-ом (см. runDurationRule).
// Здесь проверяем только, что невалид не роняет резолв и не даёт мусорный порог.
func TestResolveCertRotatorConfig_InvalidThreshold(t *testing.T) {
	cfg := &config.KeeperConfig{
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:         true,
					RotateThreshold: "not-a-duration",
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)
	if out.Threshold != 0 {
		t.Errorf("Threshold = %v on invalid input, want 0 (rule stays inert)", out.Threshold)
	}
}
