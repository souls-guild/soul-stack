// Package state implements the keeper-side core module `core.state.present`
// ([ADR-0083] §4) — the single write of a service state field that carries
// declared secrets.
//
// One call is read-or-write. On a property declared `type: secret` in the
// service's `state_schema` the semantics are `present`, never `set`: an existing
// Vault value is kept and the incoming [secretpolicy] request discarded. An
// incidental re-render must not rotate a live credential, so deliberate rotation
// is not expressible here by design — it gets its own decision and its own ticket.
//
//   - name: Redis users
//     on: keeper
//     module: core.state.present
//     register: redis_users
//     params:
//     key: redis_users
//     set: "${ compute.acl_inventory }"
//
// The secret VALUE goes to the derived Vault path and nowhere else. What the
// module returns is the EFFECTIVE state — what is now stored, existing values
// included, not what the caller proposed — with every secret property replaced by
// its `vault:` reference ([ADR-0083] §6). Only the writer knows which value won,
// so only the writer can be quoted; and a reference in the register means
// plaintext never reaches `apply_task_register`.
//
// The Vault path is DERIVED, never authored: `shared/config.SecretField` builds it
// from (service, incarnation, state field, key), and every segment is checked
// against the [ADR-064] grammar. The run's service and incarnation travel on the
// module context (coremod/util runctx), like `core.cloud`'s incarnation.
package state

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/secretpolicy"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the base module name without the state suffix (Registry key). The
// author form of the task address is `core.state.present`.
const Name = "core.state"

// StatePresent is the only state. `present` and not `set` is the whole point of
// §4: a declared secret that already has a value keeps it.
const StatePresent = "present"

// Output keys of the register payload.
const (
	// OutputEffective — the effective state of the field: what is stored now,
	// with each secret property carrying its `vault:` reference. Consumers read
	// `register.<name>.effective`.
	OutputEffective = "effective"
	// OutputKey — the state field this task wrote, echoed for diagnostics.
	OutputKey = "key"
	// OutputGenerated — the derived Vault paths whose secret this run MINTED,
	// sorted, in flat form (`<mount>/<service>/…#<field>`, no `vault:` prefix).
	// Paths, never values.
	OutputGenerated = "generated"
)

// VaultKV is the narrow subset of keeper/internal/vault.Client the module needs:
// read (does this secret already exist?) and write (mint the missing one). Same
// shape as coremod/vault.VaultWriter, declared locally so a fake needs no HTTP.
type VaultKV interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// AuditWriter is the narrow audit dependency. nil is allowed (the module skips
// the write and continues), like the other keeper-side modules.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module implements sdk/module.SoulModule.
type Module struct {
	Vault VaultKV
	Audit AuditWriter
	// Mount is the Vault KV mount from keeper.yml (`vault.kv_mount`). "" means
	// the default mount, resolved by [config.SecretField.VaultPath].
	Mount string
}

// New is the wire helper.
func New(v VaultKV, a AuditWriter, mount string) *Module {
	return &Module{Vault: v, Audit: a, Mount: mount}
}

// Validate is the runtime guard for the author form (soul-lint checks it
// statically). It cannot check the field against `state_schema`: Validate has no
// run context, so the schema is unavailable here — Apply does that check.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "" && req.State != StatePresent {
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s)", req.State, StatePresent))
	}
	params := req.Params.AsMap()
	if _, err := stringParam(params, paramKey); err != nil {
		errs = append(errs, err.Error())
	}
	if _, ok := params[paramSet]; !ok {
		errs = append(errs, fmt.Sprintf("param %q: missing", paramSet))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op, like the other keeper-side core modules.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Param names.
const (
	paramKey = "key"
	paramSet = "set"
)

// Apply resolves the field's secrets and returns the effective state. Every
// failure is a failed EVENT rather than a gRPC error, so the run enters
// onfail/error_locked like any other task.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if req.State != "" && req.State != StatePresent {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want %s)", req.State, StatePresent))
	}
	params := req.Params.AsMap()
	field, err := stringParam(params, paramKey)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	value, ok := params[paramSet]
	if !ok {
		return util.SendFailed(stream, fmt.Sprintf("param %q: missing", paramSet))
	}

	service, incarnation := util.ServiceFrom(ctx), util.IncarnationFrom(ctx)
	if service == "" || incarnation == "" {
		// Fail closed rather than derive a path with an empty segment: the owner
		// is what makes the path unforgeable.
		return util.SendFailed(stream, "the run's service and incarnation are unknown -- core.state.present derives its Vault path from them")
	}
	schema := util.StateSchemaFrom(ctx)
	if schema == nil {
		return util.SendFailed(stream, "the service state_schema is unavailable -- core.state.present resolves declared secrets from it")
	}
	declared, issues := config.CollectSecretFields(schema)
	if len(issues) > 0 {
		// Unreachable through a loaded manifest (validateSecretFields rejects the
		// same issues at load), so this is a torn invariant, not author error.
		return util.SendFailed(stream, fmt.Sprintf("service state_schema declares an unsupported secret at %s: %s", issues[0].Path, issues[0].Message))
	}
	if !topLevelProperty(schema, field) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: %q is not a top-level property of the service state_schema", paramKey, field))
	}

	res, err := m.resolve(ctx, resolveScope{
		service:     service,
		incarnation: incarnation,
		field:       field,
		secrets:     secretsOfField(declared, field),
	}, value)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if m.Audit != nil && len(res.generated) > 0 {
		ev := &audit.Event{
			// The fact is the one `core.vault.kv-present` already records — a
			// secret was ensured present at these paths — so it reuses that event
			// type rather than splitting the same fact across two names an
			// operator would have to know to filter on both.
			EventType: audit.EventVaultKVPresent,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"paths":       res.generated, // derived paths; no values
				"state_field": field,
				"service":     service,
				"incarnation": incarnation,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	return util.SendFinal(stream, len(res.generated) > 0, map[string]any{
		OutputKey:       field,
		OutputEffective: res.effective,
		OutputGenerated: toAnySlice(res.generated),
	})
}

// resolveScope is the addressing context of one Apply: which service field is
// being written, and which of its properties are declared secrets.
type resolveScope struct {
	service     string
	incarnation string
	field       string
	secrets     []config.SecretField
}

// resolveResult is what Apply reports: the effective state and the derived paths
// this run minted.
type resolveResult struct {
	effective any
	generated []string
}

// resolve applies present-semantics to one state field.
//
// The incoming value is first scanned for EVERY secret-request marker wherever it
// sits, and a marker in a position no declared property claims is an error. A
// request the module does not resolve must not travel on into `incarnation.state`
// as ordinary data: it would look like a password was asked for when nothing minted
// one. The check runs BEFORE the write, so a misplaced request costs nothing.
func (m *Module) resolve(ctx context.Context, sc resolveScope, value any) (resolveResult, error) {
	markers := map[string]bool{}
	scanMarkers(value, "", markers)

	var out resolveResult
	var err error
	switch {
	case len(sc.secrets) == 0:
		// No declared secret on this field — the module is still the write point,
		// the value passes through unchanged.
		if err = noStrayMarkers(markers, nil); err != nil {
			return resolveResult{}, err
		}
		out.effective = value
	case sc.secrets[0].Collection():
		items, ok := value.([]any)
		if !ok {
			return resolveResult{}, fmt.Errorf("param %q: %q holds a collection of secrets, so the value must be a list, got %T", paramSet, sc.field, value)
		}
		if err = noStrayMarkers(markers, collectionClaims(sc, len(items))); err != nil {
			return resolveResult{}, err
		}
		out.effective, out.generated, err = m.resolveCollection(ctx, sc, items)
	default:
		if err = noStrayMarkers(markers, map[string]bool{"": true}); err != nil {
			return resolveResult{}, err
		}
		out.effective, out.generated, err = m.resolveScalar(ctx, sc, sc.secrets[0], value)
	}
	if err != nil {
		return resolveResult{}, err
	}
	sort.Strings(out.generated)
	return out, nil
}

// noStrayMarkers fails on a secret request sitting where no declared property would
// resolve it.
func noStrayMarkers(markers, claimable map[string]bool) error {
	for _, path := range sortedKeys(markers) {
		if claimable[path] {
			continue
		}
		return fmt.Errorf("param %q%s: a secret request sits where no property is declared `type: secret` -- declare it in state_schema, or drop the generate_secret() call", paramSet, path)
	}
	return nil
}

// collectionClaims lists the positions a collection field's declared secrets would
// resolve, in the path form [scanMarkers] produces.
func collectionClaims(sc resolveScope, n int) map[string]bool {
	out := make(map[string]bool, n*len(sc.secrets))
	for i := 0; i < n; i++ {
		for _, s := range sc.secrets {
			out[fmt.Sprintf("[%d].%s", i, s.Property)] = true
		}
	}
	return out
}

// resolveCollection handles a field whose ELEMENTS carry secrets: `set` is a list
// of objects, each addressed by the declared `key:` sibling.
func (m *Module) resolveCollection(ctx context.Context, sc resolveScope, items []any) (any, []string, error) {
	var generated []string
	out := make([]any, 0, len(items))
	for i, raw := range items {
		elem, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("param %q[%d]: expected an object, got %T", paramSet, i, raw)
		}
		eff := make(map[string]any, len(elem))
		for k, v := range elem {
			eff[k] = v
		}
		for _, s := range sc.secrets {
			key, err := elementKey(elem, s, i)
			if err != nil {
				return nil, nil, err
			}
			path := fmt.Sprintf("[%d].%s", i, s.Property)
			ref, minted, err := m.present(ctx, sc, s, key, elem[s.Property], path)
			if err != nil {
				return nil, nil, err
			}
			eff[s.Property] = ref
			if minted != "" {
				generated = append(generated, minted)
			}
		}
		out = append(out, eff)
	}
	return out, generated, nil
}

// resolveScalar handles a field that IS one secret: one value for the whole
// incarnation, no key.
func (m *Module) resolveScalar(ctx context.Context, sc resolveScope, s config.SecretField, value any) (any, []string, error) {
	ref, minted, err := m.present(ctx, sc, s, "", value, "")
	if err != nil {
		return nil, nil, err
	}
	if minted == "" {
		return ref, nil, nil
	}
	return ref, []string{minted}, nil
}

// present is the read-or-write itself, for one secret. It returns the `vault:`
// reference the register carries, and the derived path when a value was minted
// ("" when the existing one was kept).
//
// The declared property accepts exactly two things: a secret request, or nothing
// at all. A literal string is refused — a plaintext password written into a task
// param has already travelled through render and the run's diagnostics by the
// time it gets here, which is the leak this ADR closes.
func (m *Module) present(ctx context.Context, sc resolveScope, s config.SecretField, key string, requested any, at string) (string, string, error) {
	switch policy, isMarker, err := secretpolicy.FromMarker(requested); {
	case err != nil:
		return "", "", fmt.Errorf("param %q%s: %w", paramSet, at, err)
	case isMarker:
		return m.mintIfEmpty(ctx, sc, s, key, policy)
	case requested == nil:
		// Nothing requested: the value must already exist, or there is nothing to
		// reference.
		return m.requireExisting(ctx, sc, s, key, at)
	default:
		return "", "", fmt.Errorf("param %q%s: %q is declared `type: secret` -- its value is minted by generate_secret() and never written literally", paramSet, at, s.Property)
	}
}

// mintIfEmpty reads the derived path and writes a new value only when the field
// is absent or empty. `present`, not `set`.
func (m *Module) mintIfEmpty(ctx context.Context, sc resolveScope, s config.SecretField, key string, policy secretpolicy.Policy) (string, string, error) {
	path, ref, err := m.derive(s, sc, key)
	if err != nil {
		return "", "", err
	}
	payload, err := m.readPath(ctx, path)
	if err != nil {
		return "", "", fmt.Errorf("vault read %q: %w", path, err)
	}
	if fieldPresent(payload, s.VaultField()) {
		return ref, "", nil
	}
	value, err := policy.Generate()
	if err != nil {
		return "", "", fmt.Errorf("generate secret for %q: %w", path, err)
	}
	// Read-merge-write: a neighbouring field of the same KV entry must survive.
	data := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		data[k] = v
	}
	data[s.VaultField()] = value
	if err := m.Vault.WriteKV(ctx, path, data); err != nil {
		// WriteKV does not put values in its error text (vault.Client invariant);
		// neither does this — path only.
		return "", "", fmt.Errorf("vault write %q: %w", path, err)
	}
	return ref, path + "#" + s.VaultField(), nil
}

// requireExisting resolves a property left empty by the author: the reference is
// only meaningful if something already minted the value.
func (m *Module) requireExisting(ctx context.Context, sc resolveScope, s config.SecretField, key, at string) (string, string, error) {
	path, ref, err := m.derive(s, sc, key)
	if err != nil {
		return "", "", err
	}
	payload, err := m.readPath(ctx, path)
	if err != nil {
		return "", "", fmt.Errorf("vault read %q: %w", path, err)
	}
	if !fieldPresent(payload, s.VaultField()) {
		return "", "", fmt.Errorf("param %q%s: %q has no value yet and none was requested -- set it to generate_secret({…})", paramSet, at, s.Property)
	}
	return ref, "", nil
}

// derive builds the Vault path and the reference for one secret. Both come from
// [config.SecretField], which validates every segment and fails closed.
func (m *Module) derive(s config.SecretField, sc resolveScope, key string) (string, string, error) {
	path, err := s.VaultPath(m.Mount, sc.service, sc.incarnation, key)
	if err != nil {
		return "", "", err
	}
	ref, err := s.VaultRef(m.Mount, sc.service, sc.incarnation, key)
	if err != nil {
		return "", "", err
	}
	return path, ref, nil
}

// readPath reads a KV path; a path that does not exist is not an error (nothing
// minted it yet), transport and policy errors are.
func (m *Module) readPath(ctx context.Context, path string) (map[string]any, error) {
	if m.Vault == nil {
		return nil, errors.New("the Vault client is not configured")
	}
	payload, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		if errors.Is(err, keepervault.ErrVaultKVNotFound) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return payload, nil
}

// elementKey reads the sibling property that addresses one element's secret. It
// is the one path segment that comes from state DATA, so it is checked here as
// well as inside VaultPath — this way the diagnostic names the element.
func elementKey(elem map[string]any, s config.SecretField, idx int) (string, error) {
	raw, ok := elem[s.Key]
	if !ok || raw == nil {
		return "", fmt.Errorf("param %q[%d]: %q addresses the secret %q and is missing", paramSet, idx, s.Key, s.Property)
	}
	key, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("param %q[%d].%s: expected a string, got %T", paramSet, idx, s.Key, raw)
	}
	if !config.ValidVaultPathSegment(key) {
		return "", fmt.Errorf("param %q[%d].%s: %q is not a safe Vault path segment (letters, digits, `_` and `-`)", paramSet, idx, s.Key, key)
	}
	return key, nil
}

// secretsOfField returns the declared secrets belonging to one top-level state
// field, ordered by property name so the walk is deterministic.
func secretsOfField(declared []config.SecretField, field string) []config.SecretField {
	var out []config.SecretField
	for _, s := range declared {
		if s.State == field {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Property < out[j].Property })
	return out
}

// topLevelProperty reports whether the state_schema declares field at its root.
// A typo in `key:` would otherwise write a field the schema does not know, and
// the state commit would reject the whole run one phase later.
func topLevelProperty(schema map[string]any, field string) bool {
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[field]
	return ok
}

// scanMarkers records the path of EVERY secret-request marker in the value. The
// walk is structural rather than shape-aware, so a request in a position nobody
// anticipated is found and reported instead of travelling on as data.
func scanMarkers(v any, path string, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[secretpolicy.MarkerKey]; ok {
			out[path] = true
			return
		}
		for _, k := range sortedKeys(t) {
			scanMarkers(t[k], path+"."+k, out)
		}
	case []any:
		for i, e := range t {
			scanMarkers(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// fieldPresent reports whether a KV payload holds a non-empty string at field.
// An empty string counts as absent: an empty password is useless, and treating
// it as present would leave the field permanently unmintable (parity with
// coremod/vault.fieldPresent).
func fieldPresent(payload map[string]any, field string) bool {
	v, ok := payload[field]
	if !ok || v == nil {
		return false
	}
	s, ok := v.(string)
	return ok && s != ""
}

// stringParam reads a required string param out of the decoded params map.
func stringParam(params map[string]any, key string) (string, error) {
	v, ok := params[key]
	if !ok || v == nil {
		return "", fmt.Errorf("param %q: missing", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("param %q: empty", key)
	}
	return s, nil
}

// sortedKeys returns a map's keys in sorted order — every walk must produce
// byte-identical output for identical input.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toAnySlice converts to the []any structpb needs.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
