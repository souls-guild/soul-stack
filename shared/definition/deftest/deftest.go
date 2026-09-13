// Package deftest carries the ONE input the plugin-params check is proved
// against, and the manifest it is checked with, so that every tool's tests are
// pointed at the same bytes.
//
// It exists for the acceptance criterion of NIM-790: the tools must reach the
// SAME verdict on the same definition. Three of them live in three Go modules
// (`soul-lint`, `keeper`, `shared`), so no single test process runs all three;
// what ties their tests together instead is this fixture. A tool that drops the
// finding, or reports a different one, stops matching a constant its own test
// compares against — and the constant is shared, so no copy of it can be quietly
// adjusted to fit a regression.
//
// The definition is deliberately built in two halves: one plugin step written
// INLINE in the entry file, and one behind an `include:`. Those are two different
// wiring paths — the entry file's parse options, and the resolver threaded through
// expansion — and every historical gap in this area lost exactly one of them.
package deftest

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// Alias is address level 1 — the registration alias an operator picks, and the
// word every binding below states. It is NOT read off any path: the artifact
// carries no self-name (NIM-377).
const Alias = "redis"

// Address is the module state every step in this package targets. Soul-side, so
// the same step is legal in a destiny as well as in a scenario
// (`keeper_module_in_destiny`, NIM-749, would otherwise reject the destiny half
// and the two verdicts could not be compared).
const Address = Alias + ".acl.present"

// SchemaJSON is the manifest: one module, one state, exactly two declared params,
// one of them required. Written as text rather than built through `sdk/schema` —
// a fixture reaches a tool the way an author's own `dist/schema.json` does.
const SchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"acl","description":"Redis ACL users","states":{"present":{` +
	`"description":"the user exists","input":{` +
	`"user":{"type":"string","required":true},"port":{"type":"int"}}}}}]}`

// The three param blocks, one per outcome the check has. Each is a list of
// `key: value` lines, indented by the renderer rather than by the constant, so
// the same block can sit at a scenario's indentation and at a flat task list's.
type Params []string

var (
	// Declared matches the manifest exactly: the check runs and finds nothing.
	Declared = Params{"user: alice", "port: 6379"}
	// Undeclared carries a param the manifest does not declare — `prot`, the
	// typo shape of NIM-778, where `tls: "true"` reached a host as a string.
	Undeclared = Params{"user: alice", "prot: 6379"}
	// MissingRequired omits the param the manifest demands.
	MissingRequired = Params{"port: 6379"}
)

// The codes a tool must produce, named so a test reads as the rule it enforces.
const (
	// Unchecked — the manifest could not be resolved, so the params were not
	// checked at all. A HINT: the author of a definition usually cannot produce
	// somebody else's plugin's manifest. Never silence.
	Unchecked = config.DiagPluginParamsUnchecked
	// UndeclaredParam / MissingParam — the manifest WAS read and the definition
	// contradicts it. Errors.
	UndeclaredParam = "unknown_param"
	MissingParam    = "missing_required_param"
)

// TaskList renders the canonical plugin step as a flat top-level task list. That
// one shape serves three roles: a scenario's included body, a destiny's
// `tasks/main.yml`, and a destiny's included body.
func TaskList(name string, p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- name: %s\n  module: %s\n  params:\n", name, Address)
	for _, kv := range p {
		fmt.Fprintf(&b, "    %s\n", kv)
	}
	return b.String()
}

// ScenarioMain renders a scenario entry point holding the step INLINE and an
// `include:` of body — the two wiring paths in one file. scenario is the
// scenario's name; body is the include target's file name.
func ScenarioMain(scenario, body string, p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\ndescription: one plugin step inline, one behind an include\n\ntasks:\n", scenario)
	fmt.Fprintf(&b, "  - name: inline step\n    module: %s\n    params:\n", Address)
	for _, kv := range p {
		fmt.Fprintf(&b, "      %s\n", kv)
	}
	fmt.Fprintf(&b, "  - include: %s\n", body)
	return b.String()
}

// DestinyTasksMain renders a destiny's `tasks/main.yml` in the same two halves:
// the step inline, then an `include:` of body.
func DestinyTasksMain(body string, p Params) string {
	return TaskList("inline step", p) + fmt.Sprintf("- include: %s\n", body)
}

// WriteSchemaFile drops the manifest at <dir>/<name>/schema.json and returns the
// file. The directory is named after a BINARY on purpose — the alias must come
// from the binding, never from the path.
func WriteSchemaFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	slot := filepath.Join(dir, name)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(slot, plugin.SchemaFileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// WriteStampedArtifact fakes what `soul-mod stamp` produces: arbitrary executable
// bytes followed by <payload><uint64 length><magic>.
//
// The magic is spelled out rather than imported: the offline tools read artifacts
// through `shared/plugin` and have no business depending on the SDK. If the
// trailer format ever changes, this fixture stops being recognized and the test
// fails loudly — which is the correct signal, not a silent pass.
func WriteStampedArtifact(t *testing.T, path, body string) string {
	t.Helper()
	const magic = "SOULSTACKSCHEMA1"
	out := []byte("\x7fELF not really an executable, but loaders ignore trailing bytes")
	out = append(out, body...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	out = append(out, n[:]...)
	out = append(out, magic...)
	if err := os.WriteFile(path, out, 0o700); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

// Binding writes the manifest into a temporary directory and returns the
// `--modules` binding that binds it to [Alias].
func Binding(t *testing.T) string {
	t.Helper()
	return Alias + "=" + WriteSchemaFile(t, t.TempDir(), "redis", SchemaJSON)
}
