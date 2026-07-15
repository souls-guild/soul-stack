package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AuditWriter — narrow surface over [audit.Writer], so test mocks don't
// need to pull in pgx.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// PermissionChecker — narrow surface over rbac.Enforcer/Holder for the
// MCP service's RBAC check before tool execution.
type PermissionChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// HandlerDeps — dependencies of the MCP handler (tools/call dispatcher).
//
// OperatorSvc is the same instance the HTTP server uses — single source of
// truth (PM-decision M0.7 #6).
//
// AuditWriter and RBAC mirror HTTP. EventTypes for operator-tools match the
// HTTP handlers; source differs (ADR-022(b), audit.SourceMCP) for a granular
// audit trail (`WHERE source='mcp'` separates MCP from HTTP).
//
// Incarnation dependencies are the same values api.Deps passes to
// IncarnationHandler (single source of truth, not a duplicate construction):
//   - IncarnationDB   — REQUIRED (validated in NewHandler): needed by all
//     incarnation-tools (get/list/history need only this).
//   - ScenarioRunner  — optional (create/run). nil → tool returns internal-error
//     "scenario runner is not configured" (mirrors REST 500).
//   - ServiceRegistry — optional (create/run/upgrade). nil → same.
//   - ServiceLoader   — optional (upgrade). nil → same.
//
// Interfaces are reused from package handlers (handlers.IncarnationDB etc.),
// not copied; real values (*pgxpool.Pool / scenario.Runner /
// scenario.ServiceRegistry / artifact.ServiceLoader) satisfy them.
type HandlerDeps struct {
	OperatorSvc *operator.Service
	RBAC        PermissionChecker

	// RBACRoles — mutating RBAC-CRUD facade ([rbac.Service]) for the
	// role.*-tools (Slice 2b). Not to be confused with RBAC above: that one is
	// the read-only PermissionChecker (Check before tool execution), this one
	// is CreateRole / DeleteRole / GrantOperator / ListRoles etc. nil → role.*
	// -tools simply don't dispatch.
	RBACRoles *rbac.Service

	// SigilSvc — Sigil allow-list business logic ([sigil.Service]) for the
	// plugin.*-tools (S4b). Same instance REST passes into api.Deps.SigilSvc
	// (single source of truth). nil → plugin.*-tools dispatch but return
	// internal-error "not configured" (RBACRoles pattern): keeper starts with
	// Sigil disabled, no panic.
	SigilSvc *sigil.Service

	// SigilKeySvc — Sigil signing-key rotation business logic
	// ([sigil.KeyService]) for the sigil.key.*-tools (R3-S7). Same instance
	// REST passes into api.Deps.SigilKeySvc (single source of truth). nil →
	// sigil.key.*-tools dispatch but return internal-error "not configured"
	// (SigilSvc pattern).
	SigilKeySvc *sigil.KeyService

	// ServiceSvc — Service registry business logic ([serviceregistry.Service])
	// for the service.*-tools (ADR-028 RBAC-storage pattern). Same instance
	// REST passes into api.Deps.ServiceSvc (single source of truth, carries
	// the S2 invalidate hook). nil → service.*-tools dispatch but return
	// internal-error "not configured" (RBACRoles/SigilSvc pattern).
	ServiceSvc *serviceregistry.Service

	// AugurSvc — Augur registry management logic ([augur.Service]) for the
	// augur.omen.*/augur.rite.*-tools (ADR-025, augur.md §4). Same instance
	// REST passes into api.Deps.AugurSvc (single source of truth). nil →
	// augur.*-tools dispatch but return internal-error "not configured"
	// (ServiceSvc pattern). Not to be confused with the Augur broker
	// (resolve/broker), which resolves AugurRequest from a Soul over gRPC.
	AugurSvc *augur.Service

	// OracleSvc — Oracle registries management logic ([oracle.Service]) for
	// the oracle.vigil.*/oracle.decree.*-tools (ADR-030, beacons S3). Same
	// instance REST passes into api.Deps.OracleSvc (single source of truth).
	// nil → oracle.*-tools dispatch but return internal-error "not configured"
	// (AugurSvc pattern). Not to be confused with the reactor router
	// (match/enqueue), which resolves a Portent from a Soul over gRPC.
	OracleSvc *oracle.Service

	AuditWriter AuditWriter
	Logger      *slog.Logger

	IncarnationDB   handlers.IncarnationDB
	ScenarioRunner  handlers.ScenarioStarter
	ServiceRegistry handlers.ServiceResolver
	ServiceLoader   handlers.ServiceSnapshotLoader

	// ScenarioDestroyer — narrow scenario.Runner surface for the destroy-tool
	// (async-teardown scenario `destroy` in TerminalDestroy, S-D4). Separate
	// field from ScenarioRunner (StartDestroy instead of Start), though
	// production wiring passes the same *scenario.Runner. nil →
	// keeper.incarnation.destroy returns internal-error "destroy is not
	// configured" (mirrors REST 500).
	ScenarioDestroyer handlers.DestroyStarter

	// ScenarioDrift — narrow scenario.Runner surface for the check-drift-tool
	// (Scry on-demand pilot, ADR-031 Slice B). CheckDrift is sync (unlike
	// Start/StartDestroy). Production wiring passes the same *scenario.Runner.
	// nil → keeper.incarnation.check-drift returns internal-error "drift
	// checker is not configured" (mirrors REST 500).
	ScenarioDrift handlers.DriftChecker

	// SoulDB — same [handlers.SoulPool] (`*pgxpool.Pool`) REST passes into
	// SoulHandler (single source of truth). Needed by soul-tools (create /
	// issue-token); read-only soul.list remains a stub. nil → soul-tools
	// return internal-error "soul DB is not configured" (mirrors REST, where
	// SoulHandler without a pool isn't mounted).
	SoulDB handlers.SoulPool

	// PurviewResolver — read surface of the operator's scope boundary for
	// bulk keeper.soul.coven-assign (scope-intersection of selector and
	// operator's permission labels). Same [rbac.Holder] REST passes into
	// NewSoulHandler (single source of truth; implements
	// [handlers.PurviewResolver]). The coven dimension of Purview yields the
	// usual (covens, unrestricted) shape. nil → coven-assign-tool returns
	// internal-error "coven-assign unavailable" (mirrors REST, where a nil
	// scoper returns 500). Not to be confused with RBAC above: that one is
	// the permission gate (Check), this one is the scope of the bulk coven
	// mutation.
	PurviewResolver handlers.PurviewResolver

	// PushRun — multi-host push orchestrator (Variant C, docs/keeper/push.md)
	// for the keeper.push.apply tool. Same instance REST passes into
	// api.Deps.PushRun (single source of truth). nil → push-tools dispatch
	// but return internal-error "not configured" (SigilSvc pattern).
	PushRun *pushorch.PushRun

	// PushProviderSvc — CRUD business logic for the Push-Provider registry
	// (push-provider.create/update/delete/list/read tools, ADR-032 amendment
	// 2026-05-26, S7-2). Same instance REST passes into
	// api.Deps.PushProviderSvc (single source of truth). nil →
	// push-provider.*-tools dispatch but return internal-error "push-provider
	// registry is not configured" (PushRun pattern).
	PushProviderSvc *pushprovider.Service

	// HeraldSvc — CRUD business logic for the Herald (channels) / Tiding
	// (rules) notification registries (keeper.herald.* / keeper.tiding.*,
	// ADR-052, S4). Same instance REST passes into api.Deps.HeraldSvc (single
	// source of truth). nil → herald/tiding-tools dispatch but return
	// internal-error "herald registry is not configured" (PushProviderSvc
	// pattern).
	HeraldSvc *herald.Service

	// ProviderSvc / ProfileSvc — Cloud CRUD facades (keeper.provider.* /
	// keeper.profile.*, ADR-017). Same instances REST passes into
	// api.Deps.ProviderSvc/ProfileSvc (single source of truth). nil →
	// corresponding tools dispatch but return internal-error
	// "provider/profile registry is not configured" (PushProviderSvc pattern).
	ProviderSvc *provider.Service
	ProfileSvc  *profile.Service

	// ErrandDispatcher / ErrandStore — pull ad-hoc Errand contour (ADR-033)
	// for keeper.soul.errand.run / keeper.errand.list / keeper.errand.get.
	// Same instances REST passes into api.Deps.ErrandDispatcher / ErrandStore
	// (single source of truth). nil → errand-tools dispatch but return
	// internal-error "not configured" (SigilSvc pattern).
	ErrandDispatcher *errand.Dispatcher
	ErrandStore      *errand.Store

	// VoyageDB / VoyageScenarioResolver / VoyageCommandResolver — Voyage
	// contour (ADR-043, S5) for the keeper.voyage.{start,list,get} (+ cancel)
	// tools. Same instances REST passes into api.Deps.VoyageDB /
	// VoyageScenarioResolver / VoyageCommandResolver (single source of truth,
	// *pgxpool.Pool + PG resolvers). nil → voyage-tools return internal-error
	// "voyage orchestrator is not configured" (ErrandRunStore/ErrandRunSouls
	// pattern). RBAC-by-kind is done by VoyageHandler itself (enforcer=RBAC,
	// IncarnationDB for per-incarnation scope-check).
	VoyageDB               handlers.VoyageStore
	VoyageScenarioResolver handlers.VoyageScenarioResolver
	VoyageCommandResolver  handlers.VoyageCommandResolver
	// VoyageMaxScope — upper bound on resolved scope size (DoS-guard S-med-3);
	// 0 → unlimited. Same source as REST: cfg.Voyage.ResolvedMaxScope().
	VoyageMaxScope int
	// VoyageMaxBatchSize — upper bound on batch/window size (DoS-guard S-W4);
	// 0 → unlimited. Same source as REST: cfg.Voyage.ResolvedMaxBatchSize().
	VoyageMaxBatchSize int
}

// Handler — dispatcher for the MCP server's JSON-RPC methods. One instance
// per listener, goroutine-safe (deps immutable).
type Handler struct {
	deps HandlerDeps
}

// NewHandler builds the dispatcher. Returns an error if required deps are nil.
func NewHandler(d HandlerDeps) (*Handler, error) {
	if d.OperatorSvc == nil {
		return nil, fmt.Errorf("mcp: HandlerDeps.OperatorSvc is nil")
	}
	if d.RBAC == nil {
		return nil, fmt.Errorf("mcp: HandlerDeps.RBAC is nil")
	}
	if d.AuditWriter == nil {
		return nil, fmt.Errorf("mcp: HandlerDeps.AuditWriter is nil")
	}
	if d.Logger == nil {
		return nil, fmt.Errorf("mcp: HandlerDeps.Logger is nil")
	}
	if d.IncarnationDB == nil {
		return nil, fmt.Errorf("mcp: HandlerDeps.IncarnationDB is nil")
	}
	return &Handler{deps: d}, nil
}

// Dispatch handles one JSON-RPC request and returns a response. claims is
// non-nil (guaranteed by auth-middleware); used for archon_aid in audit
// events and AID in RBAC.Check.
//
// MCP-spec contract:
//
//   - `initialize`      — handshake; returns capabilities.
//   - `tools/list`      — list of tool declarations.
//   - `tools/call`      — execute a tool with arguments.
//   - `notifications/*` — ignored (no id, nothing returned).
//   - unknown method    — MethodNotFound (-32601).
func (h *Handler) Dispatch(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest) (resp jsonRPCResponse, isNotification bool) {
	if req.JSONRPC != jsonRPCVersion {
		return newRPCError(req.ID, rpcCodeInvalidRequest,
			"unsupported jsonrpc version; expected \""+jsonRPCVersion+"\"", nil), false
	}

	// Notifications (no id) — JSON-RPC 2.0 spec §4.1: server must not
	// reply. Accepted and silently ignored (e.g. `notifications/initialized`).
	if isNotificationMethod(req.Method) || len(req.ID) == 0 || string(req.ID) == "null" {
		return jsonRPCResponse{}, true
	}

	switch req.Method {
	case "initialize":
		return h.handleInitialize(req)
	case "tools/list":
		return h.handleToolsList(req)
	case "tools/call":
		return h.handleToolsCall(ctx, claims, req)
	default:
		return newRPCError(req.ID, rpcCodeMethodNotFound,
			"method not found: "+req.Method, nil), false
	}
}

// isNotificationMethod reports whether method must not get an MCP-spec
// response. MCP clients send `notifications/initialized` after handshake and
// (in the future) `notifications/cancelled` to cancel long-running tool calls.
func isNotificationMethod(method string) bool {
	return strings.HasPrefix(method, "notifications/")
}

// --- initialize ---

type initializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	ServerInfo      serverInfo             `json:"serverInfo"`
	Capabilities    initializeCapabilities `json:"capabilities"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeCapabilities struct {
	Tools toolsCapability `json:"tools"`
}

type toolsCapability struct {
	// listChanged=false — the MCP server never sends
	// `notifications/tools/list_changed` (tool catalog is static for the
	// process lifetime). Stays false until tool-catalog hot-reload exists.
	ListChanged bool `json:"listChanged"`
}

// mcpProtocolVersion — MCP spec version this server implements. MCP clients
// compare this string against their own supported version; on mismatch the
// client decides what to do (most continue best-effort).
const mcpProtocolVersion = "2025-06-18"

// serverInfoName / serverInfoVersion — Keeper server identity for the
// MCP handshake. Version is pinned to "0.7" until build-info infra exists
// (CI tagging is a separate slice).
const (
	serverInfoName    = "soul-stack-keeper"
	serverInfoVersion = "0.7"
)

func (h *Handler) handleInitialize(req jsonRPCRequest) (jsonRPCResponse, bool) {
	res := initializeResult{
		ProtocolVersion: mcpProtocolVersion,
		ServerInfo:      serverInfo{Name: serverInfoName, Version: serverInfoVersion},
		Capabilities:    initializeCapabilities{Tools: toolsCapability{ListChanged: false}},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		h.deps.Logger.Error("mcp: initialize marshal failed", slog.Any("error", err))
		return newRPCError(req.ID, rpcCodeInternalError, "initialize marshal failed", nil), false
	}
	return newRPCResult(req.ID, raw), false
}

// --- tools/list ---

type toolsListResult struct {
	Tools []toolDeclaration `json:"tools"`
}

func (h *Handler) handleToolsList(req jsonRPCRequest) (jsonRPCResponse, bool) {
	res := toolsListResult{Tools: listAllTools()}
	raw, err := json.Marshal(res)
	if err != nil {
		h.deps.Logger.Error("mcp: tools/list marshal failed", slog.Any("error", err))
		return newRPCError(req.ID, rpcCodeInternalError, "tools/list marshal failed", nil), false
	}
	return newRPCResult(req.ID, raw), false
}

// --- tools/call ---

// toolsCallParams — params of the MCP-spec JSON-RPC `tools/call` method.
//
// arguments is an arbitrary JSON object (per-tool typing happens in the
// dispatch phase via json.Unmarshal into the matching input struct).
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult — MCP-spec shape of a successful tool-call result.
// `content` is an array of content blocks (MCP-spec supports text/image/
// resource); for our structured outputs we use a single text block with
// JSON-serialized structuredContent.
// `structuredContent` — typed output per the tool declaration's outputSchema
// (MCP-spec 2025-06: server SHOULD include structuredContent when
// outputSchema is present).
// `isError` — MCP clients use it for UI notification; we do NOT set
// isError=true for tool-execution errors since we use the JSON-RPC error
// channel instead (see mapServiceErrorToMCP).
type toolsCallResult struct {
	Content           []toolContentBlock `json:"content"`
	StructuredContent json.RawMessage    `json:"structuredContent,omitempty"`
	IsError           bool               `json:"isError,omitempty"`
}

type toolContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (h *Handler) handleToolsCall(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest) (jsonRPCResponse, bool) {
	var p toolsCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return newRPCError(req.ID, rpcCodeInvalidParams,
			"invalid params for tools/call: "+err.Error(), nil), false
	}
	if p.Name == "" {
		return newRPCError(req.ID, rpcCodeInvalidParams,
			"tools/call: 'name' is required", nil), false
	}

	entry, ok := toolByName(p.Name)
	if !ok {
		return h.toolError(req.ID, p.Name, mcpCodeNotFound,
			"tool not found: "+p.Name), false
	}

	// Stub-tools (incarnation/soul/push/cloud) not implemented in M0.7.a.
	// Return `internal-error` with `data.code: "not-implemented"` — the
	// client agent sees the tool exists in the catalog but isn't wired up
	// yet. Better than `MethodNotFound` so the LLM doesn't forget the tool.
	if entry.status == toolStatusStub {
		return h.toolError(req.ID, p.Name, mcpCodeNotImplemented,
			"tool not implemented in this build (M0.7.a)"), false
	}

	// Implemented-tools — dispatch by name.
	switch p.Name {
	case "keeper.operator.create":
		return h.callOperatorCreate(ctx, claims, req, p.Arguments), false
	case "keeper.operator.revoke":
		return h.callOperatorRevoke(ctx, claims, req, p.Arguments), false
	case "keeper.operator.issue-token":
		return h.callOperatorIssueToken(ctx, claims, req, p.Arguments), false

	// Role-tools (6) — RBAC CRUD over rbac.Service (Slice 2b). 1:1 with
	// permission (keeper.role.<action> ↔ role.<action>).
	case "keeper.role.create":
		return h.callRoleCreate(ctx, claims, req, p.Arguments), false
	case "keeper.role.delete":
		return h.callRoleDelete(ctx, claims, req, p.Arguments), false
	case "keeper.role.list":
		return h.callRoleList(ctx, claims, req, p.Arguments), false
	case "keeper.role.update":
		return h.callRoleUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.role.grant-operator":
		return h.callRoleGrantOperator(ctx, claims, req, p.Arguments), false
	case "keeper.role.revoke-operator":
		return h.callRoleRevokeOperator(ctx, claims, req, p.Arguments), false

	// Synod-tools (ADR-049): 1:1 keeper.synod.<action> ↔ permission
	// synod.<action>. Same *rbac.Service (RBACRoles) as role-tools.
	case "keeper.synod.create":
		return h.callSynodCreate(ctx, claims, req, p.Arguments), false
	case "keeper.synod.update":
		return h.callSynodUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.synod.delete":
		return h.callSynodDelete(ctx, claims, req, p.Arguments), false
	case "keeper.synod.list":
		return h.callSynodList(ctx, claims, req, p.Arguments), false
	case "keeper.synod.add-operator":
		return h.callSynodAddOperator(ctx, claims, req, p.Arguments), false
	case "keeper.synod.remove-operator":
		return h.callSynodRemoveOperator(ctx, claims, req, p.Arguments), false
	case "keeper.synod.grant-role":
		return h.callSynodGrantRole(ctx, claims, req, p.Arguments), false
	case "keeper.synod.revoke-role":
		return h.callSynodRevokeRole(ctx, claims, req, p.Arguments), false

	// Incarnation-tools (all 8 implemented: C7 rollout from the get + destroy
	// pilot in S-D4). Separate case lines per review recommendation: each tool
	// gets its own branch.
	case "keeper.incarnation.get":
		return h.callIncarnationGet(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.list":
		return h.callIncarnationList(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.history":
		return h.callIncarnationHistory(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.unlock":
		return h.callIncarnationUnlock(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.rerun-last":
		return h.callIncarnationRerunLast(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.create":
		return h.callIncarnationCreate(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.run":
		return h.callIncarnationRun(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.upgrade":
		return h.callIncarnationUpgrade(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.destroy":
		return h.callIncarnationDestroy(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.check-drift":
		return h.callIncarnationCheckDrift(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.traits-set":
		return h.callIncarnationTraitsSet(ctx, claims, req, p.Arguments), false

	// Soul-tools (parity with REST POST /v1/souls + issue-token). soul.list
	// remains a stub (caught by status==toolStatusStub above). 1:1 with
	// permission (keeper.soul.<action> ↔ soul.<action>).
	case "keeper.soul.create":
		return h.callSoulCreate(ctx, claims, req, p.Arguments), false
	case "keeper.soul.issue-token":
		return h.callSoulIssueToken(ctx, claims, req, p.Arguments), false
	case "keeper.soul.coven-assign":
		return h.callSoulCovenAssign(ctx, claims, req, p.Arguments), false
	case "keeper.soul.traits-assign":
		return h.callSoulTraitsAssign(ctx, claims, req, p.Arguments), false
	case "keeper.soul.ssh-target.update":
		return h.callSoulSshTargetUpdate(ctx, claims, req, p.Arguments), false

	// Plugin-tools (Sigil allow-list, S4b). 1:1 with REST POST/GET/DELETE
	// /v1/plugins/sigils* and permission (keeper.plugin.<action> ↔ plugin.<action>).
	// All three dispatch only when SigilSvc is non-nil (optional HandlerDeps
	// field); otherwise the call method returns "sigil is not configured".
	case "keeper.plugin.allow":
		return h.callPluginAllow(ctx, claims, req, p.Arguments), false
	case "keeper.plugin.revoke":
		return h.callPluginRevoke(ctx, claims, req, p.Arguments), false
	case "keeper.plugin.list":
		return h.callPluginList(ctx, claims, req, p.Arguments), false

	// Sigil-key-tools (signing-key rotation, R3-S7). 3-segment tool name
	// keeper.sigil.key.<verb> ↔ 2-segment permission sigil.key-<verb>. All
	// four dispatch only when SigilKeySvc is non-nil (optional HandlerDeps
	// field); otherwise the call method returns "sigil is not configured".
	case "keeper.sigil.key.introduce":
		return h.callSigilKeyIntroduce(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.list":
		return h.callSigilKeyList(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.set-primary":
		return h.callSigilKeySetPrimary(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.retire":
		return h.callSigilKeyRetire(ctx, claims, req, p.Arguments), false

	// Service-tools (Service registry, ADR-028 RBAC-storage pattern). 1:1 with
	// REST POST/GET/PATCH/DELETE /v1/services* and permission (keeper.service.
	// <action> ↔ service.<action>). All four dispatch only when ServiceSvc is
	// non-nil (optional HandlerDeps field); otherwise the call method returns
	// "service registry is not configured".
	case "keeper.service.register":
		return h.callServiceRegister(ctx, claims, req, p.Arguments), false
	case "keeper.service.update":
		return h.callServiceUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.service.list":
		return h.callServiceList(ctx, claims, req, p.Arguments), false
	case "keeper.service.deregister":
		return h.callServiceDeregister(ctx, claims, req, p.Arguments), false

	// Augur-tools (Omen / Rite registries, ADR-025). 2-segment resource in
	// permission (omen.<action> / rite.<action>) ↔ 4-segment tool name
	// keeper.augur.<resource>.<action>. All six dispatch only when AugurSvc is
	// non-nil (optional HandlerDeps field); otherwise the call method returns
	// "augur registry is not configured".
	case "keeper.augur.omen.create":
		return h.callAugurOmenCreate(ctx, claims, req, p.Arguments), false
	case "keeper.augur.omen.list":
		return h.callAugurOmenList(ctx, claims, req, p.Arguments), false
	case "keeper.augur.omen.delete":
		return h.callAugurOmenDelete(ctx, claims, req, p.Arguments), false
	case "keeper.augur.rite.create":
		return h.callAugurRiteCreate(ctx, claims, req, p.Arguments), false
	case "keeper.augur.rite.list":
		return h.callAugurRiteList(ctx, claims, req, p.Arguments), false
	case "keeper.augur.rite.delete":
		return h.callAugurRiteDelete(ctx, claims, req, p.Arguments), false

	// Oracle-tools (Vigil / Decree registries, ADR-030 beacons). 2-segment
	// resource in permission (vigil.<action> / decree.<action>) ↔ 4-segment
	// tool name keeper.oracle.<resource>.<action>. All six dispatch only when
	// OracleSvc is non-nil (optional HandlerDeps field); otherwise the call
	// method returns "oracle registry is not configured".
	case "keeper.oracle.vigil.create":
		return h.callOracleVigilCreate(ctx, claims, req, p.Arguments), false
	case "keeper.oracle.vigil.list":
		return h.callOracleVigilList(ctx, claims, req, p.Arguments), false
	case "keeper.oracle.vigil.delete":
		return h.callOracleVigilDelete(ctx, claims, req, p.Arguments), false
	case "keeper.oracle.decree.create":
		return h.callOracleDecreeCreate(ctx, claims, req, p.Arguments), false
	case "keeper.oracle.decree.list":
		return h.callOracleDecreeList(ctx, claims, req, p.Arguments), false
	case "keeper.oracle.decree.delete":
		return h.callOracleDecreeDelete(ctx, claims, req, p.Arguments), false

	// Push-tool keeper.push.apply (Variant C orchestrator, docs/keeper/push.md).
	// Dispatches only when PushRun is non-nil (optional HandlerDeps field);
	// otherwise the call method returns "push orchestrator is not configured"
	// (SigilSvc pattern). keeper.push.cleanup remains a stub (separate slice).
	case "keeper.push.apply":
		return h.callPushApply(ctx, claims, req, p.Arguments), false

	// Push-Provider-tools (CRUD push_providers, ADR-032 amendment 2026-05-26,
	// S7-2). 1:1 with REST POST/GET/PUT/DELETE /v1/push-providers* and
	// permission (keeper.push-provider.<verb> ↔ push-provider.<verb>). All
	// five dispatch only when PushProviderSvc is non-nil (optional HandlerDeps
	// field); otherwise the call method returns "push-provider registry is
	// not configured".
	case "keeper.push-provider.create":
		return h.callPushProviderCreate(ctx, claims, req, p.Arguments), false
	case "keeper.push-provider.update":
		return h.callPushProviderUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.push-provider.delete":
		return h.callPushProviderDelete(ctx, claims, req, p.Arguments), false
	case "keeper.push-provider.list":
		return h.callPushProviderList(ctx, claims, req, p.Arguments), false
	case "keeper.push-provider.read":
		return h.callPushProviderRead(ctx, claims, req, p.Arguments), false

	// Cloud Provider / Profile-tools (CRUD providers/profiles registries,
	// ADR-017). 1:1 with REST POST/GET/DELETE /v1/providers* and /v1/profiles*
	// and permission (keeper.provider.<verb> ↔ provider.<verb>,
	// keeper.profile.<verb> ↔ profile.<verb>). Dispatch only when
	// ProviderSvc/ProfileSvc is non-nil (optional HandlerDeps fields);
	// otherwise the call method returns "... registry is not configured".
	// No update (Provider/Profile are immutable).
	case "keeper.provider.create":
		return h.callProviderCreate(ctx, claims, req, p.Arguments), false
	case "keeper.provider.read":
		return h.callProviderRead(ctx, claims, req, p.Arguments), false
	case "keeper.provider.delete":
		return h.callProviderDelete(ctx, claims, req, p.Arguments), false
	case "keeper.provider.list":
		return h.callProviderList(ctx, claims, req, p.Arguments), false
	case "keeper.profile.create":
		return h.callProfileCreate(ctx, claims, req, p.Arguments), false
	case "keeper.profile.read":
		return h.callProfileRead(ctx, claims, req, p.Arguments), false
	case "keeper.profile.delete":
		return h.callProfileDelete(ctx, claims, req, p.Arguments), false
	case "keeper.profile.list":
		return h.callProfileList(ctx, claims, req, p.Arguments), false

	// Herald/Tiding-tools (CRUD notification registries, ADR-052, S4). 1:1 with
	// REST POST/GET/PUT/DELETE /v1/heralds* and /v1/tidings* and permission
	// (keeper.herald.<verb> ↔ herald.<verb>, keeper.tiding.<verb> ↔ tiding.<verb>).
	// All 10 dispatch only when HeraldSvc is non-nil (optional HandlerDeps
	// field); otherwise the call method returns "herald registry is not
	// configured".
	case "keeper.herald.create":
		return h.callHeraldCreate(ctx, claims, req, p.Arguments), false
	case "keeper.herald.update":
		return h.callHeraldUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.herald.delete":
		return h.callHeraldDelete(ctx, claims, req, p.Arguments), false
	case "keeper.herald.list":
		return h.callHeraldList(ctx, claims, req, p.Arguments), false
	case "keeper.herald.read":
		return h.callHeraldRead(ctx, claims, req, p.Arguments), false
	case "keeper.tiding.create":
		return h.callTidingCreate(ctx, claims, req, p.Arguments), false
	case "keeper.tiding.update":
		return h.callTidingUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.tiding.delete":
		return h.callTidingDelete(ctx, claims, req, p.Arguments), false
	case "keeper.tiding.list":
		return h.callTidingList(ctx, claims, req, p.Arguments), false
	case "keeper.tiding.read":
		return h.callTidingRead(ctx, claims, req, p.Arguments), false

	// Errand-tools (ADR-033, slice E4). 1:1 with REST POST /v1/souls/{sid}/exec
	// + GET /v1/errands{,/{errand_id}} and permission (errand.run / errand.list).
	// All three dispatch only when ErrandDispatcher/ErrandStore are non-nil
	// (optional HandlerDeps fields); otherwise the call method returns "errand
	// orchestrator is not configured" (PushRun pattern).
	case "keeper.soul.errand.run":
		return h.callSoulErrandRun(ctx, claims, req, p.Arguments), false
	case "keeper.errand.list":
		return h.callErrandList(ctx, claims, req, p.Arguments), false
	case "keeper.errand.get":
		return h.callErrandGet(ctx, claims, req, p.Arguments), false
	case "keeper.errand.cancel":
		return h.callErrandCancel(ctx, claims, req, p.Arguments), false

	// Voyage-tools (ADR-043, S5). 4 tools: start/list/get/cancel. Unified
	// SSE-progress is deferred (MCP clients poll get instead). All dispatch
	// only when Voyage-deps are non-nil (VoyageDB + resolvers); otherwise the
	// call method returns "voyage orchestrator is not configured" (ErrandRun
	// -tools pattern).
	case "keeper.voyage.start":
		return h.callVoyageStart(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.list":
		return h.callVoyageList(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.get":
		return h.callVoyageGet(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.cancel":
		return h.callVoyageCancel(ctx, claims, req, p.Arguments), false
	}

	// Unreachable: status=Implemented only for tools dispatched above.
	return h.toolError(req.ID, p.Name, mcpCodeInternalError,
		"tool declared implemented but dispatch missing"), false
}

// incarnationRBACContext — context map for [PermissionChecker.Check] on
// incarnation-tools. Mirrors REST selectors (rbac.md § Selectors,
// handlers.IncarnationNameSelector):
//   - list — no selector (nil): list has no name targeting, REST also
//     applies NoSelector (see router.go).
//
// Single-incarnation tools (get/history/run/upgrade/destroy) do NOT use this
// helper for the final check: they mirror REST coven/service-scope via
// [Handler.checkIncarnationScope] (OR-Check over covens ∪ {name}). The helper
// remains for list semantics (nil context).
func incarnationRBACContext(name string) map[string]string {
	if name == "" {
		return nil
	}
	return map[string]string{"incarnation": name}
}

// checkIncarnationScope — RBAC OR-Check for single-incarnation tools (get /
// history / run / upgrade / destroy). Mirrors REST [middleware.
// RequirePermissionMulti] + [handlers.IncarnationScopeSelector]: effective
// coven-scope = covens ∪ {name}, each candidate becomes a separate context
// `{incarnation, service, coven}`, granted if ANY ONE matches (bare/`*` on
// any, scoped only within its own scope label). Without this, a coven-scoped
// operator could bypass REST protection through MCP.
//
// Contexts are built with the same [handlers.IncarnationCovenContexts] as
// REST (single source of truth, matching uses the existing enforcer). An
// empty set (malformed name) → one attempt with nil-context: bare/`*` pass,
// scoped is denied (fail-closed, parity with middleware).
func (h *Handler) checkIncarnationScope(claims *jwt.Claims, action, name, service string, covens []string) error {
	contexts := handlers.IncarnationCovenContexts(name, service, covens)
	if len(contexts) == 0 {
		contexts = []map[string]string{nil}
	}
	var lastErr error
	for _, ctx := range contexts {
		err := h.deps.RBAC.Check(claims.Subject, "incarnation", action, ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// toolError builds an MCP-tool error response over the JSON-RPC error
// channel: code=rpcCodeInternalError (generic for tool-execution per
// MCP-spec), detail in message, semantic URN-suffix in data.code.
func (h *Handler) toolError(id json.RawMessage, toolName, code, detail string) jsonRPCResponse {
	return newRPCError(id, rpcCodeInternalError, detail, mcpToolError{
		Code:     code,
		Instance: "tool:" + toolName,
	})
}

// --- Operator-tool implementations (3) ---

type operatorCreateArgs struct {
	AID         string `json:"aid"`
	DisplayName string `json:"display_name"`
}

type operatorCreateOutput struct {
	AID          string    `json:"aid"`
	DisplayName  string    `json:"display_name"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedByAID string    `json:"created_by_aid"`
	JWT          string    `json:"jwt"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (h *Handler) callOperatorCreate(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.operator.create"

	var a operatorCreateArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'aid' must match "+operator.AIDPattern)
	}
	if a.DisplayName == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'display_name' is required")
	}

	// RBAC check — `operator.create` has no selector (NoSelector
	// equivalent: empty context).
	if err := h.deps.RBAC.Check(claims.Subject, "operator", "create", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission operator.create")
	}

	res, err := h.deps.OperatorSvc.Create(ctx, operator.CreateInput{
		AID:         a.AID,
		DisplayName: a.DisplayName,
		CallerAID:   claims.Subject,
	})
	if err != nil {
		code, detail := mapServiceErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: operator.create service failed",
				slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	// Audit write mirrors the HTTP handler: payload {aid, display_name,
	// auth_method, created_by_aid}. JWT is sensitive, not written.
	h.writeAudit(audit.EventOperatorCreated, claims.Subject, map[string]any{
		"aid":            res.AID,
		"display_name":   res.DisplayName,
		"auth_method":    string(res.AuthMethod),
		"created_by_aid": res.CreatedByAID,
	})

	return h.toolResult(req.ID, operatorCreateOutput{
		AID:          res.AID,
		DisplayName:  res.DisplayName,
		CreatedAt:    res.CreatedAt,
		CreatedByAID: res.CreatedByAID,
		JWT:          res.JWT,
		ExpiresAt:    res.ExpiresAt,
	})
}

type operatorRevokeArgs struct {
	AID    string `json:"aid"`
	Reason string `json:"reason"`
}

func (h *Handler) callOperatorRevoke(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.operator.revoke"

	var a operatorRevokeArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'aid' must match "+operator.AIDPattern)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "operator", "revoke", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission operator.revoke")
	}

	err := h.deps.OperatorSvc.Revoke(ctx, operator.RevokeInput{
		AID:       a.AID,
		Reason:    a.Reason,
		CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapServiceErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: operator.revoke service failed",
				slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	payload := map[string]any{"aid": a.AID}
	if a.Reason != "" {
		payload["reason"] = a.Reason
	}
	h.writeAudit(audit.EventOperatorRevoked, claims.Subject, payload)

	// HTTP returns 204 No Content; MCP equivalent is an empty output object.
	return h.toolResult(req.ID, struct{}{})
}

type operatorIssueTokenArgs struct {
	AID string `json:"aid"`
}

type operatorIssueTokenOutput struct {
	AID       string    `json:"aid"`
	JWT       string    `json:"jwt"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *Handler) callOperatorIssueToken(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.operator.issue-token"

	var a operatorIssueTokenArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.AID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'aid' is required")
	}
	if !operator.ValidAID(a.AID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'aid' must match "+operator.AIDPattern)
	}

	if err := h.deps.RBAC.Check(claims.Subject, "operator", "issue-token", nil); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission operator.issue-token")
	}

	res, err := h.deps.OperatorSvc.IssueToken(ctx, operator.IssueTokenInput{
		AID:       a.AID,
		CallerAID: claims.Subject,
	})
	if err != nil {
		code, detail := mapServiceErrorToMCP(err)
		if code == mcpCodeInternalError {
			h.deps.Logger.Error("mcp: operator.issue-token service failed",
				slog.String("aid", a.AID),
				slog.String("by_aid", claims.Subject),
				slog.Any("error", err),
			)
		}
		return h.toolError(req.ID, toolName, code, detail)
	}

	h.writeAudit(audit.EventOperatorTokenIssued, claims.Subject, map[string]any{
		"aid":        res.AID,
		"expires_at": res.ExpiresAt.Format(time.RFC3339),
	})

	return h.toolResult(req.ID, operatorIssueTokenOutput{
		AID:       res.AID,
		JWT:       res.JWT,
		ExpiresAt: res.ExpiresAt,
	})
}

// toolResult serializes typed output into the MCP-spec `toolsCallResult`
// (structuredContent + text representation in content[0].text for legacy
// clients that don't read structuredContent).
func (h *Handler) toolResult(id json.RawMessage, out any) jsonRPCResponse {
	raw, err := json.Marshal(out)
	if err != nil {
		h.deps.Logger.Error("mcp: tool result marshal failed", slog.Any("error", err))
		return newRPCError(id, rpcCodeInternalError, "tool result marshal failed", nil)
	}
	res := toolsCallResult{
		Content: []toolContentBlock{
			{Type: "text", Text: string(raw)},
		},
		StructuredContent: raw,
	}
	resRaw, err := json.Marshal(res)
	if err != nil {
		h.deps.Logger.Error("mcp: tools/call result marshal failed", slog.Any("error", err))
		return newRPCError(id, rpcCodeInternalError, "tools/call result marshal failed", nil)
	}
	return newRPCResult(id, resRaw)
}

// writeAudit writes an audit event best-effort: write errors are logged but
// not returned to the client (the operation already completed). Mirrors
// HTTP audit-middleware.
//
// Source is [audit.SourceMCP] per ADR-022(b): the MCP channel is separated
// from HTTP in audit_log for a granular trail (`WHERE source='mcp'`).
// Background context has the same rationale as HTTP audit-middleware: the
// client may have disconnected, audit must not be lost. The tools/call-level
// ctx param isn't needed here — it arrives in M0.7.c with cancellation / SSE.
func (h *Handler) writeAudit(eventType audit.EventType, aid string, payload map[string]any) {
	ev := &audit.Event{
		EventType: eventType,
		Source:    audit.SourceMCP,
		ArchonAID: aid,
		Payload:   payload,
	}
	if err := h.deps.AuditWriter.Write(context.Background(), ev); err != nil {
		h.deps.Logger.Error("mcp: audit write failed",
			slog.String("event_type", string(eventType)),
			slog.String("archon_aid", aid),
			slog.Any("error", err),
		)
	}
}

// writeAuditCorrelated extends writeAudit with a CorrelationID (run/check
// ULID). Needed for events that must coalesce with a task.executed /
// run.completed chain (e.g. `incarnation.drift_checked` → all task.executed
// events within one check-drift). Background context has the same rationale
// as writeAudit (client may have disconnected).
func (h *Handler) writeAuditCorrelated(eventType audit.EventType, aid, correlationID string, payload map[string]any) {
	ev := &audit.Event{
		EventType:     eventType,
		Source:        audit.SourceMCP,
		ArchonAID:     aid,
		CorrelationID: correlationID,
		Payload:       payload,
	}
	if err := h.deps.AuditWriter.Write(context.Background(), ev); err != nil {
		h.deps.Logger.Error("mcp: audit write failed",
			slog.String("event_type", string(eventType)),
			slog.String("archon_aid", aid),
			slog.String("correlation_id", correlationID),
			slog.Any("error", err),
		)
	}
}

// strictUnmarshal is json.Unmarshal with DisallowUnknownFields, so a client
// sending an extra field in arguments gets MalformedRequest instead of
// silent drop (security-defensive by default).
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// rawArgHasKey reports whether a top-level key is present in the args JSON
// object. Distinguishes an omitted field from explicit null (different PATCH
// semantics for default_scope in role.update). args already passed
// strictUnmarshal above, so malformed JSON doesn't reach here — parse errors
// are swallowed. Empty args → false.
func rawArgHasKey(args json.RawMessage, key string) bool {
	if len(args) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(args, &top); err != nil {
		return false
	}
	_, ok := top[key]
	return ok
}
