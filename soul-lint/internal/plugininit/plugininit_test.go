package plugininit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul-lint/internal/validate"
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
		Author:      "co-cy",
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
	check("GoModulePath", v.GoModulePath, "github.com/co-cy/soul-mod-official-postgres-user")
	check("AuthorName", v.AuthorName, "co-cy")
	check("Description", v.Description, "Manages PostgreSQL roles")
	if v.ProtocolVersion != currentProtocolVersion {
		t.Errorf("ProtocolVersion: got %d, want %d", v.ProtocolVersion, currentProtocolVersion)
	}
}

func TestBuildVars_DefaultsAndInvalidAuthor(t *testing.T) {
	// Пустой description / author → placeholder. Невалидный (с пробелом)
	// author → EXAMPLE, но AuthorName сохраняется как есть в README.
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
		Author:      "co-cy",
	}, &out, &errOut)

	if code != ExitOK {
		t.Fatalf("Run: code=%d, stderr=%s", code, errOut.String())
	}

	// Ожидаемые файлы должны существовать и быть отрендерены.
	want := []string{
		"manifest.yaml",
		"go.mod",
		"Makefile",
		"README.md",
		".gitignore",
		"cmd/soul-mod-official-postgres-user/main.go",
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

	// Placeholder-substitution: ни одного нерендерёного `{{.Foo}}` в выводе.
	if err := assertNoUnrenderedPlaceholders(outDir); err != nil {
		t.Errorf("placeholders: %v", err)
	}

	// Подстановки в характерных файлах.
	mustContain(t, filepath.Join(outDir, "manifest.yaml"),
		"namespace: official",
		"name: postgres-user",
		"protocol_version: 1",
	)
	mustContain(t, filepath.Join(outDir, "go.mod"),
		"module github.com/co-cy/soul-mod-official-postgres-user",
	)
	mustContain(t, filepath.Join(outDir, "README.md"),
		"# soul-mod-official-postgres-user",
		"Manages PostgreSQL roles",
		"official.postgres-user",
	)
	// internal/<package>/handler.go должен ссылаться на пакет postgres_user
	// и компилироваться: cmd/<binary>/main.go импортирует .../internal/postgres_user.
	mustContain(t, filepath.Join(outDir, "internal", "postgres_user", "handler.go"),
		"package postgres_user",
		"PostgresUserModule",
	)
	mustContain(t, filepath.Join(outDir, "cmd", "soul-mod-official-postgres-user", "main.go"),
		"\"github.com/co-cy/soul-mod-official-postgres-user/internal/postgres_user\"",
		"&postgres_user.PostgresUserModule{}",
	)
}

// TestRun_GeneratedManifestPassesValidate — sanity-check: manifest, который
// scaffold кладёт «из коробки», обязан проходить `validate-manifest` без
// errors. Покрывает регрессии вроде secret-без-pattern (semantic rule
// input_secret_without_vault_pattern), drift между шаблоном и схемой
// shared/plugin и т.п.
func TestRun_GeneratedManifestPassesValidate(t *testing.T) {
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "soul-mod-official-postgres-user")

	code := Run(Options{
		Namespace:   "official",
		Name:        "postgres-user",
		Out:         outDir,
		Description: "Manages PostgreSQL roles",
		Author:      "co-cy",
	}, io.Discard, io.Discard)
	if code != ExitOK {
		t.Fatalf("Run: code=%d", code)
	}

	manifest := filepath.Join(outDir, "manifest.yaml")
	var out, errOut bytes.Buffer
	vcode := validate.Run(validate.Options{
		Path: manifest,
		Kind: validate.KindManifest,
	}, &out, &errOut)
	if vcode != 0 {
		t.Fatalf("validate-manifest failed (code=%d):\nstdout:\n%s\nstderr:\n%s",
			vcode, out.String(), errOut.String())
	}
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
	// Run с пустым Options.Out пишет в CWD. Прыгаем в TempDir, чтобы не
	// замусорить рабочее дерево.
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

	if _, err := os.Stat(filepath.Join(tmp, "soul-mod-official-nginx-vhost", "manifest.yaml")); err != nil {
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

// assertNoUnrenderedPlaceholders — после рендера в дереве не должно остаться
// маркеров `{{.X}}` (text/template). Любой такой маркер в *.go-файле сломает
// компиляцию плагина, в README/manifest введёт пользователя в заблуждение.
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
