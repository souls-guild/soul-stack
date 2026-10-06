package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// fakeManifests — a resolver over an in-memory table, standing in for whatever
// the caller can actually see: keeper resolves from its Sigil grants, soul-lint
// from a directory of manifests.
type fakeManifests map[string]plugin.ModuleDef

func (f fakeManifests) ResolveModule(ns, name string) (plugin.ModuleDef, bool) {
	m, ok := f[ns+"."+name]
	return m, ok
}

// redisManifest — a minimal `redis.instance` with one state, enough to exercise
// all four checks.
func redisManifest() fakeManifests {
	return fakeManifests{"redis.instance": {
		Name: "instance",
		States: map[string]plugin.StateDef{
			"configured": {Input: map[string]plugin.InputParamDef{
				"addr":    {Type: "string", Required: true},
				"config":  {Type: "map"},
				"rewrite": {Type: "bool"},
			}},
		},
	}}
}

func firstWithCode(ds []diag.Diagnostic, code string) *diag.Diagnostic {
	for i := range ds {
		if ds[i].Code == code {
			return &ds[i]
		}
	}
	return nil
}

// The point of the ticket: an undeclared param on a PLUGIN module is an error
// with a position, before the run — not a module.unknown_param discovered on the
// host after NIM-204 made the runtime gate enforce plugins too.
func TestPluginParams_UnknownParamIsRejected(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    addr: 127.0.0.1:6379\n    confgi: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})

	d := firstWithCode(diags, "unknown_param")
	if d == nil {
		t.Fatalf("a typo'd param on a plugin module was accepted: %v", diagCodesP(diags))
	}
	if d.Line == 0 || d.Column == 0 {
		t.Errorf("diagnostic carries no position (line=%d col=%d) — the acceptance bar is line/col before the run", d.Line, d.Column)
	}
	if hasCodeP(diags, "plugin_params_unchecked") {
		t.Error("reported the module as unchecked while checking it")
	}
}

func TestPluginParams_MissingRequiredIsRejected(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    rewrite: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("a missing required param on a plugin module passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_TypeMismatchIsRejected(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    addr: 127.0.0.1:6379\n    rewrite: \"yes\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("a string where the manifest declares bool passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_UnknownStateIsRejected(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configurd\n  params:\n    addr: 127.0.0.1:6379\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "module_state_unknown") {
		t.Errorf("a state the manifest does not declare passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_ValidTaskIsClean(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    addr: 127.0.0.1:6379\n    config: {maxmemory: 1gb}\n    rewrite: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if diag.HasErrors(diags) {
		t.Fatalf("a task matching the manifest produced errors: %v", diags)
	}
	if hasCodeP(diags, "plugin_params_unchecked") {
		t.Error("a checked module was reported as unchecked")
	}
}

// The other half of the ticket: where the manifest does NOT resolve, say so.
// Before this, "checked and clean" and "never checked" were the same output.
func TestPluginParams_NoResolverSaysSoInsteadOfPassing(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    confgi: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})

	d := firstWithCode(diags, "plugin_params_unchecked")
	if d == nil {
		t.Fatalf("silence: no resolver and no notice, so a lint pass means nothing here: %v", diagCodesP(diags))
	}
	if d.Level != diag.LevelHint {
		t.Errorf("plugin_params_unchecked is %q — it must not fail a lint, the author cannot act on a manifest they do not have", d.Level)
	}
	if diag.HasErrors(diags) {
		t.Errorf("unresolvable plugin params produced errors: %v", diags)
	}
}

// A resolver that simply does not carry this module is the same case as no
// resolver at all — the module is unchecked, and that is not the author's fault.
func TestPluginParams_ResolverMissIsAlsoReported(t *testing.T) {
	src := "- name: t\n  module: mongo.instance.configured\n  params:\n    whatever: 1\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "plugin_params_unchecked") {
		t.Errorf("a module the resolver cannot see passed silently: %v", diagCodesP(diags))
	}
	if diag.HasErrors(diags) {
		t.Errorf("an unresolvable module produced errors: %v", diags)
	}
}

// One notice per module address, not per task: a definition using the same
// module twenty times has one thing to fix, and twenty identical hints would
// train the reader to skip them.
func TestPluginParams_UncheckedNoticeIsPerModuleNotPerTask(t *testing.T) {
	src := "- name: a\n  module: redis.instance.configured\n  params: {}\n" +
		"- name: b\n  module: redis.instance.configured\n  params: {}\n" +
		"- name: c\n  module: mongo.instance.configured\n  params: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "plugin_params_unchecked"); got != 2 {
		t.Errorf("plugin_params_unchecked ×%d, want 2 (one per module address): %v", got, diagCodesP(diags))
	}
}

// Core is compiled into the binary, so it is checked whether or not a resolver
// was supplied, and never reported as unchecked. This pins that the new pass did
// not take over the path that already worked.
func TestPluginParams_CoreIsUnaffected(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    command: \"true\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("core stopped being checked: %v", diagCodesP(diags))
	}
	if hasCodeP(diags, "plugin_params_unchecked") {
		t.Error("core reported as unchecked — its manifest ships in the binary")
	}
}

// Tasks nested inside block: are tasks. The walk must not depend on knowing the
// grammar's nesting rules, or a param check would quietly stop at the first
// construct someone adds.
func TestPluginParams_ReachesTasksInsideBlock(t *testing.T) {
	src := "- name: outer\n  block:\n    - name: inner\n      module: redis.instance.configured\n      params:\n        addr: x\n        confgi: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("a plugin task inside block: went unchecked: %v", diagCodesP(diags))
	}
}

// Reserved names never reach a resolver (NIM-377).
//
// The bypass at the top of the walk used to read `ns == "core"`, which was safe only
// while a namespace meant a publisher. Address level 1 is now an alias an operator
// picks, so that test would let anything registered as `core` into the branch reserved
// for built-ins — checked by no one and reported by no one. These pin the two halves of
// the fix: the branch keys on the compiled-in registry, and no catalog gets to answer
// for a reserved name.

// spyManifests records every lookup, so a test can assert the walk never CONSULTED the
// resolver — a stronger claim than "ignored what it said".
type spyManifests struct {
	table fakeManifests
	asked []string
}

func (s *spyManifests) ResolveModule(alias, name string) (plugin.ModuleDef, bool) {
	s.asked = append(s.asked, alias+"."+name)
	return s.table.ResolveModule(alias, name)
}

// coreDecoy — the resolver a plugin registered under a reserved alias would produce:
// a schema for `core.redis` that says `bogus` is a fine parameter.
func coreDecoy() *spyManifests {
	return &spyManifests{table: fakeManifests{
		"core.redis": {Name: "redis", States: map[string]plugin.StateDef{
			"config": {Input: map[string]plugin.InputParamDef{"bogus": {Type: "int"}}},
		}},
		"core.exec": {Name: "exec", States: map[string]plugin.StateDef{
			"run": {Input: map[string]plugin.InputParamDef{"command": {Type: "string"}}},
		}},
		"core.augur": {Name: "augur", States: map[string]plugin.StateDef{
			"fetch": {Input: map[string]plugin.InputParamDef{"bogus": {Type: "int"}}},
		}},
		"keeper.push": {Name: "push", States: map[string]plugin.StateDef{
			"run": {Input: map[string]plugin.InputParamDef{"anything": {Type: "string"}}},
		}},
	}}
}

func TestPluginParams_ReservedAliasDoesNotReachTheBuiltinBypass(t *testing.T) {
	r := coreDecoy()
	src := "- name: t\n  module: core.redis.config\n  params:\n    bogus: 1\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: r})

	if len(r.asked) != 0 {
		t.Errorf("the resolver was consulted for a reserved name %v — a plugin registered as `core` would define what core.* accepts", r.asked)
	}
	if !hasCodeP(diags, DiagCoreModuleUnknown) {
		t.Errorf("core.redis was not refused: %v", diagCodesP(diags))
	}
}

// Served without a schema, `core.augur` is the address that still reaches the
// reserved-name branch — and a decoy schema for it must not get to judge its params.
func TestPluginParams_ReservedAliasDoesNotDescribeAServedCoreModule(t *testing.T) {
	r := coreDecoy()
	src := "- name: t\n  module: core.augur.fetch\n  params:\n    omen: vault\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: r})

	if len(r.asked) != 0 {
		t.Errorf("the resolver was consulted for %v — a plugin registered as `core` would define what core.augur accepts", r.asked)
	}
	if hasCodeP(diags, "unknown_param") {
		t.Errorf("the decoy schema judged core.augur's params: %v", diagCodesP(diags))
	}
	if !hasCodeP(diags, DiagPluginParamsUnchecked) {
		t.Errorf("core.augur passed in silence: %v", diagCodesP(diags))
	}
}

// The same for a reserved name that is not `core`: `ns != "core"` used to be the whole
// definition of "this is a plugin", which sent `keeper.push` straight to the catalog.
func TestPluginParams_ReservedNonCoreNameDoesNotReachTheResolver(t *testing.T) {
	r := coreDecoy()
	src := "- name: t\n  module: keeper.push.run\n  params:\n    anything: x\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: r})

	if len(r.asked) != 0 {
		t.Errorf("the resolver answered for reserved `keeper`: %v", r.asked)
	}
	if !hasCodeP(diags, DiagPluginParamsUnchecked) {
		t.Errorf("keeper.push passed in silence: %v", diagCodesP(diags))
	}
}

// A module the compiled-in registry really does serve is still bypassed here and still
// checked at decode — and a decoy schema for the same address does not soften it.
func TestPluginParams_BuiltinModuleIsCheckedAgainstTheEmbeddedRegistry(t *testing.T) {
	r := coreDecoy()
	src := "- name: t\n  module: core.exec.run\n  params:\n    command: \"true\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: r})

	if len(r.asked) != 0 {
		t.Errorf("a built-in module was resolved through the catalog: %v", r.asked)
	}
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("a decoy schema declaring `command` suppressed the built-in check: %v", diagCodesP(diags))
	}
	if hasCodeP(diags, DiagPluginParamsUnchecked) {
		t.Error("a compiled-in module reported as unchecked")
	}
}

// ★ GUARD (NIM-888). `core.<something no binary serves>` is refused: `core` is reserved,
// so no plugin can supply it and the task cannot run on this engine. As a hint it passed
// soul-lint with exit 0 and then, at run time, silently changed side (NIM-863). One
// error, not an error plus the old unchecked hint for the same address.
func TestPluginParams_UnservedCoreModuleIsAnError(t *testing.T) {
	cases := map[string]string{
		"removed module":    "- name: t\n  module: core.cloud.created\n  params:\n    count: 1\n",
		"never existed":     "- name: t\n  module: core.haproxy.present\n  params:\n    whatever: 1\n",
		"nested in a block": "- name: b\n  block:\n    - name: t\n      module: core.cloud.created\n      params: {}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
			d := firstWithCode(diags, DiagCoreModuleUnknown)
			if d == nil {
				t.Fatalf("an unserved core address was not refused: %v", diagCodesP(diags))
			}
			if d.Level != diag.LevelError {
				t.Errorf("level is %q, want error", d.Level)
			}
			if hasCodeP(diags, DiagPluginParamsUnchecked) {
				t.Errorf("the refused address was ALSO reported as merely unchecked: %v", diagCodesP(diags))
			}
		})
	}
}

// ★ GUARD (NIM-888). The other half: a core module that is served but ships no schema
// document is not refused, and the hint says what is true about it. It used to say
// "this engine serves no built-in module core.augur" — about a module the Soul runs.
func TestPluginParams_ServedCoreModuleWithoutSchemaIsUnchecked(t *testing.T) {
	for _, src := range []string{
		"- name: t\n  module: core.augur.fetch\n  params:\n    omen: vault\n",
		"- name: t\n  module: core.cert.registered\n  params: {}\n",
	} {
		_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
		if diag.HasErrors(diags) {
			t.Errorf("a served module was refused: %v", diags)
		}
		d := firstWithCode(diags, DiagPluginParamsUnchecked)
		if d == nil {
			t.Fatalf("a module with no schema passed in silence: %v", diagCodesP(diags))
		}
		if d.Level != diag.LevelHint {
			t.Errorf("level is %q, want hint", d.Level)
		}
		if strings.Contains(d.Message, "serves no built-in module") {
			t.Errorf("a served module was reported as not served: %s", d.Message)
		}
	}
}

// A CEL-wrapped value has no static type, exactly as on core — the rule is the
// module's, not the namespace's.
func TestPluginParams_CELValueSkipsTypeCheck(t *testing.T) {
	src := "- name: t\n  module: redis.instance.configured\n  params:\n    addr: 127.0.0.1:6379\n    rewrite: \"${ input.rewrite }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("a CEL expression was type-checked as a literal: %v", diags)
	}
}
