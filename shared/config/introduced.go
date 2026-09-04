package config

// The automatic layer over the declared compat window (ADR-0076(i)/(j)/(k)).
//
// `compat:` is written by a human and can lie: an author declares `min: 0.1.0`
// and then uses something that only exists from 0.3.0 — the declaration passes
// every check in compat.go, and the run breaks on a keeper the window itself
// admits. `introduced_in` metadata says which keeper release first understood a
// feature, so the floor a definition ACTUALLY requires can be derived from its
// body and compared with the floor its author declared.
//
// Two boundaries, both deliberate:
//
//   - Inference yields a floor, NEVER a ceiling (ADR-0076(j)). Nothing in the
//     code can know that a later keeper changed a behavior this definition
//     depends on — only the author who tested it knows where the upper bound is.
//     The declaration therefore stays primary; this layer verifies its lower
//     half.
//   - The mismatch is an authoring diagnostic, NOT a render-time abort
//     (ADR-0076(k)). On a keeper that does carry the feature such a run works
//     correctly, and blocking a run that would succeed is a worse failure than
//     the stale declaration it reports. It also leaves the E2 invariant intact:
//     the declared window is still read from the manifest BEFORE any body is
//     parsed (compat.go), and this is a separate pass over the body.

import (
	"fmt"
	"sort"

	"github.com/Masterminds/semver/v3"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// Unreleased — the `introduced_in` of a feature that has landed on a release
// branch but has not shipped in a tagged version yet.
//
// It contributes NO floor, on purpose. An author cannot declare a window against
// a version that does not exist, so there is nothing to cross-check until the
// release is cut; and stamping a guessed future number would be worse than
// silence — every definition using the feature would be rejected by the E2 gate
// on today's keeper, which does render it. RELEASING.md turns these into real
// versions at the moment the number becomes a fact.
const Unreleased = ""

// KeeperFeature — one keeper-side feature a definition uses, with the release
// that introduced it. ID is the stable name shown in diagnostics; Where is the
// YAML-ish location it was found at (empty for manifest-level features, whose
// location is the manifest itself).
type KeeperFeature struct {
	ID           string
	IntroducedIn string
	Where        string
}

// dslFeature — one row of the keeper-side DSL registry: a grammar element and
// the release that first understood it. The detectors live at the call sites
// (KeeperFeaturesOf*) rather than as closures here, so a feature that spans
// several structures stays one row.
type dslFeature struct {
	id           string
	introducedIn string
}

// Keeper-side DSL feature IDs. Stable strings — they appear in operator-facing
// diagnostics, so renaming one is a user-visible change.
const (
	// FeatureServiceCompat / FeatureDestinyCompat — the `compat:` block itself
	// (ADR-0076(a)). Self-referential on purpose: a keeper older than the release
	// that introduced the block rejects it as a generic `unknown_key` (the
	// bootstrap paradox, ADR-0076(m)), so declaring a window that reaches BELOW
	// that release promises compatibility with keepers that refuse the manifest
	// outright.
	FeatureServiceCompat = "service.compat"
	FeatureDestinyCompat = "destiny.compat"

	// FeatureDestinyValidate — the top-level `validate:` block of `destiny.yml`
	// (ADR-009 amendment, NIM-167). An older keeper has no such key.
	FeatureDestinyValidate = "destiny.validate"

	// FeatureDestinyInputRequiredWhen — `required_when:` on a destiny input
	// parameter (NIM-167). The sharp case: the key PARSED before, it was simply
	// never evaluated for destiny inputs, so an older keeper accepts the manifest
	// and silently drops the invariant.
	FeatureDestinyInputRequiredWhen = "destiny.input.required_when"

	// FeatureDestinyInputConstraints — per-value constraints on a destiny input
	// parameter (`enum` / `pattern` / `format` / `min_length` / `max_length`,
	// NIM-167). Same silent shape as required_when: parsed before, enforced only
	// from the release that brought destiny input to full parity with scenario.
	FeatureDestinyInputConstraints = "destiny.input.constraints"

	// FeatureTaskAsync — the task key `async:` (ADR-0075, NIM-150). An older
	// keeper has no such key and rejects the manifest as a generic `unknown_key`,
	// which names the key but not the reason — the same bootstrap-paradox shape as
	// `compat:` itself.
	FeatureTaskAsync = "task.async"

	// FeatureTaskRequire — the task key `require:` reaching a runtime (ADR-0075(g),
	// NIM-150). The sharp case, like required_when: the key PARSED and was
	// reference-checked long before, it simply resolved to nothing on the wire, so
	// an older keeper accepts the manifest and silently drops the ordering
	// invariant instead of refusing it.
	FeatureTaskRequire = "task.require"

	// FeatureScenarioIDTemplate — the create-scenario key `id_template`
	// (ADR-0079, NIM-177; `name_template` before [ADR-0085]): the incarnation id is
	// composed server-side from `input:` instead of being taken as free text. The
	// first SCENARIO-level grammar to carry a floor — until it, a scenario's own
	// manifest contributed none, only its task list did. A keeper that predates it
	// does not compose anything: it expects the identifier in the request, so a
	// definition relying on the template cannot be created there at all.
	//
	// ONE feature id for both spellings: the two are one grammar, and a keeper old
	// enough to refuse one refuses the other for the same reason.
	FeatureScenarioIDTemplate = "scenario.id_template"

	// FeatureTaskBlockInclude — an `include:` nested inside `block:` (NIM-169).
	// Top-level `include:` is baseline grammar; what arrived this cycle is
	// expanding one INSIDE a block, in both layers. An older keeper walks the
	// block and produces no tasks for the nested include — it accepts the
	// definition and quietly renders less of it, the same shape as async: and
	// require:.
	FeatureTaskBlockInclude = "task.block.include"
)

// keeperDSLFeatures — the registry of keeper-side grammar features that arrived
// AFTER the baseline release. A feature absent from this table contributes no
// floor: everything the baseline already understood needs no metadata, and
// listing it would grow a table nobody can keep honest.
//
// The task-level rows are the intra-host concurrency keys (ADR-0075): they are the
// first grammar the task DSL gained since the baseline. Everything else a task can
// carry is covered by the module manifest metadata instead (see
// [KeeperFeaturesOfTasks]).
var keeperDSLFeatures = map[string]dslFeature{
	FeatureServiceCompat:            {id: FeatureServiceCompat, introducedIn: Unreleased},
	FeatureDestinyCompat:            {id: FeatureDestinyCompat, introducedIn: Unreleased},
	FeatureDestinyValidate:          {id: FeatureDestinyValidate, introducedIn: Unreleased},
	FeatureDestinyInputRequiredWhen: {id: FeatureDestinyInputRequiredWhen, introducedIn: Unreleased},
	FeatureDestinyInputConstraints:  {id: FeatureDestinyInputConstraints, introducedIn: Unreleased},
	FeatureTaskAsync:                {id: FeatureTaskAsync, introducedIn: Unreleased},
	FeatureTaskRequire:              {id: FeatureTaskRequire, introducedIn: Unreleased},
	FeatureScenarioIDTemplate:       {id: FeatureScenarioIDTemplate, introducedIn: Unreleased},
	FeatureTaskBlockInclude:         {id: FeatureTaskBlockInclude, introducedIn: Unreleased},
}

// dslFeatureUse builds a used-feature record from the registry. An id absent from
// the registry yields ok=false — a caller detecting a feature nobody registered
// must not silently invent a floor.
func dslFeatureUse(id, where string) (KeeperFeature, bool) {
	f, ok := keeperDSLFeatures[id]
	if !ok {
		return KeeperFeature{}, false
	}
	return KeeperFeature{ID: f.id, IntroducedIn: f.introducedIn, Where: where}, true
}

// KeeperFeaturesOfService collects the keeper-side features a `service.yml`
// uses. The scenarios it owns are walked separately ([KeeperFeaturesOfTasks]) —
// they are separate files and the linter reads them one at a time.
func KeeperFeaturesOfService(m *ServiceManifest) []KeeperFeature {
	if m == nil {
		return nil
	}
	var out []KeeperFeature
	if m.Compat != nil {
		if f, ok := dslFeatureUse(FeatureServiceCompat, "$.compat"); ok {
			out = append(out, f)
		}
	}
	return out
}

// KeeperFeaturesOfScenario collects the keeper-side features one scenario uses:
// its own manifest grammar plus everything its task list needs.
//
// Added because the scenario manifest contributed NO floor anywhere (NIM-354):
// both callers — soul-lint's scenario path and keeper's render path — walked
// only `scn.Tasks`, so a scenario-level key was invisible to the cross-check.
// `id_template` is the first such key; the collector exists so the next one
// has somewhere to go. The reported path is the spelling the file wrote, so a
// scenario inside the [ADR-0085] compatibility window is cited at a key it has.
func KeeperFeaturesOfScenario(m *ScenarioManifest) []KeeperFeature {
	if m == nil {
		return nil
	}
	var out []KeeperFeature
	if m.IDTemplate != "" {
		// The address is the key the file wrote. Equality is what says the fold
		// ran: normalizeIDTemplate copies the legacy value across only when the
		// canonical key is absent, so two DIFFERENT values mean the file declared
		// both — already an `id_template_conflict` error, and the canonical key is
		// the one to cite there, exactly as writtenIDTemplateKey decides it.
		where := "$." + idTemplateKey
		if m.LegacyNameTemplate != "" && m.LegacyNameTemplate == m.IDTemplate {
			where = "$." + nameTemplateKey
		}
		if f, ok := dslFeatureUse(FeatureScenarioIDTemplate, where); ok {
			out = append(out, f)
		}
	}
	return append(out, KeeperFeaturesOfTasks(m.Tasks)...)
}

// KeeperFeaturesOfDestiny collects the keeper-side features one destiny uses:
// the manifest grammar plus everything its task list needs. tasks may be nil
// (the manifest linted on its own).
func KeeperFeaturesOfDestiny(m *DestinyManifest, tasks []Task) []KeeperFeature {
	var out []KeeperFeature
	if m != nil {
		if m.Compat != nil {
			if f, ok := dslFeatureUse(FeatureDestinyCompat, "$.compat"); ok {
				out = append(out, f)
			}
		}
		if len(m.Validate) > 0 {
			if f, ok := dslFeatureUse(FeatureDestinyValidate, "$.validate"); ok {
				out = append(out, f)
			}
		}
		out = append(out, destinyInputFeatures(m.Input)...)
	}
	out = append(out, KeeperFeaturesOfTasks(tasks)...)
	return out
}

// destinyInputFeatures — the input-contract half of [KeeperFeaturesOfDestiny].
// Names are visited in sorted order so the attribution of a floor is the same on
// every run (Go map iteration is not).
func destinyInputFeatures(in InputSchemaMap) []KeeperFeature {
	var out []KeeperFeature
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := in[name]
		if s == nil {
			continue
		}
		where := "$.input." + name
		if s.RequiredWhen != "" {
			if f, ok := dslFeatureUse(FeatureDestinyInputRequiredWhen, where); ok {
				out = append(out, f)
			}
		}
		if len(s.Enum) > 0 || s.Pattern != "" || s.Format != "" || s.MinLength != nil || s.MaxLength != nil {
			if f, ok := dslFeatureUse(FeatureDestinyInputConstraints, where); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

// KeeperFeaturesOfTasks collects the keeper-side features a task list uses. Today
// that is the core-module metadata of every module task (module, state and the
// params actually passed); blocks are walked recursively.
//
// Only the `core` namespace contributes. A plugin module ships on its own version
// line and its manifest states nothing about keeper releases — the same reason
// the soul axis leaves plugin modules out of its capability set (ADR-0076(i),
// NIM-161). Core modules do contribute: their manifests are EMBEDDED in the
// binary, so an older keeper carries an older catalog and rejects an unknown
// state (`module_state_unknown`) or an unknown parameter (`unknown_param`) of a
// module it does know — the definition genuinely cannot render there.
func KeeperFeaturesOfTasks(tasks []Task) []KeeperFeature {
	return keeperFeaturesOfTasks(tasks, coremanifest.Default())
}

// coreModuleLookup — the embedded core registry narrowed to what the floor walk
// needs. An interface, not the concrete registry, so the walk is testable
// against a stamped module: no core manifest declares `introduced_in` today
// (the catalog has not changed since the baseline release).
type coreModuleLookup interface {
	Lookup(module string) (plugin.ModuleDef, bool)
	State(module, state string) (plugin.StateDef, bool)
}

func keeperFeaturesOfTasks(tasks []Task, reg coreModuleLookup) []KeeperFeature {
	var out []KeeperFeature
	collectTaskFeatures(tasks, "$.tasks", false, reg, &out)
	return out
}

// insideBlock says whether this list is the body of a `block:`. It exists for one
// feature — a nested `include:` — because the top-level form is baseline grammar
// and only the nested one arrived this cycle. Carried as a parameter rather than
// derived from the path prefix: a string match on ".block" would be a second,
// silent definition of the same fact.
func collectTaskFeatures(tasks []Task, prefix string, insideBlock bool, reg coreModuleLookup, out *[]KeeperFeature) {
	for i := range tasks {
		t := &tasks[i]
		where := fmt.Sprintf("%s[%d]", prefix, i)
		if insideBlock && t.Include != nil {
			if f, ok := dslFeatureUse(FeatureTaskBlockInclude, where+".include"); ok {
				*out = append(*out, f)
			}
		}
		if t.Async {
			if f, ok := dslFeatureUse(FeatureTaskAsync, where+".async"); ok {
				*out = append(*out, f)
			}
		}
		if _, _, hasRequire := t.RequireSpec(); hasRequire {
			if f, ok := dslFeatureUse(FeatureTaskRequire, where+".require"); ok {
				*out = append(*out, f)
			}
		}
		if t.Module != nil {
			*out = append(*out, coreModuleFeatures(t.Module.Module, t.Module.Params, where, reg)...)
		}
		if t.Block != nil {
			collectTaskFeatures(t.Block.Block, where+".block", true, reg, out)
		}
	}
}

// coreModuleFeatures — the `introduced_in` metadata of one module task, from the
// embedded core registry: the module, the state, and each parameter the task
// actually passes. A parameter the task does not pass introduces nothing.
func coreModuleFeatures(addr string, params map[string]any, where string, reg coreModuleLookup) []KeeperFeature {
	ns, mod, state, ok := splitModuleAddress(addr)
	if !ok || ns != "core" || reg == nil {
		return nil
	}
	name := ns + "." + mod
	m, ok := reg.Lookup(name)
	if !ok {
		return nil
	}
	var out []KeeperFeature
	if m.IntroducedIn != "" {
		out = append(out, KeeperFeature{ID: name, IntroducedIn: m.IntroducedIn, Where: where})
	}
	def, ok := reg.State(name, state)
	if !ok {
		return out
	}
	if def.IntroducedIn != "" {
		out = append(out, KeeperFeature{ID: addr, IntroducedIn: def.IntroducedIn, Where: where})
	}
	names := make([]string, 0, len(params))
	for p := range params {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		schema, known := def.Input[p]
		if !known || schema.IntroducedIn == "" {
			continue
		}
		out = append(out, KeeperFeature{
			ID:           addr + ".params." + p,
			IntroducedIn: schema.IntroducedIn,
			Where:        where + ".params." + p,
		})
	}
	return out
}

// InferKeeperFloor returns the used feature with the highest `introduced_in` —
// the lowest keeper release able to render this definition. nil means nothing
// above the baseline is used (an empty set, or only features not yet shipped in
// any release, see [Unreleased]).
//
// Ties and unparseable versions: the first feature by ID wins, so the attribution
// of a floor does not depend on traversal order; a version that is not plain
// MAJOR.MINOR.PATCH is skipped rather than guessed at (the manifest validator
// reports the malformed value where it is written).
func InferKeeperFloor(used []KeeperFeature) *KeeperFeature {
	var best *KeeperFeature
	var bestV *semver.Version
	for i := range used {
		f := used[i]
		if f.IntroducedIn == Unreleased {
			continue
		}
		v, err := semver.StrictNewVersion(f.IntroducedIn)
		if err != nil {
			continue
		}
		switch {
		case bestV == nil, v.GreaterThan(bestV):
		case v.Equal(bestV) && f.ID < best.ID:
		default:
			continue
		}
		picked := f
		best, bestV = &picked, v
	}
	return best
}

// CompatFloorDiagnostic reports `compat_floor_too_low` when the declared window
// promises to run on keepers that are below the floor the body actually needs
// (ADR-0076(k)) — the declaration is too permissive and would break on a keeper
// INSIDE the window the author wrote.
//
// Silent in three cases: nothing above the baseline is used; no window is
// declared at all (a missing block is unbounded by ADR-0076(b) — an author who
// declared nothing has claimed nothing, and demanding a block here would
// retro-require one on every existing manifest); and a declared min at or above
// the floor, which includes an author who deliberately pinned higher than the
// code requires.
//
// The reverse mistake is louder than the name suggests: when the declared max is
// at or below the floor, no version in the window can render the definition at
// all, so the hint says to move both bounds rather than just the min.
func CompatFloorDiagnostic(window *VersionWindow, floor *KeeperFeature) *diag.Diagnostic {
	if floor == nil || !window.Declared() {
		return nil
	}
	need, err := semver.StrictNewVersion(floor.IntroducedIn)
	if err != nil {
		return nil
	}
	yamlPath := "$.compat.keeper.min"
	if window.Min != "" {
		min, err := semver.StrictNewVersion(window.Min)
		if err != nil || !min.LessThan(need) {
			return nil
		}
	} else {
		// A one-sided `{max: …}` window is unbounded below, so it promises every
		// keeper that ever existed - including the ones without the feature.
		yamlPath = "$.compat.keeper"
	}

	at := ""
	if floor.Where != "" {
		at = fmt.Sprintf(" at %s", floor.Where)
	}
	hint := fmt.Sprintf("set compat.keeper.min to %s (or higher), or stop using %s", floor.IntroducedIn, floor.ID)
	if window.Max != "" {
		if max, err := semver.StrictNewVersion(window.Max); err == nil && !need.LessThan(max) {
			hint = fmt.Sprintf(
				"no keeper in %s can render this - raise compat.keeper.min to %s and compat.keeper.max above it, or stop using %s",
				window.String(), floor.IntroducedIn, floor.ID)
		}
	}
	return &diag.Diagnostic{
		Level: diag.LevelError,
		Phase: diag.PhaseSemanticValidate,
		Code:  "compat_floor_too_low",
		Message: fmt.Sprintf(
			"declared compat.keeper %s admits keepers below %s, the version that introduced %s%s",
			window.String(), floor.IntroducedIn, floor.ID, at),
		Hint:     hint,
		YAMLPath: yamlPath,
	}
}
