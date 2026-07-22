// Package cloud implements keeper-side core module `core.cloud.provisioned`
// (ADR-017, docs/keeper/cloud.md).
//
// States:
//   - created: CloudDriver.Create via PluginHost → []VmInfo → INSERT into
//     souls (status: pending) + INSERT bootstrap_tokens (one per VM).
//     Output: hosts: [{sid, vm_id, primary_ip, attributes}].
//   - destroyed: PluginHost.Destroy(vm_ids) → cascade in single PG transaction
//     (ADR-017 cascade): souls→destroyed + active soul_seeds→orphaned +
//     active bootstrap_tokens→burned. Output: destroyed_vm_ids + sids +
//     cascade-counts.
//   - resized: PluginHost.Resize(vm_ids, desired) — driver expands VM resources
//     (cpu/ram/disk, our units). Keeper-agnostic to stop/start: driver encapsulates
//     full sequence. Database untouched (resize does not change souls registry).
//     Output: results[{vm_id, caused_downtime, error}].
//     Driver without Resizable-capability → resize.unsupported.
//
// Replaces pattern "destiny `cloud-provision` with `on: keeper`" (ADR-017):
// this is keeper-side operation, not task package for Soul.
package cloud

import (
	"context"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// VMNameBasePattern is the form of VM base name (param `name`, self-onboard Variant T).
// Single source of truth: NIM-58 guard-assert in provision bodies validates
// incarnation.name byte-for-byte against this literal (nameguard_pin_test).
// lowercase-alnum + hyphens, start/end alnum, 1..50 — driver adds `-<index>`,
// FQDN=`<name>-<index>.<suffix>` must pass SID validation.
const VMNameBasePattern = `^[a-z][a-z0-9-]{0,48}[a-z0-9]$`

var VMNameBaseRe = regexp.MustCompile(VMNameBasePattern)

// ValidVMNameBase validates VM base name against [VMNameBasePattern].
func ValidVMNameBase(name string) bool { return VMNameBaseRe.MatchString(name) }

// Name is module base name without state suffix (Registry key). Author-form
// of task address — `core.cloud.created` / `core.cloud.destroyed` (base + state);
// state comes in pluginv1.ApplyRequest.state and is dispatched in Apply.
const Name = "core.cloud"

// Module states.
const (
	StateCreated   = "created"
	StateDestroyed = "destroyed"
	StateResized   = "resized"
)

// SoulStore is narrow subset for INSERT into souls. DeleteBySID is needed for
// orphan-cleanup self-onboard (Variant T): souls are inserted BEFORE create, and on
// create/validation failure they must be rolled back (else rerun hits PK conflict).
type SoulStore interface {
	Insert(ctx context.Context, soul *keepersoul.Soul) error
	UpdateStatus(ctx context.Context, sid string, status keepersoul.Status, kid *string) error
	DeleteBySID(ctx context.Context, sid string) error
}

// TokenStore is narrow subset for INSERT into bootstrap_tokens. DeleteByTokenID
// is needed for orphan-cleanup self-onboard (see SoulStore): tokens are issued BEFORE
// create and rolled back on failure.
type TokenStore interface {
	Generate() (bootstraptoken.PlainToken, error)
	Insert(ctx context.Context, sid, tokenHash string, createdByAID *string) (*bootstraptoken.Record, error)
	DeleteByTokenID(ctx context.Context, tokenID string) error
}

// AuditWriter writes audit-event `cloud.provisioned`.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Cascader processes cascade for `destroyed` state (ADR-017 cascade):
// single PG transaction transitioning souls/soul_seeds/bootstrap_tokens to
// terminal states. Production implementation — [CascadePG] over pgxpool.Pool;
// for unit tests of module — fake (see provisioned_test.go).
//
// May be nil in wire builds without PG (then destroyed state fails scenario
// with clear error; see applyDestroyed).
type Cascader interface {
	CascadeDestroy(ctx context.Context, sids []string, usedByKID string) (CascadeCounts, error)
}

// UserdataProvider resolves cloud-init userdata per scenario parameter
// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
// Implementation — keeper/internal/cloudinit.Resolver+GenerateUserdata wrapped
// in daemon: reads current snapshot KeeperConfig.CloudInit (hot-reload via
// config.Store.Get) and Vault.ReadKV for PEM CA. Returns ready cloud-config
// YAML without secrets.
//
// Cross-package isolation via interface: cloud module does not know cloudinit
// package, tested on fake provider.
//
// May be nil — then `generate_userdata: true` returns error "not configured";
// explicit `userdata: "..."` continues working without UserdataProvider.
//
// GenerateUserdataSelfOnboard renders userdata with baked-in per-VM tokens
// for self-onboard "Variant T" (ADR-017(h) amendment): keeper predicts VM FQDN
// BEFORE create and passes map FQDN→plain-token; cloud-init on VM picks its token
// by hostname and onboards in one cycle (without claim/keeper.push).
type UserdataProvider interface {
	GenerateUserdata(ctx context.Context) (string, error)
	GenerateUserdataSelfOnboard(ctx context.Context, tokens map[string]string) (string, error)
}

// Module implements sdk/module.SoulModule.
type Module struct {
	Plugins  PluginHost
	Resolver ProviderResolver
	Souls    SoulStore
	Tokens   TokenStore
	Cascade  Cascader
	Audit    AuditWriter
	Userdata UserdataProvider
}

// New is wire-helper. `cascade` may be nil in test builds where
// destroyed-state is not used; applyDestroyed returns explicit error.
// `resolver` is required: both created and destroyed resolve Provider registry to
// driver-name + credentials (A-flow); nil returns explicit error at Apply.
//
// UserdataProvider is not passed through New (optional dependency,
// added after first 6 cloud providers fixed) — wire-up done via
// direct field assignment or [Module.WithUserdata].
func New(p PluginHost, r ProviderResolver, s SoulStore, t TokenStore, c Cascader, a AuditWriter) *Module {
	return &Module{Plugins: p, Resolver: r, Souls: s, Tokens: t, Cascade: c, Audit: a}
}

// WithUserdata returns copy of module with UserdataProvider wired. Convenient
// for daemon wire-up (`coremod.Default(...).Lookup("core.cloud.provisioned")
// .WithUserdata(...)`), without breaking existing [New] callsites.
func (m *Module) WithUserdata(p UserdataProvider) *Module {
	cp := *m
	cp.Userdata = p
	return &cp
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateCreated:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		// profile = NAME of Profile in profiles registry (Variant A, ADR-017
		// amendment 2026-06-29). Optional; empty/absent → VM without registry spec.
		// Name resolution → params — in applyCreated.
		if _, err := util.OptStringParam(req.Params, "profile"); err != nil {
			errs = append(errs, err.Error())
		}
		if n, ok, err := util.OptIntParam(req.Params, "count"); err != nil {
			errs = append(errs, err.Error())
		} else if ok && n < 1 {
			errs = append(errs, "param \"count\": must be >= 1")
		}
		// userdata + generate_userdata — type-check + mutually-exclusive validation
		// (ADR-017(h) amendment 2026-05-27, B-flat).
		userdata, uerr := util.OptStringParam(req.Params, "userdata")
		if uerr != nil {
			errs = append(errs, uerr.Error())
		}
		gen, _, gerr := util.OptBoolParam(req.Params, "generate_userdata")
		if gerr != nil {
			errs = append(errs, gerr.Error())
		}
		if gen && userdata != "" {
			errs = append(errs, "params \"userdata\" and \"generate_userdata: true\" are mutually exclusive")
		}
		// name — base name of VM batch (self-onboard Variant T, ADR-017(h)): keeper
		// passes it in CreateRequest.name, driver names `<name>-<index>`,
		// FQDN=`<name>-<index>.<suffix>` is predictable. Validated as name fragment
		// (same pattern as SID-labels; provider-specific constraints checked by driver).
		name, nerr := util.OptStringParam(req.Params, "name")
		if nerr != nil {
			errs = append(errs, nerr.Error())
		} else if name != "" && !ValidVMNameBase(name) {
			errs = append(errs, fmt.Sprintf("param %q: %q must match %s (VM-name base for predictable FQDN)", "name", name, VMNameBasePattern))
		}
		// self_onboard: true — VM onboards itself from cloud-init (Variant T):
		// requires both name (for FQDN prediction) and generate_userdata path (tokens
		// in userdata). Explicit `userdata:` with self_onboard is incompatible (we must
		// bake tokens ourselves). generate_userdata is NOT required as flag —
		// self_onboard implies userdata render with tokens (see applyCreated).
		selfOnboard, _, serr := util.OptBoolParam(req.Params, "self_onboard")
		if serr != nil {
			errs = append(errs, serr.Error())
		}
		if selfOnboard {
			if name == "" {
				errs = append(errs, "param \"self_onboard: true\" requires \"name\" (base VM name for predictable FQDN)")
			}
			if userdata != "" {
				errs = append(errs, "params \"userdata\" and \"self_onboard: true\" are mutually exclusive (self-onboard renders userdata with per-VM tokens)")
			}
		}
	case StateDestroyed:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.StringSliceParam(req.Params, "vm_ids"); err != nil {
			errs = append(errs, err.Error())
		}
	case StateResized:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.StringSliceParam(req.Params, "vm_ids"); err != nil {
			errs = append(errs, err.Error())
		}
		// desired — required object; at least one dimension (cpu/ram/disk)
		// must be set (>0), else resize is meaningless no-op.
		if _, _, _, derr := parseDesired(req.Params); derr != nil {
			errs = append(errs, derr.Error())
		}
		if _, _, aerr := util.OptBoolParam(req.Params, "allow_downtime"); aerr != nil {
			errs = append(errs, aerr.Error())
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want created/destroyed/resized)", req.State))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case StateCreated:
		return m.applyCreated(req, stream)
	case StateDestroyed:
		return m.applyDestroyed(req, stream)
	case StateResized:
		return m.applyResized(req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// maskErr masks possible secret leak in error text before sending to
// failed-event (goes to status_details/error_summary, observable channel).
// Credential resolution may produce error with embedded vault-ref
// (`vault:secret/...`) — pass string through same substring filter from
// shared/audit that cleans register-output. Key `_` is not secret — only
// vault-ref filter by value will match.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"_": err.Error()})
	if s, ok := masked["_"].(string); ok {
		return s
	}
	return "***MASKED***"
}

// applyCreated implements state=created. See package doc-comment.
//
// Two modes of token userdata delivery:
//   - B-flat (default): create → per-VM tokens issued AFTER (SID=FQDN from
//     driver response) → plain placed in register (delivery via separate step).
//   - self-onboard "Variant T" (ADR-017(h) amendment, `self_onboard: true`):
//     keeper PREDICTS FQDN (`<name>-<i>.<suffix>`) BEFORE create, issues tokens
//     and bakes them in userdata; VM onboards itself. Plain NOT placed in register
//     (no delivery). CreateRequest.name = base name (driver names `<name>-<i>`).
func (m *Module) applyCreated(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	profileName, err := util.OptStringParam(req.Params, "profile")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	count, ok, err := util.OptIntParam(req.Params, "count")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !ok {
		count = 1
	}
	if count < 1 {
		return util.SendFailed(stream, "count must be >= 1")
	}

	userdata, err := util.OptStringParam(req.Params, "userdata")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	generate, _, err := util.OptBoolParam(req.Params, "generate_userdata")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	name, err := util.OptStringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	selfOnboard, _, err := util.OptBoolParam(req.Params, "self_onboard")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if generate && userdata != "" {
		return util.SendFailed(stream, "cloud created: params \"userdata\" and \"generate_userdata: true\" are mutually exclusive (set one or the other)")
	}
	if selfOnboard {
		if name == "" {
			return util.SendFailed(stream, "cloud created: self_onboard=true requires \"name\" (base VM name for predictable FQDN)")
		}
		if userdata != "" {
			return util.SendFailed(stream, "cloud created: params \"userdata\" and \"self_onboard: true\" are mutually exclusive")
		}
	}
	if generate && !selfOnboard {
		if m.Userdata == nil {
			return util.SendFailed(stream, "cloud created: generate_userdata=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
		}
		rendered, gerr := m.Userdata.GenerateUserdata(ctx)
		if gerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: generate userdata: %s", maskErr(gerr)))
		}
		userdata = rendered
	}

	// A-flow: Keeper resolves Provider registry to driver-name + plain-credentials
	// (+ FQDNSuffix for self-onboard prediction) and Profile registry to VM-spec params.
	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud created: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	var profileMap map[string]any
	if profileName != "" {
		profileMap, err = m.Resolver.ResolveProfile(ctx, profileName)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("resolve profile %q: %s", profileName, maskErr(err)))
		}
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	if selfOnboard {
		return m.applyCreatedSelfOnboard(ctx, stream, provider, name, int(count), resolved, profileMap)
	}

	vms, err := m.Plugins.Create(ctx, resolved.Driver, profileMap, resolved.Credentials, int32(count), userdata, name)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", provider, maskErr(err)))
	}

	hosts := make([]any, 0, len(vms))
	vmIDs := make([]any, 0, len(vms))
	for _, vm := range vms {
		sid := vm.GetFqdn()
		if sid == "" {
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", provider, vm.GetVmId()))
		}
		soul := &keepersoul.Soul{
			SID:       sid,
			Transport: keepersoul.TransportAgent,
			Status:    keepersoul.StatusPending,
		}
		if err := m.Souls.Insert(ctx, soul); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert soul %q: %v", sid, err))
		}
		tok, err := m.Tokens.Generate()
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
		}
		if _, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
		}

		hostEntry := map[string]any{
			"sid":        sid,
			"vm_id":      vm.GetVmId(),
			"primary_ip": vm.GetPrimaryIp(),
		}
		if attrs := vm.GetAttributes(); attrs != nil {
			hostEntry["attributes"] = attrs.AsMap()
		}
		// WARNING (security, H1): plain bootstrap-token is intentionally placed in
		// register-output — cloud-init flow requires sending it to VM on initial boot.
		// This is the only moment when plain-token is visible; database stores only
		// hash, token cannot be recovered later.
		//
		// Secrecy of key `bootstrap_token` is ensured on ALL register-output outlets
		// (audit-log / OTel / SSE / any logs): key matches substring-filter
		// [audit.MaskSecrets] (`token` fragment). Any new register-output channel MUST
		// pass payload through audit.MaskSecrets — else risk one-time token leak (see
		// .pm/tasks/2026-05-22-security-review). Cannot change key name without
		// filter verification.
		hostEntry["bootstrap_token"] = tok.Reveal()
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if werr := m.writeCreatedAudit(ctx, provider, len(vms), vmIDs); werr != nil {
		return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
	}

	return util.SendFinal(stream, true, map[string]any{
		"hosts":  hosts,
		"count":  float64(len(vms)),
		"vm_ids": vmIDs,
		"action": StateCreated,
	})
}

// applyCreatedSelfOnboard is self_onboard=true branch (Variant T, ADR-017(h)):
// keeper predicts VM FQDN BEFORE create, issues per-VM tokens, bakes them in
// userdata, creates VM (passing base name in CreateRequest.name) and validates
// actual FQDN against predicted. Plain tokens NOT placed in register —
// no delivery, VM onboards from cloud-init.
func (m *Module) applyCreatedSelfOnboard(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], provider, name string, count int, resolved *ResolvedProvider, profileMap map[string]any) error {
	if resolved.FQDNSuffix == "" {
		return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard requires provider %q to have fqdn_suffix (keeper predicts FQDN=<name>-<i>.<suffix>); set providers.fqdn_suffix", provider))
	}
	if m.Userdata == nil {
		return util.SendFailed(stream, "cloud created: self_onboard=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
	}

	// Souls (pending) + tokens issued BEFORE create — so any failure AFTER
	// insertion (create-fail, empty/mismatched FQDN, userdata-render error)
	// would leave orphaned records: presence barrier await_online would hang on
	// onboarding non-existent VMs, and rerun-last would hit PK conflict inserting
	// soul with same predicted FQDN. Accumulated records rolled back via defer if
	// successful completion not reached (success flag). On success — kept.
	//
	// DeleteBySID cascades on bootstrap_tokens (FK ON DELETE CASCADE, migrations
	// 008/009), but token rolled back explicitly — don't rely on schema and cover
	// case soul-insert-ok / token-insert-fail.
	type provisionedRecord struct{ sid, tokenID string }
	var provisioned []provisionedRecord
	success := false
	defer func() {
		if success {
			return
		}
		for _, rec := range provisioned {
			// Best-effort: terminal failed-event already sent, rollback errors
			// cannot go to stream.
			// TODO(prod): log failed rollback to OTel/daemon log — at
			// unit-level module has no logger; orphan after rollback failure
			// will be picked by Reaper (purge_souls) by pending-record age.
			_ = m.Tokens.DeleteByTokenID(ctx, rec.tokenID)
			_ = m.Souls.DeleteBySID(ctx, rec.sid)
		}
	}()

	// Predict FQDN of each VM and issue token. Souls (pending) + tokens
	// (hash in DB) created BEFORE create — presence barrier await_online then waits
	// for their onboarding. Tokens accumulated in map FQDN→plain for baking in userdata.
	predicted := make([]string, count)
	tokens := make(map[string]string, count)
	for i := 0; i < count; i++ {
		sid := fmt.Sprintf("%s-%d.%s", name, i, resolved.FQDNSuffix)
		if !keepersoul.ValidSID(sid) {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: predicted FQDN %q is not a valid SID (check name/fqdn_suffix)", sid))
		}
		predicted[i] = sid

		soul := &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
		if err := m.Souls.Insert(ctx, soul); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert soul %q: %v", sid, err))
		}
		tok, err := m.Tokens.Generate()
		if err != nil {
			// soul already inserted — roll back via defer (token-id empty, DeleteByTokenID
			// on non-existent id is safe).
			provisioned = append(provisioned, provisionedRecord{sid: sid})
			return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
		}
		rec, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil)
		if err != nil {
			provisioned = append(provisioned, provisionedRecord{sid: sid})
			return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
		}
		provisioned = append(provisioned, provisionedRecord{sid: sid, tokenID: rec.TokenID})
		tokens[sid] = tok.Reveal()
	}

	userdata, err := m.Userdata.GenerateUserdataSelfOnboard(ctx, tokens)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud created: generate self-onboard userdata: %s", maskErr(err)))
	}

	vms, err := m.Plugins.Create(ctx, resolved.Driver, profileMap, resolved.Credentials, int32(count), userdata, name)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", provider, maskErr(err)))
	}

	// Validate actual FQDN against predicted: if provider named VM differently,
	// self-onboard silently broken (token in userdata under predicted FQDN, but VM
	// has different hostname → soul init won't find token). Fail-fast to avoid
	// indefinite wait at presence barrier.
	predictedSet := make(map[string]bool, len(predicted))
	for _, f := range predicted {
		predictedSet[f] = true
	}
	hosts := make([]any, 0, len(vms))
	vmIDs := make([]any, 0, len(vms))
	for _, vm := range vms {
		sid := vm.GetFqdn()
		if sid == "" {
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", provider, vm.GetVmId()))
		}
		if !predictedSet[sid] {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard: provider %q named VM %q, not among predicted FQDN %v — token in userdata will not match VM hostname (driver must honor CreateRequest.name)", provider, sid, predicted))
		}
		// NO bootstrap_token in register: self-onboard delivered token via userdata,
		// no separate delivery step. Onboarding done from cloud-init on VM.
		hostEntry := map[string]any{
			"sid":        sid,
			"vm_id":      vm.GetVmId(),
			"primary_ip": vm.GetPrimaryIp(),
		}
		if attrs := vm.GetAttributes(); attrs != nil {
			hostEntry["attributes"] = attrs.AsMap()
		}
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if werr := m.writeCreatedAudit(ctx, provider, len(vms), vmIDs); werr != nil {
		return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
	}

	// Reached successful completion — souls/tokens remain, defer-cleanup not triggered.
	success = true
	return util.SendFinal(stream, true, map[string]any{
		"hosts":        hosts,
		"count":        float64(len(vms)),
		"vm_ids":       vmIDs,
		"action":       StateCreated,
		"self_onboard": true,
	})
}

// writeCreatedAudit writes audit-event `cloud.provisioned` for created phase
// (shared by B-flat and self-onboard paths). nil Audit → no-op.
func (m *Module) writeCreatedAudit(ctx context.Context, provider string, n int, vmIDs []any) error {
	if m.Audit == nil {
		return nil
	}
	return m.Audit.Write(ctx, &audit.Event{
		EventType: audit.EventCloudProvisioned,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"action":   StateCreated,
			"provider": provider,
			"count":    float64(n),
			"vm_ids":   vmIDs,
		},
	})
}

// applyDestroyed implements state=destroyed. See package doc-comment.
//
// Cascade (ADR-017) runs AFTER successful PluginHost.Destroy: if cloud-destroy
// failed, registries remain untouched — host still "alive" from cloud-provider
// perspective, premature to transition souls→destroyed.
func (m *Module) applyDestroyed(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	vmIDs, err := util.StringSliceParam(req.Params, "vm_ids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Optional SID list for cascade (per-VM sid↔vm_id binding held by caller:
	// cloud-driver doesn't know our SID, we don't know provider-vm-id-mapping).
	sids, err := util.OptStringSliceParam(req.Params, "sids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud destroyed: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	destroyed, err := m.Plugins.Destroy(ctx, resolved.Driver, resolved.Credentials, vmIDs)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud destroy via provider %q: %s", provider, maskErr(err)))
	}

	var counts CascadeCounts
	if len(sids) > 0 {
		if m.Cascade == nil {
			return util.SendFailed(stream, "cloud destroyed: cascade store not configured (wire CascadePG in main)")
		}
		counts, err = m.Cascade.CascadeDestroy(ctx, sids, bootstraptoken.SystemKIDCloudDestroy)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("cascade destroy: %v", err))
		}
	}

	destroyedAny := make([]any, len(destroyed))
	for i, id := range destroyed {
		destroyedAny[i] = id
	}
	sidsAny := make([]any, len(sids))
	for i, s := range sids {
		sidsAny[i] = s
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":         StateDestroyed,
				"provider":       provider,
				"vm_ids":         destroyedAny,
				"sids":           sidsAny,
				"souls_updated":  float64(counts.SoulsUpdated),
				"seeds_orphaned": float64(counts.SeedsOrphaned),
				"tokens_burned":  float64(counts.TokensBurned),
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	return util.SendFinal(stream, len(destroyed) > 0, map[string]any{
		"action":         StateDestroyed,
		"vm_ids":         destroyedAny,
		"sids":           sidsAny,
		"destroyed_n":    float64(len(destroyed)),
		"souls_updated":  float64(counts.SoulsUpdated),
		"seeds_orphaned": float64(counts.SeedsOrphaned),
		"tokens_burned":  float64(counts.TokensBurned),
	})
}

// parseDesired extracts target resources from params.desired (our units:
// cpu=cores / ram_mb=MB / disk_gb=GB). All fields optional, but at least one
// must be set (>0) — else resize is meaningless. Returns values
// (0 = no change) + validation error (missing desired / wrong type /
// all zeros / negative).
func parseDesired(params *structpb.Struct) (cpu int32, ramMB, diskGB int64, err error) {
	desired, derr := util.OptStructParam(params, "desired")
	if derr != nil {
		return 0, 0, 0, derr
	}
	if desired == nil {
		return 0, 0, 0, fmt.Errorf("param %q: missing (resize requires target resources)", "desired")
	}
	cpu64, _, cerr := util.OptIntParam(desired, "cpu_cores")
	if cerr != nil {
		return 0, 0, 0, cerr
	}
	ramMB, _, rerr := util.OptIntParam(desired, "ram_mb")
	if rerr != nil {
		return 0, 0, 0, rerr
	}
	diskGB, _, gerr := util.OptIntParam(desired, "disk_gb")
	if gerr != nil {
		return 0, 0, 0, gerr
	}
	if cpu64 < 0 || ramMB < 0 || diskGB < 0 {
		return 0, 0, 0, fmt.Errorf("param %q: cpu_cores/ram_mb/disk_gb must be >= 0", "desired")
	}
	if cpu64 == 0 && ramMB == 0 && diskGB == 0 {
		return 0, 0, 0, fmt.Errorf("param %q: at least one of cpu_cores/ram_mb/disk_gb must be > 0", "desired")
	}
	return int32(cpu64), ramMB, diskGB, nil
}

// applyResized implements state=resized. See package doc-comment. Database
// untouched: resize changes VM resources, not souls registry.
func (m *Module) applyResized(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	vmIDs, err := util.StringSliceParam(req.Params, "vm_ids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	cpu, ramMB, diskGB, err := parseDesired(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	allowDowntime, _, err := util.OptBoolParam(req.Params, "allow_downtime")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud resized: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	desired := &pluginv1.ResizeSpec{CpuCores: cpu, RamMb: ramMB, DiskGb: diskGB}
	results, err := m.Plugins.Resize(ctx, resolved.Driver, resolved.Credentials, vmIDs, desired, allowDowntime)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud resize via provider %q: %s", provider, maskErr(err)))
	}

	resultsOut := make([]any, 0, len(results))
	causedDowntime := false
	var perVMErrors []string
	for _, r := range results {
		entry := map[string]any{
			"vm_id":           r.GetVmId(),
			"caused_downtime": r.GetCausedDowntime(),
		}
		if e := r.GetError(); e != "" {
			entry["error"] = e
			perVMErrors = append(perVMErrors, fmt.Sprintf("%s: %s", r.GetVmId(), e))
		}
		if r.GetCausedDowntime() {
			causedDowntime = true
		}
		resultsOut = append(resultsOut, entry)
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":          StateResized,
				"provider":        provider,
				"vm_ids":          toAnySlice(vmIDs),
				"cpu_cores":       float64(cpu),
				"ram_mb":          float64(ramMB),
				"disk_gb":         float64(diskGB),
				"caused_downtime": causedDowntime,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// changed=true: resize always changes resource (idempotency to target size
	// is driver responsibility; at module level we treat resize as changing operation).
	// Per-VM errors don't fail entire step (some VMs may have resized), but
	// included in output for observability.
	out := map[string]any{
		"action":          StateResized,
		"vm_ids":          toAnySlice(vmIDs),
		"results":         resultsOut,
		"caused_downtime": causedDowntime,
	}
	if len(perVMErrors) > 0 {
		out["errors"] = toAnySlice(perVMErrors)
	}
	return util.SendFinal(stream, true, out)
}

// toAnySlice converts []string to []any for structpb-output.
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
