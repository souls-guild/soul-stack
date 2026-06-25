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

// TestModuleParams_ExecCommandTypo — `command:` вместо `cmd:` у core.exec.run:
// ловится и как unknown_param (command неизвестен), и как missing_required_param
// (cmd обязателен и не передан). Ключевой негативный кейс ТЗ.
func TestModuleParams_ExecCommandTypo(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    command: \"true\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("ожидался unknown_param для command:, got %v", diagCodesP(diags))
	}
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("ожидался missing_required_param для cmd, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_ExecValid — корректный core.exec.run (cmd + args) проходит.
func TestModuleParams_ExecValid(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: install\n    args: [\"-d\", \"/var/run/x\"]\n    creates: /var/run/x\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("валидный core.exec.run дал ошибки: %v", diags)
	}
}

// TestModuleParams_FileUnknownParam — неизвестный param у core.file.present.
func TestModuleParams_FileUnknownParam(t *testing.T) {
	src := "- name: t\n  module: core.file.present\n  params:\n    path: /etc/x\n    contnet: hi\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("ожидался unknown_param для contnet:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_FileMissingPath — core.file.absent без обязательного path.
func TestModuleParams_FileMissingPath(t *testing.T) {
	src := "- name: t\n  module: core.file.absent\n  params: {}\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("ожидался missing_required_param для path, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_RenderedAuthorForm — author-форма core.file.rendered
// (template:+vars:) валидна; template_content/render_context тут НЕ ожидаются
// (это runtime-форма Keeper-side).
func TestModuleParams_RenderedAuthorForm(t *testing.T) {
	src := "- name: t\n  module: core.file.rendered\n  params:\n    path: /etc/x.conf\n    template: templates/x.conf.tmpl\n    mode: \"0640\"\n    vars:\n      a: \"${ input.a }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("author-форма core.file.rendered дала ошибки: %v", diags)
	}
}

// TestModuleParams_RenderedMissingTemplate — rendered без template: → ошибка.
func TestModuleParams_RenderedMissingTemplate(t *testing.T) {
	src := "- name: t\n  module: core.file.rendered\n  params:\n    path: /etc/x.conf\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("ожидался missing_required_param для template, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_TypeMismatch — args должен быть list; строка → param_type_mismatch.
func TestModuleParams_TypeMismatch(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: ls\n    args: \"not a list\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("ожидался param_type_mismatch для args, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CELWrappedSkipsTypeCheck — args как `${ … }` (CEL non-string
// результат) не должен ловиться type-check-ом: рантайм-тип статически неизвестен.
func TestModuleParams_CELWrappedSkipsTypeCheck(t *testing.T) {
	src := "- name: t\n  module: core.exec.run\n  params:\n    cmd: ls\n    args: \"${ input.args }\"\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("CEL-обёрнутый args не должен давать ошибок: %v", diags)
	}
}

// TestModuleParams_UnknownState — core.exec.runn (модуль есть, state нет).
func TestModuleParams_UnknownState(t *testing.T) {
	src := "- name: t\n  module: core.exec.runn\n  params:\n    cmd: ls\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "module_state_unknown") {
		t.Errorf("ожидался module_state_unknown, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CustomNamespaceSkipped — non-core namespace не валидируется
// против coremanifest (custom-manifest лежит на диске, отдельный путь).
func TestModuleParams_CustomNamespaceSkipped(t *testing.T) {
	src := "- name: t\n  module: wb.haproxy.running\n  params:\n    whatever: 1\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "unknown_param") || hasCodeP(diags, "missing_required_param") {
		t.Errorf("custom-namespace не должен валидироваться coremanifest-ом: %v", diagCodesP(diags))
	}
}

// TestModuleParams_ScenarioPath — фаза работает и через scenario-манифест.
func TestModuleParams_ScenarioPath(t *testing.T) {
	src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { command: \"true\" }\n"
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("ожидался unknown_param в scenario-пути, got %v", diagCodesP(diags))
	}
}

// --- Тираж H2: негативы на тиражированных модулях. ---

// TestModuleParams_ServiceUnknownParam — опечатка `enabledd` у core.service.running.
func TestModuleParams_ServiceUnknownParam(t *testing.T) {
	src := "- name: t\n  module: core.service.running\n  params:\n    name: nginx\n    enabledd: true\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("ожидался unknown_param для enabledd:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_GitMissingRequired — core.git.cloned без обязательного path.
func TestModuleParams_GitMissingRequired(t *testing.T) {
	src := "- name: t\n  module: core.git.cloned\n  params:\n    repo: https://x/y.git\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("ожидался missing_required_param для path, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_CronMissingPerState — core.cron.present без schedule/command
// (per-state required приходят из manifest, не из захардкоженного Validate).
func TestModuleParams_CronMissingPerState(t *testing.T) {
	src := "- name: t\n  module: core.cron.present\n  params:\n    name: nightly\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "missing_required_param") {
		t.Errorf("ожидался missing_required_param для schedule/command, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_UserNewParamsValid — новые params system/group (коммит 2b2c4cc)
// признаются валидными у core.user.present (а не unknown_param).
func TestModuleParams_UserNewParamsValid(t *testing.T) {
	src := "- name: t\n  module: core.user.present\n  params:\n    name: redis\n    system: true\n    group: redis\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("system/group у core.user должны быть валидны, got %v", diags)
	}
}

// TestModuleParams_KeeperSoulRegistered — keeper-side core.soul.registered:
// 3-сегментный адрес ns=core/mod=soul/state=registered резолвится в манифест
// shared/coremanifest/soul.yaml. Валидная форма (sid+coven) проходит, неизвестный
// param ловится.
func TestModuleParams_KeeperSoulRegistered(t *testing.T) {
	valid := "- name: t\n  on: keeper\n  module: core.soul.registered\n  params:\n    sid: host.example.com\n    coven: [prod]\n    mode: append\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(valid), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("валидный core.soul.registered дал ошибки: %v", diags)
	}
	bad := "- name: t\n  on: keeper\n  module: core.soul.registered\n  params:\n    sid: host.example.com\n    coven: [prod]\n    covenn: oops\n"
	_, diags, _ = LoadDestinyTasksFromBytes("tasks/main.yml", []byte(bad), ValidateOptions{})
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("ожидался unknown_param для covenn:, got %v", diagCodesP(diags))
	}
}

// TestModuleParams_KeeperSoulRegistered_AwaitFields — барьер онбординга
// (ADR-061): новые await-поля проходят статическую валидацию author-формы;
// sid-список через CEL-выражение от register предыдущего шага не реджектится
// type-check-ом (CEL-значение статически не типизируется, ADR-010).
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
		t.Fatalf("валидная await-форма дала ошибки: %v", diags)
	}
}
