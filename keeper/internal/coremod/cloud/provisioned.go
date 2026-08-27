// Package cloud implements keeper-side core module `core.cloud.provisioned`
// (ADR-017, docs/keeper/cloud.md).
//
// States:
//   - created: CloudDriver.Create via PluginHost → []VmInfo → idempotent
//     registration in souls (status: pending) + INSERT bootstrap_tokens (one per
//     VM that needs one). Re-running over the same incarnation converges rather
//     than collides: leftovers of an earlier attempt are re-armed (NIM-170) and
//     hosts that are already up are passed through untouched, without a token
//     (NIM-189). Output: hosts: [{sid, vm_id, primary_ip, attributes}].
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
//
// Every state takes its driver from one of two sources — `provider: <name>` out
// of the providers registry, or `driver` + `credentials` carried by the step
// itself (NIM-668, source.go). They meet at one *ResolvedProvider before any
// plugin call, so the driver, the audit event and the output are the same either
// way, and a fleet can provision with zero rows in the registries.
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

// SoulStore is narrow subset for registering provisioned VMs in souls.
// DeleteBySID is needed for orphan-cleanup self-onboard (Variant T): souls are
// inserted BEFORE create, and on create/validation failure they must be rolled
// back (else the next run inherits pending records for VMs that never existed).
//
// EnsureProvisionable, not a plain INSERT (NIM-170): a re-run over a
// half-finished provision must reuse the records the previous attempt left
// behind instead of dying on the souls PK, and a re-run over hosts that are
// already up must pass through them (NIM-189). Anything but
// [keepersoul.ProvisionInserted] marks a record that predates this run —
// orphan-cleanup leaves those alone.
type SoulStore interface {
	EnsureProvisionable(ctx context.Context, soul *keepersoul.Soul, incarnationName string) (keepersoul.ProvisionOutcome, error)
	UpdateStatus(ctx context.Context, sid string, status keepersoul.Status, kid *string) error
	DeleteBySID(ctx context.Context, sid string) error
}

// TokenStore is narrow subset for INSERT into bootstrap_tokens. DeleteByTokenID
// is needed for orphan-cleanup self-onboard (see SoulStore): tokens are issued BEFORE
// create and rolled back on failure.
//
// ExpireActiveForSID is the token half of provision idempotency (NIM-170): a SID
// holds at most ONE active token (partial unique `bootstrap_tokens_active_by_sid_idx`),
// so re-provisioning a SID must invalidate the token baked for the VM that never
// came up before issuing the replacement.
type TokenStore interface {
	Generate() (bootstraptoken.PlainToken, error)
	Insert(ctx context.Context, sid, tokenHash string, createdByAID *string) (*bootstraptoken.Record, error)
	DeleteByTokenID(ctx context.Context, tokenID string) error
	ExpireActiveForSID(ctx context.Context, sid string) error
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
// `resolver` is what reads the Provider and Profile registries (A-flow:
// driver-name + credentials); nil is legal and leaves the inline source
// (`driver` + `credentials`, NIM-668) working — a step that names a registry
// entry then fails with an explicit error at Apply.
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

// Validate is the static half of the contract. The rules it states are the same
// functions Apply enforces (source.go): a keeper-side core module is dispatched
// straight to Apply and no ValidateRequest is built for it in production, so a
// rule living only here would read as a guard and never run.
//
// What it does NOT share is the shape of `credentials` — it reads the step
// BEFORE vault-resolve, where the correct value is the `vault:` ref an author
// typed, while Apply reads it after, where the correct value is the secret map
// that phase put there. Hence [authoredParams] here and [resolvedParams] there:
// judging both with one predicate made this half refuse the only form an author
// can legitimately write.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateCreated:
		src, serrs := parseProviderParams(req.Params, authoredParams)
		errs = append(errs, serrs...)
		errs = append(errs, checkProviderParams(src)...)
		// profile = the VM spec itself (an object, handed to the driver as-is)
		// or the NAME of a Profile in the profiles registry (a string; Variant A,
		// ADR-017 amendment 2026-06-29). Optional; absent → VM without a spec.
		// Resolution → params — in applyCreated.
		if _, _, err := profileValue(req.Params); err != nil {
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
			// The suffix keeper predicts the FQDN from. Inline it is a param of
			// this step, so its absence is visible here; in registry mode it is
			// a column nothing can read without resolving, so that half of the
			// check lives in applyCreatedSelfOnboard.
			if src.inline() && src.fqdnSuffix == "" {
				errs = append(errs, fmt.Sprintf("param \"self_onboard: true\" requires %q with an inline driver (keeper predicts FQDN=<name>-<i>.<suffix>)", paramFQDNSuffix))
			}
		}
	case StateDestroyed:
		src, serrs := parseProviderParams(req.Params, authoredParams)
		errs = append(errs, serrs...)
		errs = append(errs, checkProviderParams(src)...)
		if _, err := util.StringSliceParam(req.Params, "vm_ids"); err != nil {
			errs = append(errs, err.Error())
		}
	case StateResized:
		src, serrs := parseProviderParams(req.Params, authoredParams)
		errs = append(errs, serrs...)
		errs = append(errs, checkProviderParams(src)...)
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
	// First, and without touching anything external: this is where `provider`
	// used to be read (see checkSourceParams).
	if err := checkSourceParams(req.Params); err != nil {
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

	// Profile first, then the driver tuple — the order the registry path has
	// always had. A name that is not in the profiles registry is refused without
	// reading a provider row and its Vault secret.
	profileMap, err := m.resolveProfileParam(ctx, req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// The driver tuple: resolved from the providers registry (A-flow — keeper
	// reads the credentials, the driver never touches Vault) or carried inline by
	// the step (NIM-668). Both produce the same *ResolvedProvider; nothing below
	// this line knows which.
	src, err := m.resolveProviderSource(ctx, req.Params, StateCreated)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if selfOnboard {
		return m.applyCreatedSelfOnboard(ctx, stream, src, name, int(count), profileMap)
	}

	vms, err := m.Plugins.Create(ctx, src.resolved.Driver, profileMap, src.resolved.Credentials, int32(count), userdata, name)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", src.label, maskErr(err)))
	}

	incarnationName := util.IncarnationFrom(ctx)
	hosts := make([]any, 0, len(vms))
	vmIDs := make([]any, 0, len(vms))
	reused, existing := 0, 0
	for _, vm := range vms {
		sid := vm.GetFqdn()
		if sid == "" {
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", src.label, vm.GetVmId()))
		}
		soul := &keepersoul.Soul{
			SID:       sid,
			Transport: keepersoul.TransportAgent,
			Status:    keepersoul.StatusPending,
		}
		// B-flat registers AFTER create: a reused record here is a leftover of an
		// earlier attempt at the same FQDN (NIM-170), an existing one is a host of
		// this incarnation the driver handed back instead of creating (NIM-189),
		// and a refusal means the provider handed us the FQDN of a host we don't own.
		outcome, err := m.Souls.EnsureProvisionable(ctx, soul, incarnationName)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("register provisioned soul %q: %v", sid, err))
		}

		hostEntry := map[string]any{
			"sid":        sid,
			"vm_id":      vm.GetVmId(),
			"primary_ip": vm.GetPrimaryIp(),
		}
		if attrs := vm.GetAttributes(); attrs != nil {
			hostEntry["attributes"] = attrs.AsMap()
		}

		switch outcome {
		case keepersoul.ProvisionExisting:
			existing++
			// No token for a host that already holds an identity — a bootstrap
			// token is a capability, and this one has nothing to redeem it for.
			// The flag makes core.bootstrap.delivered skip the host instead of
			// failing on the missing `bootstrap_token` (NIM-189).
			hostEntry["onboarded"] = true
		default:
			if outcome == keepersoul.ProvisionReused {
				reused++
				// The record we took over may still hold the active token of the
				// previous attempt — one active token per SID (NIM-170).
				if err := m.Tokens.ExpireActiveForSID(ctx, sid); err != nil {
					return util.SendFailed(stream, fmt.Sprintf("invalidate previous bootstrap token for %q: %v", sid, err))
				}
			}
			tok, err := m.Tokens.Generate()
			if err != nil {
				return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
			}
			if _, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil); err != nil {
				return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
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
		}
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if werr := m.writeCreatedAudit(ctx, src, len(vms), vmIDs); werr != nil {
		return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
	}

	return util.SendFinal(stream, true, map[string]any{
		"hosts":  hosts,
		"count":  float64(len(vms)),
		"vm_ids": vmIDs,
		"action": StateCreated,
		// How many souls records this run took over from an earlier attempt
		// instead of creating (NIM-170) — 0 on a clean first run.
		"reused": float64(reused),
		// How many hosts were already up and were passed through untouched
		// (NIM-189) — 0 on a clean first run.
		"existing": float64(existing),
	})
}

// applyCreatedSelfOnboard is self_onboard=true branch (Variant T, ADR-017(h)):
// keeper predicts VM FQDN BEFORE create, issues per-VM tokens, bakes them in
// userdata, creates VM (passing base name in CreateRequest.name) and validates
// actual FQDN against predicted. Plain tokens NOT placed in register —
// no delivery, VM onboards from cloud-init.
func (m *Module) applyCreatedSelfOnboard(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], src providerSource, name string, count int, profileMap map[string]any) error {
	if src.resolved.FQDNSuffix == "" {
		// The suffix comes from wherever the driver came from, and so does the
		// fix: a step param inline, a registry column otherwise.
		if src.inline {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard requires param %q (keeper predicts FQDN=<name>-<i>.<suffix>)", paramFQDNSuffix))
		}
		return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard requires provider %q to have fqdn_suffix (keeper predicts FQDN=<name>-<i>.<suffix>); set providers.fqdn_suffix", src.label))
	}
	if m.Userdata == nil {
		return util.SendFailed(stream, "cloud created: self_onboard=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
	}

	// Souls (pending) + tokens issued BEFORE create — so any failure AFTER
	// insertion (create-fail, empty/mismatched FQDN, userdata-render error)
	// would leave orphaned records: presence barrier await_online would hang on
	// onboarding non-existent VMs. Accumulated records rolled back via defer if
	// successful completion not reached (success flag). On success — kept.
	//
	// DeleteBySID cascades on bootstrap_tokens (FK ON DELETE CASCADE, migrations
	// 008/009), but token rolled back explicitly — don't rely on schema and cover
	// case soul-insert-ok / token-insert-fail.
	//
	// ownSoul=false — the record predates this run (reused leftover, NIM-170)
	// and is NOT ours to delete: rolling it back would erase a registration this
	// run did not create. Hosts that were already up never reach this list at
	// all — nothing was issued for them (NIM-189).
	type provisionedRecord struct {
		sid, tokenID string
		ownSoul      bool
	}
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
			if rec.ownSoul {
				_ = m.Souls.DeleteBySID(ctx, rec.sid)
			}
		}
	}()

	// Predict FQDN of each VM and issue token. Souls (pending) + tokens
	// (hash in DB) created BEFORE create — presence barrier await_online then waits
	// for their onboarding. Tokens accumulated in map FQDN→plain for baking in userdata.
	incarnationName := util.IncarnationFrom(ctx)
	predicted := make([]string, count)
	tokens := make(map[string]string, count)
	onboarded := make(map[string]bool, count)
	reused, existing := 0, 0
	for i := 0; i < count; i++ {
		sid := fmt.Sprintf("%s-%d.%s", name, i, src.resolved.FQDNSuffix)
		if !keepersoul.ValidSID(sid) {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: predicted FQDN %q is not a valid SID (check name/fqdn_suffix)", sid))
		}
		predicted[i] = sid

		// Re-run over a half-finished provision reuses the pending/destroyed
		// record of the same predicted FQDN instead of failing on the PK
		// (NIM-170); a host of this incarnation that is already up is passed
		// through untouched (NIM-189); a record held by another incarnation is
		// refused, not taken over.
		soul := &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
		outcome, err := m.Souls.EnsureProvisionable(ctx, soul, incarnationName)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("register provisioned soul %q: %v", sid, err))
		}
		if outcome == keepersoul.ProvisionExisting {
			existing++
			onboarded[sid] = true
			// Nothing to issue and nothing to roll back: the host holds its own
			// seed, and its cloud-init ran on the boot that onboarded it (NIM-189).
			continue
		}
		if outcome == keepersoul.ProvisionReused {
			reused++
			// The record we took over may still hold the active token baked into
			// the userdata of the VM that never came up — one active token per SID
			// (NIM-170); the replacement VM needs a fresh one.
			if err := m.Tokens.ExpireActiveForSID(ctx, sid); err != nil {
				return util.SendFailed(stream, fmt.Sprintf("invalidate previous bootstrap token for %q: %v", sid, err))
			}
		}
		ownSoul := outcome == keepersoul.ProvisionInserted
		tok, err := m.Tokens.Generate()
		if err != nil {
			// soul record already in place — roll back via defer (token-id empty,
			// DeleteByTokenID on non-existent id is safe).
			provisioned = append(provisioned, provisionedRecord{sid: sid, ownSoul: ownSoul})
			return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
		}
		rec, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil)
		if err != nil {
			provisioned = append(provisioned, provisionedRecord{sid: sid, ownSoul: ownSoul})
			return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
		}
		provisioned = append(provisioned, provisionedRecord{sid: sid, tokenID: rec.TokenID, ownSoul: ownSoul})
		tokens[sid] = tok.Reveal()
	}

	// An empty token map means every predicted host is already up (a re-run over
	// a fully live incarnation): there is nothing to bake, and the VMs the driver
	// hands back will not run cloud-init again. Create is still called — the
	// driver is what confirms the VMs exist and returns the vm_ids day-2 destroy
	// needs (NIM-189).
	userdata := ""
	if len(tokens) > 0 {
		rendered, gerr := m.Userdata.GenerateUserdataSelfOnboard(ctx, tokens)
		if gerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: generate self-onboard userdata: %s", maskErr(gerr)))
		}
		userdata = rendered
	}

	vms, err := m.Plugins.Create(ctx, src.resolved.Driver, profileMap, src.resolved.Credentials, int32(count), userdata, name)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", src.label, maskErr(err)))
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
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", src.label, vm.GetVmId()))
		}
		if !predictedSet[sid] {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard: provider %q named VM %q, not among predicted FQDN %v — token in userdata will not match VM hostname (driver must honor CreateRequest.name)", src.label, sid, predicted))
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
		if onboarded[sid] {
			// Passed through: the host was already up before this run (NIM-189).
			hostEntry["onboarded"] = true
		}
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if werr := m.writeCreatedAudit(ctx, src, len(vms), vmIDs); werr != nil {
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
		// Souls records taken over from an earlier attempt (NIM-170).
		"reused": float64(reused),
		// Hosts already up, passed through untouched (NIM-189).
		"existing": float64(existing),
	})
}

// writeCreatedAudit writes audit-event `cloud.provisioned` for created phase
// (shared by B-flat and self-onboard paths). nil Audit → no-op.
//
// What identifies the provider in the payload is [auditPayloadSource]: the
// registry name, or the driver alias when the step carried the driver itself.
// Credentials never appear — the event names WHO was called, not with what.
func (m *Module) writeCreatedAudit(ctx context.Context, src providerSource, n int, vmIDs []any) error {
	if m.Audit == nil {
		return nil
	}
	payload := auditPayloadSource(src)
	payload["action"] = StateCreated
	payload["count"] = float64(n)
	payload["vm_ids"] = vmIDs
	return m.Audit.Write(ctx, &audit.Event{
		EventType: audit.EventCloudProvisioned,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	})
}

// auditPayloadSource is the provider identity every cloud.provisioned event
// carries. `provider` keeps its meaning — what the operator wrote to select the
// driver — and reads as the driver alias inline, where no registry name exists.
// `driver` always carries the alias, so an auditor reading the two together can
// tell an inline step from a registry entry that happens to be named after its
// driver. Neither is a secret; the credentials are not here in either mode.
func auditPayloadSource(src providerSource) map[string]any {
	return map[string]any{
		"provider": src.label,
		"driver":   src.resolved.Driver,
	}
}

// applyDestroyed implements state=destroyed. See package doc-comment.
//
// Cascade (ADR-017) runs AFTER successful PluginHost.Destroy: if cloud-destroy
// failed, registries remain untouched — host still "alive" from cloud-provider
// perspective, premature to transition souls→destroyed.
func (m *Module) applyDestroyed(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
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

	src, err := m.resolveProviderSource(ctx, req.Params, StateDestroyed)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	destroyed, err := m.Plugins.Destroy(ctx, src.resolved.Driver, src.resolved.Credentials, vmIDs)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud destroy via provider %q: %s", src.label, maskErr(err)))
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
		payload := auditPayloadSource(src)
		payload["action"] = StateDestroyed
		payload["vm_ids"] = destroyedAny
		payload["sids"] = sidsAny
		payload["souls_updated"] = float64(counts.SoulsUpdated)
		payload["seeds_orphaned"] = float64(counts.SeedsOrphaned)
		payload["tokens_burned"] = float64(counts.TokensBurned)
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload:   payload,
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

	src, err := m.resolveProviderSource(ctx, req.Params, StateResized)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	desired := &pluginv1.ResizeSpec{CpuCores: cpu, RamMb: ramMB, DiskGb: diskGB}
	results, err := m.Plugins.Resize(ctx, src.resolved.Driver, src.resolved.Credentials, vmIDs, desired, allowDowntime)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud resize via provider %q: %s", src.label, maskErr(err)))
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
		payload := auditPayloadSource(src)
		payload["action"] = StateResized
		payload["vm_ids"] = toAnySlice(vmIDs)
		payload["cpu_cores"] = float64(cpu)
		payload["ram_mb"] = float64(ramMB)
		payload["disk_gb"] = float64(diskGB)
		payload["caused_downtime"] = causedDowntime
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload:   payload,
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
