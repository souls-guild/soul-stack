package config

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// The verb modules' examples in docs/ and examples/, run through the linter's own
// params check (NIM-912). Examples in docs/ passed `core.exec.run` a `command:` string
// — a param neither verb module has, and the case validateModuleParams names as
// unknown_param; the documents were simply never fed to it.
//
// Two checks here are not the linter's, and both are what the obvious repair of
// such an example runs into. A `cmd` holding a whole command line is a string, so
// the linter accepts it, and on the host the entire line is argv[0]. A bare `6379`
// in `args` passes the linter too, which checks that `args` is a list and not what
// its elements are, and OptStringSliceParam refuses it at Apply. Both are checked
// against the literal only: what a `${ … }` cell renders to is not knowable here.
//
// A task with no `params:` is an elided example (`module:` beside the key being
// discussed) and is skipped. Only the params are checked, not the rest of the task:
// a documentation fragment is rarely a whole definition.

// verbExampleRoots are filesystem paths relative to this package directory; both
// sit at the repo root, outside the shared module.
var verbExampleRoots = []string{"../../docs", "../../examples"}

var verbExampleModules = map[string]bool{
	"core.exec.run":  true,
	"core.cmd.shell": true,
}

// reVerbModuleMention selects the fenced blocks to parse by their content rather
// than their info string, so an example fenced as ```text is not out of reach.
var reVerbModuleMention = regexp.MustCompile(`module:\s*["']?core\.(exec\.run|cmd\.shell)\b`)

// reFenceOpen admits an indent: a fence nested in a list item is indented. Its body
// is parsed as is — goccy measures indentation against the enclosing node, so a
// uniformly indented document reads the same; only a `---`/`...` marker, which it
// recognises at column 1 alone, would not, and no example uses one.
var reFenceOpen = regexp.MustCompile("^ *(```+|~~~+)")

// reCELCell drops `${ … }` cells before the argv[0] check; a cell is resolved at
// render, and a nested `}` inside one is not a form these documents use.
var reCELCell = regexp.MustCompile(`\$\{[^}]*\}`)

const shellMetachars = " \t\n|&;<>()$`"

type docFence struct {
	file string // relative to the parent of the scanned root
	line int    // 1-based line of the first body line
	body string
}

type verbProblem struct {
	line int // relative to the fence body
	msg  string
}

func TestDocVerbExamples_ParamsMeetTheModuleContract(t *testing.T) {
	seen, findings := scanVerbExamples(t, verbExampleRoots...)
	for _, f := range findings {
		t.Error(f)
	}
	// The floor keeps the guard from going green on an extractor that finds
	// nothing — a fence form the scanner stopped matching.
	for _, addr := range slices.Sorted(maps.Keys(verbExampleModules)) {
		if seen[addr] == 0 {
			t.Errorf("no %s example with params: found under %v — the scan is blind, not the docs clean", addr, verbExampleRoots)
		}
	}
}

// On clean docs a check that stopped firing is green as well, so each one is held
// to a known-bad here, together with the fence extraction it depends on (a block
// nested in a list item and fenced as text, and one that does not parse). The
// known-good block pins that none of
// them fires on the forms the docs are meant to use; the comparison is exact, so a
// stray finding there fails as surely as a missing one above it.
func TestDocVerbExamples_KnownBadIsFlagged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "docs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# known-bad",             // 1
		"",                        // 2
		"```yaml",                 // 3
		"- name: unknown param",   // 4
		"  module: core.exec.run", // 5
		`  params: { command: "redis-migrate up" }`, // 6
		"```", // 7
		"",    // 8
		"1. Nested in a list item, fenced as text:", // 9
		"",                               // 10
		"   ```text",                     // 11
		"   - name: command line in cmd", // 12
		"     module: core.exec.run",     // 13
		`     params: { cmd: "redis-cli role | head -1" }`, // 14
		"   ```",                              // 15
		"",                                    // 16
		"```yaml",                             // 17
		"- name: CEL cell beside text in cmd", // 18
		"  module: core.exec.run",             // 19
		`  params: { cmd: "redis-cli ${ vars.verb }" }`,                                              // 20
		"- name: non-string list element and map value",                                              // 21
		"  module: core.exec.run",                                                                    // 22
		`  params: { cmd: redis-cli, args: [replicaof, "${ vars.ip }", 6379], env: { RETRIES: 3 } }`, // 23
		"- name: unknown param on the shell module",                                                  // 24
		"  module: core.cmd.shell",                                                                   // 25
		`  params: { command: "redis-cli role | head -1" }`,                                          // 26
		"```",     // 27
		"",        // 28
		"```yaml", // 29
		"- name: argv with rendered and quoted elements", // 30
		"  module: core.exec.run",                        // 31
		`  params: { cmd: redis-cli, args: [replicaof, "${ vars.ip }", "6379"], env: { RETRIES: "3" } }`, // 32
		"- name: program from a CEL cell",               // 33
		"  module: core.exec.run",                       // 34
		`  params: { cmd: "${ vars.bin }" }`,            // 35
		"- name: pipeline through the shell",            // 36
		"  module: core.cmd.shell",                      // 37
		`  params: { cmd: "redis-cli role | head -1" }`, // 38
		"- name: elided",                                // 39
		"  module: core.exec.run",                       // 40
		"```",                                           // 41
		"",                                              // 42
		"```yaml",                                       // 43
		"- name: unterminated quote",                    // 44
		"  module: core.exec.run",                       // 45
		`  params: { cmd: "redis-cli }`,                 // 46
		"```",                                           // 47
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "known-bad.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"docs/known-bad.md:6: core.exec.run: unknown_param:",
		"docs/known-bad.md:5: core.exec.run: missing_required_param:",
		`docs/known-bad.md:14: core.exec.run: cmd "redis-cli role | head -1" is a command line`,
		`docs/known-bad.md:20: core.exec.run: cmd "redis-cli ${ vars.verb }" is a command line`,
		`docs/known-bad.md:23: core.exec.run: args holds a non-string literal "6379"`,
		`docs/known-bad.md:23: core.exec.run: env holds a non-string literal "3"`,
		"docs/known-bad.md:26: core.cmd.shell: unknown_param:",
		"docs/known-bad.md:25: core.cmd.shell: missing_required_param:",
		"docs/known-bad.md:44: a block holding a verb-module task does not parse as YAML",
	}
	_, got := scanVerbExamples(t, root)
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("finding %d:\n got  %s\n want %s…", i, got[i], want[i])
		}
	}
}

// scanVerbExamples returns how many verb-module tasks with params it checked, per
// address, and every finding as `file:line: …`, in path order.
func scanVerbExamples(t *testing.T, roots ...string) (map[string]int, []string) {
	t.Helper()
	seen := map[string]int{}
	var findings []string
	for _, root := range roots {
		for _, f := range collectDocFences(t, root) {
			if !reVerbModuleMention.MatchString(f.body) {
				continue
			}
			file, err := parser.ParseBytes([]byte(f.body), 0)
			if err != nil {
				findings = append(findings, fmt.Sprintf("%s:%d: a block holding a verb-module task does not parse as YAML, so this guard cannot read it: %v", f.file, f.line, err))
				continue
			}
			for _, doc := range file.Docs {
				for _, n := range ast.Filter(ast.MappingType, doc) {
					addr, problems := verbTaskProblems(n.(*ast.MappingNode))
					if addr == "" {
						continue
					}
					seen[addr]++
					for _, p := range problems {
						findings = append(findings, fmt.Sprintf("%s:%d: %s: %s", f.file, f.line+p.line-1, addr, p.msg))
					}
				}
			}
		}
	}
	return seen, findings
}

// verbTaskProblems returns the task's module address when it is a verb module
// that writes `params:`, and what is wrong with those params.
func verbTaskProblems(mm *ast.MappingNode) (string, []verbProblem) {
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range mm.Values {
		if tok := kv.Key.GetToken(); tok != nil {
			present[tok.Value] = kv
		}
	}
	moduleKV, paramsKV := present["module"], present["params"]
	if moduleKV == nil || paramsKV == nil {
		return "", nil
	}
	sn, ok := moduleKV.Value.(*ast.StringNode)
	if !ok || !verbExampleModules[sn.Value] {
		return "", nil
	}
	addr := sn.Value

	var out []verbProblem
	for _, d := range validateModuleParams(moduleKV, paramsKV, "task") {
		if d.Level == diag.LevelError {
			out = append(out, verbProblem{d.Line, d.Code + ": " + d.Message})
		}
	}

	params, ok := paramsKV.Value.(*ast.MappingNode)
	if !ok {
		return addr, out
	}
	_, mod, state, _ := splitModuleAddress(addr)
	def, _ := coremanifest.Default().State("core."+mod, state)
	for _, kv := range params.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		if addr == "core.exec.run" && name == "cmd" {
			if v := scalarText(kv.Value); strings.ContainsAny(reCELCell.ReplaceAllString(v, ""), shellMetachars) {
				out = append(out, verbProblem{lineOf(kv.Value), fmt.Sprintf(
					"cmd %q is a command line, but core.exec.run runs cmd as argv[0] with no shell — put the program in cmd and the rest in args, or use core.cmd.shell for a pipeline", v)})
			}
		}
		p, known := def.Input[name]
		if !known || p.Items == nil || canonicalType(string(p.Items.Type)) != "string" {
			continue
		}
		for _, elem := range nonStringLiterals(kv.Value) {
			out = append(out, verbProblem{lineOf(elem), fmt.Sprintf(
				"%s holds a non-string literal %q; the module accepts strings only and refuses the task at Apply — quote it", name, elem.String())})
		}
	}
	return addr, out
}

// nonStringLiterals returns the elements of a list, or the values of a map, that
// are not string scalars.
func nonStringLiterals(v ast.Node) []ast.Node {
	var elems []ast.Node
	switch n := v.(type) {
	case *ast.SequenceNode:
		elems = n.Values
	case *ast.MappingNode:
		for _, kv := range n.Values {
			elems = append(elems, kv.Value)
		}
	}
	var out []ast.Node
	for _, e := range elems {
		switch e.(type) {
		case *ast.StringNode, *ast.LiteralNode:
		default:
			out = append(out, e)
		}
	}
	return out
}

// collectDocFences returns every fenced block under root, in path order.
func collectDocFences(t *testing.T, root string) []docFence {
	t.Helper()
	var out []docFence
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		lines := strings.Split(string(src), "\n")
		for i := 0; i < len(lines); i++ {
			m := reFenceOpen.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			fence := m[1]
			j := i + 1
			for ; j < len(lines); j++ {
				if s := strings.TrimSpace(lines[j]); strings.HasPrefix(s, fence) && strings.Trim(s, fence[:1]) == "" {
					break
				}
			}
			out = append(out, docFence{file: filepath.ToSlash(rel), line: i + 2, body: strings.Join(lines[i+1:j], "\n")})
			i = j
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
