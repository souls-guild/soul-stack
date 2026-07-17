// EVIDENCE guard for the NATIVE enum catalog (huma_enums.go, T5d-2c-full). An enum
// value on the wire = string → this test pins EVERY native enum constant to its
// exact wire byte. A mutation of any native value (typo/drift) reddens the matching
// case → the contract catalog is pinned directly, not indirectly via distant golden
// tests.
//
// RELATION TO GOLDEN. The golden-reply tests (huma_<domain>_reply_test.go) compare
// whole struct wire (a pinned reference) — a broad byte-exact. This test is narrow:
// it compares EVERY enum constant 1:1, so a catalog desync is visible immediately
// and precisely.
package api

import "testing"

func TestNativeEnumConstsPinned(t *testing.T) {
	cases := []struct {
		name        string
		native, pin string
	}{
		// AuditEventSource
		{"AuditEventSourceAPI", string(AuditEventSourceAPI), "api"},
		{"AuditEventSourceBackground", string(AuditEventSourceBackground), "background"},
		{"AuditEventSourceKeeperInternal", string(AuditEventSourceKeeperInternal), "keeper_internal"},
		{"AuditEventSourceMCP", string(AuditEventSourceMCP), "mcp"},
		{"AuditEventSourceSignal", string(AuditEventSourceSignal), "signal"},
		{"AuditEventSourceSoulGRPC", string(AuditEventSourceSoulGRPC), "soul_grpc"},

		// SoulHistoryItemType
		{"SoulHistoryItemTypeErrand", string(SoulHistoryItemTypeErrand), "errand"},
		{"SoulHistoryItemTypeScenario", string(SoulHistoryItemTypeScenario), "scenario"},

		// OmenViewSourceType
		{"OmenViewSourceTypeElk", string(OmenViewSourceTypeElk), "elk"},
		{"OmenViewSourceTypePrometheus", string(OmenViewSourceTypePrometheus), "prometheus"},
		{"OmenViewSourceTypeVault", string(OmenViewSourceTypeVault), "vault"},

		// OperatorAuthMethod
		{"OperatorAuthMethodCombined", string(OperatorAuthMethodCombined), "combined"},
		{"OperatorAuthMethodJWT", string(OperatorAuthMethodJWT), "jwt"},
		{"OperatorAuthMethodLDAP", string(OperatorAuthMethodLDAP), "ldap"},
		{"OperatorAuthMethodMTLS", string(OperatorAuthMethodMTLS), "mtls"},
		{"OperatorAuthMethodOIDC", string(OperatorAuthMethodOIDC), "oidc"},

		// HeraldType
		{"HeraldTypeWebhook", string(HeraldTypeWebhook), "webhook"},
		{"HeraldTypeTelegram", string(HeraldTypeTelegram), "telegram"},
		{"HeraldTypeSlack", string(HeraldTypeSlack), "slack"},
		{"HeraldTypeMattermost", string(HeraldTypeMattermost), "mattermost"},
		{"HeraldTypeDiscord", string(HeraldTypeDiscord), "discord"},
		{"HeraldTypeCustom", string(HeraldTypeCustom), "custom"},
		{"HeraldTypeEmail", string(HeraldTypeEmail), "email"},

		// GitRefType
		{"GitRefTypeBranch", string(GitRefTypeBranch), "branch"},
		{"GitRefTypeTag", string(GitRefTypeTag), "tag"},

		// ErrandResultStatus
		{"ErrandResultStatusCancelled", string(ErrandResultStatusCancelled), "cancelled"},
		{"ErrandResultStatusFailed", string(ErrandResultStatusFailed), "failed"},
		{"ErrandResultStatusModuleNotAllowed", string(ErrandResultStatusModuleNotAllowed), "module_not_allowed"},
		{"ErrandResultStatusRunning", string(ErrandResultStatusRunning), "running"},
		{"ErrandResultStatusSuccess", string(ErrandResultStatusSuccess), "success"},
		{"ErrandResultStatusTimedOut", string(ErrandResultStatusTimedOut), "timed_out"},

		// PushApplyViewStatus
		{"PushApplyViewStatusCancelled", string(PushApplyViewStatusCancelled), "cancelled"},
		{"PushApplyViewStatusFailed", string(PushApplyViewStatusFailed), "failed"},
		{"PushApplyViewStatusPartialFailed", string(PushApplyViewStatusPartialFailed), "partial_failed"},
		{"PushApplyViewStatusPending", string(PushApplyViewStatusPending), "pending"},
		{"PushApplyViewStatusRunning", string(PushApplyViewStatusRunning), "running"},
		{"PushApplyViewStatusSuccess", string(PushApplyViewStatusSuccess), "success"},

		// PushRunListEntryStatus
		{"PushRunListEntryStatusCancelled", string(PushRunListEntryStatusCancelled), "cancelled"},
		{"PushRunListEntryStatusFailed", string(PushRunListEntryStatusFailed), "failed"},
		{"PushRunListEntryStatusPartialFailed", string(PushRunListEntryStatusPartialFailed), "partial_failed"},
		{"PushRunListEntryStatusPending", string(PushRunListEntryStatusPending), "pending"},
		{"PushRunListEntryStatusRunning", string(PushRunListEntryStatusRunning), "running"},
		{"PushRunListEntryStatusSuccess", string(PushRunListEntryStatusSuccess), "success"},

		// SigilKeyIntroduceReplyStatus
		{"SigilKeyIntroduceReplyStatusActive", string(SigilKeyIntroduceReplyStatusActive), "active"},
		{"SigilKeyIntroduceReplyStatusRetired", string(SigilKeyIntroduceReplyStatusRetired), "retired"},

		// SigilKeyViewStatus
		{"SigilKeyViewStatusActive", string(SigilKeyViewStatusActive), "active"},
		{"SigilKeyViewStatusRetired", string(SigilKeyViewStatusRetired), "retired"},

		// VoyageKind
		{"VoyageKindCommand", string(VoyageKindCommand), "command"},
		{"VoyageKindScenario", string(VoyageKindScenario), "scenario"},

		// VoyageStatus
		{"VoyageStatusCancelled", string(VoyageStatusCancelled), "cancelled"},
		{"VoyageStatusFailed", string(VoyageStatusFailed), "failed"},
		{"VoyageStatusPartialFailed", string(VoyageStatusPartialFailed), "partial_failed"},
		{"VoyageStatusPending", string(VoyageStatusPending), "pending"},
		{"VoyageStatusRunning", string(VoyageStatusRunning), "running"},
		{"VoyageStatusScheduled", string(VoyageStatusScheduled), "scheduled"},
		{"VoyageStatusSucceeded", string(VoyageStatusSucceeded), "succeeded"},

		// VoyageBatchMode
		{"VoyageBatchModeBarrier", string(VoyageBatchModeBarrier), "barrier"},
		{"VoyageBatchModeWindow", string(VoyageBatchModeWindow), "window"},

		// VoyageOnFailure
		{"VoyageOnFailureAbort", string(VoyageOnFailureAbort), "abort"},
		{"VoyageOnFailureContinue", string(VoyageOnFailureContinue), "continue"},

		// VoyageTargetEntryStatus
		{"VoyageTargetEntryStatusAwaiting", string(VoyageTargetEntryStatusAwaiting), "awaiting"},
		{"VoyageTargetEntryStatusCancelled", string(VoyageTargetEntryStatusCancelled), "cancelled"},
		{"VoyageTargetEntryStatusFailed", string(VoyageTargetEntryStatusFailed), "failed"},
		{"VoyageTargetEntryStatusNoMatch", string(VoyageTargetEntryStatusNoMatch), "no_match"},
		{"VoyageTargetEntryStatusRunning", string(VoyageTargetEntryStatusRunning), "running"},
		{"VoyageTargetEntryStatusSucceeded", string(VoyageTargetEntryStatusSucceeded), "succeeded"},

		// VoyageTargetEntryTargetKind
		{"VoyageTargetEntryTargetKindIncarnation", string(VoyageTargetEntryTargetKindIncarnation), "incarnation"},
		{"VoyageTargetEntryTargetKindSID", string(VoyageTargetEntryTargetKindSID), "sid"},

		// VoyageCreateReplyKind
		{"VoyageCreateReplyKindCommand", string(VoyageCreateReplyKindCommand), "command"},
		{"VoyageCreateReplyKindScenario", string(VoyageCreateReplyKindScenario), "scenario"},

		// VoyageCreateReplyStatus
		{"VoyageCreateReplyStatusPending", string(VoyageCreateReplyStatusPending), "pending"},
		{"VoyageCreateReplyStatusScheduled", string(VoyageCreateReplyStatusScheduled), "scheduled"},

		// VoyagePreviewReplyBatchMode
		{"VoyagePreviewReplyBatchModeBarrier", string(VoyagePreviewReplyBatchModeBarrier), "barrier"},
		{"VoyagePreviewReplyBatchModeWindow", string(VoyagePreviewReplyBatchModeWindow), "window"},

		// VoyagePreviewReplyKind
		{"VoyagePreviewReplyKindCommand", string(VoyagePreviewReplyKindCommand), "command"},
		{"VoyagePreviewReplyKindScenario", string(VoyagePreviewReplyKindScenario), "scenario"},

		// VoyageCancelReplyStatus
		{"VoyageCancelReplyStatusCancelled", string(VoyageCancelReplyStatusCancelled), "cancelled"},

		// SoulStatus
		{"SoulStatusConnected", string(SoulStatusConnected), "connected"},
		{"SoulStatusDisconnected", string(SoulStatusDisconnected), "disconnected"},
		{"SoulStatusExpired", string(SoulStatusExpired), "expired"},
		{"SoulStatusPending", string(SoulStatusPending), "pending"},

		// SoulTransport
		{"SoulTransportAgent", string(SoulTransportAgent), "agent"},
		{"SoulTransportSSH", string(SoulTransportSSH), "ssh"},

		// IncarnationStatus
		{"IncarnationStatusApplying", string(IncarnationStatusApplying), "applying"},
		{"IncarnationStatusDestroyFailed", string(IncarnationStatusDestroyFailed), "destroy_failed"},
		{"IncarnationStatusDestroying", string(IncarnationStatusDestroying), "destroying"},
		{"IncarnationStatusDrift", string(IncarnationStatusDrift), "drift"},
		{"IncarnationStatusErrorLocked", string(IncarnationStatusErrorLocked), "error_locked"},
		{"IncarnationStatusMigrationFailed", string(IncarnationStatusMigrationFailed), "migration_failed"},
		{"IncarnationStatusProvisioning", string(IncarnationStatusProvisioning), "provisioning"},
		{"IncarnationStatusReady", string(IncarnationStatusReady), "ready"},
	}

	for _, c := range cases {
		if c.native != c.pin {
			t.Errorf("%s: native enum-const %q != pinned wire-string %q -- catalog WIRE DRIFT", c.name, c.native, c.pin)
		}
	}
}
