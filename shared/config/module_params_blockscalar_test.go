package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// H2 follow-up — block-scalar в module-params. goccy парсит folded (`>`) и literal
// (`|`) block-scalar как *ast.LiteralNode, а не *ast.StringNode. astMatchesType
// (module_params.go) для type=string признаёт обе формы строкой; для НЕ-string
// типов LiteralNode по-прежнему не совпадает. Эти два теста закрепляют обе стороны:
// (1) ослабление НЕ сломало валидный многострочный string-param;
// (2) ослабление НЕ протекло на int — type-check там ВСЁ ЕЩЁ срабатывает.

// TestModuleParams_BlockScalarStringParam_NoMismatch — core.cmd.shell с cmd: |
// многострочным literal block-scalar. cmd объявлен type:string,required — без
// LiteralNode-ветки в astMatchesType такая форма ложно реджектилась бы как
// param_type_mismatch. Проверяем, что НЕ реджектится (и ни одной ошибки вообще).
func TestModuleParams_BlockScalarStringParam_NoMismatch(t *testing.T) {
	// cmd: |  — literal block-scalar; тело с отступом 6 пробелов под ключом.
	src := "- name: t\n" +
		"  module: core.cmd.shell\n" +
		"  params:\n" +
		"    cmd: |\n" +
		"      set -e\n" +
		"      echo step1\n" +
		"      echo step2\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("block-scalar string-param дал param_type_mismatch (LiteralNode не признан строкой): %v", diagCodesP(diags))
	}
	if diag.HasErrors(diags) {
		t.Fatalf("валидный core.cmd.shell с cmd: |  дал ошибки: %v", diags)
	}
}

// TestModuleParams_BlockScalarFoldedStringParam_NoMismatch — та же проверка для
// folded (`>`) формы block-scalar, которую goccy тоже отдаёт как LiteralNode.
func TestModuleParams_BlockScalarFoldedStringParam_NoMismatch(t *testing.T) {
	src := "- name: t\n" +
		"  module: core.cmd.shell\n" +
		"  params:\n" +
		"    cmd: >\n" +
		"      echo one\n" +
		"      two\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("folded block-scalar string-param дал param_type_mismatch: %v", diagCodesP(diags))
	}
	if diag.HasErrors(diags) {
		t.Fatalf("валидный core.cmd.shell с cmd: >  дал ошибки: %v", diags)
	}
}

// TestModuleParams_BlockScalarIntParam_StillMismatches — block-scalar в int-поле
// (core.firewall.present.port, type:int,required) ВСЁ ЕЩЁ ловится как
// param_type_mismatch. Это защита от регрессии: LiteralNode-ветка в astMatchesType
// добавлена ТОЛЬКО для type=string; int-ветка ждёт *ast.IntegerNode, а block-scalar
// (LiteralNode) — строка, не число. Если кто-то ослабит astMatchesType, признав
// LiteralNode универсально валидным, этот тест упадёт.
func TestModuleParams_BlockScalarIntParam_StillMismatches(t *testing.T) {
	// port: |  — literal block-scalar в int-поле: должно остаться mismatch.
	src := "- name: t\n" +
		"  module: core.firewall.present\n" +
		"  params:\n" +
		"    port: |\n" +
		"      8080\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("block-scalar в int-поле НЕ дал param_type_mismatch — int-ветку astMatchesType ослабили: %v", diagCodesP(diags))
	}
}
