package config

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// paramsNode parses a bare params mapping (two keys, so goccy yields a
// MappingNode rather than a single MappingValueNode).
func paramsNode(t *testing.T, src string) *ast.MappingNode {
	t.Helper()
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n, ok := f.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		t.Fatalf("body is %T, want *ast.MappingNode", f.Docs[0].Body)
	}
	return n
}

func deprecatedStateDef() plugin.StateDef {
	return plugin.StateDef{Input: map[string]plugin.InputParamDef{
		"addr": {Type: "string"},
		"address": {
			Type:       "string",
			Deprecated: &plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr"},
		},
	}}
}

// TestDeprecatedParam_WarnsWithoutFailing — the half of the policy that keeps a
// contract from breaking everything at once: a deprecated param is still
// honored, so the definition stays valid and the author is merely told. An
// error here would make deprecation indistinguishable from removal.
func TestDeprecatedParam_WarnsWithoutFailing(t *testing.T) {
	diags := checkUnknownAndType(deprecatedStateDef(), paramsNode(t, "address: 127.0.0.1\nx: 1\n"), "$.tasks[0]")

	var found *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == "deprecated_param" {
			found = &diags[i]
		}
	}
	if found == nil {
		t.Fatalf("no deprecated_param diagnostic: %v", diagCodesP(diags))
	}
	if found.Level != diag.LevelWarning {
		t.Errorf("level = %v, want warning - an error would break a definition that still works", found.Level)
	}
	// The unrelated `x:` is genuinely unknown and must still be an error; the
	// deprecation must not soften the rest of the check.
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("unknown_param was lost alongside the deprecation: %v", diagCodesP(diags))
	}
	if !strings.Contains(found.Message, "0.6.0") || !strings.Contains(found.Message, "addr") {
		t.Errorf("message does not carry the deadline and the replacement: %q", found.Message)
	}
	if found.YAMLPath != "$.tasks[0].params.address" {
		t.Errorf("YAMLPath = %q", found.YAMLPath)
	}
}

// TestDeprecatedParam_LiveParamStaysSilent — only a param carrying the block is
// reported; nothing else in the state acquires a warning.
func TestDeprecatedParam_LiveParamStaysSilent(t *testing.T) {
	diags := checkUnknownAndType(deprecatedStateDef(), paramsNode(t, "addr: 127.0.0.1\naddress: x\n"), "$.t")
	n := 0
	for _, d := range diags {
		if d.Code == "deprecated_param" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("deprecated_param count = %d, want exactly 1 (only `address` is deprecated)", n)
	}
}

// TestDeprecatedParam_RemovalBecomesUnknown — the end of the window: once the
// key leaves the manifest, the very same task text is an error. This is the
// pair that makes strictness safe — deprecation first, rejection later, never
// rejection outright.
func TestDeprecatedParam_RemovalBecomesUnknown(t *testing.T) {
	after := plugin.StateDef{Input: map[string]plugin.InputParamDef{"addr": {Type: "string"}}}
	diags := checkUnknownAndType(after, paramsNode(t, "address: 127.0.0.1\naddr: y\n"), "$.t")
	if !hasCodeP(diags, "unknown_param") {
		t.Errorf("a removed param did not become unknown_param: %v", diagCodesP(diags))
	}
	if hasCodeP(diags, "deprecated_param") {
		t.Error("a removed param still reports as deprecated")
	}
}
