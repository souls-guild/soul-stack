// Enum types of the Operator API contract.
//
// huma generates no enum constants - only OpenAPI validation from struct tags -
// so the catalogue of types and values is written out. Every one is
// `type <Name> string`; an enum value on the wire is a string, and the exact
// bytes of each constant are pinned by keeper's huma_enums_test.go.
//
// Two emission modes, and the difference matters when adding one:
//
//   - INLINE (the vast majority): the property declares the enum inline
//     (`type: string` + `enum:`) and huma emits no separate named schema. A
//     type here carries no method, which is exactly what inline requires.
//   - NAMED schema with a $ref: only SoulStatus / SoulTransport /
//     IncarnationStatus, which the UI references by name. huma reaches a named
//     schema through huma.SchemaProvider - a METHOD, and Go allows a method
//     only in the declaring package - so those three keep a keeper-local shim
//     carrying it (keeper/internal/api/huma_enums.go). Adding a fourth means
//     adding a shim there, not a method here: a method here would drag huma
//     into shared/ and, through it, into soul.
//
// AuditEventType is a third case and is NOT here: it is inline like the first,
// but its values are DERIVED from the audit catalogue rather than declared, so
// a copy would drift. It stays in keeper.

package wire

// AuditEventSource — the source of an audit event. INLINE enum (the hand-written spec inlines it on the property).
type AuditEventSource string

const (
	AuditEventSourceAPI            AuditEventSource = "api"
	AuditEventSourceBackground     AuditEventSource = "background"
	AuditEventSourceKeeperInternal AuditEventSource = "keeper_internal"
	AuditEventSourceMCP            AuditEventSource = "mcp"
	AuditEventSourceSignal         AuditEventSource = "signal"
	AuditEventSourceSoulGRPC       AuditEventSource = "soul_grpc"
)

// SoulHistoryItemType — the type of a Soul history entry. INLINE enum (the hand-written spec
// inlines it inside SoulHistoryItem).
type SoulHistoryItemType string

const (
	SoulHistoryItemTypeErrand   SoulHistoryItemType = "errand"
	SoulHistoryItemTypeScenario SoulHistoryItemType = "scenario"
)

// OmenViewSourceType — the type of an Omen source (Augur). INLINE enum.
type OmenViewSourceType string

const (
	OmenViewSourceTypeElk        OmenViewSourceType = "elk"
	OmenViewSourceTypePrometheus OmenViewSourceType = "prometheus"
	OmenViewSourceTypeVault      OmenViewSourceType = "vault"
)

// OperatorAuthMethod — the operator's authentication method (ADR-014). INLINE enum.
//
// ADR-058 (LDAP part accepted) added the only-add values `ldap`/`oidc` (federated
// authentication, see operator.AuthMethodLDAP/OIDC). The counterpart to this
// extension is the OpenAPI enum struct tag `enum:"jwt,mtls,combined,ldap,oidc"`
// (huma_operator_op.go list filter) + committed openapi.yaml: the huma-layer const
// set must match the filter's wire enum. `oidc` was added up front (ADR-058 stage 2),
// so a later OIDC implementation does not touch the contract again.
type OperatorAuthMethod string

const (
	OperatorAuthMethodCombined OperatorAuthMethod = "combined"
	OperatorAuthMethodJWT      OperatorAuthMethod = "jwt"
	OperatorAuthMethodLDAP     OperatorAuthMethod = "ldap"
	OperatorAuthMethodMTLS     OperatorAuthMethod = "mtls"
	OperatorAuthMethodOIDC     OperatorAuthMethod = "oidc"
)

// HeraldType — the notification channel type (Herald, ADR-052). INLINE enum.
type HeraldType string

const (
	HeraldTypeWebhook    HeraldType = "webhook"
	HeraldTypeTelegram   HeraldType = "telegram"
	HeraldTypeSlack      HeraldType = "slack"
	HeraldTypeMattermost HeraldType = "mattermost"
	HeraldTypeDiscord    HeraldType = "discord"
	HeraldTypeCustom     HeraldType = "custom"
	HeraldTypeEmail      HeraldType = "email"
)

// GitRefType — the git ref type (ADR-007). INLINE enum.
type GitRefType string

const (
	GitRefTypeBranch GitRefType = "branch"
	GitRefTypeTag    GitRefType = "tag"
)

// ErrandResultStatus — the Errand result status. INLINE enum.
type ErrandResultStatus string

const (
	ErrandResultStatusCancelled        ErrandResultStatus = "cancelled"
	ErrandResultStatusFailed           ErrandResultStatus = "failed"
	ErrandResultStatusModuleNotAllowed ErrandResultStatus = "module_not_allowed"
	ErrandResultStatusRunning          ErrandResultStatus = "running"
	ErrandResultStatusSuccess          ErrandResultStatus = "success"
	ErrandResultStatusTimedOut         ErrandResultStatus = "timed_out"
)

// PushApplyViewStatus — the push-apply status (detail view). INLINE enum.
type PushApplyViewStatus string

const (
	PushApplyViewStatusCancelled     PushApplyViewStatus = "cancelled"
	PushApplyViewStatusFailed        PushApplyViewStatus = "failed"
	PushApplyViewStatusPartialFailed PushApplyViewStatus = "partial_failed"
	PushApplyViewStatusPending       PushApplyViewStatus = "pending"
	PushApplyViewStatusRunning       PushApplyViewStatus = "running"
	PushApplyViewStatusSuccess       PushApplyViewStatus = "success"
)

// PushRunListEntryStatus — the status of a push-run list row. INLINE enum.
type PushRunListEntryStatus string

const (
	PushRunListEntryStatusCancelled     PushRunListEntryStatus = "cancelled"
	PushRunListEntryStatusFailed        PushRunListEntryStatus = "failed"
	PushRunListEntryStatusPartialFailed PushRunListEntryStatus = "partial_failed"
	PushRunListEntryStatusPending       PushRunListEntryStatus = "pending"
	PushRunListEntryStatusRunning       PushRunListEntryStatus = "running"
	PushRunListEntryStatusSuccess       PushRunListEntryStatus = "success"
)

// SigilKeyIntroduceReplyStatus — the key status in the introduce reply. INLINE enum.
type SigilKeyIntroduceReplyStatus string

const (
	SigilKeyIntroduceReplyStatusActive  SigilKeyIntroduceReplyStatus = "active"
	SigilKeyIntroduceReplyStatusRetired SigilKeyIntroduceReplyStatus = "retired"
)

// SigilKeyViewStatus — the key status (detail view). INLINE enum.
type SigilKeyViewStatus string

const (
	SigilKeyViewStatusActive  SigilKeyViewStatus = "active"
	SigilKeyViewStatusRetired SigilKeyViewStatus = "retired"
)

// VoyageKind — the Voyage kind (scenario/command, ADR-043). INLINE enum.
type VoyageKind string

const (
	VoyageKindCommand  VoyageKind = "command"
	VoyageKindScenario VoyageKind = "scenario"
)

// VoyageStatus — the Voyage status. INLINE enum.
type VoyageStatus string

const (
	VoyageStatusCancelled     VoyageStatus = "cancelled"
	VoyageStatusFailed        VoyageStatus = "failed"
	VoyageStatusPartialFailed VoyageStatus = "partial_failed"
	VoyageStatusPending       VoyageStatus = "pending"
	VoyageStatusRunning       VoyageStatus = "running"
	VoyageStatusScheduled     VoyageStatus = "scheduled"
	VoyageStatusSucceeded     VoyageStatus = "succeeded"
)

// VoyageBatchMode — the Voyage batching mode. INLINE enum.
type VoyageBatchMode string

const (
	VoyageBatchModeBarrier VoyageBatchMode = "barrier"
	VoyageBatchModeWindow  VoyageBatchMode = "window"
)

// VoyageOnFailure — the on-failure policy in a Voyage. INLINE enum.
type VoyageOnFailure string

const (
	VoyageOnFailureAbort    VoyageOnFailure = "abort"
	VoyageOnFailureContinue VoyageOnFailure = "continue"
)

// VoyageTargetEntryStatus — the status of a voyage_targets row. INLINE enum.
type VoyageTargetEntryStatus string

const (
	VoyageTargetEntryStatusAwaiting  VoyageTargetEntryStatus = "awaiting"
	VoyageTargetEntryStatusCancelled VoyageTargetEntryStatus = "cancelled"
	VoyageTargetEntryStatusFailed    VoyageTargetEntryStatus = "failed"
	VoyageTargetEntryStatusNoMatch   VoyageTargetEntryStatus = "no_match"
	VoyageTargetEntryStatusRunning   VoyageTargetEntryStatus = "running"
	VoyageTargetEntryStatusSucceeded VoyageTargetEntryStatus = "succeeded"
)

// VoyageTargetEntryTargetKind — the target kind of a voyage_targets row. INLINE enum.
type VoyageTargetEntryTargetKind string

const (
	VoyageTargetEntryTargetKindIncarnation VoyageTargetEntryTargetKind = "incarnation"
	VoyageTargetEntryTargetKindSID         VoyageTargetEntryTargetKind = "sid"
)

// VoyageCreateReplyKind — the Voyage kind in the create reply. INLINE enum.
type VoyageCreateReplyKind string

const (
	VoyageCreateReplyKindCommand  VoyageCreateReplyKind = "command"
	VoyageCreateReplyKindScenario VoyageCreateReplyKind = "scenario"
)

// VoyageCreateReplyStatus — the Voyage status in the create reply. INLINE enum.
type VoyageCreateReplyStatus string

const (
	VoyageCreateReplyStatusPending   VoyageCreateReplyStatus = "pending"
	VoyageCreateReplyStatusScheduled VoyageCreateReplyStatus = "scheduled"
)

// VoyagePreviewReplyBatchMode — the batching mode in the preview reply. INLINE enum.
type VoyagePreviewReplyBatchMode string

const (
	VoyagePreviewReplyBatchModeBarrier VoyagePreviewReplyBatchMode = "barrier"
	VoyagePreviewReplyBatchModeWindow  VoyagePreviewReplyBatchMode = "window"
)

// VoyagePreviewReplyKind — the Voyage kind in the preview reply. INLINE enum.
type VoyagePreviewReplyKind string

const (
	VoyagePreviewReplyKindCommand  VoyagePreviewReplyKind = "command"
	VoyagePreviewReplyKindScenario VoyagePreviewReplyKind = "scenario"
)

// VoyageCancelReplyStatus — the Voyage status in the cancel reply. INLINE enum.
type VoyageCancelReplyStatus string

const VoyageCancelReplyStatusCancelled VoyageCancelReplyStatus = "cancelled"

// SoulStatus — the Soul status in the registry. NAMED schema "SoulStatus" ($ref). The const
// block is 1:1 with SoulStatus (4 hand-written values); SchemaProvider carries the full domain
// set (6, from internal/soul) — a pre-existing content drift in the hand-written spec (see
// huma_soul_status.go).
type SoulStatus string

const (
	SoulStatusConnected    SoulStatus = "connected"
	SoulStatusDisconnected SoulStatus = "disconnected"
	SoulStatusExpired      SoulStatus = "expired"
	SoulStatusPending      SoulStatus = "pending"
)

// SoulTransport — the configuration delivery method. NAMED schema "SoulTransport" ($ref). The
// const block is 1:1 with SoulTransport; SchemaProvider enum = the domain set (agent/ssh).
type SoulTransport string

const (
	SoulTransportAgent SoulTransport = "agent"
	SoulTransportSSH   SoulTransport = "ssh"
)

// IncarnationStatus — the runtime instance status (ADR-009/031). NAMED schema
// "IncarnationStatus" ($ref). The const block is 1:1 with IncarnationStatus; SchemaProvider
// enum = the hand-written set (8).
type IncarnationStatus string

const (
	IncarnationStatusApplying        IncarnationStatus = "applying"
	IncarnationStatusDestroyFailed   IncarnationStatus = "destroy_failed"
	IncarnationStatusDestroying      IncarnationStatus = "destroying"
	IncarnationStatusDrift           IncarnationStatus = "drift"
	IncarnationStatusErrorLocked     IncarnationStatus = "error_locked"
	IncarnationStatusMigrationFailed IncarnationStatus = "migration_failed"
	IncarnationStatusProvisioning    IncarnationStatus = "provisioning"
	IncarnationStatusReady           IncarnationStatus = "ready"
)
