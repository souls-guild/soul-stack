// Package plugininit реализует подкоманду `soul-lint plugin-init`.
//
// Scaffold нового SoulModule-плагина по спецификации [ADR-016 amendment
// 2026-05-27] и [ADR-020]. Все артефакты (manifest.yaml / main.go / handler.go
// / Makefile / README.md / tests/) embedded через go:embed из
// `template/`-каталога этого пакета — единого источника правды для CLI и
// (через периодическую копию) для git-template-репо
// `github.com/co-cy/soul-stack-plugins`.
//
// Контракт: оба пути (CLI и `git clone soul-stack-plugins/soul-mod-template`)
// должны выдавать идентичное дерево после substitution placeholder-ов; sync
// между двумя копиями — manual / scripts/sync-template.sh.
//
// Exit-codes симметричны остальным `soul-lint`-подкомандам:
//
//	0 — ok (scaffold создан)
//	2 — usage / I/O fatal
package plugininit

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// Embedded template-tree. `all:` обязателен — иначе go:embed по умолчанию
// игнорирует имена, начинающиеся с `.` (например, `.gitignore.tmpl`).
//
//go:embed all:template
var templateFS embed.FS

// Текущая версия plugin-протокола, проставляется в manifest.yaml.
// Должна совпадать с shared/plugin.SupportedProtocolVersions[0] и
// pluginhost.SupportedProtocolVersions. Дублирование осознанное:
// soul-lint не импортирует pluginhost; shared/plugin тянет goccy-yaml-парсер
// (тяжёлая зависимость, лишняя для scaffold-команды).
const currentProtocolVersion = 1

// Exit-codes — стабильный CLI-контракт (см. soul-lint/cmd/soul-lint/main.go).
const (
	ExitOK      = 0
	ExitIOFatal = 2
)

// Options — параметры одного запуска `plugin-init`.
type Options struct {
	// Namespace — коллекция плагина, например `official` или `community`.
	// Валидируется regex'ом плагин-манифеста (shared/plugin → namespace).
	Namespace string
	// Name — имя плагина внутри коллекции, например `postgres-user`.
	Name string
	// Out — выходная директория. Пустая = `./soul-mod-<namespace>-<name>` в CWD.
	Out string
	// Description — заполняется в README.md / manifest. Пустая → placeholder.
	Description string
	// Author — заполняется в README.md / manifest. Пустая → placeholder.
	Author string
	// Force — перезаписать out-dir, если она существует и не пуста.
	Force bool
}

// Run выполняет scaffold. Печатает диагностики в out / errOut, возвращает
// exit-code (0/2).
func Run(opts Options, out io.Writer, errOut io.Writer) int {
	vars, err := buildVars(opts)
	if err != nil {
		fmt.Fprintf(errOut, "soul-lint plugin-init: %v\n", err)
		return ExitIOFatal
	}

	outDir := opts.Out
	if outDir == "" {
		outDir = "./soul-mod-" + vars.Namespace + "-" + vars.Name
	}

	if err := prepareOutDir(outDir, opts.Force); err != nil {
		fmt.Fprintf(errOut, "soul-lint plugin-init: %v\n", err)
		return ExitIOFatal
	}

	if err := renderTree(outDir, vars); err != nil {
		fmt.Fprintf(errOut, "soul-lint plugin-init: %v\n", err)
		return ExitIOFatal
	}

	printNextSteps(out, outDir, vars)
	return ExitOK
}

// TemplateVars — данные, доступные внутри .tmpl-файлов как `{{.Field}}`.
type TemplateVars struct {
	// Namespace — `official` / `community` / автор-собственная коллекция.
	Namespace string
	// Name — kebab-case, `postgres-user`.
	Name string
	// NameSnake — snake_case, `postgres_user` (для Go-identifier-ов в шаблонах).
	// NB: имя поля сохраняем (передаётся в .tmpl), но реальное использование
	// сведено к NamePackage (см. ниже): snake_case Go-пакет = тот же `postgres_user`.
	NameSnake string
	// NamePascal — PascalCase, `PostgresUser` (для Go-type-ов).
	NamePascal string
	// NamePackage — то же, что NameSnake; отдельное имя, чтобы шаблоны явно
	// различали роль (имя Go-пакета внутри internal/ vs «snake-форма для других целей»).
	NamePackage string
	// BinaryName — `soul-mod-<namespace>-<name>`, имя итогового бинаря.
	BinaryName string
	// GoModulePath — module path в go.mod нового плагина. По умолчанию
	// `github.com/<author>/soul-mod-<namespace>-<name>`; placeholder для
	// пустого автора — `github.com/EXAMPLE/...`.
	GoModulePath string
	// AuthorName — заполняется в README.md / manifest. Default — `EXAMPLE`.
	AuthorName string
	// Description — пользовательское описание.
	Description string
	// ProtocolVersion — plugin protocol_version, см. shared/plugin.
	ProtocolVersion int
}

// ParseSpec парсит spec вида `<namespace>/<name>` (например `official/postgres-user`).
// Возвращает namespace и name отдельно. Спецификация — обязательное позиционное
// поле первой команды (`plugin-init official/postgres-user`).
func ParseSpec(spec string) (namespace, name string, err error) {
	if spec == "" {
		return "", "", errors.New("spec is empty (expected <namespace>/<name>)")
	}
	parts := strings.Split(spec, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("spec %q must be in form <namespace>/<name>", spec)
	}
	ns := strings.TrimSpace(parts[0])
	nm := strings.TrimSpace(parts[1])
	if ns == "" || nm == "" {
		return "", "", fmt.Errorf("spec %q has empty namespace or name", spec)
	}
	return ns, nm, nil
}

// Regex-ы повторяют shared/plugin.reNamespace / reName (kebab-case,
// 1..63 chars, начинается с буквы). Дублирование — чтобы plugininit не тянул
// goccy-yaml-парсер из shared/plugin ради двух regex-ов.
var (
	reNamespace = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reName      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

func buildVars(opts Options) (TemplateVars, error) {
	ns := strings.TrimSpace(opts.Namespace)
	nm := strings.TrimSpace(opts.Name)
	if !reNamespace.MatchString(ns) {
		return TemplateVars{}, fmt.Errorf("namespace %q must match %s (kebab-case, 1..63 chars, lowercase, starts with letter)", ns, reNamespace)
	}
	if !reName.MatchString(nm) {
		return TemplateVars{}, fmt.Errorf("name %q must match %s (kebab-case, 1..63 chars, lowercase, starts with letter)", nm, reName)
	}

	author := strings.TrimSpace(opts.Author)
	if author == "" {
		author = "EXAMPLE"
	}
	description := strings.TrimSpace(opts.Description)
	if description == "" {
		description = "TODO: краткое описание плагина (1-2 параграфа)."
	}

	// `<author>` в module-path — kebab-case (как GitHub username); strip пробелы,
	// для имени с пробелами/нелатиницей оставляем EXAMPLE (не угадываем).
	authorSlug := author
	if !reName.MatchString(strings.ToLower(authorSlug)) {
		authorSlug = "EXAMPLE"
	} else {
		authorSlug = strings.ToLower(authorSlug)
	}

	snake := kebabToSnake(nm)
	pascal := kebabToPascal(nm)

	return TemplateVars{
		Namespace:       ns,
		Name:            nm,
		NameSnake:       snake,
		NamePascal:      pascal,
		NamePackage:     snake,
		BinaryName:      "soul-mod-" + ns + "-" + nm,
		GoModulePath:    "github.com/" + authorSlug + "/soul-mod-" + ns + "-" + nm,
		AuthorName:      author,
		Description:     description,
		ProtocolVersion: currentProtocolVersion,
	}, nil
}

func kebabToSnake(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

func kebabToPascal(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		// ASCII upper: name проходит regex `^[a-z][a-z0-9-]{0,62}$`, поэтому
		// все символы — [a-z0-9-], unicode-кейс не релевантен.
		b := []byte(p)
		b[0] = b[0] - 'a' + 'A'
		parts[i] = string(b)
	}
	return strings.Join(parts, "")
}

func prepareOutDir(dir string, force bool) error {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create out-dir %s: %w", dir, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat out-dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("out-dir %s exists and is not a directory", dir)
	}
	// Каталог существует — проверяем, что он пуст; иначе --force.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read out-dir %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("out-dir %s is not empty; pass --force to overwrite", dir)
	}
	return nil
}

func renderTree(outDir string, vars TemplateVars) error {
	root := "template"
	return fs.WalkDir(templateFS, root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel := strings.TrimPrefix(path, root+"/")
		// Подмена директории `cmd/soul-mod-EXAMPLE/` → `cmd/<BinaryName>/`.
		rel = strings.ReplaceAll(rel, "cmd/soul-mod-EXAMPLE", "cmd/"+vars.BinaryName)
		// Подмена директории `internal/PACKAGE/` → `internal/<NamePackage>/`
		// (snake_case Go-пакет, на который ссылается cmd/<BinaryName>/main.go).
		// Литерал в имени директории — потому что text/template работает на
		// содержимом файла, не на пути; альтернатива через filepath.Walk
		// + per-segment Execute() — избыточна для одной фиксированной подмены.
		rel = strings.ReplaceAll(rel, "internal/PACKAGE", "internal/"+vars.NamePackage)
		// Strip `.tmpl`-суффикс на выходе.
		rel = strings.TrimSuffix(rel, ".tmpl")

		dst := filepath.Join(outDir, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dst, err)
			}
			return nil
		}

		data, err := templateFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}

		// `.tmpl` идёт через text/template; не-`.tmpl` (например, бинарные
		// артефакты будущих расширений) копируется as-is. Сейчас всё в дереве —
		// `.tmpl`, но проверка дешёвая и страхует от регрессии.
		if strings.HasSuffix(path, ".tmpl") {
			rendered, rerr := renderBytes(path, data, vars)
			if rerr != nil {
				return rerr
			}
			data = rendered
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir-parent %s: %w", dst, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		return nil
	})
}

func renderBytes(name string, data []byte, vars TemplateVars) ([]byte, error) {
	tpl, err := template.New(name).Option("missingkey=error").Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, vars); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", name, err)
	}
	return []byte(sb.String()), nil
}

func printNextSteps(w io.Writer, outDir string, vars TemplateVars) {
	fmt.Fprintf(w, "scaffold создан: %s\n", outDir)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "следующие шаги:")
	fmt.Fprintf(w, "  cd %s\n", outDir)
	fmt.Fprintf(w, "  # 1. отредактируйте manifest.yaml::spec.states.<state>.input под ваш ресурс\n")
	fmt.Fprintf(w, "  # 2. реализуйте Apply в internal/%s/handler.go\n", vars.NamePackage)
	fmt.Fprintln(w, "  # 3. (опционально) реализуйте Plan + PlanReadSafe для drift-detect (ADR-031)")
	fmt.Fprintln(w, "  # 4. подгоните L0/L1-тесты под реальную имплементацию")
	fmt.Fprintln(w, "  make check    # gofmt + vet + test + build")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "binary: %s/bin/%s\n", outDir, vars.BinaryName)
}
