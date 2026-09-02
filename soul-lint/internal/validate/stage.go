package validate

// Stage validation for scenarios (ADR-056 §S5): an offline run of the SAME
// Passage stratification the keeper runtime performs before dispatch
// ([config.Stratify]), so the scenario author catches staged errors BEFORE
// apply. Reuses the canonical shared/config (Stratify over the same []Task
// plan + the config validator's reads==refs check): one register-dependency
// graph for both linter and runtime (a duplicate would mean silent-wrong-target).
//
// What this detects OFFLINE (on top of unknown_register_reference, already
// caught by the config validator at parse time):
//   - register cycle (StratifyCycle) — ERROR: no topological order exists,
//     the run would never have started.
//   - passage structure (how many Passages, how many tasks each) — HINT
//     (informational, for the author).
//   - the two capture-ordering rules of [ADR-0084] — ERROR: a consumer that
//     reads a register BEFORE the `core.state.<verb>` step that stores it
//     ([config.StoreAfterUse]), and an interpolated `${ incarnation.state.<field> }`
//     read that runs after a same-Passage capture of that field
//     ([config.StaleStateRead]). Both need the EXPANDED task list, which is
//     exactly what this pass has and the per-file task rules do not.
//
// serial: + staged (N>1 Passage) is no longer an error (ADR-056 §S4 amend,
// S-2D1): 2D serial×passage is implemented — each Passage runs its serial
// waves at its own per-Passage width. Such a scenario now passes lint with
// a plain passage_plan HINT.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// stageDiagnostics runs Passage stratification over an already-parsed
// scenario and returns additional diagnostics (info/errors) that the caller
// appends to the parse diagnostics. scenarioPath is the path to main.yml.
//
// Include targets are resolved by the same two-level resolve the keeper runs
// ([scenarioIncludeResolver]) whenever the file sits in a real service tree
// ([scenarioServiceLevelDir]) — so the stage graph offline is built over the
// same task list as at apply time, and an include that resolves at NEITHER level
// is a plain ERROR. For a loose scenario file outside a service tree the service
// level does not exist offline, so resolution stays local-only and an include
// whose TARGET is not found is downgraded to the stage_include_unresolved HINT:
// the keeper will resolve it against the real snapshot. Only that one failure is
// downgraded — an error out of a body that DID resolve is the body's own, and is
// reported at the body's own coordinates wherever the scenario was linted from
// (NIM-716).
//
// m==nil (parse failed with errors) → no point stratifying (the graph is
// unreliable) → nil.
func stageDiagnostics(scenarioPath string, m *config.ScenarioManifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}

	// serviceDir == "" — the second resolve level is unavailable (a loose file);
	// it also switches the diagnostics below back to the HINT downgrade.
	serviceDir := scenarioServiceLevelDir(scenarioPath)
	dir := scenarioLocalLevelDir(scenarioPath, serviceDir)
	inUpgradeChannel := serviceDir != "" && scenarioChannel(scenarioPath) == upgradeChannelDir
	// The securejoin root for BOTH levels, and only inside a service tree: it is
	// the root, not the level, that decides which file an escaping symlink lands
	// on, so it has to be the same root the keeper uses (the snapshot root).
	var root string
	if serviceDir != "" {
		root = scenarioServiceRoot(scenarioPath)
	}
	var out []diag.Diagnostic

	tasks, expandDiags := config.ExpandIncludes(m.Tasks, scenarioIncludeResolver(root, dir, serviceDir))
	// In a service tree include resolution offline is COMPLETE (both levels are on
	// disk), so expand's diagnostics are passed through at their own level: an
	// unresolvable include, a cycle or a cross-file duplicate address is a real
	// defect the linter must fail on, not a deferral to the keeper.
	//
	// Outside one, exactly ONE class of failure may be nothing but the missing
	// service level — a target that was not FOUND ([config.IsIncludeResolveDiag],
	// asked of the producer rather than re-listed here). That one is downgraded to
	// a HINT, as before NIM-694, because the keeper will resolve it against the
	// real snapshot.
	//
	// Everything else passes through as itself, at its own File/Line/YAMLPath.
	// This is NIM-716: the condition used to be `serviceDir == "" && error`, which
	// looked at nothing but the level and the severity, so an error out of a body
	// that resolved LOCALLY and was READ — the same bytes, checked by the same code
	// as inside a service tree — came out as `include does not resolve offline`
	// with exit 0. Two failures in one: the text was false, and a real defect was
	// reported as a deferral.
	//
	// include_when_dynamic_unsupported used to need an explicit exemption from that
	// sweep, and no longer does: it is a property of the include NODE rather than
	// of resolution, so the narrowed condition never reaches it. (It is also the
	// expander's alone to report — a second producer, a pre-pass over m.Tasks, was
	// removed because it never recursed into an include's own includes, so the
	// filter that kept the two from printing twice silently ate the nested case.)
	for _, d := range expandDiags {
		if serviceDir == "" && d.Level == diag.LevelError && config.IsIncludeResolveDiag(d.Code) {
			out = append(out, diag.Diagnostic{
				Level:   diag.LevelHint,
				Phase:   diag.PhaseSemanticValidate,
				File:    scenarioPath,
				Code:    "stage_include_unresolved",
				Message: fmt.Sprintf("include target does not resolve offline: %s -- outside a service tree only the scenario-local level exists, so the stage graph is checked against locally available tasks", d.Message),
				Hint:    "lint the scenario inside its service tree (<service>/scenario/<name>/main.yml) to resolve service-level includes, or rely on full validation at the keeper",
			})
			continue
		}
		if d.File == "" {
			d.File = scenarioPath
		}
		// An upgrade scenario's author needs one sentence the producer cannot write:
		// the message names the two levels tried, and for them BOTH are directories
		// they are not in. Without it "not found locally (scenario/<slug>/x.yml)"
		// reads as a linter mistake — they are looking straight at their file.
		if d.Hint == "" && inUpgradeChannel && config.IsIncludeResolveDiag(d.Code) {
			d.Hint = "an upgrade scenario resolves include: from scenario/<slug>/ and scenario/, NOT from upgrade/<slug>/ -- the two levels are fixed directories keyed on the scenario NAME (docs/scenario/orchestration.md §6), so a body beside this file is unreachable at run time; move it to one of the two levels named above"
		}
		out = append(out, d)
	}
	// A failed expansion leaves a TRUNCATED task list (the broken branch is
	// dropped): stratifying it would report register/passage errors about a plan
	// that never existed. Report the expansion failure alone — the author fixes
	// the include first, the stage graph is checked on the next run.
	if diag.HasErrors(out) {
		return out
	}

	// Wide match is a WARN, and unlike everything below it does NOT return: the
	// wide form is legitimate ("clear every user"), it is just far more often a
	// predicate the author meant to narrow. Emitted here, before the ordering
	// errors short-circuit, so the author sees it even in a plan that also fails
	// to stratify -- and emitted from the stage pass rather than a per-task rule
	// because a capture routinely arrives through `include:`.
	for _, info := range config.WideStateMatch(tasks) {
		field := info.Ref
		if field == "" {
			field = "the field"
		}
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeStateWideMatch,
			Message: fmt.Sprintf("task %q (core.state.%s) has no narrowing match: -- it selects EVERY element of %s", info.CaptureName, info.CaptureVerb, field),
			Hint:    "give match: a predicate over elem/key/value; a constant-true or absent match: repatches or removes the whole collection",
		})
	}

	plan, err := config.Stratify(tasks)
	if err != nil {
		var se *config.StratifyError
		code := "register_graph_invalid"
		if errors.As(err, &se) {
			code = se.Code
		}
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    code,
			Message: err.Error(),
			Hint:    "staged-render will not be able to order the Passage by register dependency (ADR-056)",
		})
		return out
	}

	// serial + staged (N>1) is no longer an error (ADR-056 §S4 amend, S-2D1): 2D
	// serial×passage is implemented — each Passage runs its own serial waves at
	// its own per-Passage width. Stratification yields a plain passage_plan HINT
	// (below).

	// A within-block register dependency is an ERROR (ADR-056, §"Risks —
	// silent-wrong-target"). A block: child reading the register of a sibling
	// child in the SAME block is impossible at render time: a block is atomic
	// per Passage, peer register is available Soul-side only AFTER probe, while
	// where/when/params are resolved Keeper-side BEFORE dispatch → where would
	// silently select hosts by a stale/foreign register. Stratify doesn't catch
	// this (the within-block edge doesn't cross a top-level task boundary).
	// Caught offline BEFORE apply.
	if info, bad := config.WithinBlockRegisterDependency(tasks); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeWithinBlockRegisterDependency,
			Message: fmt.Sprintf("task %q inside a block: reads register %q emitted by a sibling %q of the SAME block -- impossible at render time (a block is atomic, the peer register is only available Soul-side AFTER probe, while where/when/params resolve Keeper-side BEFORE dispatch)", info.ReaderName, info.RegisterName, info.EmitterName),
			Hint:    "move the probe to top-level (probe and consumer become separate Passages; ADR-056 staged-render will then order them normally)",
		})
		return out
	}

	// Cross-passage when-gating is an ERROR (ADR-056:85 amend, FC-5). A task
	// gates `when:`/`changed_when:`/`failed_when:` on a register emitted in an
	// EARLIER Passage. flow-control is Soul-side per-task gating (ADR-012(d)),
	// visible only within its OWN Passage; a cross-passage register is
	// unavailable to it (a different ApplyRequest) → silent `no such key` →
	// the task FAILS. After the narrow-fix, flow-control itself doesn't split
	// the Passage, but a probe may have landed in an earlier Passage for a
	// DIFFERENT reason (another task with `where: register.X`). where: can do
	// this (Keeper re-renders with accumulated register), when: cannot.
	// Fail-closed reject, offline.
	if info, bad := config.CrossPassageWhenGating(tasks, plan); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeCrossPassageWhenGating,
			Message: fmt.Sprintf("task %q gates %s: by register %q from a different Passage (consumer passage %d, source passage %d) -- Soul-side gating sees only its own Passage, a cross-passage register is unavailable to it -> no such key", info.ConsumerName, info.Kind, info.RegisterName, info.ConsumerPassage, info.SourcePassage),
			Hint:    "when:/changed_when:/failed_when: by register from a different Passage is unsupported (Soul-side gating sees only its own Passage) -- use where: for cross-task register targeting, OR register.self for same-task gating",
		})
		return out
	}

	// Store-after-use is an ERROR ([ADR-0084], "The ordering guard"). A task
	// consumes a register that a `core.state.<verb>` step captures, and consumes
	// it FIRST: in the window between the two a crash leaves a live host
	// configured with a value that exists nowhere else. Generate -> store -> use
	// is recoverable at every crash point; this is the one order that is not.
	if info, bad := config.StoreAfterUse(tasks, plan); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeStateStoreAfterUse,
			Message: fmt.Sprintf("task %q consumes register %q BEFORE the capture step %q (core.state.%s) stores it -- a crash between the two leaves a host configured with a value that exists nowhere else", info.OtherName, info.Ref, info.CaptureName, info.CaptureVerb),
			Hint:    "move the core.state.<verb> step ahead of every consumer of that register: generate -> store -> use",
		})
		return out
	}

	// A stale same-Passage state read is an ERROR ([ADR-0084], same section). An
	// interpolated `${ incarnation.state.<field> }` refreshes only at a Passage
	// boundary, so a reader after a same-Passage capture of that field renders the
	// PRE-capture value -- while the verb engine, which always reads live, would
	// have written the new one. Rejected rather than letting the two mechanisms
	// disagree in production.
	if info, bad := config.StaleStateRead(tasks, plan); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeStateStaleRead,
			Message: fmt.Sprintf("task %q reads ${ incarnation.state.%s } in the SAME Passage (%d) where %q (core.state.%s) captures it -- an interpolated read refreshes only at a Passage boundary, so it renders the pre-capture value", info.OtherName, info.Ref, info.CapturePassage, info.CaptureName, info.CaptureVerb),
			Hint:    "read the capture's effective output through register.<name>, or push the reader into a later Passage with a register: dependency on the capture",
		})
		return out
	}

	// Passage structure — HINT (informational, for the author): how many
	// Passages and how many tasks each.
	out = append(out, diag.Diagnostic{
		Level:   diag.LevelHint,
		Phase:   diag.PhaseSemanticValidate,
		File:    scenarioPath,
		Code:    "passage_plan",
		Message: passagePlanSummary(plan),
	})

	return out
}

// passagePlanSummary is a human-readable description of the stratification:
// the number of Passages and the size of each (N=1 → a single pass,
// BIT-FOR-BIT identical to pre-staged-render behavior).
func passagePlanSummary(plan config.Passage) string {
	counts := make([]int, plan.Count)
	for _, p := range plan.TaskPassage {
		if p >= 0 && p < plan.Count {
			counts[p]++
		}
	}
	if plan.Count <= 1 {
		return fmt.Sprintf("single-passage run (%d tasks, no cross-task register dependency) -- one pass, as before staged-render", len(plan.TaskPassage))
	}
	return fmt.Sprintf("staged run: %d Passage by register dependency, tasks in each %v (register consumer executes strictly after the probe)", plan.Count, counts)
}

// The two scenario auto-discovery channels ([ADR-0068] §3). Both hold
// `<channel>/<name>/main.yml` in the same form; they differ in what the keeper
// does with the result, and — see [scenarioLocalLevelDir] — not at all in how an
// `include:` inside them resolves.
const (
	scenarioChannelDir = "scenario"
	upgradeChannelDir  = "upgrade"
)

// scenarioServiceLevelDir returns the service-level include directory for a
// linted scenario — `<service>/scenario`, the second level of the ADR-009
// resolve — or "" when the file is not part of a service tree.
//
// The marker is the layout itself: main.yml's parent directory is named
// `scenario` AND the service root above it carries service.yml. Anything else
// (a loose file, a fixture in a testdata directory) has no service level: taking
// "one directory up" there would let an unrelated neighbouring file answer an
// include, which is worse than not resolving it at all.
//
// `upgrade/<slug>/main.yml` — the second auto-discovery channel ([ADR-0068] §3) —
// is a service tree too, and its service level is `scenario/`, not `upgrade/`.
//
// This REPLACES the rule NIM-694 wrote here, which returned "" for that path. Its
// reasoning was sound as far as it went — the keeper expands an upgrade's includes
// against `scenario/`, so answering with `upgrade/` would green-light an include
// the run cannot resolve — but "" does not avoid that outcome, it only moves it.
// With no service level the file is handled as a LOOSE one, whose single level is
// its own directory: `include: install.yml` then reads `upgrade/<slug>/install.yml`,
// splices it, checks it and says nothing. That is the same false green, arrived at
// from the other side, plus a second cost — inside a real service tree an
// unresolvable include came back as a hint with exit 0.
//
// Naming `scenario/` avoids both, because it is what the engine actually does and
// what the spec actually says: the two levels are FIXED directories, `scenario/<name>/`
// then `scenario/`, "whichever file the `include:` was written in"
// ([docs/scenario/orchestration.md] §6). The keeper is conforming to that, not
// misbehaving — `scenarioIncludeResolver` is handed the scenario NAME and never the
// channel, and `scenarioTemplatePrefix` is name-keyed for the same reason, so
// `templates/` and `include:` agree. A body written beside an upgrade scenario is
// therefore unreachable at run time, and saying so is this function's whole job.
func scenarioServiceLevelDir(scenarioPath string) string {
	// Detection and the returned directory both come off the ABSOLUTE path: the
	// lexical form decides nothing. `create/main.yml` decomposes to "." (base ".",
	// not "scenario") and `main.yml` decomposes to "." as well but there "." is the
	// scenario's OWN directory, not the level above it — so the lexical form got
	// the verdict wrong in one case and the directory wrong in the other, and the
	// linter's exit code became a function of the caller's shell history.
	abs, err := filepath.Abs(scenarioPath)
	if err != nil {
		return ""
	}
	channelDir := filepath.Dir(filepath.Dir(abs))
	switch filepath.Base(channelDir) {
	case scenarioChannelDir, upgradeChannelDir:
	default:
		return ""
	}
	if _, err := os.Stat(filepath.Join(scenarioServiceRoot(scenarioPath), "service.yml")); err != nil {
		return ""
	}
	// `scenario/` for BOTH channels, because that is what the keeper does: its
	// resolver hardcodes `serviceDir = "scenario"` and is handed the scenario NAME,
	// never the channel it was loaded from (keeper/internal/scenario/include.go,
	// called from run.go/preflight.go/render_host.go with spec.ScenarioName). An
	// upgrade scenario therefore falls back to `scenario/<file>` at the service
	// level, and a linter that fell back to `upgrade/<file>` instead would bless a
	// body the run cannot find.
	return pathLike(scenarioPath, filepath.Join(filepath.Dir(channelDir), scenarioChannelDir))
}

// scenarioChannel names the auto-discovery channel a scenario entry point was
// found in — the directory two levels above main.yml. Off the ABSOLUTE path, for
// the same reason every other decision here is.
func scenarioChannel(scenarioPath string) string {
	abs, err := filepath.Abs(scenarioPath)
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(filepath.Dir(abs)))
}

// scenarioLocalLevelDir is the FIRST resolve level — the directory an `include:`
// is looked for in before the service level.
//
// Inside a service tree it is `scenario/<name>/` for both channels, and the second
// half of that sentence is the surprising one: an `upgrade/<slug>/main.yml` has its
// includes resolved out of `scenario/<slug>/`, not out of the directory it is
// sitting in. That is not a choice made here. The keeper builds the level as
// `path.Join("scenario", scenarioName)` from the scenario's NAME
// (keeper/internal/scenario/include.go:26) and never learns which channel the entry
// point came from, even though loading that entry point is channel-aware
// (`upgrade/%s/main.yml`, scenario.go:57). So a body next to an upgrade scenario is
// unreachable AT RUN TIME, and the linter's job is to say so rather than to resolve
// it and call the definition good. Being right about the file while disagreeing
// with the engine is the false green this whole ticket is about — and here the
// engine is not even wrong: [docs/scenario/orchestration.md] §6 fixes the two
// levels as directories rather than "relative to the including file", precisely so
// a body's meaning does not depend on which file spliced it.
//
// Outside a service tree there is no keeper counterpart, so the level is the
// scenario's own directory — where a loose file's neighbours actually are.
func scenarioLocalLevelDir(scenarioPath, serviceDir string) string {
	if serviceDir == "" {
		return filepath.Dir(scenarioPath)
	}
	abs, err := filepath.Abs(scenarioPath)
	if err != nil {
		return filepath.Dir(scenarioPath)
	}
	// The name off the ABSOLUTE path: `main.yml` linted from inside the scenario's
	// own directory decomposes to "." lexically, which would address the channel
	// root instead of the scenario.
	return filepath.Join(serviceDir, filepath.Base(filepath.Dir(abs)))
}

// pathLike renders target the way the operator addressed the scenario: absolute
// for an absolute invocation, cwd-relative for a relative one, so that a relative
// invocation keeps producing the short paths it always produced in diagnostics
// (the resolver builds its display strings by joining onto this directory).
//
// The service level is carried in this form and does reach [readerAt], which
// re-absolutises it before clamping under the service root. That round-trip
// (Abs after Rel) is exact only because both are purely lexical AND nothing
// chdirs between them — soul-lint never does. Should that ever stop holding,
// this is the pair to fix: the value is meant as display text, and the read side
// must not start depending on the cwd it was rendered against.
func pathLike(scenarioPath, target string) string {
	if filepath.IsAbs(scenarioPath) {
		return target
	}
	cwd, err := os.Getwd()
	if err != nil {
		return target
	}
	rel, err := filepath.Rel(cwd, target)
	if err != nil {
		return target
	}
	return rel
}

// scenarioIncludeResolver is the offline twin of the keeper's two-level
// scenario-include resolve (orchestration.md §6, keeper/internal/scenario/
// include.go): the target is read from main.yml's own directory
// (`scenario/<name>/<file>`) first, and only if it is absent there — from the
// service level (`scenario/<file>`). Both levels are ordinary directories in the
// linted tree, so the linter resolves exactly what the keeper will; the shared
// bodies of a scenario family (`include: _create/deploy.yml`) live at the
// service level and are found by the second tier.
//
// root — the service repo root, the securejoin root for BOTH levels; "" for a
// loose file outside a service tree. dir — the directory of main.yml.
// serviceDir — the second level, or "" when there is none
// ([scenarioServiceLevelDir]); then the resolver is local-only and the caller
// downgrades a failure to a HINT.
//
// The display path (diagnostics + the cycle-detection key) is the resolved path,
// so the two levels are distinct sources and a local file shadowing a
// service-level one is not mistaken for a cycle.
//
// path.Clean("/"+name) clamps `..`/absolute targets to the level's own root —
// defence in depth: the include grammar (config reIncludeFile) cannot express
// them in the first place.
//
// The READ goes through securejoin ROOTED AT THE SERVICE ROOT, with the level
// pre-joined as a relative path — byte for byte what the keeper does against a
// snapshot (keeper/internal/scenario/include.go joins `scenario/<name>/` and
// hands the whole thing to artifact.readSnapshotFile, whose root is the snapshot
// root). The root is the entire contract: securejoin re-roots an escaping
// symlink AT WHATEVER ROOT IT IS GIVEN, so rooting one tier lower — at the
// scenario's own directory — does not merely narrow what resolves, it silently
// resolves a DIFFERENT file. A `scenario/<name>/link -> /decoy` symlink then
// makes the linter read `scenario/<name>/decoy/…` while the keeper runs
// `<root>/decoy/…`, and the linter blesses a body it never opened. Rooting at
// the service root also keeps a symlink that legitimately points elsewhere
// INSIDE the repo (`scenario/create/deploy.yml -> ../../shared_bodies/…`)
// resolvable, as it was before this file clamped anything at all, while a
// symlink pointing OUT of the repo is still refused — which a purely lexical
// clamp cannot do, since it never touches the file system. Parity here is the
// point of expanding includes offline at all: a linter that accepts a plan the
// keeper rejects, or checks a different file than the keeper runs, is worse than
// one that defers.
// path.Clean is kept for the DISPLAY path only, which is diagnostic text and the
// cycle-detection key, never an argument to a read.
func scenarioIncludeResolver(root, dir, serviceDir string) config.IncludeResolver {
	localRead := readerAt(root, dir)
	serviceRead := readerAt(root, serviceDir)
	return func(name string) ([]byte, string, error) {
		rel := path.Clean("/" + name)[1:] // strips a leading `..`/absolute path up to the level root.
		local := filepath.Join(dir, rel)
		data, err := localRead(name)
		if err == nil {
			return data, local, nil
		}
		// Fall back to the service level ONLY when the local file is absent: an
		// I/O error (permission denied, a broken symlink) must never be masked
		// into "not found" — same rule as the keeper resolver.
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("include %q: reading locally (%s): %w", name, local, err)
		}
		if serviceDir == "" {
			return nil, "", fmt.Errorf("include %q not found locally (%s) and there is no service level offline", name, local)
		}
		service := filepath.Join(serviceDir, rel)
		data, err = serviceRead(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("include %q not found either locally (%s) or at service-level (%s)", name, local, service)
			}
			return nil, "", fmt.Errorf("include %q: reading service-level (%s): %w", name, service, err)
		}
		return data, service, nil
	}
}

// readerAt binds one resolve level to the securejoin root it must be read under.
// Inside a service tree that root is the service root and the level is addressed
// by its path relative to it, so the clamp behaves exactly as the keeper's does
// against a snapshot root. levelDir arrives in whatever form the caller renders
// for diagnostics, cwd-relative included, and is made absolute HERE before the
// root-relative path is derived from it — exact because Abs and Rel are both
// lexical and nothing chdirs in between.
//
// root == "" is the loose-file case — there is no service tree, hence no keeper
// counterpart to be in parity with, so the level is its own root. Every READ
// failure on that path is downgraded to a hint by the caller (a dynamic `when:`
// on an include is not: it is a defect in the include node itself and stays an
// error at every depth).
func readerAt(root, levelDir string) func(string) ([]byte, error) {
	if levelDir == "" {
		return func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	}
	if root == "" {
		return func(name string) ([]byte, error) { return readWithin(levelDir, name) }
	}
	abs, err := filepath.Abs(levelDir)
	if err == nil {
		var rel string
		if rel, err = filepath.Rel(root, abs); err == nil {
			return func(name string) ([]byte, error) { return readWithin(root, filepath.Join(rel, name)) }
		}
	}
	// Not reachable as the caller stands (root != "" already means Abs succeeded
	// on the scenario path moments earlier, and Rel between two absolute paths
	// cannot fail on a single-volume system) — but the fallback must not be
	// "root at the level itself". THAT is precisely the pre-fix rooting, and it
	// does not fail, it reads a DIFFERENT file. A read that cannot be shown
	// equivalent to the keeper's has to fail loudly, so the failure is bound once
	// here and returned for every name.
	failure := fmt.Errorf("locating %q under service root %q: %w", levelDir, root, err)
	return func(string) ([]byte, error) { return nil, failure }
}

// readWithin reads name strictly within base. base is made absolute first:
// securejoin on a RELATIVE base with a leading `..` normalizes the escape away
// instead of refusing it, and the path handed to soul-lint on the command line is
// routinely relative (the trial harness converts for the same reason).
func readWithin(base, name string) ([]byte, error) {
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	full, err := securejoin.SecureJoin(base, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}
