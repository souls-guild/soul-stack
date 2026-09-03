package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// MigrationsDirName is the state-schema ladder directory in a service repository
// ([`docs/migrations.md`] §"File layout").
const MigrationsDirName = "migrations"

// MigrationStepFile is the step's own document, inside its directory. The same
// name a scenario and an upgrade use for theirs — that symmetry is the point of
// the layout ([ADR-019] amendment 2026-09-01).
const MigrationStepFile = "main.yml"

// BaseStateSchemaVersion is the version of a service whose `migrations/` is empty.
// The ladder is not a record of where the schema stands, it IS where it stands, so
// the floor is a constant rather than a stored number.
const BaseStateSchemaVersion = 1

// reMigrationStepDir matches a step directory `<NNN>_<slug>`: three digits naming
// the version the step LEADS TO, then a slug that says what it does and carries no
// meaning for ordering.
var reMigrationStepDir = regexp.MustCompile(`^(\d{3})_([a-z0-9][a-z0-9_-]*)$`)

// reRetiredMigrationName matches the pre-NIM-735 flat form — the step file
// `<NNN>_to_<MMM>.yml` and the separate tests directory `<NNN>_to_<MMM>/` that sat
// beside it. Recognised on purpose rather than skipped as unknown: a repository
// halfway through the move has a ladder the engine reads as shorter than its author
// thinks, and "unknown directory" would not say why.
var reRetiredMigrationName = regexp.MustCompile(`^(\d{3})_to_(\d{3})(\.yml)?$`)

// reStepNumberPrefix matches anything that opens like a step number. Used only to
// decide whether a NON-directory entry under `migrations/` is a mistake worth
// reporting or just a neighbouring file.
var reStepNumberPrefix = regexp.MustCompile(`^\d{3}_`)

// MigrationStep is one rung of the ladder.
//
// Version is the version the step leads to; the source version is Version-1 and is
// never written down — the ladder is forward-only and goes by one. Dir is the
// directory name as it stands on disk (`<NNN>_<slug>`), Path is the step document
// relative to the service root.
type MigrationStep struct {
	Version int
	Dir     string
	Path    string
}

// MigrationLadder is a service's `migrations/` directory, read.
//
// Steps are sorted by Version ascending. A ladder that [ValidateMigrationLadder]
// reports no error on is gapless from 2 to [MigrationLadder.Version].
type MigrationLadder struct {
	Steps []MigrationStep
}

// Version is the service's state-schema version: the top of the ladder, or
// [BaseStateSchemaVersion] when there are no steps.
//
// This is the whole of the derivation — there is no stored number to disagree with
// it ([ADR-007] amendment 2026-09-01: `state_schema_version:` left the manifest).
func (l MigrationLadder) Version() int {
	if len(l.Steps) == 0 {
		return BaseStateSchemaVersion
	}
	return l.Steps[len(l.Steps)-1].Version
}

// Step returns the step leading to version, if the ladder has one.
func (l MigrationLadder) Step(version int) (MigrationStep, bool) {
	for i := range l.Steps {
		if l.Steps[i].Version == version {
			return l.Steps[i], true
		}
	}
	return MigrationStep{}, false
}

// ScanMigrationLadder reads `<serviceRoot>/migrations/` and returns the ladder it
// states, together with everything wrong with it.
//
// The two halves are deliberately independent: the ladder is returned even when the
// diagnostics carry errors, because the engine must answer "what version is this
// snapshot" for a repository an operator has already broken, and the honest answer
// is "the top of what is actually there". The strict reading belongs to `soul-lint`
// ([ValidateMigrationLadder]), which runs before the repository is pushed.
//
// A missing `migrations/` is not an error: a service that has never changed its
// state schema is at version 1 and has nothing to say about it.
func ScanMigrationLadder(serviceRoot string) (MigrationLadder, []diag.Diagnostic) {
	migRoot := filepath.Join(serviceRoot, MigrationsDirName)
	entries, err := os.ReadDir(migRoot)

	var (
		ladder MigrationLadder
		out    []diag.Diagnostic
		byVer  = make(map[int]string, len(entries))
	)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A missing `migrations/` is the legitimate "this service never changed its
		// state schema" and returns silently below. Anything else is the WHOLE ladder
		// disappearing, not one step failing to read: `migrations` committed as a
		// regular file (ENOTDIR), or a directory this process cannot list. Silence
		// here would answer version 1 for a service that is on 15, and every
		// incarnation above 1 would then read its own target as a downgrade — with
		// the linter green, because the linter reads through this same call.
		//
		// Reported and then CONTINUED rather than returned: os.ReadDir hands back the
		// entries it managed to read alongside the error, and this function's contract
		// is to answer with the top of what is actually there. On a total failure
		// `entries` is nil and the loop below does nothing, which is the same answer
		// an early return would have given.
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			File: migRoot, Code: "migration_ladder_unreadable",
			Message: fmt.Sprintf("%s/ cannot be listed: %v", MigrationsDirName, err),
			Hint:    fmt.Sprintf("%s must be a directory of step directories; the state-schema version is read from it, so an unreadable ladder reads as version %d", MigrationsDirName, BaseStateSchemaVersion),
		})
	}
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(migRoot, name)

		if m := reRetiredMigrationName.FindStringSubmatch(name); m != nil {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: full, Code: "migration_layout_retired",
				Message: fmt.Sprintf("%s is the retired flat step layout; a step is a directory %s/<NNN>_<slug>/ holding %s and its tests/",
					name, MigrationsDirName, MigrationStepFile),
				Hint: fmt.Sprintf("git mv %s %s/%s_<slug>/%s (the number names the version the step LEADS TO, so %s_to_%s becomes %s_<slug>)",
					name, MigrationsDirName, m[2], MigrationStepFile, m[1], m[2], m[2]),
			})
			continue
		}
		if !e.IsDir() {
			// A file named like a step is the one non-directory worth a word. An
			// author half-applying the new convention writes
			// `migrations/015_system_acl_users.yml` — new slug, old flatness — and
			// the retired-form regex above does not match it, so without this the
			// step would vanish, the version would drop by one, and the linter would
			// say OK.
			//
			// `DirEntry.IsDir()` is also false for a SYMLINK (ReadDir does not follow
			// one), so a symlinked step directory lands here too. Reporting it is the
			// right answer even though securejoin would happily resolve a link that
			// stays inside the snapshot: the LADDER SCAN is what drops the rung, so
			// the version moves whether or not the target is readable, and a silent
			// version change is the thing this whole check exists to prevent.
			//
			// Everything not named like a step — `schema.lock`, a README — is not a
			// step and never was, and stays silent. A `002_x.yml.orig` or `002_x.yml~`
			// does trip it, and that is wanted: a merge leftover or an editor backup
			// sitting in a ladder is worth one line of noise.
			if reStepNumberPrefix.MatchString(name) {
				out = append(out, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
					File: full, Code: "migration_step_name_invalid",
					Message: fmt.Sprintf("%s/%s is named like a step but is not a directory: a step is %s/<NNN>_<slug>/ holding %s and its tests/",
						MigrationsDirName, name, MigrationsDirName, MigrationStepFile),
					Hint: "the engine reads the ladder by directory name, so a step that is not a directory silently does not exist — and the version drops with it",
				})
			}
			continue
		}

		m := reMigrationStepDir.FindStringSubmatch(name)
		if m == nil {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: full, Code: "migration_step_name_invalid",
				Message: fmt.Sprintf("%s/%s is not a step: a step directory is named <NNN>_<slug> — three digits, an underscore, then a lowercase slug",
					MigrationsDirName, name),
				Hint: "the engine reads the ladder by directory name, so a directory it does not recognise is a step that silently does not exist",
			})
			continue
		}

		ver, _ := strconv.Atoi(m[1])
		if ver <= BaseStateSchemaVersion {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: full, Code: "migration_step_number_invalid",
				Message: fmt.Sprintf("%s/%s leads to version %d, which no step can: version %d is the empty ladder",
					MigrationsDirName, name, ver, BaseStateSchemaVersion),
				Hint: fmt.Sprintf("the number names the version the step LEADS TO, so the first step is 00%d_<slug>", BaseStateSchemaVersion+1),
			})
			continue
		}
		if prev, dup := byVer[ver]; dup {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: full, Code: "migration_step_duplicate",
				Message: fmt.Sprintf("%s/%s and %s/%s both lead to version %d", MigrationsDirName, prev, MigrationsDirName, name, ver),
				Hint:    "one version, one step — renumber the later one onto the top of the ladder",
			})
			continue
		}
		byVer[ver] = name

		ladder.Steps = append(ladder.Steps, MigrationStep{
			Version: ver,
			Dir:     name,
			Path:    path.Join(MigrationsDirName, name, MigrationStepFile),
		})
	}

	sort.Slice(ladder.Steps, func(i, j int) bool { return ladder.Steps[i].Version < ladder.Steps[j].Version })
	return ladder, out
}

// ValidateMigrationLadder is the strict reading of the ladder: the layout checks
// [ScanMigrationLadder] makes, plus continuity, plus each step's own document.
//
// Continuity is checked HERE and not by a script in the service repository. The
// number lives in exactly one place now, so nothing is left to disagree with itself
// — but a gap is still reachable by deleting a directory, and under a derived
// version a gap is worse than it used to be: everything below it becomes
// unreachable while the top of the ladder, and so the service's version, does not
// move at all.
func ValidateMigrationLadder(serviceRoot string) []diag.Diagnostic {
	ladder, out := ScanMigrationLadder(serviceRoot)
	out = append(out, ladderContinuityDiags(serviceRoot, ladder)...)

	for _, s := range ladder.Steps {
		stepPath := filepath.Join(serviceRoot, filepath.FromSlash(s.Path))
		data, err := os.ReadFile(stepPath)
		if err != nil {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: stepPath, Code: "migration_step_main_missing",
				Message: fmt.Sprintf("%s/%s has no %s", MigrationsDirName, s.Dir, MigrationStepFile),
				Hint:    fmt.Sprintf("a step is a directory holding %s and its tests/ — the document itself is not optional", MigrationStepFile),
			})
			continue
		}
		out = append(out, ValidateMigrationStepFile(stepPath, data)...)
	}
	return out
}

// ladderContinuityDiags reports the ladder's gaps: gapless from the floor up, one
// diagnostic per missing version rather than one for the whole ladder, because two
// deleted directories are two things to put back.
//
// Split out from [ValidateMigrationLadder] so that [ValidateSchemaLock] can ask
// whether the ladder is sound before judging a stamped version against its top,
// without re-reading every step document to find out. A second copy of the loop
// would be a second definition of "gapless", and the two would eventually disagree
// about which ladder the stamp is allowed to be compared with.
func ladderContinuityDiags(serviceRoot string, ladder MigrationLadder) []diag.Diagnostic {
	var out []diag.Diagnostic
	want := BaseStateSchemaVersion + 1
	for _, s := range ladder.Steps {
		for ; want < s.Version; want++ {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: filepath.Join(serviceRoot, MigrationsDirName), Code: "migration_chain_broken",
				Message: fmt.Sprintf("no step leads to version %d, but %s/%s leads to %d — every version from %d to the top must have one",
					want, MigrationsDirName, s.Dir, s.Version, BaseStateSchemaVersion+1),
				Hint: fmt.Sprintf("add %s/%03d_<slug>/%s, or renumber the steps above the gap down onto it — an incarnation below the gap can never be upgraded past it",
					MigrationsDirName, want, MigrationStepFile),
			})
		}
		want = s.Version + 1
	}
	return out
}

// retiredStepKeys are the header keys a step no longer carries. A step states what
// it does; where it stands is stated by its directory, once ([ADR-019] amendment
// 2026-09-01). Left as a silent no-op they would be the fifth and sixth hand-written
// copies of one integer, free to disagree with the directory holding them.
var retiredStepKeys = []string{"from_version", "to_version"}

// ValidateMigrationStepFile checks one step's `main.yml` for the retired position
// header. data is the file's bytes; path is only used to address the diagnostic.
//
// Unparseable YAML is not reported here — the step's own parser
// (`keeper/internal/statemigrate`) says that better, with the operation the parse
// broke on.
func ValidateMigrationStepFile(path string, data []byte) []diag.Diagnostic {
	file, err := parser.ParseBytes(stripBOM(data), 0)
	if err != nil || len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil
	}

	var out []diag.Diagnostic
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		for _, retired := range retiredStepKeys {
			if tok.Value != retired {
				continue
			}
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: path, Code: "migration_version_key",
				Message: fmt.Sprintf(`unknown field %q`, retired),
				Hint: fmt.Sprintf("a step states what it does, not where it stands: the version it leads to is the <NNN> of its %s/<NNN>_<slug>/ directory, and the version it comes from is the one below",
					MigrationsDirName),
				YAMLPath: "$." + retired,
			}))
		}
	}
	return out
}
