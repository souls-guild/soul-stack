package config

import "fmt"

// auditRetentionRule is the Reaper rule `audit.retention_days` aliases
// (ADR-022(d)/(i)).
const auditRetentionRule = "purge_audit_old"

// AuditEnabled returns the effective audit master toggle (ADR-022(i)): a missing
// `audit:` block or an omitted `enabled:` → true. Only an explicit `false` turns
// the audit write-path off.
//
// Default-ON is not a convenience here but the compliance contract of ADR-022 —
// an installation that never heard of the block must still be journaled.
func (c *KeeperConfig) AuditEnabled() bool {
	if c == nil || c.Audit == nil || c.Audit.Enabled == nil {
		return true
	}
	return *c.Audit.Enabled
}

// AuditOTelExport returns the effective OTel dual-write toggle (ADR-022(f)):
// nil/omitted → true. It is a permission to export, not a guarantee — the
// secondary writer only exists when the `otel:` block itself is enabled.
func (c *KeeperConfig) AuditOTelExport() bool {
	if c == nil || c.Audit == nil || c.Audit.OTelExport == nil {
		return true
	}
	return *c.Audit.OTelExport
}

// normalizeKeeperAudit materializes `audit.retention_days` as the Reaper rule it
// aliases (ADR-022(d)): with `retention_days` set and no `purge_audit_old` rule
// declared, the rule is created so retention is actually applied instead of
// being a number nobody reads. A rule already present wins — the semantic phase
// separately rejects a divergent pair (`audit_retention_mismatch`).
//
// Applied to the decoded struct only; the on-disk YAML and its AST are untouched
// so write-back never materializes an implicit rule (ADR-021(d)).
func normalizeKeeperAudit(c *KeeperConfig) {
	if c == nil || c.Audit == nil || c.Audit.RetentionDays < 1 || c.Reaper == nil {
		return
	}
	if _, ok := c.Reaper.Rules[auditRetentionRule]; ok {
		return
	}
	if c.Reaper.Rules == nil {
		c.Reaper.Rules = map[string]ReaperRule{}
	}
	c.Reaper.Rules[auditRetentionRule] = ReaperRule{
		Enabled: true,
		MaxAge:  fmt.Sprintf("%dd", c.Audit.RetentionDays),
		Action:  "delete",
	}
}
