package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	yaml "gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/shared/config"
)

// scenarioDir — canonical layout of scenario in Service repo
// (docs/scenario): directory `scenario/<name>/main.yml` per service; scenario name
// = subdirectory name. Paired constant with destinyTasksDir/migrationsDir
// to avoid magic strings throughout the package.
const scenarioDir = "scenario"

// upgradeDir — second channel for auto-discovery of scenarios (ADR-0068 §3): directory
// `upgrade/<slug>/main.yml` alongside scenarioDir. Keeps version-to-version
// upgrade scenarios separate from day-2 scenario/ (not shown in regular listings).
const upgradeDir = "upgrade"

// scenarioMainFile is the root YAML scenario file inside `<dir>/<name>/`.
const scenarioMainFile = "main.yml"

// Scenario is a listing entry for a scenario from a materialized Service repository
// snapshot (`scenario/<name>/main.yml`). It is a lightweight projection of top-level
// scenario.yml fields for the UI dropdown "Choose scenario" (handler does not need
// tasks or orchestration delta — only metadata).
//
// JSON field names match the UI API ([ServiceScenariosListReply]); types are
// minimal: InputSchema is stored as `map[string]any` (repeats raw YAML) so UI can
// render a form without server-side validation (which happens in render-pipeline).
// Description and Tags are optional — a scenario may have no top-level description
// or tags; then those fields remain "" or nil.
type Scenario struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Create is a discriminator: "scenario is suitable as a bootstrap (initial
	// incarnation creation)". Reads top-level `create: true` in main.yml (supports
	// multiple create scenarios). UI filters the "choose startup scenario" list
	// in the Create form by this flag; default choice is the scenario named
	// `create` (back-compat). omitempty: false (non-create scenario) is omitted
	// from reply exactly as before this feature. `destroy` is NOT marked with this
	// flag (teardown is a special DELETE flow).
	Create bool `json:"create,omitempty"`
	// FromVersions is a self-describing list of source versions for an upgrade
	// scenario (top-level `from:` in `upgrade/<slug>/main.yml`, ADR-0068 §3):
	// which pins this scenario can upgrade from. Populated only via [ListUpgrades];
	// for regular scenario/ entries it is nil → omitempty omits the field exactly
	// as before this feature.
	FromVersions []string `json:"from_versions,omitempty"`
	// Runnable marks "can be run by operator from Run form" (ADR-042 "dumb frontend"):
	// create=true, destroy=false (deletion is special DELETE flow), operational=true.
	// Marked by listing-handler according to scenario-package convention
	// (scenario.IsRunnableScenario); ListScenarios itself does not populate it.
	Runnable bool `json:"runnable"`
	// ComposesID marks "this create scenario composes the incarnation id
	// itself" — the manifest carries `id.template` (ADR-0079), so the server
	// builds the id from `input:` and REFUSES a request that also sends one
	// (scenario.ErrIDNotComposable → 422 id_not_composable).
	//
	// The descriptor had no way to say this, so a client asked for an id it must
	// not send: the web form required one and the backend rejected it, which left
	// no way to create a templated incarnation through the UI at all (NIM-340).
	//
	// Only the FLAG travels, never the template. What an operator is shown is the
	// composed name — the result — and not the formula that produced it; publishing
	// the expression would make an internal authoring detail part of the API.
	// omitempty: a scenario that composes nothing serializes exactly as it did
	// before this field existed.
	ComposesID  bool           `json:"composes_id,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	// Validate is the scenario's `validate:` block — the declarative requirements
	// over the request, in declaration order (NIM-833). It travels BESIDE
	// input_schema because that is what it is: the half of the input contract a
	// single field's schema cannot express ("port is required when tls is off",
	// "this id must fit the cloud's name grammar"). A form that shows only
	// input_schema shows half the contract, and the operator meets the other half
	// as a 422 after submitting.
	//
	// Only the TEXT travels — `that` and `message`. Evaluation stays keeper-side
	// (config.EvalValidateRules) and is not duplicated in a client: two evaluators
	// of one rule diverge, and the question is only when. A client that wants a
	// verdict rather than the requirements asks the server.
	//
	// omitempty: a scenario with no rules serializes exactly as it did before this
	// field existed.
	Validate []ScenarioValidateRule `json:"validate,omitempty"`
	Tags     []string               `json:"tags,omitempty"`
	// Form is an optional presentation layer (top-level `form:` in scenario
	// manifest): sections with field labels for UI Run modal. omitempty: no form:
	// in YAML → no field in reply (exactly as before this feature); UI renders
	// flat input (forward-compat). Grouping and input validation live in
	// input_schema; form is presentation only. Server-side does NOT validate form
	// (soul-lint/render-validator does); listing returns it as-is for UI.
	Form *ScenarioForm `json:"form,omitempty"`
}

// ScenarioValidateRule is one rule of [Scenario.Validate]: the CEL predicate the
// keeper will evaluate and the reason an operator is shown when it comes out
// false. JSON field names match the YAML keys, so a form renders the rule the
// author wrote and not a paraphrase of it.
type ScenarioValidateRule struct {
	That    string `json:"that"`
	Message string `json:"message"`
}

// ScenarioForm, ScenarioFormSection, and ScenarioFormField are JSON projections of
// top-level `form:` for UI listing (symmetric to [Scenario.InputSchema] as raw input).
// This is a presentation layer: sections group input fields under headings. JSON
// field names match YAML manifest keys; types are minimal (UI renders, does not
// validate). soul-lint (shared/config.validateFormLayout) validates invariants
// (field ∈ input, key uniqueness) — listing returns form as-is.
type ScenarioForm struct {
	Sections []ScenarioFormSection `json:"sections,omitempty"`
}

// ScenarioFormSection is one visual grouping of form fields.
//
// ShowWhen is an optional CEL predicate over input.* for conditional section
// visibility. CAVEAT: this is PRESENTATION, NOT a validation gate — listing
// returns the string as-is, eval happens UI client-side (variant A); backend
// does NOT compute the predicate. Hiding a section does not override validation
// of its fields by the backend. omitempty: no key → always visible.
type ScenarioFormSection struct {
	Key         string              `json:"key"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Collapsed   bool                `json:"collapsed,omitempty"`
	ShowWhen    string              `json:"show_when,omitempty"`
	Fields      []ScenarioFormField `json:"fields,omitempty"`
}

// ScenarioFormField is a reference to an input field with optional label and
// UX hints.
//
// ShowWhen is conditional field visibility (semantics/caveat same as section:
// presentation, client-side eval, not a gate). Placeholder and Hint are pure
// widget presentation (text in empty field / hint under field), NOT duplicating
// the input contract. All three omitempty: absence of any is exactly as before
// this feature.
type ScenarioFormField struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	ShowWhen    string `json:"show_when,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

// Values of [Scenario.Kind] are a closed enum discriminator for scenario type in UI
// ("dumb frontend" reads the catalog, does not hardcode names create/destroy).
// Markup is applied by listing-handler according to scenario.LifecycleScenarioNames
// convention (artifact does not import scenario-package — direction is reverse);
// ListScenarios itself leaves the field empty.
const (
	// ScenarioKindLifecycle marks a scenario name ∈ scenario.LifecycleScenarioNames
	// (create / destroy): keeper treats it as a specialized scenario-kind
	// for lifecycle phases.
	ScenarioKindLifecycle = "lifecycle"
	// ScenarioKindOperational is an ordinary scenario (free operation over state),
	// run by ordinary run. `converge` is operational (amend ADR-031, 2026-06-10):
	// extracted from lifecycle set. Since NIM-446 removed the drift circuit it
	// carries only the Apply-reconcile role — it is a scenario like any other.
	ScenarioKindOperational = "operational"
)

// scenarioYAML is a narrow top-level subset of scenario/<name>/main.yml parsed
// for UI listing. Fields are symmetric with the JSON form [Scenario];
// both `input` and `input_schema` are accepted (docs/scenario/concept.md):
// historically the field was `input_schema`, in newer examples it is `input`
// (see redis-monitored). Priority is input_schema → input.
//
// Non-standard top-level fields are ignored (yaml.Unmarshal into struct captures
// only listed fields), following the stop-rule spec.
type scenarioYAML struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Input       map[string]any `yaml:"input"`
	InputSchema map[string]any `yaml:"input_schema"`
	Tags        []string       `yaml:"tags"`
	// Extends is a covenant fragment name (`extends:`), whose section contract
	// the scenario inherits (ScenarioManifest.Extends). For UI listing, it matters
	// ONLY for input: covenant.yml.input is merged into InputSchema (mergeCovenantInputRaw)
	// — otherwise a scenario with zero input delta (all schema in covenant, like
	// create_from_souls) would return an empty form. Empty string / missing key =
	// no inheritance (forward-compat). Resolution is at raw level (see ListScenarios):
	// listing keeps InputSchema as a raw map for UI, while typed covenant-merge
	// (config.ResolveScenarioCovenant) gives another form (InputSchemaMap without
	// json tags) — which would break the frontend.
	Extends string `yaml:"extends"`
	// Create is the top-level `create:` flag for a startup scenario. `*bool` to
	// distinguish "not set" (nil → not a startup scenario) from explicit
	// `create: false`; listing projects to Scenario.Create via !=nil && *Create
	// (see loadScenario). soul-lint (config-validator) does strict type validation;
	// here is best-effort projection for UI — invalid type remains nil → false.
	Create *bool `yaml:"create"`
	// ID is the top-level `id:` block. Read only to decide [Scenario.ComposesID];
	// neither the template nor the bound leaves this struct — the form learns the
	// ceiling from the resolve endpoint, and a copy here would be a second number to
	// keep in step.
	ID *scenarioIDYAML `yaml:"id"`
	// LegacyIDTemplate is the scalar spelling `id_template:`, read for the same
	// compatibility window config.ScenarioManifest keeps open. This is a best-effort
	// UI projection, not the validator: a scenario declaring BOTH is a config error
	// reported there, and here the canonical key simply wins.
	LegacyIDTemplate string `yaml:"id_template"`
	// FromVersions is the top-level `from:` of upgrade manifest (ADR-0068).
	// Projected to Scenario.FromVersions only on upgrade path ([ListUpgrades]);
	// for scenario/ there is no key → nil. soul-lint (config-validator) does
	// strict type validation.
	FromVersions []string `yaml:"from"`
	// Form is an optional presentation layer (top-level `form:`). Non-standard
	// sub-keys are ignored (yaml.Unmarshal into struct captures only listed ones);
	// soul-lint validates form strictly, not listing.
	Form *scenarioFormYAML `yaml:"form"`
}

// scenarioIDYAML is the `id:` block as the listing reads it: presence of a template is
// all [Scenario.ComposesID] answers.
type scenarioIDYAML struct {
	Template string `yaml:"template"`
}

// composesID reports whether the manifest declares an id template under EITHER
// spelling — `id.template:`, or the scalar `id_template:` inside the NIM-899
// compatibility window. Presence, not content.
func (r scenarioYAML) composesID() bool {
	if r.ID != nil && strings.TrimSpace(r.ID.Template) != "" {
		return true
	}
	return strings.TrimSpace(r.LegacyIDTemplate) != ""
}

// scenarioFormYAML, scenarioFormSectionYAML, and scenarioFormFieldYAML are YAML
// forms of `form:` for listing parser. Structurally identical to JSON projection
// [ScenarioForm] (same field names), but a separate type with yaml tags:
// listing does not import shared/config (artifact-package isolation, import
// direction is reverse).
type scenarioFormYAML struct {
	Sections []scenarioFormSectionYAML `yaml:"sections"`
}

type scenarioFormSectionYAML struct {
	Key         string                  `yaml:"key"`
	Title       string                  `yaml:"title"`
	Description string                  `yaml:"description"`
	Collapsed   bool                    `yaml:"collapsed"`
	ShowWhen    string                  `yaml:"show_when"`
	Fields      []scenarioFormFieldYAML `yaml:"fields"`
}

type scenarioFormFieldYAML struct {
	Name        string `yaml:"name"`
	Label       string `yaml:"label"`
	ShowWhen    string `yaml:"show_when"`
	Placeholder string `yaml:"placeholder"`
	Hint        string `yaml:"hint"`
}

// scenarioValidateRuleYAML is the YAML form of one `validate:` rule. Separate from
// the JSON projection [ScenarioValidateRule] for the same reason the form types
// are: listing does not import shared/config (the import direction is the reverse).
type scenarioValidateRuleYAML struct {
	That    string `yaml:"that"`
	Message string `yaml:"message"`
}

// validateRulesYAML decodes ONLY the `validate:` section, in a pass of its own.
//
// A second Unmarshal over the same bytes rather than a field on [scenarioYAML],
// because the two sections must not share a failure. `that:` is typed as a string,
// so a rule written `that: 8080` fails the decode — and inside the main struct that
// failure takes the WHOLE scenario down: no input_schema, no form, the entry gone
// from the dropdown, over a section that is presentation only. The blast radius has
// to be the section, which is what the struct's "best-effort projection" comment
// claims of every other field here.
//
// A malformed block is soul-lint's business (shared/config.validateValidateBlock);
// this returns nothing and lets the rest of the listing through.
func validateRulesYAML(data []byte) []scenarioValidateRuleYAML {
	var doc struct {
		Validate []scenarioValidateRuleYAML `yaml:"validate"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc.Validate
}

// ListScenarios scans `scenario/*/main.yml` in a materialized Service repository
// snapshot (serviceRoot is absolute path to snapshot, typically
// [ServiceArtifact.LocalDir]) and returns a list of scenario metadata sorted by
// name for the day-2 UI dropdown.
//
// This is ONLY the scenario/ channel: upgrade/<slug>/ does NOT appear here
// (ADR-0068 §3 — upgrade scenarios do not clutter day-2 listings; they are
// returned by [ListUpgrades]).
func ListScenarios(serviceRoot string, logger *slog.Logger) ([]Scenario, error) {
	return listFromDir(serviceRoot, scenarioDir, logger)
}

// ListUpgrades mirrors [ListScenarios] for the second auto-discovery channel
// (ADR-0068 §3): scans `upgrade/<slug>/main.yml` in the same snapshot and returns
// a list with [Scenario.FromVersions] populated. Absence of `upgrade/` directory →
// empty list (a service without upgrade scenarios is valid), like ListScenarios with
// missing `scenario/`.
func ListUpgrades(serviceRoot string, logger *slog.Logger) ([]Scenario, error) {
	return listFromDir(serviceRoot, upgradeDir, logger)
}

// listFromDir is a common scan of a scenario directory (`scenario/` or `upgrade/`)
// in a service snapshot: `<dir>/<name>/main.yml` → a list of metadata sorted by
// name. The only difference between [ListScenarios] and [ListUpgrades] is the
// directory `dir`; everything else (partial-success, securejoin, type-catalog
// resolution) is shared.
//
// Partial-success semantics: each scenario is processed in isolation. Unparseable
// YAML / missing main.yml → warning in logger and skip, but NOT an error return
// (UI dropdown should display others even if one is broken in the repo). The
// directory `dir` itself may be missing — a service without scenarios/upgrades is
// valid (return empty list). Logger is optional (nil → slog.Default).
//
// Path safety: securejoin on each child join (protection against `..` /
// symlink escape from snapshot). Symlinks inside snapshot are not followed by
// design (os.ReadDir + securejoin); server-side keeps snapshot immutable.
func listFromDir(serviceRoot, dir string, logger *slog.Logger) ([]Scenario, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dirRoot, err := securejoin.SecureJoin(serviceRoot, dir)
	if err != nil {
		return nil, fmt.Errorf("artifact: unsafe directory path %s: %w", dir, err)
	}

	entries, err := os.ReadDir(dirRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Scenario{}, nil
		}
		return nil, fmt.Errorf("artifact: reading directory %s: %w", dir, err)
	}

	// Service type catalog (`types.yml`) of reusable named types is shared by all
	// scenarios and read once. $type references in each scenario's InputSchema are
	// resolved backend-side BEFORE projection in reply (see loadScenario): UI gets
	// already-substituted schema + x-type annotation, not raw $type.
	catalog := loadTypeCatalog(serviceRoot, logger)

	out := make([]Scenario, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// A `_`/`.`-prefixed directory holds shared include bodies, not a
		// scenario (config.IsSharedDirName): skipped EXPLICITLY and silently,
		// before loadScenario, so the convention does not depend on main.yml
		// being absent and the "no main.yml" warning keeps meaning a broken
		// scenario rather than firing on every listing.
		if config.IsSharedDirName(name) {
			continue
		}
		sc, ok := loadScenario(serviceRoot, dir, name, logger)
		if !ok {
			continue
		}
		// Resolve, then strip: a declared secret reaches an operator form only
		// through `$type`, so it exists to be stripped only after the substitution
		// ([ADR-0086] §5, NIM-751 — see stripFormSecrets for why this is not inside
		// the resolver, which state_schema shares).
		resolvedSchema := resolveScenarioTypeRefs(sc.InputSchema, catalog)
		sc.InputSchema = stripFormSecrets(resolvedSchema)
		// `form:` is the presentation half of the same reply, and it names input
		// fields by name. A name whose schema just went away would ship to the UI as
		// a labelled field with nothing behind it — "not on the form" has to be true
		// of both halves or it is not true.
		sc.Form = dropStrippedFormFields(sc.Form, resolvedSchema, sc.InputSchema)
		// The rule text is the third half, and it names input fields (and often
		// compares them to a literal) — see dropStrippedValidateRules.
		sc.Validate = dropStrippedValidateRules(sc.Validate, resolvedSchema, sc.InputSchema)
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// loadScenario reads one `<dir>/<name>/main.yml` (dir is scenarioDir or upgradeDir)
// and parses it into [Scenario]. Returns (_, false) on missing main.yml or invalid
// YAML (partial-success: warning is logged, caller skips the entry).
//
// Scenario name is taken from the directory (reliable source: matches what is
// written in `apply.scenario` / `apply.destiny`); top-level `name:` in YAML is
// ignored, even if set — mismatch between directory name and YAML name is a
// service-repo bug, not a listing bug.
func loadScenario(serviceRoot, dir, name string, logger *slog.Logger) (Scenario, bool) {
	relPath := filepath.ToSlash(filepath.Join(dir, name, scenarioMainFile))
	mainPath, err := securejoin.SecureJoin(serviceRoot, relPath)
	if err != nil {
		logger.Warn("artifact: scenario skipped — unsafe path",
			slog.String("scenario", name), slog.Any("error", err))
		return Scenario{}, false
	}

	data, err := os.ReadFile(mainPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: scenario skipped — no main.yml",
				slog.String("scenario", name), slog.String("path", relPath))
		} else {
			logger.Warn("artifact: scenario skipped — error reading main.yml",
				slog.String("scenario", name), slog.Any("error", err))
		}
		return Scenario{}, false
	}

	var raw scenarioYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Warn("artifact: scenario skipped — invalid YAML",
			slog.String("scenario", name), slog.Any("error", err))
		return Scenario{}, false
	}

	schema := raw.InputSchema
	if schema == nil {
		schema = raw.Input
	}
	rules := validateRulesYAML(data)
	// covenant-merge BEFORE returning input_schema/validate: a scenario with
	// `extends:` inherits covenant.yml's input and validate sections (same add-only
	// merge as runtime LoadScenarioManifest Resolved →
	// config.ResolveScenarioCovenant). Without it, a scenario with zero input delta
	// (all schema in covenant — create_from_souls) would return empty form to UI,
	// and a scenario whose requirements all live in the shared contract would
	// publish none of them.
	// Resolution is at raw level (covenant-input remains a raw map for UI), $type
	// references of covenant fields are resolved AFTER — in ListScenarios, together
	// with locals (single pass resolveScenarioTypeRefs).
	if raw.Extends != "" {
		schema, rules = mergeCovenantSectionsRaw(serviceRoot, raw.Extends, schema, rules, logger)
	}
	sc := Scenario{
		Name:   name,
		Path:   relPath,
		Create: raw.Create != nil && *raw.Create,
		// Presence, not content: any non-empty template means the server composes
		// the id. A malformed one is soul-lint's business, not the listing's.
		ComposesID:  raw.composesID(),
		Description: raw.Description,
		InputSchema: schema,
		Validate:    scenarioValidateProjection(rules),
		Tags:        raw.Tags,
		Form:        scenarioFormProjection(raw.Form),
	}
	// Channel isolation is PHYSICAL, not just directory-based (ADR-0068 §3): stray
	// `from:` in scenario/<name>/main.yml must not leak into day-2 reply —
	// FromVersions carries only upgrade/ channel.
	if dir == upgradeDir {
		sc.FromVersions = raw.FromVersions
	}
	return sc, true
}

// mergeCovenantSectionsRaw reads covenant.yml by name from `extends:` and merges
// its `input:` and `validate:` sections into the scenario's own ADD-ONLY: covenant
// is BASE, scenario delta supplements (local input fields are NOT overwritten —
// parallel to config.mergeInputSections, but at raw level for UI; validate rules
// are appended covenant-FIRST, as config.MergeCovenant orders them). This is the
// listing projection of the same covenant resolution runtime does
// (LoadScenarioManifestResolved → config.ResolveScenarioCovenant) — without it, a
// scenario with zero input delta (all schema in covenant) returns an empty form,
// and one whose requirements all live in the shared contract publishes none.
//
// Covenant name is validated by [config.ValidExtendsName] (single source of truth
// for the form), path is securejoin from serviceRoot (traversal clamp over name
// grammar). Partial-success like all listing: invalid name / missing / broken
// covenant.yml → warning + locals as-is (UI gets at least the local delta, not 500;
// full error covenant_extends_* is raised by runtime-load and soul-lint).
//
// Key conflict (field in both covenant and local) at runtime → section_key_conflict;
// here listing does NOT fail: local-schema wins (covenant does not override), form
// still renders. The returned schema may be nil if neither side provided fields.
func mergeCovenantSectionsRaw(
	serviceRoot, extends string,
	local map[string]any,
	localRules []scenarioValidateRuleYAML,
	logger *slog.Logger,
) (map[string]any, []scenarioValidateRuleYAML) {
	if !config.ValidExtendsName(extends) {
		logger.Warn("artifact: covenant sections skipped — invalid extends name",
			slog.String("extends", extends))
		return local, localRules
	}
	covenantFile := extends + ".yml"
	data, err := readSnapshotFile(serviceRoot, covenantFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: covenant sections skipped — error reading covenant.yml",
				slog.String("extends", extends), slog.Any("error", err))
		}
		// Missing covenant.yml with declared extends is a repo error
		// (covenant_extends_target_not_found at runtime); listing does not fail the
		// form, returns local delta as-is.
		return local, localRules
	}

	// `validate:` is decoded in its own pass, so a malformed rule in the covenant
	// cannot take its `input:` down with it — see validateRulesYAML.
	if fragRules := validateRulesYAML(data); len(fragRules) > 0 {
		merged := make([]scenarioValidateRuleYAML, 0, len(fragRules)+len(localRules))
		merged = append(merged, fragRules...)
		localRules = append(merged, localRules...)
	}

	var frag struct {
		Input map[string]any `yaml:"input"`
	}
	if err := yaml.Unmarshal(data, &frag); err != nil {
		logger.Warn("artifact: covenant sections skipped — invalid YAML covenant.yml",
			slog.String("extends", extends), slog.Any("error", err))
		return local, localRules
	}
	if len(frag.Input) == 0 {
		return local, localRules
	}

	if local == nil {
		local = make(map[string]any, len(frag.Input))
	}
	for name, schema := range frag.Input {
		if _, dup := local[name]; dup {
			// Key conflict — local wins (covenant add-only does NOT override).
			// runtime raises section_key_conflict; listing does not fail the form.
			continue
		}
		local[name] = schema
	}
	return local, localRules
}

// scenarioValidateProjection converts the YAML rules to the JSON projection
// [ScenarioValidateRule], preserving declaration order — the order the keeper
// evaluates them in, and therefore the order the first failure comes from.
//
// A rule missing `that` is dropped: an empty predicate names no requirement, and
// publishing a bare `message` would put a requirement on the form that the keeper
// does not enforce. The file itself is rejected by soul-lint
// (missing_required_field), which is where that is reported.
func scenarioValidateProjection(in []scenarioValidateRuleYAML) []ScenarioValidateRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]ScenarioValidateRule, 0, len(in))
	for _, r := range in {
		if r.That == "" {
			continue
		}
		out = append(out, ScenarioValidateRule{That: r.That, Message: r.Message})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scenarioFormProjection converts YAML form `form:` to JSON projection [ScenarioForm]
// for reply. nil input (no `form:` key) → nil (field omitempty is omitted from reply —
// exactly as before this feature). Trivial field rewriting: listing does not validate
// form (soul-lint does), just returns it to UI as-is.
func scenarioFormProjection(in *scenarioFormYAML) *ScenarioForm {
	if in == nil {
		return nil
	}
	out := &ScenarioForm{Sections: make([]ScenarioFormSection, 0, len(in.Sections))}
	for _, s := range in.Sections {
		sec := ScenarioFormSection{
			Key:         s.Key,
			Title:       s.Title,
			Description: s.Description,
			Collapsed:   s.Collapsed,
			ShowWhen:    s.ShowWhen,
			Fields:      make([]ScenarioFormField, 0, len(s.Fields)),
		}
		for _, f := range s.Fields {
			sec.Fields = append(sec.Fields, ScenarioFormField{
				Name:        f.Name,
				Label:       f.Label,
				ShowWhen:    f.ShowWhen,
				Placeholder: f.Placeholder,
				Hint:        f.Hint,
			})
		}
		out.Sections = append(out.Sections, sec)
	}
	return out
}
