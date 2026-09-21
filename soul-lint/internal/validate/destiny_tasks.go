package validate

// The destiny's own task file, checked from `validate-destiny` (NIM-783).
//
// `destiny.yml` carries the contract — input:/validate:/compat: — and not one task.
// The tasks live in the sibling `tasks/main.yml`, which two cross-file checks here
// already read for FACTS (the compat floor of what it uses, the vars it shadows) and
// both discard its diagnostics: the file was parsed and its verdict thrown away. So
// `validate-destiny` printed `OK:` over a task file it had opened twice and judged
// zero times — including a plugin step whose `params:` the `--modules` the command
// accepts could have checked.
//
// That is the NIM-778 shape for the third time (NIM-779 was the second, in a
// scenario's included body): silence where a check did not run is indistinguishable
// from a check that ran and found nothing, and it is the only one of the three
// outcomes that cannot be acted on.
//
// What this pass reports is the whole verdict of the file, not the plugin half. The
// keeper's own loader ([artifact.DestinyLoader.parseTasks]) refuses the destiny on
// ANY error out of the same two calls, so a linter that reports them is not widening
// what a destiny must satisfy — it is saying offline what the keeper would say at
// load, which is the entire point of an offline linter. The one check it adds beyond
// the keeper's is the one the keeper cannot make: `--modules` binds manifests the
// cluster resolves from its Sigil grants instead.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/definition"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	// destinyTasksDir is the destiny's task tree, and the only directory an
	// `include:` inside one may reach (docs/destiny/tasks.md §4).
	destinyTasksDir = "tasks"
	// destinyTasksEntryFile is its entry point — the flat top-level task list, with
	// no manifest wrapper.
	destinyTasksEntryFile = "main.yml"
)

// CodeDestinyTasksUnchecked marks a destiny with no task file beside it, so nothing
// in its definition — plugin `params:` included — was checked.
//
// A HINT, by the same argument as [config.DiagPluginParamsUnchecked]: an author
// linting a `destiny.yml` that is not at an artifact root (a fixture, a fragment
// under review) has not written a defect, and failing them would make the honest
// report the expensive one. But it cannot be silence either — the keeper REQUIRES
// `tasks/main.yml` at the destiny root and fails the load without it, so an absent
// file is a fact the reader needs, whichever of the two situations they are in.
//
// ABSENT only. A task file that IS there and cannot be read is not that situation:
// nobody lints a fragment whose neighbour returns EACCES, the keeper hard-fails on
// it, and a hint would exit 0 over a destiny that cannot load. That one is a plain
// `io_error`, the code the tree walk already uses for the same event.
const CodeDestinyTasksUnchecked = "destiny_tasks_unchecked"

// destinyTasksDiagnostics validates the `tasks/main.yml` beside a destiny manifest,
// and every file it includes.
//
// manifestPath is the path to destiny.yml; the task tree is read from its directory,
// which is the destiny artifact root and therefore the securejoin root — the same
// root the keeper clamps an include against (see [destinyIncludeResolver]).
//
// modules is the `--modules` resolver of this run, threaded into the entry file AND
// into every included body so their plugin `params:` are checked against the same
// manifests. nil is the ordinary case and is not silent: each plugin step then
// yields [config.DiagPluginParamsUnchecked].
func destinyTasksDiagnostics(manifestPath string, modules config.ModuleManifestResolver) []diag.Diagnostic {
	// Absolute FIRST, for the reason [scenarioServiceRoot] states: `destiny.yml`
	// linted from inside its own directory decomposes to "." lexically, and every
	// path derived from it would then be resolved against the process's cwd rather
	// than against the artifact.
	dir := filepath.Dir(manifestPath)
	root, err := filepath.Abs(dir)
	if err != nil {
		root = dir
	}
	tasksPath := filepath.Join(dir, destinyTasksDir, destinyTasksEntryFile)

	data, rerr := os.ReadFile(tasksPath)
	if rerr != nil {
		return []diag.Diagnostic{tasksUncheckedDiag(manifestPath, tasksPath, rerr)}
	}

	// The parse (under `DestinyTasks`), the expansion with the same resolver, and
	// the rule that every diagnostic comes out rather than the errors alone — all
	// three live in [definition.LoadDestinyTasks] since NIM-790. They used to live
	// here, in a second implementation beside [stageDiagnostics]'s: two tools, two
	// copies, and a third (`soul-trial`) that had neither.
	_, diags := definition.LoadDestinyTasks(tasksPath, data, destinyIncludeResolver(root, dir), modules)
	return diags
}

// tasksUncheckedDiag renders the "the task file was not read" finding, at the level
// its reason deserves: a hint when there is no such file, an error when there is one
// and it would not open. See [CodeDestinyTasksUnchecked] for why the two are not the
// same event.
func tasksUncheckedDiag(manifestPath, tasksPath string, err error) diag.Diagnostic {
	if !errors.Is(err, fs.ErrNotExist) {
		return diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    tasksPath,
			Code:    "io_error",
			Message: fmt.Sprintf("the destiny's task file is present but could not be read: %v", err),
			Hint:    "nothing in this destiny's definition was checked, and the keeper cannot load it either",
		}
	}
	return diag.Diagnostic{
		Level:   diag.LevelHint,
		Phase:   diag.PhaseParse,
		File:    manifestPath,
		Code:    CodeDestinyTasksUnchecked,
		Message: fmt.Sprintf("tasks of this destiny were not checked: there is no %s beside this manifest", tasksPath),
		Hint: "a destiny's tasks live in tasks/main.yml at its root and the keeper fails the load without it; " +
			"lint the manifest at the root of its artifact to have them checked too",
	}
}

// destinyIncludeResolver is the offline twin of the keeper's within-destiny include
// resolve (keeper/internal/artifact/destiny.go): ONE tier, strictly inside the
// destiny's own `tasks/` directory — a destiny is its own git artifact and has no
// service level to fall back to, unlike a scenario's two-level resolve.
//
// root is the destiny artifact root ABSOLUTE, and it is the securejoin root rather
// than `tasks/` itself for the reason spelled out at [scenarioIncludeResolver]:
// securejoin re-roots an escaping symlink at WHATEVER root it is given, so clamping
// one tier lower does not narrow what resolves, it silently resolves a different file
// than the keeper runs. The keeper roots at the snapshot root and pre-joins `tasks/`;
// so does this.
//
// dir is the same directory as the operator typed it, and is used for the display
// path — diagnostic text and the cycle-detection key — so a relative invocation keeps
// producing the short paths it always produced.
func destinyIncludeResolver(root, dir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		// Clamps a `..`/absolute target to the task directory's own root. The READ is
		// clamped by securejoin independently; the include grammar (config
		// reIncludeFile) cannot express either form in the first place — this is
		// defence in depth, as in the scenario resolver.
		rel := path.Join(destinyTasksDir, path.Clean("/" + name)[1:])
		display := filepath.Join(dir, rel)
		data, err := readWithin(root, rel)
		if err != nil {
			// No `include %q:` prefix — [config.expandOne] already adds one, and the
			// name twice in one sentence reads as a bug in the linter. Absent and
			// unreadable are told apart for the reason the scenario resolver tells them
			// apart: an I/O error worded as "not found" sends the author looking for a
			// file that is right there.
			if errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("no such body in this destiny's task tree (%s)", display)
			}
			return nil, "", fmt.Errorf("reading %s: %w", display, err)
		}
		return data, display, nil
	}
}
