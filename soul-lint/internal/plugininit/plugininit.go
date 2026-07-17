// Package plugininit implements the `soul-lint plugin-init` subcommand.
//
// It scaffolds a new SoulModule plugin per [ADR-016 amendment 2026-05-27]
// and [ADR-020]. All artifacts (manifest.yaml / main.go / handler.go /
// Makefile / README.md / tests/) are embedded via go:embed from this
// package's `template/` directory — the single source of truth for the CLI
// and, via a periodic copy, for the git template repo
// `github.com/co-cy/soul-stack-plugins`.
//
// Contract: both paths (the CLI and `git clone
// soul-stack-plugins/soul-mod-template`) must produce an identical tree
// after placeholder substitution; syncing the two copies is manual, via
// scripts/sync-template.sh.
//
// Exit codes mirror the other `soul-lint` subcommands:
//
//	0 — ok (scaffold created)
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

// Embedded template tree. `all:` is required — otherwise go:embed ignores
// names starting with `.` by default (e.g. `.gitignore.tmpl`).
//
//go:embed all:template
var templateFS embed.FS

// Current plugin protocol version, stamped into manifest.yaml. Must match
// shared/plugin.SupportedProtocolVersions[0] and
// pluginhost.SupportedProtocolVersions. The duplication is deliberate:
// soul-lint doesn't import pluginhost, and shared/plugin pulls in the
// goccy YAML parser — a heavy dependency the scaffold command doesn't need.
const currentProtocolVersion = 1

// Exit codes are a stable CLI contract (see soul-lint/cmd/soul-lint/main.go).
const (
	ExitOK      = 0
	ExitIOFatal = 2
)

// Options — parameters for one `plugin-init` run.
type Options struct {
	// Namespace is the plugin's collection, e.g. `official` or `community`.
	// Validated against the plugin manifest's namespace regex (shared/plugin).
	Namespace string
	// Name is the plugin's name within its collection, e.g. `postgres-user`.
	Name string
	// Out is the output directory. Empty means `./soul-mod-<namespace>-<name>`
	// in the CWD.
	Out string
	// Description fills README.md / the manifest. Empty falls back to a
	// placeholder.
	Description string
	// Author fills README.md / the manifest. Empty falls back to a
	// placeholder.
	Author string
	// Force overwrites out-dir if it exists and isn't empty.
	Force bool
}

// Run performs the scaffold. It prints diagnostics to out / errOut and
// returns an exit code (0/2).
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

// TemplateVars — data available inside .tmpl files as `{{.Field}}`.
type TemplateVars struct {
	// Namespace — `official` / `community` / an author's own collection.
	Namespace string
	// Name — kebab-case, `postgres-user`.
	Name string
	// NameSnake — snake_case, `postgres_user` (for Go identifiers in
	// templates). NB: the field is kept (passed into .tmpl), but actual use
	// is via NamePackage below: the snake_case Go package is the same
	// `postgres_user`.
	NameSnake string
	// NamePascal — PascalCase, `PostgresUser` (for Go types).
	NamePascal string
	// NamePackage — same value as NameSnake; a separate name so templates can
	// tell apart "the Go package name under internal/" from "the snake_case
	// form used elsewhere".
	NamePackage string
	// BinaryName — `soul-mod-<namespace>-<name>`, the resulting binary's name.
	BinaryName string
	// GoModulePath — the module path in the new plugin's go.mod. Defaults to
	// `github.com/<author>/soul-mod-<namespace>-<name>`; falls back to
	// `github.com/EXAMPLE/...` for an empty author.
	GoModulePath string
	// AuthorName fills README.md / the manifest. Defaults to `EXAMPLE`.
	AuthorName string
	// Description is the user-supplied description.
	Description string
	// ProtocolVersion is the plugin protocol_version, see shared/plugin.
	ProtocolVersion int
}

// ParseSpec parses a spec of the form `<namespace>/<name>` (e.g.
// `official/postgres-user`) and returns namespace and name separately. The
// spec is the first command's required positional argument (`plugin-init
// official/postgres-user`).
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

// These regexes mirror shared/plugin.reNamespace / reName (kebab-case,
// 1..63 chars, starts with a letter). Duplicated so plugininit doesn't pull
// in shared/plugin's goccy YAML parser just for two regexes.
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
		description = "TODO: short plugin description (1-2 paragraphs)."
	}

	// `<author>` in the module path is kebab-case (like a GitHub username);
	// strip whitespace. For names with spaces/non-Latin chars we fall back to
	// EXAMPLE instead of guessing.
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
		// ASCII upper: name matches `^[a-z][a-z0-9-]{0,62}$`, so every
		// char is [a-z0-9-] and unicode case handling does not apply.
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
	// The directory exists — check that it's empty; otherwise require --force.
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
		// Rewrite the `cmd/soul-mod-EXAMPLE/` directory to `cmd/<BinaryName>/`.
		rel = strings.ReplaceAll(rel, "cmd/soul-mod-EXAMPLE", "cmd/"+vars.BinaryName)
		// Rewrite the `internal/PACKAGE/` directory to
		// `internal/<NamePackage>/` (the snake_case Go package that
		// cmd/<BinaryName>/main.go imports). The literal directory name is
		// needed because text/template only renders file content, not paths;
		// a filepath.Walk + per-segment Execute() alternative would be
		// overkill for one fixed substitution.
		rel = strings.ReplaceAll(rel, "internal/PACKAGE", "internal/"+vars.NamePackage)
		// Strip the `.tmpl` suffix on output.
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

		// `.tmpl` files go through text/template; non-`.tmpl` files (e.g.
		// binary artifacts from future extensions) are copied as-is.
		// Everything in the tree is `.tmpl` today, but the check is cheap and
		// guards against regressions.
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
	fmt.Fprintf(w, "scaffold created: %s\n", outDir)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "next steps:")
	fmt.Fprintf(w, "  cd %s\n", outDir)
	fmt.Fprintf(w, "  # 1. edit manifest.yaml::spec.states.<state>.input for your resource\n")
	fmt.Fprintf(w, "  # 2. implement Apply in internal/%s/handler.go\n", vars.NamePackage)
	fmt.Fprintln(w, "  # 3. (optional) implement Plan + PlanReadSafe for drift-detect (ADR-031)")
	fmt.Fprintln(w, "  # 4. adapt the L0/L1 tests to the real implementation")
	fmt.Fprintln(w, "  make check    # gofmt + vet + test + build")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "binary: %s/bin/%s\n", outDir, vars.BinaryName)
}
