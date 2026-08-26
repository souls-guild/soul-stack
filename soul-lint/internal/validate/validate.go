// Package validate implements the `soul-lint validate-config` subcommand.
//
// Auto-detects kind (keeper vs soul) from the top-level `kid:` / `sid:` key,
// delegates to shared/config, and formats output (human / JSON).
package validate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Exit codes are a stable CLI contract (see delegation.md).
const (
	ExitOK        = 0
	ExitHasErrors = 1
	ExitIOFatal   = 2
)

// Kind is the document type being validated. Passed in from the CLI (each
// kind has its own subcommand); Run selects the matching Load* call.
type Kind int

const (
	// KindConfig is keeper.yml / soul.yml (auto-detected between the two via
	// the top-level `kid:` / `sid:` key).
	KindConfig Kind = iota
	// KindDestiny is destiny.yml (the root destiny manifest).
	KindDestiny
	// KindService is service.yml (the root service manifest).
	KindService
	// KindScenario is scenario/<name>/main.yml.
	KindScenario
	// KindManifest is a plugin's schema document — the published
	// `dist/schema.json`, or the copy stamped into an artifact. Parsed and
	// validated by `shared/plugin`.
	KindManifest
)

// Options holds the parameters for a single validate-* subcommand run.
type Options struct {
	Path string
	JSON bool
	Kind Kind

	// Modules holds the `--modules <alias>=<path>` bindings (NIM-228, reshaped by
	// NIM-377). Empty means no resolver: plugin `params:` cannot be checked, and
	// each such module is reported as `plugin_params_unchecked` rather than
	// passing in silence.
	Modules []string
}

// moduleSchemas builds the resolver for this run, or nil when the caller supplied no
// `--modules`. Any binding that does not resolve is a fatal error rather than a
// downgrade to "unchecked": the author asked for these checks by naming the alias.
func (o Options) moduleSchemas(errOut io.Writer) (config.ModuleManifestResolver, bool) {
	if len(o.Modules) == 0 {
		return nil, true
	}
	r, err := LoadModuleSchemas(o.Modules)
	if err != nil {
		fmt.Fprintf(errOut, "soul-lint: %v\n", err)
		return nil, false
	}
	return r, true
}

// Run performs a single validation. It prints diagnostics to `out` and
// returns an exit code per the contract (0/1/2).
func Run(opts Options, out io.Writer, errOut io.Writer) int {
	src, ioErr := os.ReadFile(opts.Path)
	if ioErr != nil {
		fmt.Fprintf(errOut, "soul-lint: %s: %v\n", opts.Path, ioErr)
		return ExitIOFatal
	}

	modules, ok := opts.moduleSchemas(errOut)
	if !ok {
		return ExitIOFatal
	}
	cfgOpts := config.ValidateOptions{ModuleManifests: modules}

	var diags []diag.Diagnostic
	switch opts.Kind {
	case KindConfig:
		kind := detectKind(stripBOM(src))
		switch kind {
		case kindKeeper:
			_, _, diags, _ = config.LoadKeeperFromBytes(opts.Path, src, cfgOpts)
		case kindSoul:
			_, _, diags, _ = config.LoadSoulFromBytes(opts.Path, src, cfgOpts)
		case kindIndeterminate:
			diags = []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseParse,
				File:    opts.Path,
				Code:    "config_kind_indeterminate",
				Message: "cannot auto-detect config kind: expected top-level `kid:` (keeper) or `sid:` (soul); neither or both found",
				Hint:    "ensure the file is either keeper.yml (kid:) or soul.yml (sid:)",
			}}
		}
	case KindDestiny:
		var dm *config.DestinyManifest
		dm, _, diags, _ = config.LoadDestinyManifestFromBytes(opts.Path, src, cfgOpts)
		// Cross-check of the declared window against the floor this destiny
		// actually needs (ADR-0076(k)): the manifest grammar plus its task file.
		diags = append(diags, destinyCompatFloorDiags(opts.Path, dm)...)
		// Cross-file check for destiny-local collisions: sibling `vars.yml`
		// (file-level vars) vs. task-level `vars:` in `tasks/main.yml`. Variant A
		// (vars.md) is deterministic, but name collisions are a common source of
		// confusion, hence the warn. Skipped if either neighbor is missing
		// (vars.yml is optional, tasks/main.yml may live elsewhere — the
		// manifest linter doesn't fail on that).
		diags = append(diags, destinyVarsCollisionDiags(opts.Path)...)
	case KindService:
		var svc *config.ServiceManifest
		svc, _, diags, _ = config.LoadServiceManifestFromBytes(opts.Path, src, cfgOpts)
		diags = append(diags, serviceCompatFloorDiags(opts.Path, svc)...)
		// vars/ lives next to service.yml and is not reachable from the manifest,
		// so it needs its own walk: a malformed _stack.yaml, a layer file parked
		// in a subdirectory the resolver never enters, or the retired directory
		// still in place (ADR-0082).
		diags = append(diags, serviceVarsDiags(opts.Path)...)
	case KindScenario:
		var scn *config.ScenarioManifest
		var scnDoc *config.Document
		scn, scnDoc, diags, _ = config.LoadScenarioManifestFromBytes(opts.Path, src, cfgOpts)
		// covenant resolution BEFORE semantic/cross-ref: merges covenant.yml (via
		// scn.Extends; the linted repo root is `<service>/scenario/<name>/main.yml`
		// → `<service>/`) and validates the form post-merge. MUST run before
		// typeRef/stage: without it, linting a covenant scenario would raise
		// FALSE form_field_unknown (form is gated before merge in the semantic
		// phase) and would skip $type on covenant fields. No-op for non-extends
		// (no FS access) — bit-for-bit identical to before this feature.
		diags = append(diags, config.ResolveScenarioCovenant(scn, scnDoc, scenarioServiceRoot(opts.Path))...)
		// Stage validation (ADR-056 §S5): offline Passage stratification using
		// the same config.Stratify function the runtime calls before dispatch.
		// Catches register cycles and serial+staged BEFORE apply (the config
		// validator already raises unknown_register at parse time). Runs even
		// if parse diagnostics exist (stageDiagnostics decides for itself, via
		// nil scn, whether the graph is reliable).
		diags = append(diags, stageDiagnostics(opts.Path, scn)...)
		// Resolves $type references against the service type catalog
		// (`../../types.yml`): catches input_type_unknown/cycle/duplicate BEFORE
		// keeper. Structural $type-ref-conflict is already raised by the config
		// validator at scenario parse time. Also resolves covenant fields
		// (already merged into scn.Input above).
		diags = append(diags, typeRefDiagnostics(opts.Path, scn)...)
		// A scenario or task local taking over one of the service's own var
		// names. The one cost of merging the two namespaces (ADR-0082): the
		// shadow is deterministic and sometimes deliberate, so it warns.
		diags = append(diags, serviceVarShadowDiags(opts.Path, scn)...)
		// `on: ["${ incarnation.name }"]` is fail-closed (ADR-008 amendment/NIM-124:
		// incarnation.name is not a Coven). Offline parity with the keeper render
		// resolver (resolveCovenList) — the literal is visible without CEL eval.
		if scn != nil {
			diags = append(diags, onIncarnationNameDiagnostics(opts.Path, scn.Tasks)...)
		}
		// A scenario renders under the window its service declares (it has no
		// compat: block of its own) — so its features are weighed against
		// `../../service.yml` (ADR-0076(k)).
		diags = append(diags, scenarioCompatFloorDiags(opts.Path, scn)...)
		// A Vault path under the service's own derived prefix, in any spelling
		// ([ADR-0083] §7) — read over the include-expanded list, so a path in a
		// sibling file is caught offline and not only at render.
		diags = append(diags, scenarioVaultNamespaceDiags(opts.Path, scn)...)
		// Two elements of one literal collection addressing the same declared
		// secret ([ADR-0083] §1) — read over the include-expanded list, against
		// the state_schema of `../../service.yml`.
		diags = append(diags, scenarioSecretKeyDiags(opts.Path, scn)...)
	case KindManifest:
		diags = schemaDocumentDiags(opts.Path, src)
	default:
		fmt.Fprintf(errOut, "soul-lint: unknown kind %d\n", opts.Kind)
		return ExitIOFatal
	}

	printDiagnostics(opts, diags, out)
	if diag.HasErrors(diags) {
		return ExitHasErrors
	}
	return ExitOK
}

// schemaDocumentDiags validates one plugin schema document — the published
// `dist/schema.json`, or the copy stamped into an artifact.
//
// Which of the two it got is decided by content: a canonical document is a JSON object
// and so begins with `{`, and anything else is read as an artifact through its trailer,
// where a missing or malformed trailer is itself the finding. Deciding by filename would
// read an artifact that happens to be called `schema.json` as text and report a parse
// error rather than "this was never stamped".
func schemaDocumentDiags(path string, src []byte) []diag.Diagnostic {
	if trimmed := bytes.TrimLeft(src, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		_, diags := sharedplugin.ParseDocument(path, src)
		return diags
	}
	_, diags, _ := sharedplugin.ReadArtifact(path)
	return diags
}

// scenarioServiceRoot derives the linted service repo root from a
// scenario's main.yml path. Layout `<service>/scenario/<name>/main.yml` →
// `<service>` (three Dir calls up; same base typeRefDiagnostics builds for
// types.yml). The root holds the covenant.yml family (sibling of
// service.yml/types.yml) that extends resolves against.
func scenarioServiceRoot(scenarioPath string) string {
	// Absolute FIRST. The three Dir calls are lexical, so `main.yml` linted from
	// inside the scenario directory decomposes to "." and every consumer then
	// resolves against the wrong root — silently, because a wrong-but-existing
	// root reads as "covenant not found" rather than as an error. Absolutising
	// downstream does not repair it: readCovenantFile and readWithin both call
	// filepath.Abs and were still handed "." from here. The verdict must be a
	// function of the file on disk, never of how the operator typed its path.
	abs, err := filepath.Abs(scenarioPath)
	if err != nil {
		abs = scenarioPath
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(abs)))
}

// destinyVarsCollisionDiags raises a warn for every name declared BOTH in
// the sibling `vars.yml` (file-level destiny locals) AND in the task-level
// `vars:` of at least one task in `tasks/main.yml`. Variant A is
// deterministic (task overrides file, vars.md), but the collision is a
// common source of confusion.
//
// manifestPath is the path to destiny.yml; neighbors are read from its
// directory. Any I/O or parse error on a neighbor skips the check (vars.yml
// is optional; errors in the tasks themselves are caught by
// validate-scenario/runtime — this only checks for the collision). Doesn't
// fail when neighbors are absent.
func destinyVarsCollisionDiags(manifestPath string) []diag.Diagnostic {
	dir := filepath.Dir(manifestPath)

	fileVars, err := config.LoadDestinyVars(filepath.Join(dir, "vars.yml"))
	if err != nil || len(fileVars) == 0 {
		return nil
	}

	tasksPath := filepath.Join(dir, "tasks", "main.yml")
	tasksData, rerr := os.ReadFile(tasksPath)
	if rerr != nil {
		return nil
	}
	tasks, _, terr := config.LoadDestinyTasksFromBytes(tasksPath, tasksData, config.ValidateOptions{})
	if terr != nil {
		return nil
	}

	var out []diag.Diagnostic
	for _, name := range config.DestinyVarsCollisions(fileVars, tasks) {
		out = append(out, diag.Diagnostic{
			Level:    diag.LevelWarning,
			Phase:    diag.PhaseSemanticValidate,
			File:     filepath.Join(dir, "vars.yml"),
			Code:     "vars_collision",
			Message:  fmt.Sprintf("vars.%s declared in both vars.yml and a task-level vars:; task-level wins (Variant A)", name),
			Hint:     "rename one, or rely on task-level override intentionally (docs/destiny/vars.md)",
			YAMLPath: "$." + name,
		})
	}
	return out
}

// printDiagnostics formats and writes diagnostics to `w` in the selected
// mode. JSON mode: one JSON line per diagnostic (JSON-Lines); 0 diagnostics
// → empty stdout. Human mode: gcc-style, plus a single `OK: <path>` line
// when there are 0 errors.
func printDiagnostics(opts Options, diags []diag.Diagnostic, w io.Writer) {
	if opts.JSON {
		bw := bufio.NewWriter(w)
		defer bw.Flush()
		enc := json.NewEncoder(bw)
		for _, d := range diags {
			_ = enc.Encode(d)
		}
		return
	}
	for _, d := range diags {
		writeHumanDiag(w, d)
	}
	if !diag.HasErrors(diags) {
		fmt.Fprintf(w, "OK: %s\n", opts.Path)
	}
}

func writeHumanDiag(w io.Writer, d diag.Diagnostic) {
	file := d.File
	if file == "" {
		file = "<input>"
	}
	// gcc-style `file:line:col: level: [code] message`. When line/col are
	// unknown (cross-field invariant), those sections are simply omitted,
	// without an empty ":" — avoids a double space in `path: error:`.
	prefix := file + ":"
	if d.Line > 0 {
		if d.Column > 0 {
			prefix += fmt.Sprintf("%d:%d:", d.Line, d.Column)
		} else {
			prefix += fmt.Sprintf("%d:", d.Line)
		}
	}
	fmt.Fprintf(w, "%s %s: [%s] %s\n", prefix, d.Level, d.Code, d.Message)
	if d.YAMLPath != "" {
		fmt.Fprintf(w, "  yaml_path: %s\n", d.YAMLPath)
	}
	if d.Hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", d.Hint)
	}
}

// detectKind auto-detects kind from the set of top-level keys.
//
// Priority 1: `kid:` → keeper, `sid:` → soul (either one is explicitly
// present). Priority 2: if neither `kid:` nor `sid:` is present, vote by
// unique top-level keys (`postgres`/`vault`/`plugins`/… → keeper;
// `keeper`/`paths`/`soulprint`/`cleanup`/`metrics` → soul). Needed because
// `sid:` in soul.yml is optional (defaults to being computed from the host
// FQDN).
//
// Returns `kindIndeterminate` if both explicit keys are present, or if no
// signal is found / the vote is ambiguous.
func detectKind(src []byte) configKind {
	keys := readTopLevelKeys(src)
	hasKID, hasSID := keys["kid"], keys["sid"]
	switch {
	case hasKID && hasSID:
		return kindIndeterminate
	case hasKID:
		return kindKeeper
	case hasSID:
		return kindSoul
	}
	keeperVotes := 0
	soulVotes := 0
	for k := range keys {
		switch k {
		// `services`/`default_destiny_source`/`default_module_source` are
		// excluded from the vote: the Service registry and these scalars moved
		// to Postgres (ADR-029 hard-cut), so they're no longer in keeper.yml and
		// can't be used to detect keeper. `rbac` was excluded earlier (ADR-028).
		// The keeper signature is still strong (postgres/vault/auth/plugins/reaper).
		case "postgres", "vault", "auth", "plugins", "reaper":
			keeperVotes++
		case "keeper", "paths", "soulprint", "cleanup", "metrics":
			soulVotes++
		}
	}
	switch {
	case keeperVotes > 0 && soulVotes == 0:
		return kindKeeper
	case soulVotes > 0 && keeperVotes == 0:
		return kindSoul
	default:
		return kindIndeterminate
	}
}

func readTopLevelKeys(src []byte) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexByte(trimmed, ':')
		if idx <= 0 {
			continue
		}
		key := trimmed[:idx]
		out[key] = true
	}
	return out
}

type configKind int

const (
	kindIndeterminate configKind = iota
	kindKeeper
	kindSoul
)

// stripBOM is a local helper kept in sync with shared/config (the goccy
// parser does the same strip via LoadKeeperFromBytes/LoadSoulFromBytes).
// Without it, detectKind wouldn't find `kid:`/`sid:` under a leading BOM.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
