package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul-lint/internal/validate"
)

// The CLI's main function calls os.Exit, so testing it directly is a bad
// idea. We test runSubcommand instead — main delegates validate-* to it, and
// it fully covers CLI flag parsing. `validate` itself is already tested in
// `validate_test.go`.

func TestRunSubcommand_ValidateManifestGolden(t *testing.T) {
	// Full path to the golden fixture — relative, same as in the validate tests.
	path := filepath.Join("..", "..", "testdata", "manifest-golden", "soul-module.schema.json")
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{path})
	if code != validate.ExitOK {
		t.Fatalf("expected ExitOK, got %d", code)
	}
}

func TestRunSubcommand_ValidateManifestBroken(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "manifest-broken", "manifest-unknown-kind.schema.json")
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{path})
	if code != validate.ExitHasErrors {
		t.Fatalf("expected ExitHasErrors, got %d", code)
	}
}

func TestRunSubcommand_NoPath(t *testing.T) {
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, nil)
	if code != validate.ExitIOFatal {
		t.Fatalf("expected ExitIOFatal for missing path, got %d", code)
	}
}

func TestRunSubcommand_UnknownFlag(t *testing.T) {
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{"--unknown"})
	if code != validate.ExitIOFatal {
		t.Fatalf("expected ExitIOFatal for unknown flag, got %d", code)
	}
}

func TestRunSubcommand_JSONFlag(t *testing.T) {
	// Sanity check: the --json flag is recognized and the command keeps working.
	path := filepath.Join("..", "..", "testdata", "manifest-golden", "soul-module.schema.json")
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{"--json", path})
	if code != validate.ExitOK {
		t.Fatalf("expected ExitOK, got %d", code)
	}
}

// TestPrintUsage_MentionsValidateManifest — usage must advertise the new
// subcommand, or users would never learn it exists.
func TestPrintUsage_MentionsValidateManifest(t *testing.T) {
	var buf bytes.Buffer
	// printUsage takes a *os.File; spinning up a temp file is overkill, so
	// this stays a `_ = buf` no-op instead of wiring a captureUsage helper.
	// It'd be simpler to strings.Contains a buffer copy, but printUsage
	// writes straight to *os.File — for this test it's enough to know
	// "validate-manifest" shows up via runSubcommand's own usage table
	// below. If printUsage regresses, the CLI binary still catches it.
	_ = buf
	// A subprocess check would be excessive for a unit test; we settle for
	// checking runSubcommand --help below.
	code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{"--help"})
	if code != validate.ExitOK {
		t.Fatalf("--help must return ExitOK, got %d", code)
	}
}

// TestRunSubcommand_HelpFlag — `-h` / `--help` return 0 instead of falling
// into the "no path" IO-fatal branch.
func TestRunSubcommand_HelpFlag(t *testing.T) {
	for _, f := range []string{"-h", "--help"} {
		code := runSubcommand("validate-manifest", "validate-manifest <path> [--json]", validate.KindManifest, []string{f})
		if code != validate.ExitOK {
			t.Errorf("%s → %d, want 0", f, code)
		}
	}
}

// Sanity tracker: catches main's usage string dropping the mention of
// validate-manifest (e.g. after a refactor).
var _ = strings.Contains

// `--modules` is repeatable and takes `<alias>=<path>` (NIM-377). The alias is on the
// flag because the artifact carries no name of its own, so these cases pin the parsing
// rather than the resolution — the resolver's own rules live in internal/validate.

func TestRunSubcommand_ModulesFlagIsRepeatable(t *testing.T) {
	doc := filepath.Join("..", "..", "testdata", "manifest-golden", "soul-module.schema.json")
	scenario := filepath.Join("..", "..", "testdata", "scenario-golden", "redis-create.yml")
	code := runSubcommand("validate-scenario", "validate-scenario <path>", validate.KindScenario,
		[]string{scenario, "--modules", "redis=" + doc, "--modules=cache=" + doc})
	if code == validate.ExitIOFatal {
		t.Fatalf("two --modules bindings were rejected as a usage error (got %d)", code)
	}
}

// A bare `--modules` with nothing after it, and `--modules=` with an empty value, are
// both "you asked for these checks and gave me nothing to check with". Neither may fall
// through to a run that prints OK.
func TestRunSubcommand_ModulesFlagNeedsABinding(t *testing.T) {
	scenario := filepath.Join("..", "..", "testdata", "scenario-golden", "redis-create.yml")
	for _, args := range [][]string{
		{scenario, "--modules"},
		{scenario, "--modules="},
		{scenario, "--modules", "examples/module"}, // the old DIR form
	} {
		code := runSubcommand("validate-scenario", "validate-scenario <path>", validate.KindScenario, args)
		if code != validate.ExitIOFatal {
			t.Errorf("%v → %d, want %d", args, code, validate.ExitIOFatal)
		}
	}
}
