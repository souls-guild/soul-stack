package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// fakeManifests — a resolver over an in-memory table, standing in for whatever
// the caller can actually see: keeper resolves from its Sigil grants, soul-lint
// from a directory of manifests.
type fakeManifests map[string]*plugin.Manifest

func (f fakeManifests) ResolveModule(ns, name string) (*plugin.Manifest, bool) {
	m, ok := f[ns+"."+name]
	return m, ok
}

// redisManifest — a minimal `community.redis` with one state, enough to exercise
// all four checks.
func redisManifest() fakeManifests {
	return fakeManifests{"community.redis": {
		Kind:      "soul_module",
		Namespace: "community",
		Name:      "redis",
		Spec: plugin.ManifestSpec{States: map[string]plugin.StateDef{
			"config": {Input: map[string]plugin.InputParamDef{
				"addr":    {Type: "string", Required: true},
				"config":  {Type: "map"},
				"rewrite": {Type: "bool"},
			}},
		}},
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
	src := "- name: t\n  module: community.redis.config\n  params:\n    addr: 127.0.0.1:6379\n    confgi: {}\n"
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
	src := "- name: t\n  module: community.redis.config\n  params:\n    rewrite: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("a missing required param on a plugin module passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_TypeMismatchIsRejected(t *testing.T) {
	src := "- name: t\n  module: community.redis.config\n  params:\n    addr: 127.0.0.1:6379\n    rewrite: \"yes\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("a string where the manifest declares bool passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_UnknownStateIsRejected(t *testing.T) {
	src := "- name: t\n  module: community.redis.confgi\n  params:\n    addr: 127.0.0.1:6379\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "module_state_unknown") {
		t.Errorf("a state the manifest does not declare passed: %v", diagCodesP(diags))
	}
}

func TestPluginParams_ValidTaskIsClean(t *testing.T) {
	src := "- name: t\n  module: community.redis.config\n  params:\n    addr: 127.0.0.1:6379\n    config: {maxmemory: 1gb}\n    rewrite: true\n"
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
	src := "- name: t\n  module: community.redis.config\n  params:\n    confgi: {}\n"
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
	src := "- name: t\n  module: community.mongo.config\n  params:\n    whatever: 1\n"
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
	src := "- name: a\n  module: community.redis.config\n  params: {}\n" +
		"- name: b\n  module: community.redis.config\n  params: {}\n" +
		"- name: c\n  module: community.mongo.config\n  params: {}\n"
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
	src := "- name: outer\n  block:\n    - name: inner\n      module: community.redis.config\n      params:\n        addr: x\n        confgi: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("a plugin task inside block: went unchecked: %v", diagCodesP(diags))
	}
}

// A CEL-wrapped value has no static type, exactly as on core — the rule is the
// module's, not the namespace's.
func TestPluginParams_CELValueSkipsTypeCheck(t *testing.T) {
	src := "- name: t\n  module: community.redis.config\n  params:\n    addr: 127.0.0.1:6379\n    rewrite: \"${ input.rewrite }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src),
		ValidateOptions{ModuleManifests: redisManifest()})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("a CEL expression was type-checked as a literal: %v", diags)
	}
}
