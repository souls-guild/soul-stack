package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func diagCodesP(ds []diag.Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func hasCodeP(ds []diag.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestModuleParams_ExecCommandTypo — `command:` instead of `cmd:` on core.exec.run:
// caught both as unknown_param (command is unknown) and as missing_required_param
// (cmd is required and not passed). Key negative case.
func TestModuleParams_ExecCommandTypo(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    command: \"true\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("expected unknown_param for command:, got %v", diagCodesP(diags))
	}
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("expected missing_required_param for cmd, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_ExecValid — a correct core.exec.run (cmd + args) passes.
func TestModuleParams_ExecValid(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: install\n    args: [\"-d\", \"/var/run/x\"]\n    creates: /var/run/x\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("valid core.exec.run produced errors: %v", diags)
	}
}

// TestModuleParams_FileUnknownParam — an unknown param on core.file.present.
func TestModuleParams_FileUnknownParam(t *testing.T) {
	src := "- name: t\n  module: core.file.present\n  params:\n    path: /etc/x\n    contnet: hi\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("expected unknown_param for contnet:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_FileMissingPath — core.file.absent without the required path.
func TestModuleParams_FileMissingPath(t *testing.T) {
	src := "- name: t\n  module: core.file.absent\n  params: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("expected missing_required_param for path, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_RenderedAuthorForm — the author form of core.file.rendered
// (template:+vars:) is valid; template_content/render_context are NOT expected here
// (that's the Keeper-side runtime form).
func TestModuleParams_RenderedAuthorForm(t *testing.T) {
	src := "- name: t\n  module: core.file.rendered\n  params:\n    path: /etc/x.conf\n    template: templates/x.conf.tmpl\n    mode: \"0640\"\n    vars:\n      a: \"${ input.a }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("author form of core.file.rendered produced errors: %v", diags)
	}
}

// TestModuleParams_RenderedMissingTemplate — rendered without template: → error.
func TestModuleParams_RenderedMissingTemplate(t *testing.T) {
	src := "- name: t\n  module: core.file.rendered\n  params:\n    path: /etc/x.conf\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("expected missing_required_param for template, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_TypeMismatch — args must be a list; a string → param_type_mismatch.
func TestModuleParams_TypeMismatch(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: ls\n    args: \"not a list\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("expected param_type_mismatch for args, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CELWrappedSkipsTypeCheck — args as `${ … }` (a CEL non-string
// result) must not be caught by the type check: the runtime type is statically unknown.
func TestModuleParams_CELWrappedSkipsTypeCheck(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: ls\n    args: \"${ input.args }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("CEL-wrapped args must not produce errors: %v", diags)
	}
}

// TestModuleParams_UnknownState — core.exec.runn (module exists, state doesn't).
func TestModuleParams_UnknownState(t *testing.T) {
	src := "- name: t\n  module: core.exec.runn\n  params:\n    cmd: ls\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "module_state_unknown") {
		t.Errorf("expected module_state_unknown, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CustomNamespaceSkipped — a non-core namespace is not validated
// against coremanifest (the custom manifest lives on disk, a separate path).
func TestModuleParams_CustomNamespaceSkipped(t *testing.T) {
	src := "- name: t\n  module: acme.haproxy.running\n  params:\n    whatever: 1\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "unknown_param") || hasCodeP(diags, "missing_required_param") {
		t.Errorf("custom namespace must not be validated by coremanifest: %v", diagCodesP(diags))
	}
}

// TestModuleParams_ScenarioPath — the phase also works through the scenario manifest.
func TestModuleParams_ScenarioPath(t *testing.T) {
	src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { command: \"true\" }\n"
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("expected unknown_param on the scenario path, got %v", diagCodesP(diags))
	}
}

// --- Batch H2: negative cases on the replicated modules. ---

// TestModuleParams_ServiceUnknownParam — the `enabledd` typo on core.service.running.
func TestModuleParams_ServiceUnknownParam(t *testing.T) {
	src := "- name: t\n  module: core.service.running\n  params:\n    name: nginx\n    enabledd: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("expected unknown_param for enabledd:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_GitMissingRequired — core.git.cloned without the required path.
func TestModuleParams_GitMissingRequired(t *testing.T) {
	src := "- name: t\n  module: core.git.cloned\n  params:\n    repo: https://x/y.git\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("expected missing_required_param for path, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CronMissingPerState — core.cron.present without schedule/command
// (per-state required params come from the manifest, not a hardcoded Validate).
func TestModuleParams_CronMissingPerState(t *testing.T) {
	src := "- name: t\n  module: core.cron.present\n  params:\n    name: nightly\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("expected missing_required_param for schedule/command, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_UserNewParamsValid — the new system/group params (commit 2b2c4cc)
// are recognized as valid on core.user.present (not unknown_param).
func TestModuleParams_UserNewParamsValid(t *testing.T) {
	src := "- name: t\n  module: core.user.present\n  params:\n    name: redis\n    system: true\n    group: redis\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("system/group of core.user should be valid, got %v", diags)
	}
}

// TestModuleParams_KeeperSoulRegistered — keeper-side core.soul.registered: the
// 3-segment address ns=core/mod=soul/state=registered resolves to the manifest
// shared/coremanifest/soul.yaml. The valid form (sid+coven) passes, an unknown
// param is caught.
func TestModuleParams_KeeperSoulRegistered(t *testing.T) {
	valid := "- name: t\n  on: keeper\n  module: core.soul.registered\n  params:\n    sid: host.example.com\n    coven: [prod]\n    mode: append\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(valid), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("valid core.soul.registered produced errors: %v", diags)
	}
	bad := "- name: t\n  on: keeper\n  module: core.soul.registered\n  params:\n    sid: host.example.com\n    coven: [prod]\n    covenn: oops\n"
	_, diags, _ = LoadDestinyTasksFromBytes("tasks/main.yml", []byte(bad), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("expected unknown_param for covenn:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_KeeperSoulRegistered_AwaitFields — onboarding barrier
// (ADR-061): the new await fields pass static validation of the author form; an
// sid list via a CEL expression from a previous step's register is not rejected
// by the type check (a CEL value is not statically typed, ADR-010).
func TestModuleParams_KeeperSoulRegistered_AwaitFields(t *testing.T) {
	valid := "- name: provision\n  on: keeper\n  module: core.exec.run\n  register: provision\n" +
		"  changed_when: \"false\"\n" +
		"  params:\n    cmd: echo\n    args: [ok]\n" +
		"- name: t\n  on: keeper\n  module: core.soul.registered\n  register: r\n" +
		"  params:\n" +
		"    sid: \"${ register.provision.stdout }\"\n" +
		"    coven: [redis, prod]\n" +
		"    await_online: true\n" +
		"    await_timeout: 10m\n" +
		"    await_min_count: 3\n" +
		"    await_poll_interval: 2s\n" +
		"    refresh_soulprint: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(valid), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("valid await form produced errors: %v", diags)
	}
}
