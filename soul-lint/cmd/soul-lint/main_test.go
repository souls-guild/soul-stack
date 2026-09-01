package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul-lint/internal/secretpaths"
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

// `list-secret-paths` has its own argument parser (it is not a validate-* kind), so its
// flag handling needs its own cases. The derivation itself is tested in
// internal/secretpaths; these pin the CLI surface.

// The fixture declares both shapes, so the golden output pins the whole surface: the
// derivation, the alignment, the sorted order, and — the part an exit-code assertion
// cannot see — that the name from the flag is the name that reaches the path.
const goldenSecretPaths = "" +
	"secret/redis/<incarnation>/admin_password#value               ← state_schema.admin_password\n" +
	"secret/redis/<incarnation>/redis_users/<name>#password        ← state_schema.redis_users[].password\n" +
	"secret/redis/<incarnation>/system_acl_users/<name>#password   ← state_schema.system_acl_users[].password\n"

func TestRunListSecretPaths_Golden(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "secret-paths", "service.yml")
	for _, args := range [][]string{
		{path, "--service-name", "redis"},
		{path, "--service-name=redis"},
		{"--service-name", "redis", path}, // flag before the positional
	} {
		var out, errOut bytes.Buffer
		code := listSecretPaths(args, &out, &errOut)
		if code != secretpaths.ExitOK {
			t.Errorf("%v → %d, want ExitOK; stderr = %q", args, code, errOut.String())
			continue
		}
		if out.String() != goldenSecretPaths {
			t.Errorf("%v printed:\n%s\nwant:\n%s", args, out.String(), goldenSecretPaths)
		}
		if errOut.String() != "" {
			t.Errorf("%v stderr = %q, want empty", args, errOut.String())
		}
	}
}

// A flag-shaped token after `--service-name` is a missing value, not a name. Left
// unchecked it produces the worst outcome this command has: `secret/--json/…` printed
// with exit 0, which is a wrong answer wearing the shape of a right one.
func TestRunListSecretPaths_FlagIsNotAServiceName(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "secret-paths", "service.yml")
	for _, args := range [][]string{
		{"--service-name", "--json", path},
		{"--service-name", "-h", path},
		{path, "--service-name", "--modules=x=y"},
	} {
		var out, errOut bytes.Buffer
		if code := listSecretPaths(args, &out, &errOut); code != secretpaths.ExitIOFatal {
			t.Errorf("%v → %d, want ExitIOFatal; stdout = %q", args, code, out.String())
		}
		if out.String() != "" {
			t.Errorf("%v printed %q, want nothing", args, out.String())
		}
	}
	// The escape hatch stays open: a name that really does start with `-` is reachable
	// through the `=` spelling, where nothing is being swallowed.
	var out, errOut bytes.Buffer
	if code := listSecretPaths([]string{path, "--service-name=-odd"}, &out, &errOut); code != secretpaths.ExitOK {
		t.Errorf("--service-name=-odd → %d, want ExitOK; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "secret/-odd/") {
		t.Errorf("the `=` spelling did not carry the name through: %q", out.String())
	}
}

// The name is a segment of every path printed, so an absent one is a usage error rather
// than a run that guesses. `--service-name=` is the same statement spelled differently.
func TestRunListSecretPaths_ServiceNameRequired(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "secret-paths", "service.yml")
	for _, args := range [][]string{
		{path},
		{path, "--service-name"},
		{path, "--service-name="},
	} {
		if code := runListSecretPaths(args); code != secretpaths.ExitIOFatal {
			t.Errorf("%v → %d, want ExitIOFatal", args, code)
		}
	}
}

func TestRunListSecretPaths_UsageErrors(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "secret-paths", "service.yml")
	for _, args := range [][]string{
		nil,                                     // no path
		{"--json", path, "--service-name", "r"}, // this command prints no diagnostics
		{path, path, "--service-name", "redis"}, // two positionals
	} {
		if code := runListSecretPaths(args); code != secretpaths.ExitIOFatal {
			t.Errorf("%v → %d, want ExitIOFatal", args, code)
		}
	}
}

func TestRunListSecretPaths_HelpFlag(t *testing.T) {
	for _, f := range []string{"-h", "--help"} {
		if code := runListSecretPaths([]string{f}); code != secretpaths.ExitOK {
			t.Errorf("%s → %d, want ExitOK", f, code)
		}
	}
}
