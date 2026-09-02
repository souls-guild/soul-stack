package validate

// Whole-service validation: one invocation covers every part of a service
// repository and reports ALL of them, so that one broken part cannot hide the
// rest.
//
// Why this is a mode of the linter and not a loop in a shell script (NIM-753).
// The per-file commands check one document per invocation, so a service
// repository had to orchestrate the sequence itself — and the orchestrator, being
// an ordinary `set -e` script, stopped at the first non-zero exit. In the WB redis
// service that first exit was a one-line manifest error, and behind it sat eight
// accumulated divergences that nobody saw for as long as the manifest stayed red:
// a dead `state_changes:` block (thirteen state fields silently not written), the
// renamed capture params, the changed L0 case form, `required: true` next to a
// `default`. None of them were new. The rule that keeps them visible — a failure
// in one part does not suppress the diagnostics of another — belongs to the tool,
// where it is written once, and not to every service repository, where it is
// re-derived and eventually forgotten.
//
// So nothing in this file may return early on a failed part. A part that cannot
// even be read contributes an `io_error` diagnostic and the walk continues; only a
// caller error (this is not a service tree, a `--modules` binding does not resolve)
// is fatal, because then there is no run to speak of rather than a partly broken
// one.
//
// The per-file commands stay: they are what an editor calls on the buffer being
// typed in, and what an author reaches for to re-check one file.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	// serviceManifestFile is the service root manifest, and the marker that a
	// directory IS a service tree.
	serviceManifestFile = "service.yml"
	// scenarioDir holds one directory per scenario, each with a main.yml entry
	// point; secondary files are reached from it through `include:`.
	scenarioDir = "scenario"
	// scenarioEntryFile is that entry point.
	scenarioEntryFile = "main.yml"
	// upgradeDir is the SECOND scenario auto-discovery channel ([ADR-0068] §3):
	// `upgrade/<slug>/main.yml`, in the same file form as a scenario and checked by
	// the same rules (keeper: artifact.ListUpgrades beside artifact.ListScenarios).
	upgradeDir = "upgrade"
	// migrationsDir holds the state-schema ladder.
	migrationsDir = "migrations"
)

// TreeOptions holds the parameters of one whole-service run.
//
// It is deliberately not [Options]: that one addresses a single document by Path
// and carries a Kind, and neither is a property of a tree. The two flags that do
// carry over — the module bindings and the service name — mean here exactly what
// they mean there, and are handed to every part that reads them.
type TreeOptions struct {
	// Root is the service directory, or the path of the `service.yml` inside it
	// (both spellings are accepted — see [serviceTreeRoot]).
	Root string
	JSON bool

	// Modules holds the `--modules <alias>=<path>` bindings, applied to every
	// part of the tree. A binding that does not resolve is fatal for the whole
	// run, by the same rule as in [Options]: the author asked for those checks by
	// naming the alias, and running the tree without them would print a clean
	// result for a check that never ran.
	Modules []string

	// ServiceName is `--service-name <name>`, the name the service is registered
	// under (NIM-726). Empty means the own-namespace Vault fence cannot run and
	// every scenario says so (`own_namespace_fence_unchecked`) rather than
	// passing in silence. Nothing in the tree can supply it: the directory name
	// is a convention, not the registry.
	ServiceName string
}

// treePart is one checked part of the tree, kept together with its own
// diagnostics so the report can say which part each finding came from — and,
// more importantly, so that a part with errors is still just one entry in a
// list that the walk finishes.
type treePart struct {
	// path is how the part is addressed on disk, and what the `OK:` line names.
	path string
	// validated is false for a part that was only DISCOVERED — found on disk with
	// no check available for it yet. Such a part prints no `OK:` line: saying
	// "OK" about something nobody looked at is the exact silence this mode exists
	// to remove.
	validated bool
	diags     []diag.Diagnostic
}

// RunTree validates a whole service tree and returns an exit code per the same
// contract as [Run]: 0 = no errors anywhere, 1 = at least one error somewhere,
// 2 = the caller is wrong (not a service tree, or an unresolvable binding).
func RunTree(opts TreeOptions, out io.Writer, errOut io.Writer) int {
	root, err := serviceTreeRoot(opts.Root)
	if err != nil {
		fmt.Fprintf(errOut, "soul-lint: %v\n", err)
		return ExitIOFatal
	}

	// Resolved ONCE, before the walk, and fatal when it fails — a broken binding
	// is a statement about the command line, not about the service, so reporting
	// it per scenario would print the same usage error five times and bury it
	// among the service's own findings.
	modules, ok := Options{Modules: opts.Modules}.moduleSchemas(errOut)
	if !ok {
		return ExitIOFatal
	}

	parts := treeParts(root, opts, modules)
	printTree(opts, parts, out)
	for _, p := range parts {
		if diag.HasErrors(p.diags) {
			return ExitHasErrors
		}
	}
	return ExitOK
}

// serviceTreeRoot resolves the positional argument to a service root.
//
// Both spellings are accepted — the directory, and the `service.yml` inside it —
// because both are what people already type: a service repository's own
// `make validate` passes its repository root (there is no such target in THIS
// repo — the corpus gate is `make lint`), while an author who has just linted the
// manifest re-runs the same path with the wider command. Anything else is refused rather than guessed:
// a directory with no manifest is not a service tree, and walking it would report
// a service's worth of absences instead of the one fact that matters.
func serviceTreeRoot(arg string) (string, error) {
	if arg == "" {
		return "", fmt.Errorf("no service directory given")
	}
	root := arg
	if info, err := os.Stat(arg); err == nil && !info.IsDir() {
		// A file, and only the manifest will do. Taking any file's directory would
		// mean `validate-service-tree types.yml` quietly walked the whole service —
		// not a wrong answer, but a wider one than was asked for, and the reader
		// would have to notice from the report which question got answered.
		if filepath.Base(arg) != serviceManifestFile {
			return "", fmt.Errorf("%s: expected a service directory, or the %s inside it", arg, serviceManifestFile)
		}
		root = filepath.Dir(arg)
	}
	manifest := filepath.Join(root, serviceManifestFile)
	if _, err := os.Stat(manifest); err != nil {
		return "", fmt.Errorf("%s: not a service tree (no %s here): %w", root, serviceManifestFile, err)
	}
	return root, nil
}

// treeParts walks the tree in a FIXED order — manifest, type catalog, scenarios,
// upgrade scenarios, migrations, each directory sorted by name — so that identical
// trees produce identical output, run to run. Nothing here reads a map in
// iteration order or a directory in whatever order the filesystem returns.
//
// Nothing is deduplicated either, and that is a decision rather than an omission.
// One file is read by several parts — a broken types.yml is reported by the
// manifest's `$type` resolve, by each scenario's, and by its own part — so an
// identical sentence can appear more than once in one report. Collapsing them
// costs more than it buys: the verdict of a part is computed from ITS OWN
// diagnostics, so a part whose every finding was already printed elsewhere would
// come out looking clean and take an `OK:` line, which is the exact failure this
// mode exists to abolish. Keeping every part's diagnostics whole gives the report
// a contract worth stating instead: it is the per-file reports concatenated in a
// fixed order, plus the parts no per-file command covers. A repeated sentence is
// also information — it says which parts this one broken file is breaking.
func treeParts(root string, opts TreeOptions, modules config.ModuleManifestResolver) []treePart {
	var parts []treePart

	// The manifest first, because it is the part the reader will fix first — and
	// because it is the one whose breakage used to end the whole run.
	// No ServiceName on the manifest: the own-namespace fence reads it, and the
	// fence runs over a scenario's tasks. Handing it here would suggest the
	// manifest checks consume it.
	parts = append(parts, documentPart(filepath.Join(root, serviceManifestFile), Options{
		Kind: KindService,
	}, modules))

	if p, ok := typeCatalogPart(root); ok {
		parts = append(parts, p)
	}
	parts = append(parts, scenarioParts(root, opts, modules)...)
	if p, ok := migrationsPart(root); ok {
		parts = append(parts, p)
	}
	return parts
}

// documentPart reads one document and runs the check set for its Kind.
//
// A read failure becomes an ERROR diagnostic instead of an exit: at tree level a
// scenario that cannot be opened is one broken part among several, and the other
// parts are exactly what the reader came for. [Run] makes the same failure fatal,
// and correctly so — there the file IS the run.
func documentPart(path string, opts Options, modules config.ModuleManifestResolver) treePart {
	opts.Path = path
	src, err := os.ReadFile(path)
	if err != nil {
		return treePart{path: path, validated: true, diags: []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
			Hint:    "the file is part of the service tree but could not be read - the rest of the tree was still checked",
		}}}
	}
	// The bool from diagnose is dropped deliberately. It says "this Kind has no
	// case in diagnose", which is a defect of the linter and not of the service,
	// and there is no diagnostic code for that — inventing one would put a message
	// nobody can act on into the catalogue every author reads. What must not
	// happen is a Kind added without a case turning into a clean part, and that is
	// held by TestDiagnose_HandlesEveryKind.
	return treePart{path: path, validated: true, diags: safeDiags(path, func() []diag.Diagnostic {
		diags, _ := diagnose(opts, src, modules)
		return diags
	})}
}

// safeDiags runs one part's checks and converts a panic into a diagnostic.
//
// A panic is the ultimate early exit: a nil dereference deep in one document's
// parser would abort the process, print a stack trace over a half-written report
// and leave every part after it unchecked — which is the failure this whole mode
// exists to remove, arriving by the one route the rest of the file cannot guard.
// Per-file [Run] can afford to die, because there the file IS the run; here it
// would cost the other parts, and it is a linter bug rather than the author's, so
// making them pay for it is the wrong trade.
//
// The panic is not swallowed: it comes back as an ERROR with its value in the
// message, so the run is red, the author knows which file provoked it, and the
// report is still complete for everything else.
//
// Two things sit outside it on purpose. What runs BEFORE the walk — resolving the
// root, loading the `--modules` bindings — has no report to protect yet, so a
// crash there is just a crash. And a recover does not catch stack exhaustion or a
// concurrent-map-write fatal, so "no part can end the walk" is true of panics and
// not of every way a process can die; a schema recursive enough to blow the stack
// still takes it down.
func safeDiags(path string, run func() []diag.Diagnostic) (diags []diag.Diagnostic) {
	defer func() {
		if r := recover(); r != nil {
			diags = append(diags, diag.Diagnostic{
				Level:   diag.LevelError,
				Phase:   diag.PhaseParse,
				File:    path,
				Code:    CodeLintInternalPanic,
				Message: fmt.Sprintf("the linter crashed while checking this file: %v", r),
				Hint:    "this is a defect in soul-lint, not in the file - the rest of the service tree was still checked; please report it with the file that provoked it",
			})
		}
	}()
	return run()
}

// typeCatalogPart parses `types.yml` on its own account.
//
// It is already parsed as a side effect of resolving a `$type` reference, from
// the manifest's `state_schema:` and from each scenario's `input:` — but only
// when there IS such a reference. A catalog nobody currently references is
// unparsed and therefore unjudged, so a service can carry a broken types.yml
// through every per-file check and meet it later, at the keeper, on the first
// scenario that starts using a type. Here the catalog is a part of the tree like
// any other, referenced or not.
//
// An absent types.yml is not a finding: types are optional.
func typeCatalogPart(root string) (treePart, bool) {
	return typeCatalogPartWith(root, config.ParseTypeCatalog)
}

// typeCatalogPartWith is [typeCatalogPart] with the parser injected, so a test can
// hand it one that panics and assert the recover is actually WIRED here.
//
// The seam earns its keep: this is the one part whose checks are not reached
// through [diagnose], so it is the one place the recover can be forgotten while
// every other part stays protected — and it was forgotten, in the first cut of
// this file. A test over [safeDiags] alone passes against that bug.
func typeCatalogPartWith(root string, parse func(string, []byte) (config.TypeCatalog, []diag.Diagnostic)) (treePart, bool) {
	path := filepath.Join(root, config.TypesCatalogFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return treePart{}, false
		}
		return treePart{path: path, validated: true, diags: []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
			Hint:    "the type catalog is present but unreadable - every $type reference in this service resolves against nothing",
		}}}, true
	}
	// Behind the same recover as every other part. The catalog parser is reached
	// through [diagnose] from the manifest part and from each scenario part, so
	// without this a types.yml that crashes it would be survivable in two parts
	// and fatal in the third — the mechanism's one hole, in the part that exists
	// precisely because nothing else was reading this file.
	return treePart{path: path, validated: true, diags: safeDiags(path, func() []diag.Diagnostic {
		_, diags := parse(path, data)
		return diags
	})}, true
}

// scenarioParts validates every scenario of the service, in sorted order, across
// BOTH auto-discovery channels: `scenario/<name>/main.yml` and
// `upgrade/<slug>/main.yml` ([ADR-0068] §3, the keeper's ListScenarios and
// ListUpgrades). The two differ in what the keeper does with the result, not in
// the file — an upgrade scenario is checked by the same rules — so a walk that
// covered only the first would print a clean tree while never opening the other
// half of it.
//
// Each scenario also pulls in the covenant it extends (ResolveScenarioCovenant,
// run inside the scenario check set), so `covenant.yml` and its family are
// covered here rather than as a part of their own: a covenant is a fragment, and
// what it means is only decided once merged into a scenario.
//
// A tree with no scenario in EITHER channel gets one warning — a service that can
// be registered and never run.
func scenarioParts(root string, opts TreeOptions, modules config.ModuleManifestResolver) []treePart {
	var parts []treePart
	for _, dirName := range []string{scenarioDir, upgradeDir} {
		parts = append(parts, scenarioDirParts(filepath.Join(root, dirName), opts, modules)...)
	}
	if len(parts) == 0 {
		dir := filepath.Join(root, scenarioDir)
		// validated: false — nothing was checked here, and the directory this part
		// is named after may not even exist. `OK: <root>/scenario` under a warning
		// saying there are no scenarios is the same optimistic silence the rule
		// against it was written for.
		parts = append(parts, treePart{path: dir, validated: false, diags: []diag.Diagnostic{{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseSemanticValidate,
			File:    dir,
			Code:    CodeServiceTreeNoScenarios,
			Message: "the service declares no scenario in scenario/ or upgrade/ - it can be registered but never run",
			Hint:    "a scenario is a directory scenario/<name>/ (or upgrade/<slug>/) with a main.yml entry point",
		}}})
	}
	return parts
}

// scenarioDirParts walks one scenario channel. An absent directory contributes
// nothing (a service may have either channel and not the other).
func scenarioDirParts(dir string, opts TreeOptions, modules config.ModuleManifestResolver) []treePart {
	var parts []treePart

	// The entries are used EVEN WHEN ReadDir also returned an error: it returns
	// what it managed to read alongside the failure, and discarding that would let
	// one unreadable entry hide every scenario beside it — this file's own rule,
	// broken by the code meant to enforce it. The failure is reported as well.
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		parts = append(parts, treePart{path: dir, validated: true, diags: []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    dir,
			Code:    "io_error",
			Message: err.Error(),
			Hint:    "the directory could not be listed in full - any scenario it holds beyond this point was not checked",
		}}})
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A directory holding SHARED include bodies rather than a scenario. The rule
		// is asked of its owner (config.IsSharedDirName), which exists so every
		// scenario walker — the keeper's listing, the linters — reads one contract
		// instead of re-deriving it; a third copy here would be the drift that
		// function was written to prevent.
		if config.IsSharedDirName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	// ReadDir already sorts, and this says out loud that the order is REQUIRED
	// rather than inherited: byte-identical output on identical input is a
	// property the report is asserted on, not a side effect of the filesystem.
	sort.Strings(names)

	for _, name := range names {
		entry := filepath.Join(dir, name, scenarioEntryFile)
		if _, err := os.Stat(entry); err != nil {
			// A scenario directory without its entry point. Not skipped in
			// silence: `<channel>/<name>/` is how a scenario is addressed, so a
			// directory that answers to a name and cannot be run is a defect.
			parts = append(parts, treePart{path: entry, validated: true, diags: []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseParse,
				File:    entry,
				Code:    "io_error",
				Message: err.Error(),
				Hint:    "every scenario/<name>/ and upgrade/<slug>/ needs a main.yml entry point; a directory of shared include bodies belongs under a name starting with _",
			}}})
			continue
		}
		parts = append(parts, documentPart(entry, Options{
			Kind:        KindScenario,
			ServiceName: opts.ServiceName,
		}, modules))
	}
	return parts
}

// migrationsPart reports the migration ladder, checked.
//
// It used to report the ladder as discovered-and-NOT-checked, because parsing a
// step and checking continuity lived in each service repository's own shell
// script. NIM-736 moved both into the engine, so this part now runs
// [config.ValidateMigrationLadder] — the SAME scan the keeper derives the
// state-schema version from, which is what keeps the linter and the engine from
// disagreeing about what a ladder says.
//
// An absent `migrations/` is not a part at all; an EMPTY one is a complete
// statement (the service is at state-schema version 1) and equally has nothing to
// report. Both are skipped rather than reported clean, which is how this walk
// treats every part that does not exist.
func migrationsPart(root string) (treePart, bool) {
	dir := filepath.Join(root, migrationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return treePart{}, false
		}
		// Not io_error any more: an unlistable `migrations/` has its own code now,
		// and it is raised by the scan below rather than guessed at here — the
		// version is read through that scan, so it is the scan that has to say the
		// ladder went missing.
		return treePart{path: dir, validated: true, diags: config.ValidateMigrationLadder(root)}, true
	}
	if len(entries) == 0 {
		return treePart{}, false
	}
	return treePart{path: dir, validated: true, diags: config.ValidateMigrationLadder(root)}, true
}

// printTree writes the report.
//
// JSON mode is the same JSON-Lines stream the per-file commands produce — one
// object per diagnostic, in walk order — so a consumer written against one mode
// reads the other unchanged. Human mode adds an `OK: <path>` line per part that
// came out clean, which is what makes the report say WHICH parts ran: a tree
// whose scenarios were never reached would otherwise look exactly like a tree
// whose scenarios are fine.
func printTree(opts TreeOptions, parts []treePart, w io.Writer) {
	if opts.JSON {
		bw := bufio.NewWriter(w)
		defer bw.Flush()
		enc := json.NewEncoder(bw)
		for _, p := range parts {
			for _, d := range p.diags {
				_ = enc.Encode(d)
			}
		}
		return
	}
	for _, p := range parts {
		for _, d := range p.diags {
			writeHumanDiag(w, d)
		}
		if p.validated && !diag.HasErrors(p.diags) {
			fmt.Fprintf(w, "OK: %s\n", p.path)
		}
	}
}

// Codes introduced by the whole-service mode. Both are about the WALK rather than
// about a document, which is why neither has a per-file command that could raise
// it.
//
// `migrations_unchecked` was a third, and it is RETIRED (NIM-736): the ladder is
// read now, so there is no unchecked part left for it to name. It was a hint
// about a gap in the tool, and the gap is closed.
const (
	// CodeServiceTreeNoScenarios — the tree holds no scenario/<name>/main.yml. A
	// warning: the service parses, registers and is inert.
	CodeServiceTreeNoScenarios = "service_tree_no_scenarios"
	// CodeLintInternalPanic — a check crashed on this file. An error, so the run
	// is red and nothing downstream treats the tree as clean, but it accuses the
	// linter rather than the author.
	CodeLintInternalPanic = "lint_internal_panic"
)
