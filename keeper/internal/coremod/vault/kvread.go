// Package vault implements keeper-side core module `core.vault`
// (ADR-017, docs/architecture.md → ADR-017) — single module with state-based dispatch
// (pattern `core.cloud` / `core.choir`).
//
// States:
//   - kv-read ([StateRead], applyReadKV): read secret from Vault KV (v1/v2)
//     on keeper side and return to task's register-output.
//   - kv-present ([StatePresent], applyPresent in kvpresent.go): generate-if-absent
//     — ensure secrets exist, generating missing ones with cryptographically random
//     values per author-specified password-policy.
//
// Both states are handled by one [Module] (Registry key is base name `core.vault`,
// state arrives in pluginv1.ApplyRequest.state and is routed to Validate/Apply).
//
// Why kv-read exists when CEL has implicit `${ vault(...) }`:
// implicit-vault is cheap for render but not a distinct audit-trail entry.
// This state is the explicit form for cases requiring explicit audit-event
// `vault.kv-read` (PCI-DSS, SOC2, compliance-strict code). kv-present
// similarly writes `vault.kv-present` during generation.
//
// Output kv-read:
//
//	register.<name>.data       — map[string]any with extracted keys.
//	register.<name>.path       — echo of requested path.
//	register.<name>.fields     — list of keys in data (sorted).
//
// Output kv-present:
//
//	register.<name>.generated  — map path → [generated fields] (no values).
//
// Secret values themselves are **not** included in audit-payload or output of either state:
// only fact is recorded (path + field names). Output kv-read is masked at write-phase
// in destiny/scenario (shared/audit.MaskSecrets) — known secret keys; output kv-present
// carries no value names at all.
package vault

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the base module name without state suffix (Registry key). Author form
// of task address is base + state (`core.vault.kv-read` / `core.vault.kv-present`);
// state arrives in pluginv1.ApplyRequest.state.
const Name = "core.vault"

// StateRead is the read state. Matches author-state suffix `kv-read`
// of address `core.vault.kv-read` (SplitModuleAddr extracts "kv-read").
const StateRead = "kv-read"

// VaultWriter is a narrow subset of keeper/internal/vault.Client needed by the module:
// read (kv-read + existence check in kv-present) and write (generation in kv-present).
// Narrowing over *vault.Client simplifies unit tests (fake without HTTP).
//
// kv-read uses only ReadKV (write path not called in read state); single common interface
// keeps wire-up uniform (`Deps.Vault` — *vault.Client, implements both).
type VaultWriter interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// AuditWriter is a narrow dependency for audit-events `vault.kv-read` /
// `vault.kv-present`. Name matches shared/audit.Writer signature; typing
// local to keep module from pulling entire pipeline transitively.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module implements sdk/module.SoulModule over VaultWriter+AuditWriter.
// One module per base name `core.vault`; state (`kv-read`/`kv-present`)
// is routed inside Validate/Apply.
type Module struct {
	Vault VaultWriter
	Audit AuditWriter
}

// New is a wire helper.
func New(v VaultWriter, a AuditWriter) *Module {
	return &Module{Vault: v, Audit: a}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateRead:
		if _, err := util.StringParam(req.Params, "path"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.OptStringSliceParam(req.Params, "fields"); err != nil {
			errs = append(errs, err.Error())
		}
	case StatePresent:
		errs = append(errs, validatePresent(req)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s/%s)", req.State, StateRead, StatePresent))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply routes by state: kv-read → applyReadKV (read-only, changed=false),
// kv-present → applyPresent (generate-if-absent, kvpresent.go). Unknown state
// → failed-event (scenario-applier enters onfail).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case StateRead:
		return m.applyReadKV(req, stream)
	case StatePresent:
		return m.applyPresent(req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// applyReadKV does Vault.ReadKV → filter by `fields` field → write audit-event.
// changed: always false — read operation, does not mutate state.
func (m *Module) applyReadKV(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	wantedFields, err := util.OptStringSliceParam(req.Params, "fields")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// ReadKV returns flattened payload (secret fields) for BOTH KV versions — version
	// resolution is transparent in vault.Client (ADR-017(b), amendment 2026-06-22).
	// Wrappers `{data,metadata}` already removed here.
	payload, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("vault read %q: %v", path, err))
	}

	out := filterFields(payload, wantedFields)
	fields := sortedKeys(out)

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventVaultKVRead,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"path":   path,
				"fields": toAnySlice(fields),
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			// audit failure does not block read but is logged as failed —
			// otherwise module silently skips mandatory compliance step.
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	resp := map[string]any{
		"path":   path,
		"data":   out,
		"fields": toAnySlice(fields),
	}
	return util.SendFinal(stream, false, resp)
}

// filterFields keeps only specified keys. Empty/nil wanted → entire payload.
// Requested but missing key is silently skipped (not module failure: caller wanted
// optional fields; secret read already consumed audit-event).
func filterFields(payload map[string]any, wanted []string) map[string]any {
	if len(wanted) == 0 {
		return cloneMap(payload)
	}
	out := make(map[string]any, len(wanted))
	for _, k := range wanted {
		if v, ok := payload[k]; ok {
			out[k] = v
		}
	}
	return out
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toAnySlice(xs []string) []any {
	if xs == nil {
		return []any{}
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
