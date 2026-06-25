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

// AuditWriter — узкая поверхность над [audit.Writer]. Сужение нужно,
// чтобы тест-mock-и не тянули pgx.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// PermissionChecker — узкая поверхность над rbac.Enforcer/Holder, нужная
// MCP-сервису для RBAC-проверки до tool-execution.
type PermissionChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// HandlerDeps — зависимости MCP-handler-а (диспетчера tools/call).
//
// OperatorSvc передаётся из того же экземпляра, что использует HTTP-сервер,
// — один источник правды (PM-decision M0.7 #6).
//
// AuditWriter и RBAC — те же, что HTTP. EventType-ы для operator-tools
// совпадают с HTTP-handler-ами; source различается (ADR-022(b),
// audit.SourceMCP) — это нужно для granular audit trail
// (`WHERE source='mcp'` отделяет MCP от HTTP).
//
// Incarnation-зависимости — те же значения, что api.Deps прокидывает в
// IncarnationHandler (single source of truth, не дубль конструирования):
//   - IncarnationDB    — ОБЯЗАТЕЛЬНА (валидируется в NewHandler): нужна всем
//     incarnation-tools (get/list/history требуют только её).
//   - ScenarioRunner   — опц. (create/run). nil → tool вернёт internal-error
//     «scenario runner is not configured» (симметрично REST 500).
//   - ServiceRegistry  — опц. (create/run/upgrade). nil → аналогично.
//   - ServiceLoader    — опц. (upgrade). nil → аналогично.
//
// Интерфейсы переиспользованы из пакета handlers (handlers.IncarnationDB
// и т.д.) — не копируются; реальные значения (*pgxpool.Pool / scenario.Runner
// / scenario.ServiceRegistry / artifact.ServiceLoader) удовлетворяют им.
type HandlerDeps struct {
	OperatorSvc *operator.Service
	RBAC        PermissionChecker

	// RBACRoles — мутирующий RBAC-CRUD-фасад ([rbac.Service]) для будущих
	// role.*-tools (Slice 2b). НЕ путать с RBAC выше: тот — read-only
	// PermissionChecker (Check до tool-execution), этот — CreateRole /
	// DeleteRole / GrantOperator / ListRoles и т.п. При nil role.*-tools
	// просто не диспатчатся (Slice 1.5 поле прокидывает, tools регистрирует
	// Slice 2b).
	RBACRoles *rbac.Service

	// SigilSvc — бизнес-логика Sigil allow-list-а ([sigil.Service]) для
	// plugin.*-tools (S4b). Тот же экземпляр, что REST прокидывает в
	// api.Deps.SigilSvc (single source of truth). При nil plugin.*-tools
	// диспатчатся, но возвращают internal-error «не сконфигурировано»
	// (паттерн RBACRoles): keeper стартует с выключенным Sigil без паники.
	SigilSvc *sigil.Service

	// SigilKeySvc — бизнес-логика ротации ключей подписи Sigil ([sigil.KeyService])
	// для sigil.key.*-tools (R3-S7). Тот же экземпляр, что REST прокидывает в
	// api.Deps.SigilKeySvc (single source of truth). При nil sigil.key.*-tools
	// диспатчатся, но возвращают internal-error «не сконфигурировано»
	// (паттерн SigilSvc).
	SigilKeySvc *sigil.KeyService

	// ServiceSvc — бизнес-логика реестра Service-ов ([serviceregistry.Service])
	// для service.*-tools (ADR-028-паттерн RBAC-storage). Тот же экземпляр, что
	// REST прокидывает в api.Deps.ServiceSvc (single source of truth, несёт
	// S2-invalidate-хук). При nil service.*-tools диспатчатся, но возвращают
	// internal-error «не сконфигурировано» (паттерн RBACRoles/SigilSvc).
	ServiceSvc *serviceregistry.Service

	// AugurSvc — management-логика реестра Augur ([augur.Service]) для
	// augur.omen.*/augur.rite.*-tools (ADR-025, augur.md §4). Тот же экземпляр,
	// что REST прокидывает в api.Deps.AugurSvc (single source of truth). При nil
	// augur.*-tools диспатчатся, но возвращают internal-error «не
	// сконфигурировано» (паттерн ServiceSvc). НЕ путать с Augur-брокером
	// (resolve/broker), который резолвит AugurRequest от Soul-а по gRPC.
	AugurSvc *augur.Service

	// OracleSvc — management-логика реестров Oracle ([oracle.Service]) для
	// oracle.vigil.*/oracle.decree.*-tools (ADR-030, beacons S3). Тот же
	// экземпляр, что REST прокидывает в api.Deps.OracleSvc (single source of
	// truth). При nil oracle.*-tools диспатчатся, но возвращают internal-error
	// «не сконфигурировано» (паттерн AugurSvc). НЕ путать с reactor-роутером
	// (match/enqueue), который резолвит Portent от Soul-а по gRPC.
	OracleSvc *oracle.Service

	AuditWriter AuditWriter
	Logger      *slog.Logger

	IncarnationDB   handlers.IncarnationDB
	ScenarioRunner  handlers.ScenarioStarter
	ServiceRegistry handlers.ServiceResolver
	ServiceLoader   handlers.ServiceSnapshotLoader

	// ScenarioDestroyer — узкая поверхность scenario.Runner для destroy-tool-а
	// (async-teardown scenario `destroy` в TerminalDestroy, S-D4). Отдельное
	// поле от ScenarioRunner (StartDestroy вместо Start), хотя production-wire-up
	// передаёт тот же *scenario.Runner. nil → keeper.incarnation.destroy вернёт
	// internal-error «destroy is not configured» (симметрично REST 500).
	ScenarioDestroyer handlers.DestroyStarter

	// ScenarioDrift — узкая поверхность scenario.Runner для check-drift-tool-а
	// (Scry on-demand-пилот, ADR-031 Slice B). CheckDrift sync (не async, в
	// отличие от Start/StartDestroy). Production-wire-up передаёт тот же
	// *scenario.Runner. nil → keeper.incarnation.check-drift вернёт internal-error
	// «drift checker is not configured» (симметрично REST 500).
	ScenarioDrift handlers.DriftChecker

	// SoulDB — тот же [handlers.SoulPool] (`*pgxpool.Pool`), что REST прокидывает
	// в SoulHandler (single source of truth). Нужна soul-tools (create /
	// issue-token); read-only soul.list остаётся stub. При nil soul-tools
	// возвращают internal-error «soul DB is not configured» (симметрично REST,
	// где SoulHandler без pool не монтируется).
	SoulDB handlers.SoulPool

	// PurviewResolver — read-поверхность scope-границы оператора для bulk
	// keeper.soul.coven-assign (scope-intersection селектора и метки с правами
	// оператора). Тот же [rbac.Holder], что REST прокидывает в NewSoulHandler
	// (single source of truth; реализует [handlers.PurviewResolver]). coven-
	// измерение Purview даёт прежнюю (covens, unrestricted)-форму. При nil
	// coven-assign-tool вернёт internal-error «coven-assign unavailable»
	// (симметрично REST, где nil scoper отдаёт 500). НЕ путать с RBAC выше:
	// тот — permission-гейт (Check), этот — объём массовой мутации coven.
	PurviewResolver handlers.PurviewResolver

	// PushRun — multi-host push-orchestrator (Variant C, docs/keeper/push.md)
	// для keeper.push.apply tool-а. Тот же экземпляр, что REST прокидывает в
	// api.Deps.PushRun (single source of truth). При nil push-tools диспатчатся,
	// но возвращают internal-error «не сконфигурировано» (паттерн SigilSvc).
	PushRun *pushorch.PushRun

	// PushProviderSvc — бизнес-логика CRUD реестра Push-Provider-ов
	// (push-provider.create/update/delete/list/read tools, ADR-032 amendment
	// 2026-05-26, S7-2). Тот же экземпляр, что REST прокидывает в
	// api.Deps.PushProviderSvc (single source of truth). При nil
	// push-provider.*-tools диспатчатся, но возвращают internal-error
	// «push-provider registry is not configured» (паттерн PushRun).
	PushProviderSvc *pushprovider.Service

	// HeraldSvc — бизнес-логика CRUD реестров Herald (каналы) / Tiding (правила)
	// уведомлений (keeper.herald.* / keeper.tiding.*, ADR-052, S4). Тот же
	// экземпляр, что REST прокидывает в api.Deps.HeraldSvc (single source of
	// truth). При nil herald/tiding-tools диспатчатся, но возвращают internal-
	// error «herald registry is not configured» (паттерн PushProviderSvc).
	HeraldSvc *herald.Service

	// ProviderSvc / ProfileSvc — Cloud-CRUD-фасады (keeper.provider.* /
	// keeper.profile.*, ADR-017). Те же экземпляры, что REST прокидывает в
	// api.Deps.ProviderSvc/ProfileSvc (single source of truth). При nil
	// соответствующие tools диспатчатся, но возвращают internal-error
	// «provider/profile registry is not configured» (паттерн PushProviderSvc).
	ProviderSvc *provider.Service
	ProfileSvc  *profile.Service

	// ErrandDispatcher / ErrandStore — pull-ad-hoc Errand contour (ADR-033) для
	// keeper.soul.errand.run / keeper.errand.list / keeper.errand.get. Те же
	// экземпляры, что REST прокидывает в api.Deps.ErrandDispatcher / ErrandStore
	// (single source of truth). При nil errand-tools диспатчатся, но возвращают
	// internal-error «не сконфигурировано» (паттерн SigilSvc).
	ErrandDispatcher *errand.Dispatcher
	ErrandStore      *errand.Store

	// VoyageDB / VoyageScenarioResolver / VoyageCommandResolver — Voyage contour
	// (ADR-043, S5) для keeper.voyage.{start,list,get} (+ cancel) tool-ов. Те же
	// экземпляры, что REST прокидывает в api.Deps.VoyageDB / VoyageScenarioResolver
	// / VoyageCommandResolver (single source of truth, *pgxpool.Pool + PG-резолверы).
	// При nil voyage-tools возвращают internal-error «voyage orchestrator is not
	// configured» (паттерн ErrandRunStore/ErrandRunSouls). RBAC-by-kind делает сам
	// VoyageHandler (enforcer=RBAC, IncarnationDB — per-incarnation scope-check).
	VoyageDB               handlers.VoyageStore
	VoyageScenarioResolver handlers.VoyageScenarioResolver
	VoyageCommandResolver  handlers.VoyageCommandResolver
	// VoyageMaxScope — верхний лимит размера резолвнутого scope (DoS-guard
	// S-med-3); 0 → безлимит. Тот же источник, что REST: cfg.Voyage.ResolvedMaxScope().
	VoyageMaxScope int
	// VoyageMaxBatchSize — верхний предел размера батча/окна (DoS-guard S-W4);
	// 0 → без предела. Тот же источник, что REST: cfg.Voyage.ResolvedMaxBatchSize().
	VoyageMaxBatchSize int
}

// Handler — диспетчер JSON-RPC методов MCP-сервера. Один экземпляр на
// listener, потокобезопасен (deps immutable).
type Handler struct {
	deps HandlerDeps
}

// NewHandler собирает диспетчер. Возвращает ошибку, если obligated-deps
// нулевые.
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

// Dispatch — обрабатывает один JSON-RPC request и возвращает response.
// claims — non-nil (auth-middleware гарантирует); используется для
// archon_aid в audit-event-ах и AID в RBAC.Check.
//
// Контракт по MCP-spec:
//
//   - `initialize`         — handshake; возвращает capabilities.
//   - `tools/list`         — список tool-declarations.
//   - `tools/call`         — выполнение tool-а с arguments.
//   - `notifications/*`    — игнорируем (id отсутствует, ничего не возвращаем).
//   - неизвестный method   — MethodNotFound (-32601).
func (h *Handler) Dispatch(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest) (resp jsonRPCResponse, isNotification bool) {
	if req.JSONRPC != jsonRPCVersion {
		return newRPCError(req.ID, rpcCodeInvalidRequest,
			"unsupported jsonrpc version; expected \""+jsonRPCVersion+"\"", nil), false
	}

	// Notifications (no id) — спецификация JSON-RPC 2.0 §4.1: server must
	// not reply. Принимаем и тихо игнорируем (e.g. `notifications/initialized`).
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

// isNotificationMethod — true для методов, у которых не должно быть
// response по MCP-spec. MCP-clients посылают `notifications/initialized`
// после handshake и (в дальнейшем) `notifications/cancelled` для отмены
// long-running tool-call-ов.
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
	// listChanged=false — MCP-server не отправляет
	// `notifications/tools/list_changed` (каталог tools статичен в
	// рамках процесса). При расширении каталога останется
	// false до появления hot-reload-а tool-catalog.
	ListChanged bool `json:"listChanged"`
}

// mcpProtocolVersion — версия MCP spec, против которой сделан сервер.
// MCP клиенты сравнивают эту строку с собственной поддерживаемой
// версией; на mismatch клиент сам решает, что делать (большинство
// продолжает работать на best-effort).
const mcpProtocolVersion = "2025-06-18"

// serverInfoName / serverInfoVersion — идентичность Keeper-сервера для
// MCP-handshake. Version фиксированная "0.7" до появления build-info
// инфраструктуры (CI-таггинг — отдельный slice).
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

// toolsCallParams — params JSON-RPC `tools/call`-метода по MCP-spec.
//
// arguments — произвольный JSON-объект (типизация per-tool делается в
// dispatch-фазе через json.Unmarshal в соответствующую input-struct).
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult — MCP-spec форма успешного результата tool-call-а.
// `content` — массив content-block-ов (MCP-spec поддерживает text/image/
// resource); для наших structured outputs мы используем один text-block с
// JSON-сериализованным structuredContent.
// `structuredContent` — typed output согласно outputSchema из tool-declaration
// (MCP-spec 2025-06: server SHOULD include structuredContent если есть
// outputSchema).
// `isError` — bool, MCP-клиент использует для UI-уведомления; для
// tool-execution-ошибок мы НЕ ставим isError=true, потому что используем
// JSON-RPC-error-channel (см. mapServiceErrorToMCP).
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

	// Stub-tools (incarnation/soul/push/cloud) — не реализованы в M0.7.a.
	// Возвращаем `internal-error` с `data.code: "not-implemented"` —
	// клиент-агент видит, что tool существует в каталоге, но handler ещё
	// не подключён. Это лучше, чем `MethodNotFound`, чтобы LLM не
	// удалял tool из памяти.
	if entry.status == toolStatusStub {
		return h.toolError(req.ID, p.Name, mcpCodeNotImplemented,
			"tool not implemented in this build (M0.7.a)"), false
	}

	// Implemented-tools — диспетчер по name.
	switch p.Name {
	case "keeper.operator.create":
		return h.callOperatorCreate(ctx, claims, req, p.Arguments), false
	case "keeper.operator.revoke":
		return h.callOperatorRevoke(ctx, claims, req, p.Arguments), false
	case "keeper.operator.issue-token":
		return h.callOperatorIssueToken(ctx, claims, req, p.Arguments), false

	// Role-tools (6) — RBAC-CRUD поверх rbac.Service (Slice 2b). 1:1 с
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
	// synod.<action>. Тот же *rbac.Service (RBACRoles), что у role-tools.
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

	// Incarnation-tools (все 8 реализованы: тираж C7 по пилоту get + destroy
	// в S-D4). Отдельные case-строки (по рекомендации review): каждый tool —
	// своя ветка.
	case "keeper.incarnation.get":
		return h.callIncarnationGet(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.list":
		return h.callIncarnationList(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.history":
		return h.callIncarnationHistory(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.unlock":
		return h.callIncarnationUnlock(ctx, claims, req, p.Arguments), false
	case "keeper.incarnation.rerun-create":
		return h.callIncarnationRerunCreate(ctx, claims, req, p.Arguments), false
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

	// Soul-tools (паритет REST POST /v1/souls + issue-token). soul.list
	// остаётся stub (ловится status==toolStatusStub выше). 1:1 с permission
	// (keeper.soul.<action> ↔ soul.<action>).
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

	// Plugin-tools (Sigil allow-list, S4b). 1:1 с REST POST/GET/DELETE
	// /v1/plugins/sigils* и permission (keeper.plugin.<action> ↔ plugin.<action>).
	// Все три диспатчатся только при непустом SigilSvc (опц. поле HandlerDeps);
	// иначе call-метод вернёт «sigil is not configured».
	case "keeper.plugin.allow":
		return h.callPluginAllow(ctx, claims, req, p.Arguments), false
	case "keeper.plugin.revoke":
		return h.callPluginRevoke(ctx, claims, req, p.Arguments), false
	case "keeper.plugin.list":
		return h.callPluginList(ctx, claims, req, p.Arguments), false

	// Sigil-key-tools (ротация ключей подписи, R3-S7). 3-сегментный tool-name
	// keeper.sigil.key.<verb> ↔ 2-сегментная permission sigil.key-<verb>. Все
	// четыре диспатчатся только при непустом SigilKeySvc (опц. поле HandlerDeps);
	// иначе call-метод вернёт «sigil is not configured».
	case "keeper.sigil.key.introduce":
		return h.callSigilKeyIntroduce(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.list":
		return h.callSigilKeyList(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.set-primary":
		return h.callSigilKeySetPrimary(ctx, claims, req, p.Arguments), false
	case "keeper.sigil.key.retire":
		return h.callSigilKeyRetire(ctx, claims, req, p.Arguments), false

	// Service-tools (реестр Service-ов, ADR-028-паттерн RBAC-storage). 1:1 с
	// REST POST/GET/PATCH/DELETE /v1/services* и permission (keeper.service.
	// <action> ↔ service.<action>). Все четыре диспатчатся только при непустом
	// ServiceSvc (опц. поле HandlerDeps); иначе call-метод вернёт «service
	// registry is not configured».
	case "keeper.service.register":
		return h.callServiceRegister(ctx, claims, req, p.Arguments), false
	case "keeper.service.update":
		return h.callServiceUpdate(ctx, claims, req, p.Arguments), false
	case "keeper.service.list":
		return h.callServiceList(ctx, claims, req, p.Arguments), false
	case "keeper.service.deregister":
		return h.callServiceDeregister(ctx, claims, req, p.Arguments), false

	// Augur-tools (реестры Omen / Rite, ADR-025). 2-сегментный resource в
	// permission (omen.<action> / rite.<action>) ↔ 4-сегментный tool-name
	// keeper.augur.<resource>.<action>. Все шесть диспатчатся только при непустом
	// AugurSvc (опц. поле HandlerDeps); иначе call-метод вернёт «augur registry
	// is not configured».
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

	// Oracle-tools (реестры Vigil / Decree, ADR-030 beacons). 2-сегментный
	// resource в permission (vigil.<action> / decree.<action>) ↔ 4-сегментный
	// tool-name keeper.oracle.<resource>.<action>. Все шесть диспатчатся только
	// при непустом OracleSvc (опц. поле HandlerDeps); иначе call-метод вернёт
	// «oracle registry is not configured».
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
	// Диспатчится только при непустом PushRun (опц. поле HandlerDeps); иначе
	// call-метод возвращает «push orchestrator is not configured» (паттерн
	// SigilSvc). keeper.push.cleanup пока остаётся stub (отдельный slice).
	case "keeper.push.apply":
		return h.callPushApply(ctx, claims, req, p.Arguments), false

	// Push-Provider-tools (CRUD push_providers, ADR-032 amendment 2026-05-26,
	// S7-2). 1:1 с REST POST/GET/PUT/DELETE /v1/push-providers* и permission
	// (keeper.push-provider.<verb> ↔ push-provider.<verb>). Все пять
	// диспатчатся только при непустом PushProviderSvc (опц. поле HandlerDeps);
	// иначе call-метод вернёт «push-provider registry is not configured».
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

	// Cloud Provider / Profile-tools (CRUD реестров providers/profiles, ADR-017).
	// 1:1 с REST POST/GET/DELETE /v1/providers* и /v1/profiles* и permission
	// (keeper.provider.<verb> ↔ provider.<verb>, keeper.profile.<verb> ↔
	// profile.<verb>). Диспатчатся только при непустом ProviderSvc/ProfileSvc
	// (опц. поля HandlerDeps); иначе call-метод вернёт «… registry is not
	// configured». БЕЗ update (Provider/Profile иммутабельны).
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

	// Herald/Tiding-tools (CRUD реестров уведомлений, ADR-052, S4). 1:1 с REST
	// POST/GET/PUT/DELETE /v1/heralds* и /v1/tidings* и permission
	// (keeper.herald.<verb> ↔ herald.<verb>, keeper.tiding.<verb> ↔ tiding.<verb>).
	// Все 10 диспатчатся только при непустом HeraldSvc (опц. поле HandlerDeps);
	// иначе call-метод вернёт «herald registry is not configured».
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

	// Errand-tools (ADR-033, slice E4). 1:1 с REST POST /v1/souls/{sid}/exec +
	// GET /v1/errands{,/{errand_id}} и permission (errand.run / errand.list).
	// Все три диспатчатся только при непустых ErrandDispatcher/ErrandStore
	// (опц. поля HandlerDeps); иначе call-метод возвращает «errand orchestrator
	// is not configured» (паттерн PushRun).
	case "keeper.soul.errand.run":
		return h.callSoulErrandRun(ctx, claims, req, p.Arguments), false
	case "keeper.errand.list":
		return h.callErrandList(ctx, claims, req, p.Arguments), false
	case "keeper.errand.get":
		return h.callErrandGet(ctx, claims, req, p.Arguments), false
	case "keeper.errand.cancel":
		return h.callErrandCancel(ctx, claims, req, p.Arguments), false

	// Voyage-tools (ADR-043, S5). 4 tools: start/list/get/cancel. SSE-progress
	// единого вида отложен (MCP-clients используют polling get). Все диспатчатся
	// только при непустых Voyage-deps (VoyageDB + резолверы); иначе call-метод
	// вернёт «voyage orchestrator is not configured» (паттерн ErrandRun-tools).
	case "keeper.voyage.start":
		return h.callVoyageStart(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.list":
		return h.callVoyageList(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.get":
		return h.callVoyageGet(ctx, claims, req, p.Arguments), false
	case "keeper.voyage.cancel":
		return h.callVoyageCancel(ctx, claims, req, p.Arguments), false
	}

	// Unreachable: status=Implemented только у задиспатченных выше tools.
	return h.toolError(req.ID, p.Name, mcpCodeInternalError,
		"tool declared implemented but dispatch missing"), false
}

// incarnationRBACContext — context-map для [PermissionChecker.Check] по
// incarnation-tool-ам. Паритет с REST-селекторами (rbac.md § Селекторы,
// handlers.IncarnationNameSelector):
//   - list — без селектора (nil): list без таргетинга по имени, REST тоже
//     навешивает NoSelector (см. router.go).
//
// Single-incarnation tools (get/history/run/upgrade/destroy) НЕ используют
// этот helper для финальной проверки: они зеркалят REST coven/service-scope
// через [Handler.checkIncarnationScope] (OR-Check по covens ∪ {name}). Helper
// остаётся для list-семантики (nil-context).
func incarnationRBACContext(name string) map[string]string {
	if name == "" {
		return nil
	}
	return map[string]string{"incarnation": name}
}

// checkIncarnationScope — RBAC OR-Check для single-incarnation tools (get /
// history / run / upgrade / destroy). Зеркало REST [middleware.
// RequirePermissionMulti] + [handlers.IncarnationScopeSelector]: эффективный
// coven-scope = covens ∪ {name}, каждый кандидат → отдельный context
// `{incarnation, service, coven}`, грант если матчит ХОТЯ БЫ ОДИН (bare/`*` —
// при любом, scoped — только при метке в своём scope). Без этого
// coven-scoped оператор обходил бы REST-защиту через MCP.
//
// Контексты строятся тем же [handlers.IncarnationCovenContexts], что и REST
// (single source of truth, матчинг — существующий enforcer). Пустой набор
// (битый name) → одна попытка с nil-context: bare/`*` пройдут, scoped — deny
// (fail-closed, паритет middleware).
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

// toolError собирает MCP-tool error response. JSON-RPC-error-channel:
// code=rpcCodeInternalError (общий-для-tool-execution per MCP-spec),
// detail в message, smysловой URN-suffix в data.code.
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

	// RBAC-check — `operator.create` без селектора (NoSelector
	// эквивалент: пустой context).
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

	// Audit-write — параллельно HTTP-handler-у: payload {aid, display_name,
	// auth_method, created_by_aid}. JWT — sensitive, не пишем.
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

	// HTTP возвращает 204 No Content; MCP-эквивалент — пустой output-объект.
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

// toolResult сериализует typed-output в MCP-spec `toolsCallResult`
// (structuredContent + текстовое представление в content[0].text для
// legacy-клиентов, которые не читают structuredContent).
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

// writeAudit пишет audit-event «best-effort»: ошибка записи логируется,
// но клиенту НЕ возвращается (операция уже выполнена). Симметрично
// HTTP-middleware audit.go.
//
// Source — [audit.SourceMCP] по ADR-022(b): MCP-канал отделён от HTTP в
// audit_log для granular trail (`WHERE source='mcp'`). Background-context
// тот же resolved-смысл, что у HTTP audit-middleware: клиент мог разорвать
// соединение, audit терять нельзя. Параметр ctx из tools/call-уровня не
// нужен — добавится в M0.7.c, когда появится cancellation / SSE-сторона.
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

// writeAuditCorrelated — расширение writeAudit с CorrelationID (ULID
// прогона/проверки). Нужно для событий, которые нужно coalescить с цепочкой
// task.executed / run.completed (например `incarnation.drift_checked` → все
// task.executed-events внутри одного check-drift). Background-context тот же
// resolved-смысл, что у writeAudit (клиент мог разорвать соединение).
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

// strictUnmarshal — json.Unmarshal с DisallowUnknownFields. Используется,
// чтобы клиент, передавший лишнее поле в arguments, получил MalformedRequest,
// а не молчаливое отбрасывание (security-defensive по умолчанию).
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// rawArgHasKey — присутствует ли ключ верхнего уровня в JSON-объекте args.
// Отличает omitted-поле от explicit-null (разная PATCH-семантика для
// default_scope в role.update). args уже прошёл strictUnmarshal выше, битый
// JSON сюда не доходит — ошибку парсинга глотаем. Пустые args → false.
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
