// Soul domain bodies: the registry row, its history, the ssh-target mutation and
// the typed Soulprint facts (ADR-018).
//
// status / transport are SoulStatus / SoulTransport, two of the three enums the
// UI references by $ref (see enums.go).
//
// SoulListReply is the one envelope in the contract that carries the keyset
// fields (next_cursor, total_approximate) beside offset/limit/total: the souls
// registry is the domain that paginates by keyset (ADR-047 S3b-2). Its
// offset/limit/total are int32, matching the spec's format:int32, which is why
// it is a named struct rather than the generic shared/api.PagedResponse.
//
// The Soulprint* types are the shape source for OpenAPI emission only: on the
// wire typed_facts is a byte-passthrough json.RawMessage, and these types are
// not serialised on the hot path.

package wire

import (
	"time"
)

// SoulCreateReply — the native 201 body of POST /v1/souls. Shape 1:1 with SoulCreateReply:
// bootstrap_token/covens/expires_at — *-optional WITH omitempty (present only for
// transport=agent; nil → key omitted); status/transport — oapi-enum ($ref via alias);
// registered_at — nanosecond time-wire.
type SoulCreateReply struct {
	BootstrapToken *string       `json:"bootstrap_token,omitempty"`
	Covens         *[]string     `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	CreatedByAID   string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`   // ← operator.AIDPattern
	ExpiresAt      *time.Time    `json:"expires_at,omitempty"`
	RegisteredAt   time.Time     `json:"registered_at"`
	SID            string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status         SoulStatus    `json:"status"`
	Transport      SoulTransport `json:"transport"`
}

// SoulIssueTokenReply — the native 200 body of POST /v1/souls/{sid}/issue-token. Shape 1:1 with
// SoulIssueTokenReply: bootstrap_token/expires_at/sid (all required); expires_at —
// nanosecond time-wire.
type SoulIssueTokenReply struct {
	BootstrapToken string    `json:"bootstrap_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	SID            string    `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
}

// SoulForgetReply — the native 200 body of DELETE /v1/souls/{sid} (NIM-386).
// Shape 1:1 with handlers.SoulForgetView; every field required.
//
// Two groups of fields, and the split is the point. The counts say what the
// delete TOOK — including the two cascades that reach objects the operator did
// not name (`memberships_severed`, `choir_voices_removed`). The release fields
// say what was RELEASED — the live stream, the cluster notice, the per-SID Redis
// keys. `warnings` is non-nullable and names, in words an operator can act on,
// every resource the release could not free; an empty array is the only shape
// that means "fully released". A client that renders the counts and ignores
// `warnings` will show a host as forgotten while something still holds it, which
// is the exact failure this body exists to make impossible to miss.
type SoulForgetReply struct {
	Broadcast          bool     `json:"broadcast"`
	CacheKeysPurged    int64    `json:"cache_keys_purged"`
	ChoirVoicesRemoved int64    `json:"choir_voices_removed"`
	LocalStreamClosed  bool     `json:"local_stream_closed"`
	MembershipsSevered int64    `json:"memberships_severed"`
	SeedsRevoked       int64    `json:"seeds_revoked"`
	SID                string   `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	StatusBefore       string   `json:"status_before"`
	BootstrapsBurned   int64    `json:"bootstraps_burned"`
	Warnings           []string `json:"warnings"`
}

// SoulSshTargetReply — the native 200 body of PUT /v1/souls/{sid}/ssh-target (CLASS A, reuse). Shape
// 1:1 with SoulSSHTargetReply (the reference :6399): sid + ssh_target (a snapshot of the saved
// target), both required. ssh_target — REUSES the existing native SoulSshTarget (the same type as the
// input PUT body, huma_soul_op.go) → one valid SoulSshTarget schema for input↔output.
// Struct name = the contract schema name (huma DefaultSchemaNamer → "SoulSshTargetReply");
// the native Body emits the schema itself — the rename-alias SoulSSHTargetReply → soulSshTargetReply is removed.
type SoulSshTargetReply struct {
	SID       string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	SSHTarget SoulSshTarget `json:"ssh_target"`
}

// SoulListEntry — the native projection of the souls registry (shared get-Body GET /v1/souls/{sid} +
// list-envelope element). covens — []string WITHOUT omitempty (always an array); traits — map WITHOUT
// omitempty (always an object; bare-soul → `{}` via coalesceTraits, ADR-060 read-path);
// created_by_aid/last_seen_at/last_seen_by_kid/requested_at — *-WITHOUT-omitempty (nil → `null`);
// status/transport — oapi-enum ($ref via aliasSoulStatusTransport); registered_at —
// nanosecond time-wire. Struct name = the contract schema name. ★ The envelope element
// references this same schema through the alias key PagedResponse[SoulListEntry] → the get-Body
// and envelope-element schemas are identical (TestFullSpec_NoSchemaCollision).
type SoulListEntry struct {
	Covens        []string       `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	Traits        map[string]any `json:"traits" doc:"operator-set key→value labels (ADR-060); value - scalar or list of scalars; bare-soul → {}"`
	CreatedByAID  *string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	LastSeenAt    *time.Time     `json:"last_seen_at"`
	LastSeenByKid *string        `json:"last_seen_by_kid"`
	RegisteredAt  time.Time      `json:"registered_at"`
	RequestedAt   *time.Time     `json:"requested_at"`
	SID           string         `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status        SoulStatus     `json:"status"`
	Transport     SoulTransport  `json:"transport"`
}

// SoulHistoryReply — the native 200 envelope of GET /v1/souls/{sid}/history (a standalone envelope,
// NOT the generic PagedResponse). Shape 1:1 with SoulHistoryReply (types.gen.go :3302):
// items/limit/offset/total (limit/offset/total — int, parity with the legacy-generated type) + top-level sid (host
// echo). Struct name = the contract schema name.
type SoulHistoryReply struct {
	Items  []SoulHistoryItem `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	SID    string            `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Total  int               `json:"total"`
}

// SoulHistoryItem — the native element of history.items (shape 1:1 with SoulHistoryItem, types.gen.go
// :3276): finished_at/incarnation/module/scenario/voyage_id — *-WITH omitempty (mutually exclusive
// nil fields for type=scenario/errand → key omitted); id/status — string; started_at — nanosecond
// time-wire; type — SoulHistoryItemType (enum INLINE in the reference, huma inlines it as `type: string`,
// no alias needed).
type SoulHistoryItem struct {
	FinishedAt  *time.Time          `json:"finished_at,omitempty"`
	ID          string              `json:"id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (apply_id|errand_id)
	Incarnation *string             `json:"incarnation,omitempty"`
	Module      *string             `json:"module,omitempty"`
	Scenario    *string             `json:"scenario,omitempty"`
	StartedAt   time.Time           `json:"started_at"`
	Status      string              `json:"status"`
	Type        SoulHistoryItemType `json:"type"`
	VoyageID    *string             `json:"voyage_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// SoulCreateRequest — Go form of the POST /v1/souls body (code-first source of schema AND validation).
// Struct name = contract schema name of the reference (docs/keeper/openapi.yaml → SoulCreateRequest):
// huma DefaultSchemaNamer takes reflect.Type.Name() directly. sid + transport + opt. covens +
// server-only note (written to souls.note; the reference SoulCreateRequest does NOT declare a note field —
// here it's present as a code-first body extension, not wire-affecting for golden). The format of
// sid/transport/coven — domain validation (422 in CreateTyped). additionalProperties:false
// (huma default) → unknown body field → 400.
type SoulCreateRequest struct {
	SID       string   `json:"sid" required:"true" doc:"SID of new host = FQDN"`
	Transport string   `json:"transport" required:"true" enum:"agent,ssh" doc:"delivery method: agent (mTLS gRPC stream) / ssh (push without agent)"`
	Covens    []string `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"stable Coven tags of host (kebab-case, ADR-008)"`
	Note      string   `json:"note,omitempty" doc:"server-only note (souls.note)"`
}

// SoulCovenAssignRequest — Go form of the POST /v1/souls/coven body. Struct name = contract name of
// the reference schema (docs/keeper/openapi.yaml → SoulCovenAssignRequest). mode (append/remove/
// replace) + XOR label↔labels (the domain validates the XOR → 422) + selector (at least one criterion) +
// opt. dry_run. additionalProperties:false → unknown field → 400.
type SoulCovenAssignRequest struct {
	Mode     string                  `json:"mode" required:"true" enum:"append,remove,replace" doc:"append — add label; remove — remove; replace — replace set"`
	Label    string                  `json:"label,omitempty" maxLength:"63" doc:"label for append/remove (forbidden for replace)"`
	Labels   []string                `json:"labels,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"set for replace (may be empty = remove all; forbidden for append/remove)"`
	DryRun   bool                    `json:"dry_run,omitempty" doc:"count matched without UPDATE"`
	Selector SoulCovenAssignSelector `json:"selector" required:"true" doc:"targeting (at least one criterion; AND combinations)"`
}

// SoulCovenAssignSelector — Go form of the selector (all/sids/coven/incarnation/status). Struct
// name = contract name of the reference schema (SoulCovenAssignSelector; input-only — CLASS C).
type SoulCovenAssignSelector struct {
	All         bool     `json:"all,omitempty" doc:"no host filter (entire registry ∩ scope)"`
	Sids        []string `json:"sids,omitempty" doc:"point list of hosts (SID = FQDN)"`
	Coven       string   `json:"coven,omitempty" maxLength:"63" doc:"hosts carrying this Coven tag on themselves; belonging to an incarnation attaches no tag (NIM-281)"`
	Incarnation string   `json:"incarnation,omitempty" maxLength:"63" doc:"members of this incarnation, resolved from incarnation_membership — a membership question, never answered from the tags above (ADR-008 amendment NIM-124)"`
	Status      string   `json:"status,omitempty" enum:"pending,connected,disconnected,revoked,expired,destroyed" doc:"Soul status in registry"`
}

// SoulTraitsAssignRequest — Go form of the POST /v1/souls/traits body (code-first source of schema AND
// validation; ADR-060). Struct name = contract schema name (huma DefaultSchemaNamer). mode
// (merge/replace/remove, default merge) + XOR traits↔keys (the domain validates → 422) + selector
// (at least one criterion) + opt. dry_run. traits — map key→(scalar|list of scalars); nested
// objects/arrays are rejected by the domain. additionalProperties:false → unknown field → 400.
type SoulTraitsAssignRequest struct {
	Mode     string                  `json:"mode,omitempty" enum:"merge,replace,remove" doc:"merge (default) - set/overwrite keys; replace - replace the whole map; remove - delete keys from keys"`
	Traits   map[string]any          `json:"traits,omitempty" doc:"key->value set for merge/replace (value - scalar or list of scalars); forbidden for remove"`
	Keys     []string                `json:"keys,omitempty" doc:"list of key names for remove (kebab-case); forbidden for merge/replace"`
	DryRun   bool                    `json:"dry_run,omitempty" doc:"count matched without UPDATE"`
	Selector SoulCovenAssignSelector `json:"selector" required:"true" doc:"targeting (at least one criterion; AND combinations)"`
}

// SoulSshTarget — Go form of the PUT /v1/souls/{sid}/ssh-target body (CLASS A, shared input↔output).
// Struct name = contract schema name of the reference (docs/keeper/openapi.yaml → SoulSshTarget;
// the reference SoulSshTargetRequest is a $ref to SoulSshTarget). All fields required except
// ssh_provider (optional 3-tier routing) — the required set [ssh_port,ssh_user,soul_path] is verified
// against the reference :6394. OUTPUT (SoulSshTargetReply.ssh_target) is collapsed onto the same schema via
// aliasSoulSshTarget (SoulSSHTarget → SoulSshTarget). ssh_port range / soul_path absoluteness /
// ssh_provider format — domain validation (422). additionalProperties:false →
// unknown → 400.
// ★ Field order mirrors SoulSSHTarget (soul_path, ssh_port, ssh_provider, ssh_user):
// encoding/json marshals in declaration order → the aligned order gives a byte-exact wire
// nested ssh_target in SoulSshTargetReply (output) vs the former legacy generator. For input parsing the order
// of JSON keys is irrelevant.
// PushSoulBinaryPath is where a push run delivers the `soul` binary and,
// therefore, the path it execs — the default for [SoulSshTarget.SoulPath] and
// for `soulctl souls ssh-target --soul-path`.
//
// It lives in the wire package because it is operator-facing and read from
// three sides that cannot import each other: the keeper's delivery layer, the
// CLI's flag default, and the OpenAPI field description. It had three separate
// literals and two of them said `/usr/local/bin/soul` — the PULL install path,
// which no push delivery writes to — so a host registered through the CLI ran
// `exit 127` (NIM-869).
const PushSoulBinaryPath = "/var/lib/soul-stack/bin/soul"

type SoulSshTarget struct {
	SoulPath    string `json:"soul_path" required:"true" pattern:"^/" doc:"absolute path the applier is exec'd from; send /var/lib/soul-stack/bin/soul, which is where push delivery writes the binary (an override opts out of delivery)"`
	SSHPort     int    `json:"ssh_port" required:"true" minimum:"1" maximum:"65535" doc:"SSH port [1..65535]"`
	SSHProvider string `json:"ssh_provider,omitempty" doc:"opt. SshProvider name (3-tier routing); empty -> coven/cluster default"`
	SSHUser     string `json:"ssh_user" required:"true" minLength:"1" doc:"SSH user"`
}

// ErrandRunRequest — Go form of the POST /v1/souls/{sid}/exec body (code-first source of schema AND
// validation). Struct name = contract request-schema name of the reference (docs/keeper/openapi.yaml
// → ErrandRunRequest, $ref at requestBody exec). module — required (empty → 422 in ExecTyped
// via dispatcher); input/timeout_seconds/dry_run — optional-pointer (the handler dereferences).
// timeout range / dry_run-for-verb / module format — domain validation (422/400 in ExecTyped).
type ErrandRunRequest struct {
	Module         string          `json:"module" required:"true" doc:"fully-qualified <ns>.<name>.<state>; without dry_run - core.cmd.shell / core.exec.run / an ErrandReadSafe module, with dry_run - a PlanReadSafe module"`
	Input          *map[string]any `json:"input,omitempty" doc:"input for the module (validated against input_schema)"`
	TimeoutSeconds *int            `json:"timeout_seconds,omitempty" maximum:"300" doc:"total Errand timeout [1..300]; 0/omitted -> default 30s; > server-cap (30s) -> 202 + Location"`
	DryRun         *bool           `json:"dry_run,omitempty" doc:"only for PlanReadSafe modules; a verb-shell module (core.cmd.shell / core.exec.run) has no pure-read Plan on any host -> 400; target soul must announce the dry_run capability -> 409 otherwise"`
}

// SoulprintFacts — typed Soulprint facts (ADR-018). Name = the contract schema name from the hand-written spec.
type SoulprintFacts struct {
	CPU      *SoulprintCpuFacts     `json:"cpu,omitempty"`
	Hostname *string                `json:"hostname,omitempty" doc:"short hostname, uname -n"`
	Kernel   *SoulprintKernelFacts  `json:"kernel,omitempty"`
	Memory   *SoulprintMemoryFacts  `json:"memory,omitempty" doc:"memory amounts in MB"`
	Network  *SoulprintNetworkFacts `json:"network,omitempty"`
	Os       *SoulprintOsFacts      `json:"os,omitempty" doc:"operating-system facts (ADR-018)"`
	SID      *string                `json:"sid,omitempty" doc:"echo SID for logs; authority - mTLS peer cert"`
}

// SoulprintCpuFacts — the CPU sub-fact under the CONTRACT name (hand-written spec :7009; the oapi
// generator would capitalize the acronym into SoulprintCPUFacts — here the name is contract from the start).
type SoulprintCpuFacts struct {
	Count  *int32  `json:"count,omitempty" doc:"number of logical CPUs (accounting for HT/SMT)"`
	Model  *string `json:"model,omitempty"`
	Vendor *string `json:"vendor,omitempty"`
}

// SoulprintKernelFacts — kernel facts.
type SoulprintKernelFacts struct {
	Release *string `json:"release,omitempty" doc:"kernel version only (5.15.0)"`
	Version *string `json:"version,omitempty" doc:"full version with dist-suffix (5.15.0-101-generic)"`
}

// SoulprintMemoryFacts — memory amounts in MB.
type SoulprintMemoryFacts struct {
	AvailableMb *int64 `json:"available_mb,omitempty"`
	SwapMb      *int64 `json:"swap_mb,omitempty"`
	TotalMb     *int64 `json:"total_mb,omitempty"`
}

// SoulprintNetworkFacts — network facts.
type SoulprintNetworkFacts struct {
	Fqdn       *string                      `json:"fqdn,omitempty"`
	Interfaces *[]SoulprintNetworkInterface `json:"interfaces,omitempty"`
	PrimaryIP  *string                      `json:"primary_ip,omitempty" doc:"primary IPv4 (interface with default route)"`
}

// SoulprintNetworkInterface — a single network interface.
type SoulprintNetworkInterface struct {
	Ipv4 *[]string `json:"ipv4,omitempty" doc:"IPv4 addresses in CIDR (10.0.0.1/24)"`
	Ipv6 *[]string `json:"ipv6,omitempty"`
	Mac  *string   `json:"mac,omitempty"`
	Mtu  *int32    `json:"mtu,omitempty"`
	Name *string   `json:"name,omitempty"`
}

// SoulprintOsFacts — operating-system facts (ADR-018).
type SoulprintOsFacts struct {
	Arch       *string `json:"arch,omitempty" doc:"amd64 / arm64"`
	Codename   *string `json:"codename,omitempty"`
	Distro     *string `json:"distro,omitempty"`
	Family     *string `json:"family,omitempty" doc:"debian / rhel / alpine / windows / darwin"`
	InitSystem *string `json:"init_system,omitempty" doc:"systemd / openrc / sysv / launchd"`
	PkgMgr     *string `json:"pkg_mgr,omitempty" doc:"apt / dnf / apk / pacman"`
	Version    *string `json:"version,omitempty"`
}

// SoulListReply — the alias target schema for the GET /v1/souls envelope (CURSOR, 6 fields). The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml :6766 → SoulListReply): items/offset/limit/total
// (required) + next_cursor (string, optional) + total_approximate (boolean, optional). offset/
// limit/total — int32 (the spec's format:int32). items.$ref to the CONTRACT native element
// SoulListEntry (the same schema the get-Body emits — final T5b, otherwise a huma duplicate-name panic
// between api.SoulListEntry and SoulListEntry). The type name = the contract schema name (huma
// DefaultSchemaNamer capitalizes → "SoulListReply"). The json tags repeat sharedapi.PagedResponse
// (next_cursor/total_approximate omitempty) → the wire does not change.
type SoulListReply struct {
	Items            []SoulListEntry `json:"items" doc:"page of the souls registry"`
	Offset           int32           `json:"offset" doc:"offset from start of set (offset mode)"`
	Limit            int32           `json:"limit" doc:"page size"`
	Total            int32           `json:"total" doc:"total record count; meaningful only in offset mode"`
	NextCursor       *string         `json:"next_cursor,omitempty" doc:"opaque keyset cursor for the next page (keyset mode); absent in offset mode and when the set is exhausted"`
	TotalApproximate *bool           `json:"total_approximate,omitempty" doc:"total is NOT exact (keyset mode); omitted in offset mode"`
}
