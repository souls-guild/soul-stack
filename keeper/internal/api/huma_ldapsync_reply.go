package api

// HUMA-NATIVE wire-DTO of the LDAP-SYNC domain (ADR-058 amendment NIM-1053).
// The settings are a nested object, so later settings join it rather than the
// top level.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/ldapsync"
)

// LDAPSyncReply — the 200 body of GET and PUT /v1/ldap-sync.
type LDAPSyncReply struct {
	Settings      LDAPSyncSettings `json:"settings"`
	SettingsSet   bool             `json:"settings_set" doc:"false: the ldap_sync key is absent and these are the defaults"`
	SettingsError string           `json:"settings_error,omitempty" doc:"the stored ldap_sync value could not be read; settings shows the default the sync runs on"`
	Status        LDAPSyncStatus   `json:"status"`
}

// LDAPSyncSettings — the effective settings.
type LDAPSyncSettings struct {
	Interval string `json:"interval" doc:"how often each LDAP operator's groups are re-read"`
}

// LDAPSyncStatus — where the sync stands over the active LDAP operators.
type LDAPSyncStatus struct {
	Enabled       bool                   `json:"enabled" doc:"this Keeper takes part in the sync (auth.ldap configured, Redis there for the leader lease); one such Keeper leads"`
	LastRunAt     *time.Time             `json:"last_run_at,omitempty" doc:"latest attempt for any LDAP operator"`
	LastSuccessAt *time.Time             `json:"last_success_at,omitempty" doc:"latest answer from the directory the sync accepted, for any LDAP operator"`
	Operators     LDAPSyncOperatorCounts `json:"operators"`
}

// LDAPSyncOperatorCounts — active LDAP operators by the outcome of their last
// attempt.
type LDAPSyncOperatorCounts struct {
	Synced          int `json:"synced" doc:"groups read, roles reconciled to them"`
	NotFound        int `json:"not_found" doc:"no directory entry: group roles dropped, operator not revoked"`
	Unreachable     int `json:"unreachable" doc:"groups could not be read; the second attempt in a row drops the group roles"`
	LockoutRefused  int `json:"lockout_refused" doc:"dropping its roles would have emptied the cluster admin set; nothing changed"`
	Guarded         int `json:"guarded" doc:"the pass would have stripped every operator holding group roles, read as a broken lookup; nothing changed"`
	ReconcileFailed int `json:"reconcile_failed" doc:"the directory answered but the roles could not be written; retried on the next pass"`
	Pending         int `json:"pending" doc:"not attempted yet"`
}

func newLDAPSyncReply(v handlers.LDAPSyncView) LDAPSyncReply {
	return LDAPSyncReply{
		Settings:      LDAPSyncSettings{Interval: v.Interval},
		SettingsSet:   v.SettingsSet,
		SettingsError: v.SettingsError,
		Status: LDAPSyncStatus{
			Enabled:       v.Enabled,
			LastRunAt:     v.LastRunAt,
			LastSuccessAt: v.LastSuccessAt,
			Operators: LDAPSyncOperatorCounts{
				Synced:          v.Operators[ldapsync.OutcomeSynced],
				NotFound:        v.Operators[ldapsync.OutcomeNotFound],
				Unreachable:     v.Operators[ldapsync.OutcomeUnreachable],
				LockoutRefused:  v.Operators[ldapsync.OutcomeLockoutRefused],
				Guarded:         v.Operators[ldapsync.OutcomeGuarded],
				ReconcileFailed: v.Operators[ldapsync.OutcomeReconcileFailed],
				Pending:         v.Pending,
			},
		},
	}
}
