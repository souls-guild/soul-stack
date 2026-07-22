package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// StatePresent is generate-if-absent state. Matches author-state suffix
// `kv-present` of address `core.vault.kv-present` (SplitModuleAddr extracts "kv-present").
//
// Twin of [StateRead]: kv-read reads secret into register-output, kv-present
// ensures secret exists (generates missing value cryptographically per
// author-described password-policy), returns nothing from values.
const StatePresent = "kv-present"

// defaultPasswordField is default field name for target without explicit `field`.
// Matches redis-secret convention (`secret/redis/<inc>/users/<name>#password`).
const defaultPasswordField = "password"

// presentTarget is one generation target: Vault path + field name + generation policy.
// policy resolved at parse time (step-level default + per-target override),
// so Apply operates with ready alphabet/length without re-parsing.
type presentTarget struct {
	path   string
	field  string
	policy passwordPolicy
}

// validatePresent is runtime guard for kv-present params (soul-lint validates static
// author form). Checks targets and both policy forms.
func validatePresent(req *pluginv1.ValidateRequest) []string {
	var errs []string
	if _, err := parseTargets(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

// applyPresent: for each target, reads path; if field missing — generates value per
// its policy and writes (read-merge-write preserves neighboring fields of same path);
// if present — no-op. changed=true only when something actually generated
// (like core.soul.registered).
//
// SECURITY (ADR-010): generated VALUE does not leak to register-output, audit-payload,
// logs, or OTel. Only fact + path + list of generated fields exposed
// (mirrors sigil.KeyService.Introduce).
func (m *Module) applyPresent(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	targets, err := parseTargets(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// pendingWrites aggregates generation BY PATHS: multiple targets on one path
	// (different fields) merged into single WriteKV preserving existing fields.
	// Avoids spurious KV versions.
	pendingWrites := make(map[string]map[string]any)
	// existing caches each path payload during Apply, so two targets on one path
	// don't hit ReadKV twice, see consistent base.
	existing := make(map[string]map[string]any)
	// generated maps paths to sorted list of generated fields (for output and audit).
	// Field names only, no values.
	generated := make(map[string][]string)

	for _, t := range targets {
		payload, ok := existing[t.path]
		if !ok {
			payload, err = m.readPath(ctx, t.path)
			if err != nil {
				return util.SendFailed(stream, fmt.Sprintf("vault read %q: %v", t.path, err))
			}
			existing[t.path] = payload
		}

		if fieldPresent(payload, t.field) {
			continue // secret already exists — skip
		}
		// Check for already planned generation of same field in this Apply
		// (two targets with same path+field): skip duplicate generation.
		if w, ok := pendingWrites[t.path]; ok {
			if _, planned := w[t.field]; planned {
				continue
			}
		}

		value, gerr := t.policy.generate()
		if gerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("generate secret for %q: %v", t.path, gerr))
		}
		w := pendingWrites[t.path]
		if w == nil {
			// Merge base is copy of existing fields (if path existed), then layered with
			// generated. WriteKV (new KV version) doesn't lose neighboring fields.
			w = cloneMap(payload)
			pendingWrites[t.path] = w
		}
		w[t.field] = value
		generated[t.path] = append(generated[t.path], t.field)
	}

	for path, data := range pendingWrites {
		if werr := m.Vault.WriteKV(ctx, path, data); werr != nil {
			// WriteKV doesn't leak values in error text (vault.Client invariant);
			// same here — path only.
			return util.SendFailed(stream, fmt.Sprintf("vault write %q: %v", path, werr))
		}
	}

	changed := len(generated) > 0
	genFields := generatedFieldsOutput(generated)

	if m.Audit != nil && changed {
		ev := &audit.Event{
			EventType: audit.EventVaultKVPresent,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"paths": genFields, // path → [generated fields]; no values
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	resp := map[string]any{
		"generated": genFields,
	}
	return util.SendFinal(stream, changed, resp)
}

// readPath reads path payload; nonexistent path is NOT error, returns empty payload
// (all fields will be generated). Transport/policy errors propagate up.
func (m *Module) readPath(ctx context.Context, path string) (map[string]any, error) {
	payload, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		if errors.Is(err, keepervault.ErrVaultKVNotFound) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return payload, nil
}

// parseTargets parses params.targets — required non-empty list of objects {path: <str>,
// field?: <str>, policy?: <object>}. Empty/missing list is error (nothing for module to
// guarantee). field defaults to [defaultPasswordField]. policy of each target resolved
// over step-level params.policy (step default) → [defaultPolicy] (if neither present).
func parseTargets(params *structpb.Struct) ([]presentTarget, error) {
	stepPolicy, err := parseStepPolicy(params)
	if err != nil {
		return nil, err
	}
	raw, err := util.ListParam(params, "targets")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("param \"targets\": empty list")
	}
	out := make([]presentTarget, 0, len(raw))
	for i, item := range raw {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param \"targets\"[%d]: expected object, got %T", i, item.Kind)
		}
		t, terr := parseTarget(sv.StructValue, i, stepPolicy)
		if terr != nil {
			return nil, terr
		}
		out = append(out, t)
	}
	return out, nil
}

// parseStepPolicy parses optional step-level params.policy — common generation
// default for all targets without own policy. Missing → [defaultPolicy].
func parseStepPolicy(params *structpb.Struct) (passwordPolicy, error) {
	obj, err := util.OptStructParam(params, "policy")
	if err != nil {
		return passwordPolicy{}, err
	}
	if obj == nil {
		return defaultPolicy(), nil
	}
	return parsePolicy(obj, defaultPolicy())
}

func parseTarget(s *structpb.Struct, idx int, stepPolicy passwordPolicy) (presentTarget, error) {
	path, err := util.StringParam(s, "path")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	if path == "" {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].path: empty", idx)
	}
	field, err := util.OptStringParam(s, "field")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	if field == "" {
		field = defaultPasswordField
	}
	// per-target policy override for step-level default.
	override, err := util.OptStructParam(s, "policy")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	policy := stepPolicy
	if override != nil {
		policy, err = parsePolicy(override, stepPolicy)
		if err != nil {
			return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
		}
	}
	return presentTarget{path: path, field: field, policy: policy}, nil
}

// fieldPresent checks if field exists in payload with non-empty value. Empty string
// is treated as "no" (reason to generate): empty password is useless.
func fieldPresent(payload map[string]any, field string) bool {
	v, ok := payload[field]
	if !ok || v == nil {
		return false
	}
	s, ok := v.(string)
	return ok && s != ""
}

// generatedFieldsOutput builds deterministic projection path → [fields] for output/audit:
// path keys and inner fields sorted; field names only, no secret values.
func generatedFieldsOutput(generated map[string][]string) map[string]any {
	out := make(map[string]any, len(generated))
	for path, fields := range generated {
		sorted := append([]string(nil), fields...)
		sort.Strings(sorted)
		out[path] = toAnySlice(sorted)
	}
	return out
}
