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
//
// Two passes, and the order is the point ([planCollection]): NOTHING is written
// until the whole collection has been accepted.
func (m *Module) resolveCollection(ctx context.Context, sc resolveScope, items []any) (any, []string, error) {
	plan, err := planCollection(sc, items)
	if err != nil {
		return nil, nil, err
	}
	// Read pass. Every remaining refusal is made here -- an element left empty
	// whose value was never minted, an unusable policy -- so the commit below cannot
	// abort a half-written collection over something a read already knew.
	var mp mintPlan
	resolved := make([][]resolvedSecret, len(plan))
	for i, pe := range plan {
		resolved[i] = make([]resolvedSecret, len(sc.secrets))
		for j, s := range sc.secrets {
			rs, err := m.resolveSecret(ctx, sc, s, pe.keys[j], pe.intents[j], sc.at(i)+"."+s.Property, &mp)
			if err != nil {
				return nil, nil, err
			}
			resolved[i][j] = rs
		}
	}
	if err := mp.commit(ctx, m.Vault); err != nil {
		return nil, nil, err
	}

	var generated []string
	out := make([]any, 0, len(plan))
	for i, pe := range plan {
		eff := make(map[string]any, len(pe.elem))
		for k, v := range pe.elem {
			eff[k] = v
		}
		for j, s := range sc.secrets {
			eff[s.Property] = resolved[i][j].ref
			if minted := resolved[i][j].minted; minted != "" {
				generated = append(generated, minted)
			}
		}
		out = append(out, eff)
	}
	return out, generated, nil
}

// plannedElement is one accepted collection element: the element itself and —
// index-parallel to sc.secrets — the key addressing each of its declared secrets
// and what the author asked for there.
type plannedElement struct {
	elem    map[string]any
	keys    []string
	intents []secretIntent
}

// planCollection validates the WHOLE collection before anything is written.
//
// Every refusal it makes is a property of the value alone — the element's shape,
// the key's grammar, the request's syntax — so none of it needs a Vault round-trip
// to decide. Deciding it inside the mint loop instead meant a bad element N aborted
// the run with elements 0..N-1 already written, and the run dies before the capture:
// live secrets in Vault that no state row references and no operator was told about
// (NIM-705).
//
// What that buys is bounded, and the bound is worth stating: a refusal made by
// [Module.resolve] writes nothing, but [Module.Apply] still refuses twice AFTER the
// writes — the audit write, and [stateop.Merge] inside the capture — so an orphan is
// reachable there. The audit event carrying the derived paths is written first, so
// that window leaves a trail; the Merge one does not.
//
// This is the same reason [noStrayMarkers] runs collection-wide in [resolve].
//
// The pass also enforces what only a whole-collection view can see: two elements
// must not derive the SAME path. The keys addressing one declared secret are
// therefore unique within the collection, or the run fails before it writes
// (NIM-704) — the alternative is the first element minting and the second silently
// keeping that value, two accounts on one credential and nothing saying so.
//
// Unique WITHIN THE COLLECTION is all it can say, and what it is handed is one
// whole collection: `add`/`append` hand over a single element, so one whose key
// already addresses a STORED element is accepted and produces exactly the harm
// above. Closing that needs the stored collection read here and its survival
// through the merge predicted — a decision, not an omission.
//
// The one route that does judge stored elements is `present` over a field that
// already has a value: [Module.Apply] resolves the STORED collection there, so a
// collection already corrupted that way fails every later `present` on it. Loud is
// the right end of that trade — the message names both positions — but the run it
// fails is not the run that caused it.
func planCollection(sc resolveScope, items []any) ([]plannedElement, error) {
	out := make([]plannedElement, 0, len(items))
	// One key set per declared secret, not one for the element: two secrets of the
	// same element may be addressed by DIFFERENT `key:` siblings, and what must be
	// distinct is the (key, property) pair the path is derived from.
	seen := make([]map[string]int, len(sc.secrets))
	for j := range seen {
		seen[j] = make(map[string]int, len(items))
	}
	for i, raw := range items {
		elem, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("param %q%s: expected an object, got %T", stateop.ParamValue, sc.at(i), raw)
		}
		pe := plannedElement{
			elem:    elem,
			keys:    make([]string, len(sc.secrets)),
			intents: make([]secretIntent, len(sc.secrets)),
		}
		for j, s := range sc.secrets {
			key, err := elementKey(elem, s, sc.at(i))
			if err != nil {
				return nil, err
			}
			if first, dup := seen[j][key]; dup {
				return nil, fmt.Errorf("param %q%s.%s: %q already addresses %q at %s -- both elements derive the same Vault path, so the second would keep the first one's secret and two accounts would share one credential",
					stateop.ParamValue, sc.at(i), s.Key, key, s.Property, sc.at(first))
			}
			seen[j][key] = i
			intent, err := requestedSecret(sc, s, elem[s.Property], sc.at(i)+"."+s.Property)
			if err != nil {
				return nil, err
			}
			pe.keys[j], pe.intents[j] = key, intent
		}
		out = append(out, pe)
	}
	return out, nil
}

// resolveScalar handles a field that IS one secret: one value for the whole
// incarnation, no key.
func (m *Module) resolveScalar(ctx context.Context, sc resolveScope, s config.SecretField, value any) (any, []string, error) {
	intent, err := requestedSecret(sc, s, value, "")
	if err != nil {
		return nil, nil, err
	}
	var mp mintPlan
	rs, err := m.resolveSecret(ctx, sc, s, "", intent, "", &mp)
	if err != nil {
		return nil, nil, err
	}
	if err := mp.commit(ctx, m.Vault); err != nil {
		return nil, nil, err
	}
	if rs.minted == "" {
		return rs.ref, nil, nil
	}
	return rs.ref, []string{rs.minted}, nil
}

// secretIntent is what one declared property asks for, decided WITHOUT touching
// Vault so the decision can be made for the whole collection before the first
// write. mint false means the value must already exist.
type secretIntent struct {
	mint   bool
	policy secretpolicy.Policy
}

// requestedSecret classifies one declared property's requested value.
//
// The property accepts exactly two things: a secret request, or nothing at all. A
// literal string is refused — a plaintext password written into a task param has
// already travelled through render and the run's diagnostics by the time it gets
// here, which is the leak this ADR closes.
func requestedSecret(sc resolveScope, s config.SecretField, requested any, at string) (secretIntent, error) {
	switch policy, isMarker, err := secretpolicy.FromMarker(requested); {
	case err != nil:
		return secretIntent{}, fmt.Errorf("param %q%s: %w", stateop.ParamValue, at, err)
	case isMarker && sc.noMint:
		// Defensive: state carries no markers (a stray one is refused before the
		// write, a claimed one is replaced by its reference), but one that did arrive
		// here must not mint for a value nobody proposed.
		return secretIntent{}, nil
	case isMarker:
		return secretIntent{mint: true, policy: policy}, nil
	case requested == nil:
		// Nothing requested: the value must already exist, or there is nothing to
		// reference.
		return secretIntent{}, nil
	default:
		return secretIntent{}, fmt.Errorf("param %q%s: %q is declared `type: secret` -- its value is minted by generate_secret() and never written literally", stateop.ParamValue, at, s.Property)
	}
}

// resolvedSecret is one declared property after Vault has been read: the `vault:`
// reference the register carries, and the derived path of the value it staged for
// minting ("" when a value was already there).
type resolvedSecret struct {
	ref    string
	minted string
}

// mintPlan is the set of writes a resolve decided on, one entry per Vault path.
//
// Two declared secrets can land on the SAME path -- a collection addressed by two
// different `key:` siblings puts one element's property under another element's key
// -- and a KV write replaces the whole entry, so the fields have to be merged here.
// Resolving each one against its own read and writing them one after the other
// would drop every field but the last.
type mintPlan struct {
	order []string
	data  map[string]map[string]any
}

// stage records one minted field, merged onto whatever is already staged for that
// path, or onto the entry as Vault holds it.
func (p *mintPlan) stage(path string, payload map[string]any, field string, value any) {
	entry, staged := p.data[path]
	if !staged {
		entry = make(map[string]any, len(payload)+1)
		for k, v := range payload {
			entry[k] = v
		}
		if p.data == nil {
			p.data = make(map[string]map[string]any, 1)
		}
		p.data[path] = entry
		p.order = append(p.order, path)
	}
	entry[field] = value
}

// commit performs the staged writes. It refuses nothing: what is left here is
// Vault's own failure, the one thing no earlier pass can rule out.
func (p *mintPlan) commit(ctx context.Context, vault VaultKV) error {
	for _, path := range p.order {
		if err := vault.WriteKV(ctx, path, p.data[path]); err != nil {
			// WriteKV does not put values in its error text (vault.Client
			// invariant); neither does this -- path only.
			return fmt.Errorf("vault write %q: %w", path, err)
		}
	}
	return nil
}

// resolveSecret reads the derived path and decides what has to happen there,
// staging a mint rather than performing it. `present`, not `set`: a value already
// in Vault is kept.
//
// Separating the decision from the write is what lets a collection refuse element N
// with elements 0..N-1 still unwritten. A property left empty whose value was never
// minted is the case that matters -- it is a refusal only a read can make, so
// [planCollection] cannot make it, and made inline it aborted the run with the
// earlier elements already live in Vault, unreferenced by any state row (NIM-705).
func (m *Module) resolveSecret(ctx context.Context, sc resolveScope, s config.SecretField, key string, in secretIntent, at string, mp *mintPlan) (resolvedSecret, error) {
	path, ref, err := m.derive(s, sc, key)
	if err != nil {
		return resolvedSecret{}, err
	}
	payload, err := m.readPath(ctx, path)
	if err != nil {
		return resolvedSecret{}, fmt.Errorf("vault read %q: %w", path, err)
	}
	if fieldPresent(payload, s.VaultField()) {
		return resolvedSecret{ref: ref}, nil
	}
	if !in.mint {
		if sc.noMint {
			return resolvedSecret{}, fmt.Errorf("state field %q%s: %q is stored but was never minted in Vault -- core.state.present keeps the stored value and cannot mint one for it", sc.field, at, s.Property)
		}
		return resolvedSecret{}, fmt.Errorf("param %q%s: %q has no value yet and none was requested -- set it to generate_secret({…})", stateop.ParamValue, at, s.Property)
	}
	value, err := in.policy.Generate()
	if err != nil {
		return resolvedSecret{}, fmt.Errorf("generate secret for %q: %w", path, err)
	}
	mp.stage(path, payload, s.VaultField(), value)
	return resolvedSecret{ref: ref, minted: path + "#" + s.VaultField()}, nil
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
