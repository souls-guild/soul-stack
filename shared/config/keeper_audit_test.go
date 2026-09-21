package config

import "testing"

// The `audit:` block is the compliance contract of ADR-022(i). Before NIM-194
// none of its fields had a consumer, so `audit.enabled: false` turned nothing
// off; these pin the resolution rules the write-path gate now depends on.

func TestKeeperAudit_DefaultsOnWhenBlockAbsent(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig)

	if cfg.Audit != nil {
		t.Fatalf("expected no audit block, got %+v", cfg.Audit)
	}
	if !cfg.AuditEnabled() {
		t.Error("AuditEnabled() = false with no audit block; an installation that never configured audit must still be journaled (ADR-022(i))")
	}
	if !cfg.AuditOTelExport() {
		t.Error("AuditOTelExport() = false with no audit block, want default-ON (ADR-022(f))")
	}
}

// A block that sets only retention must NOT read as "audit off". With the
// pre-NIM-194 plain `bool` the Go zero value made this config disable the whole
// audit pipeline the moment the gate started honoring the field.
func TestKeeperAudit_PartialBlockKeepsDefaultsOn(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
audit:
  retention_days: 30
`)

	if !cfg.AuditEnabled() {
		t.Error("AuditEnabled() = false for a block that never mentions `enabled`")
	}
	if !cfg.AuditOTelExport() {
		t.Error("AuditOTelExport() = false for a block that never mentions `otel_export`")
	}
}

func TestKeeperAudit_ExplicitFalseIsHonored(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
audit:
  enabled: false
  otel_export: false
`)

	if cfg.AuditEnabled() {
		t.Error("AuditEnabled() = true for explicit `enabled: false`")
	}
	if cfg.AuditOTelExport() {
		t.Error("AuditOTelExport() = true for explicit `otel_export: false`")
	}
}

// ADR-022(d): retention_days is an alias for the Reaper rule. With no rule
// declared it must materialize one — otherwise the number is read by nobody and
// records are never purged.
func TestKeeperAudit_RetentionMaterializesReaperRule(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
audit:
  retention_days: 90
reaper:
  enabled: true
  interval: 1h
`)

	rule, ok := cfg.Reaper.Rules["purge_audit_old"]
	if !ok {
		t.Fatal("purge_audit_old rule not materialized from audit.retention_days")
	}
	if rule.MaxAge != "90d" {
		t.Errorf("max_age = %q, want 90d", rule.MaxAge)
	}
	if !rule.Enabled || rule.Action != "delete" {
		t.Errorf("rule = %+v, want enabled with action delete", rule)
	}
}

// An explicitly declared rule wins: the alias fills a gap, it does not overrule
// operator intent. (A divergent pair is separately rejected by the semantic
// phase as `audit_retention_mismatch`.)
func TestKeeperAudit_ExplicitRuleWinsOverAlias(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
audit:
  retention_days: 365
reaper:
  enabled: true
  interval: 1h
  rules:
    purge_audit_old: { enabled: false, max_age: 365d, action: delete }
`)

	rule := cfg.Reaper.Rules["purge_audit_old"]
	if rule.Enabled {
		t.Error("alias overwrote an explicitly declared rule")
	}
}

// Without a `reaper:` block there is no Reaper to carry the rule; the alias must
// not invent one (that would silently enable a purger the operator never asked
// for).
func TestKeeperAudit_NoReaperBlockNoRule(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
audit:
  retention_days: 90
`)

	if cfg.Reaper != nil {
		t.Errorf("reaper block fabricated from audit.retention_days: %+v", cfg.Reaper)
	}
}
