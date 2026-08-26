// Package state implements the keeper-side core module `core.state` — the step
// that writes a service state field, at the step ([ADR-0084]) rather than in an
// end-of-run commit.
//
// The state suffix of the address IS the verb, one address per verb:
//
//	core.state.set      overwrite the field
//	core.state.present  write it only if it has no value yet
//	core.state.add      idempotently add one element to a collection
//	core.state.append   append one element to a list, no identity check
//	core.state.modify   patch every element matching `match:`
//	core.state.remove   drop every element matching `match:`
//	core.state.unset    drop the field itself
//
// The verbs are the ADR-057 ones, applied by the same engine the retired
// `state_changes` used ([stateop.Merge]) — so a verb cannot mean two different
// things depending on which path wrote it.
//
//   - name: Redis users
//     on: keeper
//     module: core.state.set
//     register: redis_users
//     params:
//     field: redis_users
//     value: "${ compute.acl_inventory }"
//
// Resolving declared secrets is ORTHOGONAL to the verb ([ADR-0083] §4, amended
// by [ADR-0084]). Whichever verb writes a field whose properties are declared
// `type: secret`, an absent secret is minted and an existing one is KEPT: an
// incidental re-render must not rotate a live credential. That is what
// `type: secret` means in `state_schema`, not what the verb means — `present`
// here is the field-level guard, and says nothing about the secret inside.
//
// The secret VALUE goes to the derived Vault path and nowhere else. What the
// module returns is the EFFECTIVE value — what this step resolved, existing
// secrets included, not what the caller proposed — with every secret property
// replaced by its `vault:` reference ([ADR-0083] §6). Only the writer knows
// which value won, so only the writer can be quoted; and a reference in the
// register means plaintext never reaches `apply_task_register`.
//
// The Vault path is DERIVED, never authored: `shared/config.SecretField` builds it
// from (service, incarnation, state field, key), and every segment is checked
// against the [ADR-064] grammar. The run's service and incarnation travel on the
// module context (coremod/util runctx).
package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keeperincarnation "github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/stateop"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/secretpolicy"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the base module name without the state suffix (Registry key). The
// author form of the task address is `core.state.<verb>`.
const Name = stateop.ModuleName

// Output keys of the register payload.
const (
	// OutputEffective — the effective state of the field: what is stored now,
	// with each secret property carrying its `vault:` reference. Consumers read
	// `register.<name>.effective`.
	OutputEffective = "effective"
	// OutputField — the state field this task wrote, echoed for diagnostics.
	OutputField = "field"
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
	// Store commits the captured field ([ADR-0084]). Set through [Module.WithStore];
	// without it Apply fails rather than resolving secrets it cannot record.
	Store Store
}

// New is the wire helper.
func New(v VaultKV, a AuditWriter, mount string) *Module {
	return &Module{Vault: v, Audit: a, Mount: mount}
}

// WithStore attaches the state store. Same builder shape as
// soul.New(...).WithPresence(...).
func (m *Module) WithStore(s Store) *Module {
	m.Store = s
	return m
}

// Validate is the runtime guard for the author form (soul-lint checks it
// statically). It cannot check the field against `state_schema`: Validate has no
// run context, so the schema is unavailable here — Apply does that check.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	spec, known := stateop.States[req.State]
	if !known {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{
			fmt.Sprintf("unknown state %q (want %s)", req.State, stateop.KnownStates()),
		}}, nil
	}
	errs := stateop.CheckParams(req.Params.AsMap(), spec)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op, like the other keeper-side core modules.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply resolves the field's secrets and returns the effective state. Every
// failure is a failed EVENT rather than a gRPC error, so the run enters
// onfail/error_locked like any other task.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	spec, known := stateop.States[req.State]
	if !known {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want %s)", req.State, stateop.KnownStates()))
	}
	params := req.Params.AsMap()
	if errs := stateop.CheckParams(params, spec); len(errs) > 0 {
		return util.SendFailed(stream, strings.Join(errs, "; "))
	}
	field, _ := stateop.StringParam(params, stateop.ParamField)

	service, incarnation := util.ServiceFrom(ctx), util.IncarnationFrom(ctx)
	if service == "" || incarnation == "" {
		// Fail closed rather than derive a path with an empty segment: the owner
		// is what makes the path unforgeable.
		return util.SendFailed(stream, "the run's service and incarnation are unknown -- core.state derives its Vault path from them")
	}
	schema := util.StateSchemaFrom(ctx)
	if schema == nil {
		return util.SendFailed(stream, "the service state_schema is unavailable -- core.state resolves declared secrets from it")
	}
	declared, issues := config.CollectSecretFields(schema)
	if len(issues) > 0 {
		// Unreachable through a loaded manifest (validateSecretFields rejects the
		// same issues at load), so this is a torn invariant, not author error.
		return util.SendFailed(stream, fmt.Sprintf("service state_schema declares an unsupported secret at %s: %s", issues[0].Path, issues[0].Message))
	}
	if !topLevelProperty(schema, field) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: %q is not a top-level property of the service state_schema", stateop.ParamField, field))
	}
	// Checked BEFORE the Vault work: minting a secret this task then cannot
	// record would leave a live credential nothing in state points at.
	scope, ok := util.RunScopeFrom(ctx)
	if !ok {
		return util.SendFailed(stream, "the run scope is unknown -- core.state records in state_history which run captured the field")
	}
	if m.Store == nil {
		return util.SendFailed(stream, "the state store is not configured -- core.state writes the captured field itself")
	}
	var evals util.StateOpEvaluators
	if spec.NeedsEval {
		// Without an evaluator every predicate would match nothing, and a
		// `modify`/`remove` that matched nothing reports a successful no-op.
		if evals, ok = util.StateOpEvaluatorsFrom(ctx); !ok {
			return util.SendFailed(stream, fmt.Sprintf("the merge-time evaluators are unavailable -- state %q matches its elements with a CEL predicate", req.State))
		}
	}

	op, err := stateop.BuildOp(spec, field, params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// `present` decides BEFORE the Vault work, not only inside the merge: a field
	// that already has a value discards the incoming one, and minting a secret for
	// a write that is then discarded would leave a live credential nothing in
	// state points at. What the step resolves in that case is the STORED value —
	// the register quotes what won, and what won is what was already there.
	// See [Store.ReadState] for why a lock-free read is enough.
	value, noMint := params[stateop.ParamValue], false
	if spec.Verb == config.VerbPresent {
		current, rerr := m.Store.ReadState(ctx, incarnation)
		if rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("read state of %q: %v", incarnation, rerr))
		}
		if v, ok := current[field]; ok && v != nil {
			value, noMint = v, true
		}
	}

	var res resolveResult
	if spec.TakesValue {
		res, err = m.resolve(ctx, resolveScope{
			service:     service,
			incarnation: incarnation,
			field:       field,
			element:     spec.Element,
			noMint:      noMint,
			secrets:     secretsOfField(declared, field),
		}, value)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
		// A COPY: the merge stores the value by reference and the strip of declared
		// secrets then edits it in place, which would hollow out the very
		// references the register is about to carry ([ADR-0083] §6).
		op.Value = stateop.DeepCopy(res.effective)
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

	// The capture itself ([ADR-0084]): the field lands in `incarnation.state` HERE,
	// under the run that produced it, instead of riding an end-of-run commit.
	//
	// What is stored is the field put through the SAME verb engine the retired
	// `state_changes` used ([stateop.Merge]), so a verb means here exactly what it
	// meant there — including the strip of declared secrets, which keeps the value
	// in Vault and the reference in the register.
	stateChanged := false
	_, captureErr := m.Store.CaptureState(ctx, keeperincarnation.CaptureSpec{
		Name:         incarnation,
		Scenario:     scope.Scenario,
		ApplyID:      scope.ApplyID,
		HistoryID:    audit.NewULID(),
		ChangedByAID: optionalAID(scope.StartedByAID),
	}, func(before map[string]any) (map[string]any, error) {
		after, merr := stateop.Merge(before, []render.RenderedOp{op}, schema, evals.Match, evals.Op)
		if merr != nil {
			return nil, merr
		}
		stateChanged = !reflect.DeepEqual(before[field], after[field])
		return after, nil
	})
	if captureErr != nil {
		return util.SendFailed(stream, fmt.Sprintf("capture state field %q: %v", field, captureErr))
	}

	// Changed means the run changed something durable: a secret was minted, or the
	// stored field is not what it was. Reporting only the mint would call a real
	// state change a no-op.
	out := map[string]any{
		OutputField:     field,
		OutputGenerated: toAnySlice(res.generated),
	}
	if spec.TakesValue {
		out[OutputEffective] = res.effective
	}
	return util.SendFinal(stream, len(res.generated) > 0 || stateChanged, out)
}

// optionalAID maps an empty operator id to NULL. A run with no operator behind it
// (a schedule, an internal path) records no author rather than an empty one.
func optionalAID(aid string) *string {
	if aid == "" {
		return nil
	}
	return &aid
}

// resolveScope is the addressing context of one Apply: which service field is
// being written, and which of its properties are declared secrets.
type resolveScope struct {
	service     string
	incarnation string
	field       string
	// noMint — the value being resolved is what is ALREADY stored (`present` over a
	// full field), so nothing may be minted for it. A secret property was stripped
	// on its way into state, so what is resolved for it is the reference to a value
	// that must already exist.
	noMint bool
	// element — the value being resolved is ONE element of a collection field
	// (`add`/`append`) rather than the whole field, which moves the declared
	// secrets one level up in the value and out of the `[i]` diagnostic path.
	element bool
	secrets []config.SecretField
}

// at renders the position of one element in a diagnostic: "" when the value IS
// the element, "[i]" when it is the i-th of a whole-field list.
func (sc resolveScope) at(i int) string {
	if sc.element {
		return ""
	}
	return fmt.Sprintf("[%d]", i)
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
	case sc.secrets[0].Collection() && sc.element:
		// One element of the collection: the same walk over a list of one, then
		// unwrapped, so an element carries the same rules whether it arrives alone
		// or inside the whole field.
		if err = noStrayMarkers(markers, collectionClaims(sc, 1)); err != nil {
			return resolveResult{}, err
		}
		var eff any
		eff, out.generated, err = m.resolveCollection(ctx, sc, []any{value})
		if err == nil {
			out.effective = eff.([]any)[0]
		}
	case sc.secrets[0].Collection():
		items, ok := value.([]any)
		if !ok {
			return resolveResult{}, fmt.Errorf("param %q: %q holds a collection of secrets, so the value must be a list, got %T", stateop.ParamValue, sc.field, value)
		}
		if err = noStrayMarkers(markers, collectionClaims(sc, len(items))); err != nil {
			return resolveResult{}, err
		}
		out.effective, out.generated, err = m.resolveCollection(ctx, sc, items)
	case sc.element:
		// The field IS one secret, so it has no elements to add to.
		return resolveResult{}, fmt.Errorf("%q is declared a single secret, not a collection -- write it whole with core.state.set or core.state.present", sc.field)
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
	for _, path := range stateop.SortedKeys(markers) {
		if claimable[path] {
			continue
		}
		return fmt.Errorf("param %q%s: a secret request sits where no property is declared `type: secret` -- declare it in state_schema, or drop the generate_secret() call", stateop.ParamValue, path)
	}
	return nil
}

// collectionClaims lists the positions a collection field's declared secrets would
// resolve, in the path form [scanMarkers] produces.
func collectionClaims(sc resolveScope, n int) map[string]bool {
	out := make(map[string]bool, n*len(sc.secrets))
	for i := 0; i < n; i++ {
		for _, s := range sc.secrets {
			out[sc.at(i)+"."+s.Property] = true
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
			return nil, nil, fmt.Errorf("param %q%s: expected an object, got %T", stateop.ParamValue, sc.at(i), raw)
		}
		eff := make(map[string]any, len(elem))
		for k, v := range elem {
			eff[k] = v
		}
		for _, s := range sc.secrets {
			key, err := elementKey(elem, s, sc.at(i))
			if err != nil {
				return nil, nil, err
			}
			path := sc.at(i) + "." + s.Property
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
		return "", "", fmt.Errorf("param %q%s: %w", stateop.ParamValue, at, err)
	case isMarker && sc.noMint:
		// Defensive: state carries no markers (a stray one is refused before the
		// write, a claimed one is replaced by its reference), but one that did arrive
		// here must not mint for a value nobody proposed.
		return m.requireExisting(ctx, sc, s, key, at)
	case isMarker:
		return m.mintIfEmpty(ctx, sc, s, key, policy)
	case requested == nil:
		// Nothing requested: the value must already exist, or there is nothing to
		// reference.
		return m.requireExisting(ctx, sc, s, key, at)
	default:
		return "", "", fmt.Errorf("param %q%s: %q is declared `type: secret` -- its value is minted by generate_secret() and never written literally", stateop.ParamValue, at, s.Property)
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
		if sc.noMint {
			return "", "", fmt.Errorf("state field %q%s: %q is stored but was never minted in Vault -- core.state.present keeps the stored value and cannot mint one for it", sc.field, at, s.Property)
		}
		return "", "", fmt.Errorf("param %q%s: %q has no value yet and none was requested -- set it to generate_secret({…})", stateop.ParamValue, at, s.Property)
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
func elementKey(elem map[string]any, s config.SecretField, at string) (string, error) {
	raw, ok := elem[s.Key]
	if !ok || raw == nil {
		return "", fmt.Errorf("param %q%s: %q addresses the secret %q and is missing", stateop.ParamValue, at, s.Key, s.Property)
	}
	key, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("param %q%s.%s: expected a string, got %T", stateop.ParamValue, at, s.Key, raw)
	}
	if !config.ValidVaultPathSegment(key) {
		return "", fmt.Errorf("param %q%s.%s: %q is not a safe Vault path segment (letters, digits, `_` and `-`)", stateop.ParamValue, at, s.Key, key)
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
		for _, k := range stateop.SortedKeys(t) {
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

// toAnySlice converts to the []any structpb needs.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
