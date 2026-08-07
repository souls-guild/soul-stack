package plugininit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSpec_OK(t *testing.T) {
	ns, nm, err := ParseSpec("official/postgres-user")
	if err != nil {
		t.Fatalf("ParseSpec: unexpected error: %v", err)
	}
	if ns != "official" || nm != "postgres-user" {
		t.Fatalf("ParseSpec: got %q/%q, want official/postgres-user", ns, nm)
	}
}

func TestParseSpec_Errors(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"empty", ""},
		{"no-slash", "official-postgres-user"},
		{"too-many-slashes", "official/postgres/user"},
		{"empty-namespace", "/postgres-user"},
		{"empty-name", "official/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseSpec(tc.spec); err == nil {
				t.Fatalf("ParseSpec(%q): expected error, got nil", tc.spec)
			}
		})
	}
}

func TestBuildVars_HappyPath(t *testing.T) {
	v, err := buildVars(Options{
		Namespace:   "official",
		Name:        "postgres-user",
		Description: "Manages PostgreSQL roles",
		Author:      "souls-guild",
	})
	if err != nil {
		t.Fatalf("buildVars: %v", err)
	}
	check := func(field, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", field, got, want)
		}
	}
	check("Namespace", v.Namespace, "official")
	check("Name", v.Name, "postgres-user")
	check("NameSnake", v.NameSnake, "postgres_user")
	check("NamePackage", v.NamePackage, "postgres_user")
	check("NamePascal", v.NamePascal, "PostgresUser")
	check("BinaryName", v.BinaryName, "soul-mod-official-postgres-user")
	check("GoModulePath", v.GoModulePath, "github.com/souls-guild/soul-mod-official-postgres-user")
	check("AuthorName", v.AuthorName, "souls-guild")
	check("Description", v.Description, "Manages PostgreSQL roles")
}

func TestBuildVars_DefaultsAndInvalidAuthor(t *testing.T) {
	// Empty description/author fall back to a placeholder. An invalid author
	// (with a space) falls back to EXAMPLE, but AuthorName is still kept
	// as-is in README.
	v, err := buildVars(Options{
		Namespace: "official",
		Name:      "redis-acl",
		Author:    "Some Author",
	})
	if err != nil {
		t.Fatalf("buildVars: %v", err)
	}
	if v.Description == "" {
		t.Errorf("Description: empty, want placeholder")
	}
	if v.AuthorName != "Some Author" {
		t.Errorf("AuthorName: got %q, want preserved literal", v.AuthorName)
	}
	if !strings.HasPrefix(v.GoModulePath, "github.com/EXAMPLE/") {
		t.Errorf("GoModulePath: got %q, want github.com/EXAMPLE/...", v.GoModulePath)
	}
}

func TestBuildVars_RejectInvalidNames(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		nm        string
	}{
		{"namespace-uppercase", "Official", "postgres-user"},
		{"namespace-space", "of ficial", "postgres-user"},
		{"namespace-empty", "", "postgres-user"},
		{"name-uppercase", "official", "PostgresUser"},
		{"name-space", "official", "postgres user"},
		{"name-empty", "official", ""},
		{"name-starts-with-digit", "official", "1pg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildVars(Options{Namespace: tc.namespace, Name: tc.nm}); err == nil {
				t.Fatalf("buildVars: expected error for ns=%q name=%q", tc.namespace, tc.nm)
			}
		})
	}
}

func TestRun_ScaffoldsExpectedTree(t *testing.T) {
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "soul-mod-official-postgres-user")

	var out, errOut bytes.Buffer
	code := Run(Options{
		Namespace:   "official",
		Name:        "postgres-user",
		Out:         outDir,
		Description: "Manages PostgreSQL roles",
		Author:      "souls-guild",
	}, &out, &errOut)

	if code != ExitOK {
		t.Fatalf("Run: code=%d, stderr=%s", code, errOut.String())
	}

	// Expected files must exist and be rendered.
	want := []string{
		"go.mod",
		"Makefile",
		"README.md",
		".gitignore",
		"cmd/soul-mod-official-postgres-user/main.go",
		"cmd/soul-mod-official-postgres-user/bundle_test.go",
		"internal/postgres_user/handler.go",
		"internal/postgres_user/handler_test.go",
		"tests/L0_test.go",
	}
	for _, rel := range want {
		p := filepath.Join(outDir, rel)
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected file missing: %s (%v)", rel, err)
			continue
		}
		if st.IsDir() {
			t.Errorf("expected file but got dir: %s", rel)
		}
	}

	// Placeholder substitution: no unrendered `{{.Foo}}` left in the output.
	if err := assertNoUnrenderedPlaceholders(outDir); err != nil {
		t.Errorf("placeholders: %v", err)
	}

	// The manifest form is gone: a scaffold that still shipped one would be
	// shipping a file nothing reads, and the first thing an author would do is
	// edit it and wonder why nothing changed.
	if _, err := os.Stat(filepath.Join(outDir, "manifest.yaml")); err == nil {
		t.Error("the scaffold still writes manifest.yaml; the schema is generated from Go now")
	}

	// Substitutions in representative files.
	mustContain(t, filepath.Join(outDir, "go.mod"),
		"module github.com/souls-guild/soul-mod-official-postgres-user",
	)
	mustContain(t, filepath.Join(outDir, "README.md"),
		"# soul-mod-official-postgres-user",
		"Manages PostgreSQL roles",
		"official.postgres-user",
	)
	// internal/<package>/handler.go must reference the postgres_user package
	// and compile: cmd/<binary>/main.go imports .../internal/postgres_user.
	mustContain(t, filepath.Join(outDir, "internal", "postgres_user", "handler.go"),
		"package postgres_user",
		"PostgresUserModule",
	)
	mustContain(t, filepath.Join(outDir, "cmd", "soul-mod-official-postgres-user", "main.go"),
		"\"github.com/souls-guild/soul-mod-official-postgres-user/internal/postgres_user\"",
		"module.ServeBundle(bundle)",
		"postgres_user.Module,",
	)
	// The declaration itself: a module.Def next to the code, carrying the
	// module's own name (address level 2) and NOT a namespace — level 1 is the
	// operator's registration alias and cannot live in the artifact.
	mustContain(t, filepath.Join(outDir, "internal", "postgres_user", "handler.go"),
		"var Module = module.Def{",
		"Name:        \"postgres-user\",",
		"Impl: &PostgresUserModule{},",
	)
	// stamp + verify are the build: an unstamped artifact carries no disclosure,
	// and verify is what keeps the stamped document equal to the code.
	mustContain(t, filepath.Join(outDir, "Makefile"),
		"$(SOUL_MOD) stamp $(BINARY)",
		"$(SOUL_MOD) verify $(BINARY)",
	)
}

// TestRun_GeneratedScaffoldCarriesItsOwnSchemaGate — the scaffold's declaration is
// Go now, so nothing in THIS repo can validate it without compiling the generated
// project. What the scaffold must therefore ship is the gate itself: a test in the
// generated tree that runs the SDK validator over the bundle, plus `soul-mod verify`
// in its build. Both are what the old `validate-manifest` check on the shipped
// manifest.yaml used to buy — a secret with no `^vault:.*` pattern, a bad state
// name, drift between the template and the schema rules — moved to where the
// declaration now lives.
func TestRun_GeneratedScaffoldCarriesItsOwnSchemaGate(t *testing.T) {
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "soul-mod-official-postgres-user")

	code := Run(Options{
		Namespace:   "official",
		Name:        "postgres-user",
		Out:         outDir,
		Description: "Manages PostgreSQL roles",
		Author:      "souls-guild",
	}, io.Discard, io.Discard)
	if code != ExitOK {
		t.Fatalf("Run: code=%d", code)
	}

	mustContain(t, filepath.Join(outDir, "cmd", "soul-mod-official-postgres-user", "bundle_test.go"),
		"bundle.Validate()",
	)
	mustContain(t, filepath.Join(outDir, "Makefile"), "verify:")
}

func TestRun_OutDirNotEmptyWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "existing")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	code := Run(Options{
		Namespace: "official",
		Name:      "postgres-user",
		Out:       outDir,
	}, io.Discard, io.Discard)

	if code != ExitIOFatal {
		t.Fatalf("non-empty out without --force: code=%d, want %d", code, ExitIOFatal)
	}
}

func TestRun_DefaultOutPath(t *testing.T) {
	// Run with an empty Options.Out writes to the CWD. Hop into a TempDir so
	// we don't litter the working tree.
	tmp := t.TempDir()
	prev, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	code := Run(Options{
		Namespace: "official",
		Name:      "nginx-vhost",
	}, io.Discard, io.Discard)
	if code != ExitOK {
		t.Fatalf("Run: code=%d", code)
	}

	if _, err := os.Stat(filepath.Join(tmp, "soul-mod-official-nginx-vhost", "go.mod")); err != nil {
		t.Fatalf("default out path: %v", err)
	}
}

func TestRun_ValidationErrorsExitIOFatal(t *testing.T) {
	code := Run(Options{Namespace: "Official", Name: "x"}, io.Discard, io.Discard)
	if code != ExitIOFatal {
		t.Fatalf("invalid namespace: code=%d, want %d", code, ExitIOFatal)
	}
}

func mustContain(t *testing.T, path string, needles ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, n := range needles {
		if !strings.Contains(string(data), n) {
			t.Errorf("%s: missing %q", path, n)
		}
	}
}

// assertNoUnrenderedPlaceholders — after rendering, no `{{.X}}` (text/
// template) markers should remain in the tree. Any such marker in a *.go
// file would break the plugin's build; in README/manifest it would mislead
// the user.
func assertNoUnrenderedPlaceholders(outDir string) error {
	var bad []string
	_ = filepath.Walk(outDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		s := string(data)
		if strings.Contains(s, "{{.") {
			bad = append(bad, path)
		}
		return nil
	})
	if len(bad) > 0 {
		return &placeholderErr{files: bad}
	}
	return nil
}

type placeholderErr struct{ files []string }

func (e *placeholderErr) Error() string {
	return "unrendered placeholders in: " + strings.Join(e.files, ", ")
}
