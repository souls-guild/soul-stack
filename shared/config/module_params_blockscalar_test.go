package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// H2 follow-up — block-scalar in module-params. goccy parses folded (`>`) and
// literal (`|`) block-scalars as *ast.LiteralNode, not *ast.StringNode.
// astMatchesType (module_params.go) accepts both forms as a string for
// type=string; for non-string types LiteralNode still does not match. These two
// tests lock down both sides:
// (1) the relaxation did NOT break a valid multi-line string param;
// (2) the relaxation did NOT leak into int — the type-check STILL fires there.

// TestModuleParams_BlockScalarStringParam_NoMismatch — core.cmd.shell with a
// multi-line literal block-scalar cmd: |. cmd is declared type:string,required —
// without the LiteralNode branch in astMatchesType this form would be falsely
// rejected as param_type_mismatch. We check it is NOT rejected (and no error at all).
func TestModuleParams_BlockScalarStringParam_NoMismatch(t *testing.T) {
	// cmd: |  — literal block-scalar; body indented 6 spaces under the key.
	src := "- name: t\n" +
		"  module: core.cmd.shell\n" +
		"  params:\n" +
		"    cmd: |\n" +
		"      set -e\n" +
		"      echo step1\n" +
		"      echo step2\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("block-scalar string-param gave param_type_mismatch (LiteralNode not recognized as string): %v", diagCodesP(diags))
	}
	if diag.HasErrors(diags) {
		t.Fatalf("valid core.cmd.shell with cmd: |  gave errors: %v", diags)
	}
}

// TestModuleParams_BlockScalarFoldedStringParam_NoMismatch — the same check for
// the folded (`>`) block-scalar form, which goccy also returns as LiteralNode.
func TestModuleParams_BlockScalarFoldedStringParam_NoMismatch(t *testing.T) {
	src := "- name: t\n" +
		"  module: core.cmd.shell\n" +
		"  params:\n" +
		"    cmd: >\n" +
		"      echo one\n" +
		"      two\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("folded block-scalar string-param gave param_type_mismatch: %v", diagCodesP(diags))
	}
	if diag.HasErrors(diags) {
		t.Fatalf("valid core.cmd.shell with cmd: >  gave errors: %v", diags)
	}
}

// TestModuleParams_BlockScalarIntParam_StillMismatches — a block-scalar in an int
// field (core.firewall.present.port, type:int,required) is STILL caught as
// param_type_mismatch. This guards against regression: the LiteralNode branch in
// astMatchesType was added ONLY for type=string; the int branch expects
// *ast.IntegerNode, and a block-scalar (LiteralNode) is a string, not a number. If
// someone relaxes astMatchesType to accept LiteralNode universally, this test fails.
func TestModuleParams_BlockScalarIntParam_StillMismatches(t *testing.T) {
	// port: |  — literal block-scalar in an int field: must remain a mismatch.
	src := "- name: t\n" +
		"  module: core.firewall.present\n" +
		"  params:\n" +
		"    port: |\n" +
		"      8080\n"
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeP(diags, "param_type_mismatch") {
		t.Errorf("block-scalar in int field did NOT give param_type_mismatch - the int branch of astMatchesType was weakened: %v", diagCodesP(diags))
	}
}
