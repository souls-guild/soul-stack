package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard tests for the offline half of NIM-543: `core.module.installed` takes a
// REGISTRATION ALIAS in `params.name`, and a definition that says otherwise is
// rejected with a line and a column instead of on a host.
//
// The class this closes: `params.name` is a free string in the schema, so the
// pre-NIM-377 two-level spelling (`name: redis.instance` — still what an author
// finds in older material) parsed clean, linted clean, passed `service.yml`
// validation, and failed on every host at apply. Nothing offline could see it.

// installNameDiag returns the diagnostic this check raises for one task source, or
// the zero value when it raised none. It deliberately runs the whole loader, so what
// is asserted is what an author's `soul-lint` prints.
func installNameDiag(t *testing.T, src string) diag.Diagnostic {
	t.Helper()
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	var found diag.Diagnostic
	for _, d := range diags {
		if d.Code == "module_install_name_not_an_alias" {
			if found.Code != "" {
				t.Fatalf("module_install_name_not_an_alias raised twice for one param: %v", diags)
			}
			found = d
		}
	}
	return found
}

// TestInstallName_DottedAddressRejected — the ticket's subject. `redis.instance` is
// the `modules[]` entry's spelling, not the step's: the step installs the artifact
// into the slot `redis` names, and the Soul refuses a dotted value outright.
//
// The hint must name the alias the author meant, because "that is not an alias" on
// its own leaves them guessing which half to delete.
func TestInstallName_DottedAddressRejected(t *testing.T) {
	d := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: redis.instance\n    ref: v1.0.0\n")
	if d.Code == "" {
		t.Fatal("a dotted params.name produced no diagnostic — the author learns on a host, from a failed event (NIM-543)")
	}
	if d.Level != diag.LevelError {
		t.Errorf("level = %q, want error: the value cannot be applied on any host", d.Level)
	}
	if !strings.Contains(d.Message, "redis.instance") {
		t.Errorf("message %q does not quote the offending value", d.Message)
	}
	if !strings.Contains(d.Hint, "name: redis") {
		t.Errorf("hint %q does not name the alias the author meant", d.Hint)
	}
	if d.YAMLPath != "$[0].params.name" {
		t.Errorf("YAMLPath = %q, want $[0].params.name", d.YAMLPath)
	}
	if d.Line == 0 || d.Column == 0 {
		t.Errorf("position = %d:%d, want the value's own line and column", d.Line, d.Column)
	}
}

// TestInstallName_AliasAccepted — the correct form stays silent. Paired with the
// test above: a check that rejects everything would pass that one on its own.
func TestInstallName_AliasAccepted(t *testing.T) {
	src := "- name: t\n  module: core.module.installed\n  params:\n    name: redis\n    ref: v1.0.0\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("the documented form produced errors: %v", diags)
	}
}

// TestInstallName_MalformedAliasRejected — the predicate is [plugin.ValidAlias], the
// same rule the Soul and a registration apply, so a name that is not an address but
// is not a legal alias either is caught too. `Redis` on a case-insensitive
// filesystem would fold onto another registration's slot, which is why the alias
// grammar excludes it and why this check must not narrow to "contains a dot".
func TestInstallName_MalformedAliasRejected(t *testing.T) {
	for _, name := range []string{"Redis", "1redis", "redis_instance", "redis/instance", ""} {
		d := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: \""+name+"\"\n")
		if d.Code == "" {
			t.Errorf("params.name %q accepted offline; the Soul rejects it before it does any work", name)
		}
	}
}

// TestInstallName_ReadRaw — the offline predicate must judge the value the Soul will
// judge, byte for byte. Trimming here would make the check LOOSER than the runtime:
// `reAlias` (soul/internal/coremod/module: 48) runs on the untrimmed param, so a
// padded alias is refused on the host — and `addrLevel1` does not match it either, so
// the operator's step takes over nothing and a second install lands beside it. That is
// the NIM-543 defect itself, one spelling further along.
//
// The block-scalar cases carry the DEFAULT chomping (`>` / `|`, clip), which keeps the
// trailing newline. `>-` / `|-` would strip it and make this test green against a
// trimming implementation — the shape that pins nothing.
func TestInstallName_ReadRaw(t *testing.T) {
	for _, src := range []string{
		"- name: t\n  module: core.module.installed\n  params:\n    name: \"  redis  \"\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: \"redis \"\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: >\n      redis\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: |\n      redis\n",
	} {
		if d := installNameDiag(t, src); d.Code == "" {
			t.Errorf("a value the Soul refuses passed offline:\n%s", src)
		}
	}
}

// TestInstallName_BlockScalarRead — a folded or literal scalar carries a string the
// Soul will judge exactly like a plain one. Reading only the plain form would leave
// this spelling rejected on a host and silent offline.
func TestInstallName_BlockScalarRead(t *testing.T) {
	for _, src := range []string{
		"- name: t\n  module: core.module.installed\n  params:\n    name: >-\n      redis.instance\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: |-\n      redis.instance\n",
	} {
		d := installNameDiag(t, src)
		if d.Code == "" {
			t.Errorf("a block-scalar params.name was not read:\n%s", src)
			continue
		}
		if !strings.Contains(d.Hint, "name: redis") {
			t.Errorf("hint %q does not name the alias the author meant", d.Hint)
		}
	}
}

// TestInstallName_NullRejected — `name:` with no value is a param the Soul reports
// missing, and nothing else offline speaks for it: checkParamType returns nil for a
// null (module_params.go), and checkRequired counts the key as present. Without this
// the author learns on a host that they left the value out.
func TestInstallName_NullRejected(t *testing.T) {
	for _, src := range []string{
		"- name: t\n  module: core.module.installed\n  params:\n    name:\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: ~\n",
		"- name: t\n  module: core.module.installed\n  params:\n    name: null\n",
	} {
		if d := installNameDiag(t, src); d.Code == "" {
			t.Errorf("an empty params.name produced no diagnostic:\n%s", src)
		}
	}
}

// TestInstallName_CELNameSkipped — a `${…}` cell resolves at render, so its value is
// not knowable here (ADR-010). Rejecting it would make a legal definition unlintable.
//
// PARTIAL interpolation counts: `acme-${ vars.env }` is legal (docs/templating.md §5(b),
// stringify-and-merge) and renders to an ordinary alias. The takeover half has always
// skipped any value carrying a cell; a check that skipped only a wholly wrapped one
// would be the two-ends-two-rules drift this ticket exists to close, pointed the other
// way — and its hint would cut the value at the dot INSIDE the cell.
func TestInstallName_CELNameSkipped(t *testing.T) {
	for _, name := range []string{"${ input.plugin }", "${ vars.alias }", "acme-${ vars.env }", "${ vars.a }-${ vars.b }"} {
		if d := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: \""+name+"\"\n"); d.Code != "" {
			t.Errorf("params.name %q was judged statically: %s", name, d.Message)
		}
	}
}

// TestInstallName_HintNeverTeachesAnIllegalValue — the hint that names the alias the
// author meant only fires when that alias is one. `Redis.instance` reduces to
// `Redis`, which the Soul refuses just as flatly; answering with
// "write `name: Redis`" costs the author a second round trip, and a hint teaching
// a value the runtime rejects is this ticket's own defect written in prose.
func TestInstallName_HintNeverTeachesAnIllegalValue(t *testing.T) {
	for _, name := range []string{"Redis.instance", " redis.instance", "redis_x.instance", "1redis.instance"} {
		d := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: \""+name+"\"\n")
		if d.Code == "" {
			t.Errorf("%q produced no diagnostic at all", name)
			continue
		}
		if strings.Contains(d.Hint, "write `name:") {
			t.Errorf("%q: hint proposes a value: %s", name, d.Hint)
		}
	}
	// The good case still gets the specific hint — a check that dropped it entirely
	// would pass the loop above.
	d := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: redis.instance\n")
	if !strings.Contains(d.Hint, "write `name: redis`") {
		t.Errorf("a reducible dotted value lost its hint: %s", d.Hint)
	}
}

// TestInstallName_CELPredicateIsShared — the two ends of `params.name` must read ONE
// rule about `${…}`, and this is the guard that says so out loud.
//
// They are the offline check (is this value a legal alias?) and the takeover half
// (does this step claim the slot, so no install is synthesized?). Narrow either one to
// a whole-string test and a partially interpolated name splits them: silent offline,
// yet keyed on the garbage `acme-${ vars` — the shape of the NIM-543 defect, one field
// further in. Neither end's own tests can see that, because a garbage key matches no
// real alias; only the comparison does.
func TestInstallName_CELPredicateIsShared(t *testing.T) {
	for _, tc := range []struct {
		name    string
		literal bool // the takeover half reads it as a literal claim on a slot
	}{
		{"redis", true},
		{"redis.instance", true},
		{"${ input.plugin }", false},
		{"acme-${ vars.env }", false},
		{"${ vars.a }-${ vars.b }", false},
		{"${ vars.env }-acme", false},
	} {
		_, isLiteral := literalInstallName(map[string]any{"name": tc.name})
		if isLiteral != tc.literal {
			t.Errorf("literalInstallName(%q) = %v, want %v", tc.name, isLiteral, tc.literal)
		}
		// A value the takeover half declines to read is one whose rendering is
		// unknown, and the offline check must decline to judge exactly those.
		skipped := installNameDiag(t, "- name: t\n  module: core.module.installed\n  params:\n    name: \""+tc.name+"\"\n").Code == ""
		if isLiteral && plugin.ValidAlias(tc.name) != skipped {
			t.Errorf("%q: the offline check disagrees with plugin.ValidAlias", tc.name)
		}
		if !isLiteral && !skipped {
			t.Errorf("%q: unknowable to the takeover half, but judged offline", tc.name)
		}
	}
}

// TestInstallName_KeyedOnTheBaseAddressNotOnTheParam — the negative case that pins
// WHAT the rule keys on.
//
// `name` is a param of nine core states, and dots are ordinary in most of them
// (`nginx.x86_64` is a package, `/etc/x.conf` is a path). The rule belongs to
// `core.module.installed` — the one state whose `name` is a registration alias — and
// keying it on the param's spelling instead would reject every one of these.
func TestInstallName_KeyedOnTheBaseAddressNotOnTheParam(t *testing.T) {
	for _, src := range []string{
		"- name: t\n  module: core.pkg.installed\n  params:\n    name: nginx.x86_64\n",
		"- name: t\n  module: core.service.running\n  params:\n    name: redis.service\n",
		"- name: t\n  module: core.user.present\n  params:\n    name: svc.redis\n",
	} {
		if d := installNameDiag(t, src); d.Code != "" {
			t.Errorf("the alias rule fired outside %s: %s\nsrc: %s", moduleInstalledAddr, d.Message, src)
		}
	}
}

// TestInstallName_NonStringLeavesTypeCheckAlone — a non-string value is
// param_type_mismatch's finding, not this one's. Two codes on one key would tell the
// author to fix two things where there is one.
func TestInstallName_NonStringLeavesTypeCheckAlone(t *testing.T) {
	src := "- name: t\n  module: core.module.installed\n  params:\n    name: 42\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Fatalf("expected param_type_mismatch for a numeric name, got %v", diagCodesP(diags))
	}
	if hasCodeP(diags, "module_install_name_not_an_alias") {
		t.Errorf("the alias rule doubled up on a type error: %v", diagCodesP(diags))
	}
}
